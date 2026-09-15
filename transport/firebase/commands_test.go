package firebase

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// ── the committed vector ────────────────────────────────────────────────────

// vector is examples/firebase/hmac-vector.json, which both magmux's Go test and
// any client that signs a command must reproduce.
type vector struct {
	Prefix    string `json:"prefix"`
	Key       string `json:"key"`
	Host      string `json:"host"`
	SID       string `json:"sid"`
	UID       string `json:"uid"`
	TS        int64  `json:"ts"`
	Nonce     string `json:"nonce"`
	Op        string `json:"op"`
	Args      string `json:"args"`
	Canonical string `json:"canonical"`
	Sig       string `json:"sig"`
}

func loadVector(t *testing.T) vector {
	t.Helper()
	var v vector
	readJSON(t, filepath.Join("..", "..", "examples", "firebase", "hmac-vector.json"), &v)
	return v
}

// TestCommittedHMACVector is the interop contract. A TS client, a shell script
// and magmux all have to agree on one string and one hex digest, and the file
// is where they agree.
func TestCommittedHMACVector(t *testing.T) {
	v := loadVector(t)
	if v.Prefix != sigPrefix {
		t.Fatalf("the vector's prefix is %q; this build signs %q", v.Prefix, sigPrefix)
	}
	got := CanonicalString(v.Host, v.SID, v.UID, v.TS, v.Nonce, v.Op, v.Args)
	if got != v.Canonical {
		t.Fatalf("canonical string:\n got %q\nwant %q", got, v.Canonical)
	}
	if sig := Sign([]byte(v.Key), v.Host, v.SID, v.UID, v.TS, v.Nonce, v.Op, v.Args); sig != v.Sig {
		t.Fatalf("sig = %s, want %s", sig, v.Sig)
	}
	// The canonical string is eight lines, and `args` is the last of them — so
	// a newline inside args cannot shift a later field, because there is none.
	if n := strings.Count(v.Canonical, "\n"); n != 7 {
		t.Fatalf("the canonical string has %d newlines; it must have 7", n)
	}
	if !strings.HasSuffix(v.Canonical, v.Args) {
		t.Fatal("args is not last in the canonical string")
	}
}

// TestArgsAreSignedAsTheirWireSpelling: two JSON spellings of the same object
// are two different signatures, and the one that counts is the string stored in
// the database. A verifier that parsed and re-encoded would accept a command
// nobody signed.
func TestArgsAreSignedAsTheirWireSpelling(t *testing.T) {
	v := loadVector(t)
	respelled := `{ "all" : true }`
	if respelled == v.Args {
		t.Fatal("the two spellings must differ for this test to mean anything")
	}
	a := Sign([]byte(v.Key), v.Host, v.SID, v.UID, v.TS, v.Nonce, v.Op, v.Args)
	b := Sign([]byte(v.Key), v.Host, v.SID, v.UID, v.TS, v.Nonce, v.Op, respelled)
	if a == b {
		t.Fatal("re-spelling args did not change the signature")
	}
}

// ── the harness ─────────────────────────────────────────────────────────────

// sideEffects records what the ops did, in order, alongside what the database
// was told. The two streams are interleaved into ONE log so an assertion can be
// about ORDER rather than about counts.
type sideEffects struct {
	mu  sync.Mutex
	log []string
}

func (s *sideEffects) note(what string) {
	s.mu.Lock()
	s.log = append(s.log, what)
	s.mu.Unlock()
}

func (s *sideEffects) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

