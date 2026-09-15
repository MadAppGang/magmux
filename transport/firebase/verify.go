package firebase

// Command verification: six checks, in the order that makes the cheapest
// refusal happen first and the expensive one happen only to a well-formed
// request from a known owner.
//
// The security rules already refuse a write from a uid that is not in the owner
// list, so in a correctly deployed database nothing forged ever reaches this
// file. It still runs every check, because magmux authenticates as an ADMIN and
// admin bypasses rules: a command written by magmux's own credential — a bug, a
// compromised service account, a mis-set emulator — would sail past the rules
// and arrive here as the only thing standing between it and a shell.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// sigPrefix versions the canonical string. A key is a long-lived secret and the
// canonical string is the only thing that gives it meaning, so changing the
// field order without changing this prefix would silently make old signatures
// verify against a new meaning.
const sigPrefix = "magmux.cmd.v1"

// skew is how far a command's timestamp may be from now, either way. Two
// minutes covers an unsynchronised phone and is far short of a useful replay
// window.
const skew = 120 * time.Second

// nonceCap is how many nonces are remembered. 10k at a few commands a minute is
// weeks of history; the LRU is what stops it being unbounded.
const nonceCap = 10000

// command is one entry under `commands/{pushId}`.
//
// Args is a STRING and not a json.RawMessage: the layout stores every free-form
// payload as a JSON string so a plugin's `$ref` cannot become an RTDB key, and
// the signature is over that exact string. Decoding it to re-encode it would
// mean signing one spelling and verifying another.
type command struct {
	UID   string `json:"uid"`
	TS    any    `json:"ts"`
	Nonce string `json:"nonce"`
	Op    string `json:"op"`
	Args  string `json:"args"`
	Sig   string `json:"sig"`
}

// CanonicalString is what the HMAC covers.
//
// `args` is LAST on purpose. It is the only field whose content is
// attacker-chosen and unbounded, so a newline inside it cannot shift a later
// field into a different position — there is no later field. Every other field
// has a grammar that excludes a newline, checked before this is built.
func CanonicalString(host, sid, uid string, ts int64, nonce, op, args string) string {
	var b strings.Builder
	b.Grow(len(sigPrefix) + len(host) + len(sid) + len(uid) + len(nonce) + len(op) + len(args) + 32)
	b.WriteString(sigPrefix)
	b.WriteByte('\n')
	b.WriteString(host)
	b.WriteByte('\n')
	b.WriteString(sid)
	b.WriteByte('\n')
	b.WriteString(uid)
	b.WriteByte('\n')
	b.WriteString(strconv.FormatInt(ts, 10))
	b.WriteByte('\n')
	b.WriteString(nonce)
	b.WriteByte('\n')
	b.WriteString(op)
	b.WriteByte('\n')
	b.WriteString(args)
	return b.String()
}

// Sign returns the hex HMAC of the canonical string. It is exported because the
// committed test vector and the TS harness both need to produce one, and a
// second implementation of a signing rule is a second implementation to drift.
func Sign(key []byte, host, sid, uid string, ts int64, nonce, op, args string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(CanonicalString(host, sid, uid, ts, nonce, op, args)))
	return hex.EncodeToString(mac.Sum(nil))
}

