package act

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// AuditFileName is the audit trail's file name inside magmux's state
// directory.
const AuditFileName = "sessions-audit.jsonl"

// Event is what one audit line is a record OF.
//
// # Why there are three, and why a record is never amended in place
//
// The architecture amendment describes ONE record per action, written before
// acting with a null outcome and then "amended in place" with the result.
// Append-only and amend-in-place cannot both be true, and append-only is the
// property that matters: rewriting a line means rewriting the whole file, and
// a process that dies during that rewrite loses records that were already
// safe. So the amendment's intent is kept and its mechanism is not — an
// executed action writes [EventAttempt] before it happens and [EventOutcome]
// after, the second referring back to the first by [Record.Ref].
//
// What the amendment's reasoning asked for survives exactly: a crash mid-action
// leaves an attempt with no outcome, which reads as "we tried this and do not
// know what happened", and that is the honest state and the one an operator
// needs.
type Event uint8

const (
	// EventUnknown is the zero value and is never written: [Writer.Append]
	// refuses a record without an event, because a line that does not say
	// what it is a record of cannot be read by anything.
	EventUnknown Event = iota
	// EventRefused is an action that did not happen. There is exactly one
	// line and there will be no outcome, because nothing was attempted.
	EventRefused
	// EventAttempt is written BEFORE an authorised action runs.
	EventAttempt
	// EventOutcome is written after it returns, carrying the attempt's Seq in
	// [Record.Ref].
	EventOutcome
)

var eventEnum = enumSpec{"Event", []string{
	"unknown",
	"refused",
	"attempt",
	"outcome",
}}

// String returns the wire word.
func (e Event) String() string { return eventEnum.name(int(e)) }

// MarshalJSON writes null for [EventUnknown] and the wire word otherwise.
func (e Event) MarshalJSON() ([]byte, error) { return eventEnum.marshal(int(e)) }

// UnmarshalJSON reads null as [EventUnknown]. An unrecognised name is an
// error.
func (e *Event) UnmarshalJSON(b []byte) error {
	i, err := eventEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*e = Event(i)
	return nil
}

// Target is which session an action was aimed at, recorded verbatim so that a
// trail read months later can still say what it acted on without resolving
// anything.
//
// The keys are the DOCUMENT's — `id`, `pid`, `name`, `paneId` — and not
// Claude's, which spells the first `sessionId`. Two reasons, and the second is
// enforced: a consumer joining an audit line to a session document should not
// have to translate a key, and adapters.TestOnlySchemaFileDecodesClaudeStrings
// fails when any file outside schema.go names one of Claude's. It caught this
// struct's first spelling.
type Target struct {
	SessionID string `json:"id"`
	PID       int    `json:"pid"`
	Name      string `json:"name,omitempty"`
	// PaneID is magmux's pane, or null for a session that is not in one.
	PaneID *int `json:"paneId"`
}

// Before is the session's state at the moment of the decision.
//
// It exists because an undo needs to know what it is reverting to, and because
// a trail that records only what was done cannot answer "was that reasonable
// at the time". The v1 review flagged its absence as M8.
//
// It carries no duration of its own beyond InStatusFor, which is rendered at
// record time: the model stores instants and derives durations, and a record
// is the one place a derived duration is worth freezing, because the instant
// it was derived at is the instant the record is about.
type Before struct {
	Status      obs.Status        `json:"status"`
	Waiting     obs.WaitingReason `json:"waitingOn"`
	InStatusFor string            `json:"inStatusFor,omitempty"`
	Confidence  obs.Tier          `json:"confidence"`
	Attention   *bool             `json:"attentionNeeded"`
}

// StateOf captures the part of a session an audit record keeps.
func StateOf(s obs.Session, now time.Time) Before {
	b := Before{
		Status:     s.Status,
		Waiting:    s.Waiting,
		Confidence: s.Confidence,
	}
	if s.Attention.Needed != nil {
		v := *s.Attention.Needed
		b.Attention = &v
	}
	if d, ok := s.InStatusFor(now); ok {
		b.InStatusFor = d.Round(time.Second).String()
	}
	return b
}

// Outcome is what happened. It is null on an [EventAttempt] line, which is the
// whole reason attempts are written at all.
type Outcome struct {
	// Done is whether the operation took effect.
	Done bool `json:"done"`
	// Detail is what happened, in magmux's own words.
	Detail string `json:"detail,omitempty"`
	// Refused is set on an [EventRefused] line, so that a reader counting
	// refusals does not have to infer them from Done being false.
	Refused bool `json:"refused,omitempty"`
	// Error is the failure or the refusal, as a sentence.
	Error string `json:"error,omitempty"`
}