func (s *sideEffects) count(prefix string) int {
	n := 0
	for _, e := range s.all() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// cmdFixture is one adapter wired to one fake database, with a hub carrying
// three ops: a read, a control, and a lane-bound `send`.
type cmdFixture struct {
	t     *testing.T
	f     *fakeRTDB
	a     *Adapter
	h     *hub.Hub
	fx    *sideEffects
	key   []byte
	host  string
	sid   string
	sess  string
	nonce int
}

func newCmdFixture(t *testing.T, sid string, tweak func(*Config)) *cmdFixture {
	t.Helper()
	f := newFakeRTDB(t)
	fx := &sideEffects{}

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "cmd.key")
	key := []byte("magmux-test-key-do-not-use-in-production")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Root: "magmux", Host: "test-host",
		Emulator: &EmulatorConfig{Host: f.Host(), NS: "demo-magmux"},
		Commands: CommandsConfig{
			Enabled:  true,
			Owners:   []string{"owner-uid-1"},
			KeyFile:  keyPath,
			AllowOps: []string{"list", "send", "ticket.*"},
		},
	}
	if tweak != nil {
		tweak(cfg)
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}

	h := hub.New()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(h.Register(protocol.SourceBuiltin,
		hub.Op{
			Spec: protocol.OpSpec{Name: "list", Class: protocol.ClassRead},
			Fn: func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
				fx.note("run list by " + c.Client)
				return map[string]any{"panes": []any{}}, nil
			},
		},
		hub.Op{
			Spec: protocol.OpSpec{Name: "send", Class: protocol.ClassControl},
			Fn: func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
				var a struct {
					Pane int    `json:"pane"`
					Text string `json:"text"`
				}
				_ = json.Unmarshal(args, &a)
				fx.note("pty write " + a.Text)
				return map[string]any{"sent": true}, nil
			},
		},
		hub.Op{
			Spec: protocol.OpSpec{Name: "open_pane", Class: protocol.ClassControl},
			Fn: func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
				fx.note("open_pane")
				return map[string]any{"pane": 9}, nil
			},
		},
	))

	a, err := New(cfg, Options{
		Hub:       h,
		SID:       sid,
		PID:       4242,
		Version:   "test",
		Aggregate: func() []byte { return nil },
		Log:       func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The real Run loop, at a pace a test can wait on.
	a.mirror.tick = 20 * time.Millisecond
	a.Start()
	t.Cleanup(a.Close)

	// The claim is the one write that is NOT the flusher's, and the assertions
	// below are about its position relative to a side effect. Recording it from
	// the server is what makes that observable on the wire.
	f.SetStatus(func(_ int, rec patchRecord) int {
		for path, v := range rec.Payload {
			if strings.HasPrefix(path, "results/") && strings.Contains(string(v), `"claimed"`) {
				fx.note("claim acked " + strings.TrimPrefix(path, "results/"))
			}
		}
		return 0
	})

	fixture := &cmdFixture{t: t, f: f, a: a, h: h, fx: fx, key: key,
		host: cfg.Host, sid: sid, sess: a.sess}
	fixture.waitForListener()
	return fixture
}

func (c *cmdFixture) waitForListener() {
	c.t.Helper()
	waitFor(c.t, 3*time.Second, "the command listener to attach", func() bool {
		return c.f.StreamsOpened() >= 1
	})
}

// signed builds a well-formed command, signed with the fixture's key. Each one
// gets its own nonce: a nonce is single-use by design, so a helper that reused
// one would refuse the second command in every test for the wrong reason.
func (c *cmdFixture) signed(op, args string) map[string]any {
	ts := time.Now().UnixMilli()
	c.nonce++
	nonce := fmt.Sprintf("nonce-%08d-fixture-%s", c.nonce, strings.ReplaceAll(op, ".", "-"))
	if len(nonce) > 64 {
		nonce = nonce[:64]
	}
	return map[string]any{
		"uid": "owner-uid-1", "ts": ts, "nonce": nonce, "op": op, "args": args,
		"sig": Sign(c.key, c.host, c.sid, "owner-uid-1", ts, nonce, op, args),
	}
}

// put writes a command into the database the way an owner's client would, so a
// test can then assert that magmux DELETED it.
func (c *cmdFixture) put(pushId string, cmd map[string]any) {
	c.t.Helper()
	b, _ := json.Marshal(cmd)
	c.f.mu.Lock()
	c.f.data[c.sess+"/commands/"+pushId] = b
	c.f.mu.Unlock()
}

// deliver writes the command and announces it on the stream, as RTDB does.
func (c *cmdFixture) deliver(pushId string, cmd map[string]any) {
	c.put(pushId, cmd)
	c.f.Emit("put", map[string]any{"path": "/" + pushId, "data": cmd})
}

// result reads back the result record for one pushId.
func (c *cmdFixture) result(pushId string) map[string]any {
	v, _ := c.f.Value(c.sess + "/results/" + pushId).(map[string]any)
	return v
}

func (c *cmdFixture) waitForResult(pushId string) map[string]any {
	c.t.Helper()
	var out map[string]any
	waitFor(c.t, 3*time.Second, "a result for "+pushId, func() bool {
		r := c.result(pushId)
		if r == nil || r["state"] != "done" {
			return false
		}
		out = r
		return true
	})
	return out
}

// ── the checks ──────────────────────────────────────────────────────────────

// TestCommandVerificationTable walks every refusal, each against a command that
// is otherwise perfect. The rules layer already refuses a non-owner; this is the
// layer that has to hold when the rules are bypassed, which they always are for
// magmux's own admin credential.
func TestCommandVerificationTable(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000000", nil)
	now := time.Now()

	// Each row gets its own nonce: they are single-use, so a shared one would
	// refuse every row after the first for the wrong reason.
	n := 0
	base := func() *command {
		n++
		ts := now.UnixMilli()
		nonce := fmt.Sprintf("table-nonce-%08d", n)
		args := `{"all":true}`
		return &command{
			UID: "owner-uid-1", TS: float64(ts), Nonce: nonce, Op: "list", Args: args,
			Sig: Sign(c.key, c.host, c.sid, "owner-uid-1", ts, nonce, "list", args),
		}
	}

	cases := []struct {
		name string
		edit func(*command)
		code string
	}{
		{"a well-formed command from an owner", func(*command) {}, ""},
		{"a uid that is not an owner", func(k *command) {
			k.UID = "somebody-else"
			k.Sig = Sign(c.key, c.host, c.sid, k.UID, int64(k.TS.(float64)), k.Nonce, k.Op, k.Args)
		}, protocol.CodeUnauthorized},
		{"a signature from the wrong key", func(k *command) {
			k.Sig = Sign([]byte("another key entirely"), c.host, c.sid,
				k.UID, int64(k.TS.(float64)), k.Nonce, k.Op, k.Args)
		}, protocol.CodeUnauthorized},
		{"a signature for a different sid", func(k *command) {
			k.Sig = Sign(c.key, c.host, "magmux-9999999999",
				k.UID, int64(k.TS.(float64)), k.Nonce, k.Op, k.Args)
		}, protocol.CodeUnauthorized},
		{"a signature for a different host", func(k *command) {
			k.Sig = Sign(c.key, "another-host", c.sid,
				k.UID, int64(k.TS.(float64)), k.Nonce, k.Op, k.Args)
		}, protocol.CodeUnauthorized},
		{"args tampered with after signing", func(k *command) {
			k.Args = `{"all":false}`
		}, protocol.CodeUnauthorized},
		{"a timestamp two hours old", func(k *command) {
			ts := now.Add(-2 * time.Hour).UnixMilli()
			k.TS = float64(ts)
			k.Sig = Sign(c.key, c.host, c.sid, k.UID, ts, k.Nonce, k.Op, k.Args)
		}, protocol.CodeUnauthorized},
		{"a timestamp two hours in the future", func(k *command) {
			ts := now.Add(2 * time.Hour).UnixMilli()
			k.TS = float64(ts)
			k.Sig = Sign(c.key, c.host, c.sid, k.UID, ts, k.Nonce, k.Op, k.Args)
		}, protocol.CodeUnauthorized},
		{"an op outside allowOps", func(k *command) {
			k.Op = "open_pane"
			k.Sig = Sign(c.key, c.host, c.sid, k.UID, int64(k.TS.(float64)), k.Nonce, k.Op, k.Args)
		}, protocol.CodeForbidden},
		{"a nonce that is too short", func(k *command) {
			k.Nonce = "short"
			k.Sig = Sign(c.key, c.host, c.sid, k.UID, int64(k.TS.(float64)), k.Nonce, k.Op, k.Args)
		}, protocol.CodeBadRequest},
		{"a nonce with a slash in it", func(k *command) {
			k.Nonce = "aaaa/aaaaaaaaaa1234"
			k.Sig = Sign(c.key, c.host, c.sid, k.UID, int64(k.TS.(float64)), k.Nonce, k.Op, k.Args)
		}, protocol.CodeBadRequest},
		{"a uid with a newline, which could forge a canonical line", func(k *command) {
			k.UID = "owner\nuid"
		}, protocol.CodeBadRequest},
		{"a signature that is not hex", func(k *command) { k.Sig = strings.Repeat("z", 64) }, protocol.CodeBadRequest},
		{"a signature of the wrong length", func(k *command) { k.Sig = "abcd" }, protocol.CodeBadRequest},
		{"an uppercase hex signature, which is a second spelling", func(k *command) {
			k.Sig = strings.ToUpper(k.Sig)
		}, protocol.CodeBadRequest},
		{"an op name that is not an op name", func(k *command) { k.Op = "../../etc/passwd" }, protocol.CodeBadRequest},
		{"no timestamp at all", func(k *command) { k.TS = nil }, protocol.CodeBadRequest},
		{"a fractional timestamp", func(k *command) { k.TS = 1.5 }, protocol.CodeBadRequest},
	}

	for _, tc := range cases {
		k := base()
		tc.edit(k)
		_, err := c.a.verify(k, now)
		got := protocol.CodeOf(err)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", tc.name, err)
			}
			continue
		}
		if got != tc.code {
			t.Errorf("%s: code = %q (%v), want %q", tc.name, got, err, tc.code)
		}
	}
}