// verify runs checks (a) through (f) and returns the error a result will carry.
//
// now is a parameter so the timestamp window is testable without sleeping.
func (a *Adapter) verify(cmd *command, now time.Time) (int64, error) {
	// (a) schema.
	ts, err := cmdTS(cmd.TS)
	if err != nil {
		return 0, err
	}
	switch {
	case !validUID(cmd.UID):
		return ts, protocol.Errf(protocol.CodeBadRequest, "uid is not a uid")
	case !validNonce(cmd.Nonce):
		return ts, protocol.Errf(protocol.CodeBadRequest, "nonce must be 16-64 characters of [A-Za-z0-9_-]")
	case !validOpName(cmd.Op):
		return ts, protocol.Errf(protocol.CodeBadRequest, "op is not an op name")
	case !validSig(cmd.Sig):
		return ts, protocol.Errf(protocol.CodeBadRequest, "sig must be 64 lowercase hex characters")
	}

	// (b) the uid is an owner. Same list the rules read, from the same config.
	if !a.cfg.IsOwner(cmd.UID) {
		return ts, protocol.Errf(protocol.CodeUnauthorized, "uid %q is not an owner of this host", cmd.UID)
	}

	// (c) freshness.
	if d := now.Sub(time.UnixMilli(ts)); d > skew || d < -skew {
		return ts, protocol.Errf(protocol.CodeUnauthorized,
			"ts is %s away from now; commands expire after %s", d.Round(time.Second), skew)
	}

	// (d) the nonce is unseen. Checked BEFORE the HMAC so a replay of a valid
	// command costs a map lookup rather than a hash.
	if !a.nonces.add(cmd.Nonce) {
		return ts, protocol.Errf(protocol.CodeUnauthorized, "nonce has already been used")
	}

	// (e) the signature, in constant time.
	want := Sign(a.cfg.key, a.cfg.Host, a.sid, cmd.UID, ts, cmd.Nonce, cmd.Op, cmd.Args)
	if !hmac.Equal([]byte(want), []byte(cmd.Sig)) {
		return ts, protocol.Errf(protocol.CodeUnauthorized, "signature does not match")
	}

	// (f) the op is allowed. Last, because it is the only check whose answer a
	// legitimate owner can trip by asking for something reasonable, and it
	// deserves its own code: forbidden is about permission, unauthorized is
	// about identity.
	if !a.cfg.AllowsOp(cmd.Op) {
		return ts, protocol.Errf(protocol.CodeForbidden, "op %q is not in commands.allowOps", cmd.Op)
	}
	return ts, nil
}

// cmdTS reads the timestamp, which arrives from encoding/json as a float64 and
// from a json.Number-configured decoder as a string.
//
// A float64 is EXACT for every millisecond timestamp until the year 287396, so
// the conversion is lossless here; the string branch exists because a client
// that wrote its own decoder may hand one over, and refusing it would be a
// riddle.
func cmdTS(v any) (int64, error) {
	switch t := v.(type) {
	case float64:
		if t != float64(int64(t)) {
			return 0, protocol.Errf(protocol.CodeBadRequest, "ts must be an integer number of milliseconds")
		}
		return int64(t), nil
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0, protocol.Errf(protocol.CodeBadRequest, "ts must be an integer number of milliseconds")
		}
		return n, nil
	case nil:
		return 0, protocol.Errf(protocol.CodeBadRequest, "ts is required")
	}
	return 0, protocol.Errf(protocol.CodeBadRequest, "ts must be an integer number of milliseconds")
}

// validNonce is [A-Za-z0-9_-]{16,64}.
func validNonce(s string) bool {
	if len(s) < 16 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// validSig is 64 lowercase hex characters. Lowercase only: hex.EncodeToString
// produces lowercase, and accepting both spellings would mean two byte strings
// verify for one signature, which is exactly the kind of flexibility a
// canonical form exists to remove.
func validSig(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validOpName is the op grammar as it appears on the wire: a built-in
// (`open_pane`), or a qualified plugin op (`ticket.run_ticket`). It is the same
// grammar the allowlist uses, minus the glob.
func validOpName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if plugin, op, ok := protocol.SplitQualifiedOp(s); ok {
		return protocol.ValidPluginName(plugin) && protocol.ValidOpName(op)
	}
	return protocol.ValidOpName(s)
}

// ── the nonce LRU ───────────────────────────────────────────────────────────

// nonceLRU remembers which nonces have been used, bounded.
//
// It is a map plus an insertion-ordered ring, not a linked list: nothing here
// promotes on access, because a nonce is used exactly once and the only useful
// eviction order is oldest-first. Once evicted, a nonce could be replayed — but
// only a nonce older than the last 10,000, which the 120-second freshness
// window has already refused.
type nonceLRU struct {
	mu    sync.Mutex
	seen  map[string]int
	order []string
	next  int
	cap   int
}

func newNonceLRU(capacity int) *nonceLRU {
	return &nonceLRU{seen: make(map[string]int, capacity), order: make([]string, capacity), cap: capacity}
}

// add records a nonce and reports whether it was new.
func (l *nonceLRU) add(nonce string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.seen[nonce]; dup {
		return false
	}
	if old := l.order[l.next]; old != "" {
		delete(l.seen, old)
	}
	l.order[l.next] = nonce
	l.seen[nonce] = l.next
	l.next = (l.next + 1) % l.cap
	return true
}

// has reports whether a nonce has been seen, without recording it.
func (l *nonceLRU) has(nonce string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.seen[nonce]
	return ok
}