// Record is one line of the audit trail.
//
// R5.A asks for four things — what was done, to which session, why, and by
// which rule — and each has a field: Verb and Tier, Target, Before and
// RequestedBy, and Policy.Rule. The last is not optional, and
// [Writer.Append] refuses a record without it.
type Record struct {
	// At is when the line was written, from the writer's clock.
	At time.Time `json:"at"`

	// Seq is this WRITER's counter, starting at 1, and it identifies the
	// LINE. It is not global: the file is shared by every magmux on the
	// machine, so only the (PID, Seq) pair is unique.
	Seq int `json:"seq"`

	// Ref is the Seq of the attempt an outcome line belongs to, and is absent
	// on every other kind of line.
	//
	// The two halves of one action are correlated by a REFERENCE rather than
	// by sharing a sequence number, which was the first shape and was wrong:
	// two lines with one number leave a reader unable to say which line is
	// seq 41, and the writer's counter stops meaning "lines written", which
	// is the only thing it can honestly mean when the writer is the thing
	// that stamps it.
	Ref int `json:"ref,omitempty"`

	// PID is the process that wrote the line — magmux, not the session.
	PID int `json:"pid"`

	// Event is what this line records; see [Event].
	Event Event `json:"event"`

	Verb   string    `json:"verb"`
	Tier   Tier      `json:"tier"`
	Target Target    `json:"target"`
	By     Requester `json:"requestedBy"`

	// Policy is the decision and, in Rule, its authority.
	Policy Decision `json:"policy"`

	// Before is the session's state at decision time, omitted on an outcome
	// line where the attempt beside it already carries it.
	Before *Before `json:"before,omitempty"`

	// Outcome is null on an attempt: "we tried this and do not know what
	// happened" is the honest reading of a record with no outcome beside it.
	Outcome *Outcome `json:"outcome"`
}

// Auditor is the trail an action is written to. It is an interface so that the
// gate can be tested without a filesystem, and so that a caller who has not
// decided where the trail lives cannot accidentally get one that discards.
//
// A nil Auditor in a [Gate] REFUSES every action; see [ErrUnaudited].
type Auditor interface {
	// Append writes one record and returns it as written — stamped with the
	// instant, the sequence number and the pid.
	Append(Record) (Record, error)
}

// Writer is an append-only audit trail on disk.
//
// # Single owner, no lock
//
// It holds a sequence counter and no mutex, so one goroutine owns it. In
// magmux that is the pipeline's coordinator, which is also the only goroutine
// that reaches an actor, so an action and its audit line are already
// serialised by the same channel. Two writers in one process would need
// synchronisation and this layer has none; two PROCESSES are fine, because
// every line is one O_APPEND write.
//
// # Durability, and what "survives a crash mid-write" means exactly
//
// Each record is marshalled whole, a newline is appended, and the result goes
// out in ONE write to a descriptor opened with O_APPEND. A process that dies
// mid-write can therefore only truncate the line it was writing: O_APPEND
// moves the offset to the end atomically, so no earlier byte is ever revisited
// and no earlier record can be damaged. A torn last line is detectable — it is
// not valid JSON and has no newline — and a reader skips it, which is the
// correct treatment of a record whose writer died before finishing it.
//
// The file is fsynced before the descriptor is closed. An action is a rare,
// human-rate event and an audit trail that is still in a page cache when the
// machine goes down is not an audit trail.
//
// # It never opens anything under ~/.claude, and cannot be made to
//
// [NewWriter] refuses a path with a `.claude` element in any letter case, and
// refuses one under $CLAUDE_CONFIG_DIR, after making the path ABSOLUTE — and
// it keeps the absolute path it checked, so the file it later opens is the
// one that was checked rather than one a changed working directory resolves
// differently. The check reads no file and opens nothing; see
// [CheckAuditPath] for the one stat a relative path costs.
type Writer struct {
	path string
	pid  int
	now  func() time.Time
	seq  int
}

// WriterConfig is how a [Writer] is built. A plain value, copied on the way
// in.
type WriterConfig struct {
	// Path is the trail's file. Empty takes [DefaultAuditPath].
	Path string

	// Now is the clock the records are stamped from. nil takes time.Now.
	Now func() time.Time

	// PID is the process id recorded on every line. Zero takes os.Getpid.
	// It is injectable so that a test's expectations do not change with the
	// pid the suite happens to run as.
	PID int
}