// TestNonceIsSingleUse: the same command replayed is refused, which is what the
// timestamp window alone cannot do.
func TestNonceIsSingleUse(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000001", nil)
	now := time.Now()
	ts := now.UnixMilli()
	nonce := "replay-nonce-0123456"
	args := `{}`
	k := &command{UID: "owner-uid-1", TS: float64(ts), Nonce: nonce, Op: "list", Args: args,
		Sig: Sign(c.key, c.host, c.sid, "owner-uid-1", ts, nonce, "list", args)}
	if _, err := c.a.verify(k, now); err != nil {
		t.Fatalf("the first use was refused: %v", err)
	}
	if _, err := c.a.verify(k, now); protocol.CodeOf(err) != protocol.CodeUnauthorized {
		t.Fatalf("a replay was accepted: %v", err)
	}
}

// TestNonceLRUIsBounded: the LRU forgets the oldest rather than growing without
// limit. A forgotten nonce is older than the whole ring, which the 120-second
// freshness window has already refused.
func TestNonceLRUIsBounded(t *testing.T) {
	l := newNonceLRU(4)
	for _, n := range []string{"a", "b", "c", "d"} {
		if !l.add(n) {
			t.Fatalf("%s was refused on first use", n)
		}
	}
	if l.add("a") {
		t.Fatal("a was forgotten while still in the ring")
	}
	l.add("e") // evicts "a"
	if l.has("a") {
		t.Fatal("the oldest entry was not evicted")
	}
	if !l.has("b") || !l.has("e") {
		t.Fatal("the LRU evicted the wrong entries")
	}
}