// NewWriter resolves the path, refuses a forbidden one, and creates NOTHING.
//
// The directory and the file are created by the first [Writer.Append] and not
// before: a magmux that never actuates leaves no trace, and a state directory
// that appears on every start is a directory nobody can tell the meaning of.
func NewWriter(cfg WriterConfig) (*Writer, error) {
	p := cfg.Path
	if p == "" {
		resolved, err := DefaultAuditPath()
		if err != nil {
			return nil, err
		}
		p = resolved
	}
	abs, err := resolveAuditPath(p)
	if err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	pid := cfg.PID
	if pid == 0 {
		pid = os.Getpid()
	}
	return &Writer{path: abs, pid: pid, now: now}, nil
}

// Path is the file this writer appends to. It is resolved at construction, so
// it can be shown to a user before anything has been written.
func (w *Writer) Path() string { return w.path }

// Append writes one record.
//
// The record is stamped here rather than by the caller: the instant, the
// sequence number and the pid are facts about the WRITING, and a caller that
// could set them could also forge them.
func (w *Writer) Append(r Record) (Record, error) {
	if r.Event == EventUnknown {
		return r, fmt.Errorf("audit: a record with no event says nothing; refusing to write it")
	}
	if strings.TrimSpace(r.Policy.Rule) == "" {
		// R5.A's "by which rule" is not optional. Every decision this package
		// makes fills it in, and TestEveryDecisionNamesWhatAuthorisedIt pins
		// that, so reaching here is a defect rather than a user error —
		// refusing loudly is how it gets found, and the gate turns the
		// refusal into a refused action rather than an unaudited one.
		return r, fmt.Errorf("audit: a record that cannot say what authorised the action " +
			"is not an audit record; refusing to write it")
	}

	r.At = w.now()
	w.seq++
	r.Seq = w.seq
	r.PID = w.pid

	line, err := json.Marshal(r)
	if err != nil {
		return r, fmt.Errorf("audit: encoding the record: %w", err)
	}
	line = append(line, '\n')

	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return r, fmt.Errorf("audit: creating %s: %w", filepath.Dir(w.path), err)
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return r, fmt.Errorf("audit: opening %s: %w", w.path, err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return r, fmt.Errorf("audit: writing to %s: %w", w.path, err)
	}
	if err := f.Sync(); err != nil {
		return r, fmt.Errorf("audit: flushing %s: %w", w.path, err)
	}
	return r, nil
}

// DefaultAuditPath is ${XDG_STATE_HOME:-~/.local/state}/magmux/sessions-audit.jsonl.
//
// NOT under ~/.claude, which NFR1 forbids without exception. The user did
// separately approve ~/.claude/magmux/ as a directory magmux may own, so an
// exception exists and could have been used — but a log that needs an
// exception to a rule stated as absolute is a worse default than one that does
// not, and the state directory is where a Unix program's own mutable state
// belongs anyway.
//
// It reads environment variables only. Nothing is opened, so resolving a path
// cannot itself touch the tree the rule protects.
//
// A RELATIVE XDG_STATE_HOME is ignored, as the XDG Base Directory
// specification requires ("If an implementation encounters a relative path in
// any of these variables it should consider the path invalid and ignore it").
// Honouring it would put the trail wherever the shell happened to be — inside
// ~/.claude, for someone who had changed into it to read a transcript.
func DefaultAuditPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); dir != "" && filepath.IsAbs(dir) {
		return filepath.Join(dir, "magmux", AuditFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find your home directory, so cannot place the audit "+
			"trail; set XDG_STATE_HOME: %w", err)
	}
	return filepath.Join(home, ".local", "state", "magmux", AuditFileName), nil
}

// CheckAuditPath refuses a path magmux must not write to.
//
// # Why this exists when the default is already correct
//
// Because the default is not the only path anyone will ever pass. A flag, an
// environment variable, a test harness or a future config file can all name a
// file, and NFR1 is not a property of one function's default — it is a
// property of magmux. The check is here, at the one place a trail is opened.
//
// Two refusals, both made against the ABSOLUTE path:
//
//   - any path with a `.claude` element in it, compared WITHOUT REGARD TO
//     CASE. That covers ~/.claude/… and also XDG_STATE_HOME pointed inside it,
//     which is the shape a prefix-comparison against $HOME would miss.
//   - any path under $CLAUDE_CONFIG_DIR, which is Claude Code's own override
//     and can be anywhere at all, with no `.claude` in its name. The variable
//     is made absolute the same way, and compared element by element with the
//     same case rule.
//
// # Why absolute, and why case-blind — fix round 1
//
// Until fix round 1 the path was cleaned but never made absolute, and elements
// were compared exactly. So `sessions-audit.jsonl`, `./trail.jsonl`,
// `/Users/user/.Claude/magmux/trail.jsonl` and `/Users/user/.CLAUDE/trail.jsonl`
// all passed. The first two resolve against the working directory, and
// `magmux agents focus --audit trail.jsonl` from a shell sitting inside ~/.claude
// created a file there with no error. The last two ARE ~/.claude on this
// machine, whose data volume is case-insensitive APFS, as it is by default on
// every Mac.
//
// On a case-SENSITIVE filesystem the case rule over-refuses: a directory
// genuinely named `.Claude` is refused there too. That is the right direction
// to be wrong in for a guard whose job is the accident, and a user who wants
// the trail in such a directory can name one that is not spelled like Claude
// Code's. Letter case is folded with strings.EqualFold, which is Unicode
// simple folding and not APFS's own normalisation; for `.claude` the two
// agree, and a CLAUDE_CONFIG_DIR spelled with composed and decomposed accents
// in two places is not an accident this guard can reason about.
//
// # What it touches
//
// No file is read and nothing is opened, so the guard cannot itself be the
// thing that writes under ~/.claude. An ABSOLUTE path is pure string work. A
// RELATIVE one costs os.Getwd, which on Darwin and Linux stats "." and $PWD to
// confirm they are the same directory — so when the working directory is
// inside ~/.claude, making the path absolute stats that directory. That is a
// READ of metadata, which NFR1 does not forbid and which this layer already
// does to the registry on every cycle; it is stated because the previous
// version of this comment said "no stat" and a guard that argues from its own
// purity should be exact about it.
//
// # What that purity costs, stated rather than left to be discovered
//
// A SYMLINK into ~/.claude evades it. A trail at
// ~/.local/state/magmux/sessions-audit.jsonl whose parent is a link into the
// Claude tree passes this check and then writes there. Catching it means
// resolving the path — filepath.EvalSymlinks, which stats every component —
// and the thing it would stat is the tree the rule exists to keep magmux out
// of, on a path that may not exist yet.
//
// The trade taken: the guard is for the ACCIDENT, which is a default or a flag
// naming ~/.claude because that is where the session data lives. It is not a
// defence against a user who has deliberately linked their state directory
// into Claude's, and it does not pretend to be one — that user has already
// made the decision this check would be arguing with.
func CheckAuditPath(p string) error {
	_, err := resolveAuditPath(p)
	return err
}

// resolveAuditPath is [CheckAuditPath] returning the absolute path it checked,
// which is the path [NewWriter] keeps. Checking one spelling and opening
// another is how a guard is defeated by a chdir between the two.
func resolveAuditPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("audit: no path for the audit trail")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		// Only a relative path can fail here, and only when the working
		// directory cannot be named. A path that cannot be placed cannot be
		// shown to be outside ~/.claude, so it is refused.
		return "", fmt.Errorf("audit: cannot tell where %s is, so cannot show it is outside "+
			"~/.claude (NFR1): %w", p, err)
	}
	for _, el := range pathElements(abs) {
		if strings.EqualFold(el, ".claude") {
			return "", fmt.Errorf("audit: refusing %s: magmux never writes under ~/.claude, "+
				"not even a log (NFR1) — thirty live Claude Code processes own that tree", abs)
		}
	}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		root, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("audit: refusing %s: CLAUDE_CONFIG_DIR (%s) cannot be placed, "+
				"so the trail cannot be shown to be outside it (NFR1): %w", abs, dir, err)
		}
		if withinFold(abs, root) {
			return "", fmt.Errorf("audit: refusing %s: it is inside CLAUDE_CONFIG_DIR (%s), "+
				"which magmux never writes to (NFR1)", abs, root)
		}
	}
	return abs, nil
}

// pathElements splits a cleaned path into its named elements, dropping the
// empty one a leading separator produces.
func pathElements(p string) []string {
	var out []string
	for _, el := range strings.Split(p, string(filepath.Separator)) {
		if el != "" {
			out = append(out, el)
		}
	}
	return out
}

// withinFold reports whether p is root or inside it, element by element and
// without regard to letter case. Element-wise rather than by string prefix, so
// that /opt/claude-config-backup is not inside /opt/claude-config.
func withinFold(p, root string) bool {
	pe, re := pathElements(p), pathElements(root)
	if len(pe) < len(re) {
		return false
	}
	for i := range re {
		if !strings.EqualFold(pe[i], re[i]) {
			return false
		}
	}
	return true
}