// TestSignedCommandRuns is the happy path over the whole stack: an SSE event,
// verification, a claim, the op, a result, and the command deleted.
func TestSignedCommandRuns(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000002", nil)
	c.deliver("-Nabc001", c.signed("list", `{}`))

	res := c.waitForResult("-Nabc001")
	if res["ok"] != true {
		t.Fatalf("result = %v", res)
	}
	if s, _ := res["result"].(string); !strings.Contains(s, "panes") {
		t.Errorf("the op's result did not reach the database as a JSON string: %v", res["result"])
	}
	if c.fx.count("run list") != 1 {
		t.Fatalf("the op ran %d times: %v", c.fx.count("run list"), c.fx.all())
	}
	// The command is deleted, and by the same PATCH that wrote the result.
	waitFor(t, 2*time.Second, "the command to be deleted", func() bool {
		return c.f.Value(c.sess+"/commands/-Nabc001") == nil
	})
	// The caller identity magmux saw is the uid, not something self-declared.
	found := false
	for _, e := range c.fx.all() {
		if e == "run list by owner-uid-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("the op was not told the owner uid: %v", c.fx.all())
	}
}

// TestForgedCommandIsRejectedAndDeleted: an owner uid with a bad HMAC gets an
// `unauthorized` result and the command goes, so it cannot sit in the database
// being retried by a listener that restarts.
func TestForgedCommandIsRejectedAndDeleted(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000003", nil)
	cmd := c.signed("list", `{}`)
	cmd["sig"] = strings.Repeat("0", 64) // right shape, wrong value
	c.deliver("-Nforged1", cmd)

	res := c.waitForResult("-Nforged1")
	if res["ok"] != false || res["code"] != protocol.CodeUnauthorized {
		t.Fatalf("result = %v; want ok:false unauthorized", res)
	}
	if c.fx.count("run list") != 0 {
		t.Fatalf("a forged command ran: %v", c.fx.all())
	}
	waitFor(t, 2*time.Second, "the forged command to be deleted", func() bool {
		return c.f.Value(c.sess+"/commands/-Nforged1") == nil
	})
	// And no claim was ever written: nothing ran, so nothing is "outcome
	// unknown".
	if c.fx.count("claim acked") != 0 {
		t.Fatalf("a rejected command was claimed: %v", c.fx.all())
	}
}

// TestCommandClaimPrecedesSideEffect is at-most-once, in one assertion: the
// database has ACKNOWLEDGED the claim before the op touches anything.
func TestCommandClaimPrecedesSideEffect(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000004", nil)
	c.deliver("-Nclaim01", c.signed("send", `{"pane":0,"text":"hello"}`))
	c.waitForResult("-Nclaim01")

	log := c.fx.all()
	claim, effect := -1, -1
	for i, e := range log {
		if e == "claim acked -Nclaim01" && claim < 0 {
			claim = i
		}
		if e == "pty write hello" && effect < 0 {
			effect = i
		}
	}
	if claim < 0 {
		t.Fatalf("no claim was written for a control-class op: %v", log)
	}
	if effect < 0 {
		t.Fatalf("the op never ran: %v", log)
	}
	if claim > effect {
		t.Fatalf("the side effect happened before the claim was acknowledged: %v", log)
	}
}

// TestReadClassOpIsNotClaimed: a claim is a round trip in front of every
// command, and a read has nothing to be at-most-once about.
func TestReadClassOpIsNotClaimed(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000005", nil)
	c.deliver("-Nread001", c.signed("list", `{}`))
	c.waitForResult("-Nread001")
	if c.fx.count("claim acked") != 0 {
		t.Fatalf("a read-class op was claimed: %v", c.fx.all())
	}
}

// TestSamePaneClaimsAckedOutOfOrderStillRunInPushIdOrder is the lane case.
//
// The fake delays the FIRST command's claim far longer than the second's. If
// the claim ran on the listener, the two would be in flight together and the
// second would be acknowledged first, and its text would reach the PTY first.
// Because the claim is the first step INSIDE the lane item, the second claim is
// not even sent until the first item is finished — so the acknowledgements and
// the PTY writes are both in pushId order, strictly interleaved.
func TestSamePaneClaimsAckedOutOfOrderStillRunInPushIdOrder(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000006", nil)
	c.f.SetStatus(func(_ int, rec patchRecord) int {
		for path, v := range rec.Payload {
			if !strings.HasPrefix(path, "results/") || !strings.Contains(string(v), `"claimed"`) {
				continue
			}
			id := strings.TrimPrefix(path, "results/")
			if strings.HasSuffix(id, "AAA") {
				// The EARLIER command's claim is the slow one.
				time.Sleep(80 * time.Millisecond)
			}
			c.fx.note("claim acked " + id)
		}
		return 0
	})

	first := c.signed("send", `{"pane":0,"text":"one"}`)
	second := c.signed("send", `{"pane":0,"text":"two"}`)
	// One `put` at "/" carrying both, which is the backlog shape. pushIds sort
	// lexicographically in creation order.
	c.put("-NxxxAAA", first)
	c.put("-NxxxBBB", second)
	c.f.Emit("put", map[string]any{"path": "/", "data": map[string]any{
		"-NxxxBBB": second, "-NxxxAAA": first,
	}})

	c.waitForResult("-NxxxAAA")
	c.waitForResult("-NxxxBBB")

	var order []string
	for _, e := range c.fx.all() {
		switch e {
		case "claim acked -NxxxAAA", "pty write one", "claim acked -NxxxBBB", "pty write two":
			order = append(order, e)
		}
	}
	want := []string{"claim acked -NxxxAAA", "pty write one", "claim acked -NxxxBBB", "pty write two"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestBacklogReplayOnReconnectDoesNotRerun: an SSE reconnect inside one process
// replays everything still under `commands`. A pushId already handled is not
// handled again.
func TestBacklogReplayOnReconnectDoesNotRerun(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000007", nil)
	cmd := c.signed("send", `{"pane":0,"text":"once"}`)
	c.deliver("-Nonce001", cmd)
	c.waitForResult("-Nonce001")

	// RTDB tells the listener to go away; magmux reconnects and is replayed the
	// backlog, which still holds the command as far as this fake is concerned.
	c.f.Emit("cancel", "listener cancelled")
	waitFor(t, 5*time.Second, "the listener to reconnect", func() bool {
		return c.f.StreamsOpened() >= 2
	})
	c.f.Emit("put", map[string]any{"path": "/", "data": map[string]any{"-Nonce001": cmd}})
	time.Sleep(200 * time.Millisecond)

	if n := c.fx.count("pty write once"); n != 1 {
		t.Fatalf("the command ran %d times across a reconnect: %v", n, c.fx.all())
	}
}

// TestRestartListensOnANewSessionAndReplaysNothing is the cross-crash half, and
// it needs no bookkeeping at all: sid carries the process start time, so a
// restarted magmux subscribes to a different node and never reads the old one.
func TestRestartListensOnANewSessionAndReplaysNothing(t *testing.T) {
	first := MakeSID("magmux", time.Unix(1780000000, 0))
	second := MakeSID("magmux", time.Unix(1780000060, 0))
	if first == second {
		t.Fatal("two runs of one magmux got the same sid")
	}

	c := newCmdFixture(t, first, nil)
	cmd := c.signed("send", `{"pane":0,"text":"before the crash"}`)
	c.deliver("-Ncrash01", cmd)
	c.waitForResult("-Ncrash01")
	if n := c.fx.count("pty write before the crash"); n != 1 {
		t.Fatalf("ran %d times before the restart", n)
	}

	// The crash: everything stops, and the command is left sitting in the
	// database (a real crash would not have deleted it).
	c.a.Close()
	c.put("-Ncrash01", cmd)

	// The restart, against the SAME database.
	before := c.fx.count("pty write before the crash")
	restarted := newAdapterOn(t, c, second)
	defer restarted.Close()
	// Even if something replays the old session's backlog, this listener is not
	// on that node.
	c.f.Emit("put", map[string]any{"path": "/", "data": map[string]any{"-Ncrash01": cmd}})
	time.Sleep(200 * time.Millisecond)

	if n := c.fx.count("pty write before the crash"); n != before {
		t.Fatalf("the old command ran again after a restart (%d -> %d)", before, n)
	}
	paths := c.f.StreamPaths()
	if len(paths) < 2 {
		t.Fatalf("the restarted adapter never subscribed: %v", paths)
	}
	last := paths[len(paths)-1]
	if !strings.Contains(last, second) {
		t.Fatalf("the restarted adapter listens on %q, not on the new session %q", last, second)
	}
	if strings.Contains(last, first) {
		t.Fatalf("the restarted adapter is still reading the old session's commands: %q", last)
	}
}

// newAdapterOn brings a second adapter up against the same fake and hub, as a
// restarted magmux would.
func newAdapterOn(t *testing.T, c *cmdFixture, sid string) *Adapter {
	t.Helper()
	cfg := *c.a.cfg
	a, err := New(&cfg, Options{
		Hub: c.h, SID: sid, PID: 4243, Version: "test",
		Aggregate: func() []byte { return nil },
		Log:       func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.mirror.tick = 20 * time.Millisecond
	a.Start()
	waitFor(t, 3*time.Second, "the restarted listener", func() bool {
		return c.f.StreamsOpened() >= 2
	})
	return a
}

// TestNullDataIsMagmuxsOwnDeleteAndIsIgnored: RTDB echoes magmux's own delete
// back to magmux's own listener. Treating that as a forged command would mean
// crying wolf on every command it ever completed.
func TestNullDataIsMagmuxsOwnDeleteAndIsIgnored(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000008", nil)
	c.f.EmitRaw("put", `{"path":"/-Ndeleted","data":null}`)
	c.f.EmitRaw("put", `{"path":"/","data":null}`)
	c.f.EmitRaw("patch", `{"path":"/-Ndeleted","data":null}`)
	// Something that IS a command, so the test proves the listener is alive
	// rather than merely quiet.
	c.deliver("-Nalive01", c.signed("list", `{}`))
	c.waitForResult("-Nalive01")

	if r := c.result("-Ndeleted"); r != nil {
		t.Fatalf("a null echo produced a result: %v", r)
	}
}

// TestKeepAliveAndAuthRevoked: a keep-alive is nothing, and `auth_revoked`
// drops the cached credential and reconnects rather than spinning on a token
// the server has stopped accepting.
func TestKeepAliveAndAuthRevoked(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000009", nil)
	c.f.EmitRaw("keep-alive", `null`)
	c.deliver("-Nkeep001", c.signed("list", `{}`))
	c.waitForResult("-Nkeep001")

	opened := c.f.StreamsOpened()
	c.f.Emit("auth_revoked", "credential is no longer valid")
	waitFor(t, 5*time.Second, "a reconnect after auth_revoked", func() bool {
		return c.f.StreamsOpened() > opened
	})
}

// TestUnknownOpAndUnroutableLaneAreAnswered: a command naming an op that is not
// registered, and one whose pane cannot be resolved, both get a result rather
// than silence. A command that vanishes is indistinguishable from one that is
// still running.
func TestUnknownOpAndUnroutableLaneAreAnswered(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000010", nil)

	c.deliver("-Nunk0001", c.signed("ticket.run_ticket", `{}`))
	res := c.waitForResult("-Nunk0001")
	if res["code"] != protocol.CodeUnknownVerb {
		t.Errorf("an unregistered op returned %v", res)
	}

	c.deliver("-Nnopane1", c.signed("send", `{"text":"no pane here"}`))
	res = c.waitForResult("-Nnopane1")
	if res["ok"] != false {
		t.Errorf("a send with no pane was accepted: %v", res)
	}
	if c.fx.count("pty write no pane here") != 0 {
		t.Error("an unroutable send reached the PTY")
	}
}

// TestPushIdThatIsNotAKeyIsNotAnswered: the pushId becomes a path segment, so
// one that could hold a `/` is dropped rather than answered — answering it
// would mean composing the very path that is unsafe.
func TestPushIdThatIsNotAKeyIsNotAnswered(t *testing.T) {
	c := newCmdFixture(t, "magmux-1780000011", nil)
	c.f.Emit("put", map[string]any{"path": "/", "data": map[string]any{
		"../../elsewhere": c.signed("list", `{}`),
	}})
	c.deliver("-Nsane001", c.signed("list", `{}`))
	c.waitForResult("-Nsane001")
	if c.fx.count("run list") != 1 {
		t.Fatalf("a command with an unsafe key ran: %v", c.fx.all())
	}
	if !sanitizeKey("-NxYz_123") {
		t.Error("a real pushId was rejected by sanitizeKey")
	}
	for _, bad := range []string{"a/b", "a.b", "a$b", "a#b", "a[b", "a]b", "", "a\nb"} {
		if sanitizeKey(bad) {
			t.Errorf("sanitizeKey(%q) = true", bad)
		}
	}
}
