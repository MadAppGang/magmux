// Package registry is the source that turns Claude Code's on-disk session
// registry into [obs.Session] values.
//
// It walks a directory of `<pid>.json` records, decodes each one through
// internal/obs/adapters, asks an injected oracle whether each process is still
// alive, and returns ONE [obs.Fleet] carrying every session it could read —
// plus, separately, a [Problem] for everything it could not.
//
// # Nothing is hardcoded, and that is a testability constraint rather than taste
//
// The registry root, the clock, the current instant and the liveness oracle
// are all injected. A hardcoded ~/.claude would mean the only way to test this
// is against thirty live agents' state on the developer's machine, which is
// the one thing this whole feature is forbidden from touching. The [Config] is
// a plain struct copied by value: nothing mutates it in place, so nothing needs
// atomic access, and "who may change this while a read is running" never
// arises.
//
// # Reading is read-only, and that is not a figure of speech
//
// This package opens files and stats them. It creates nothing, writes nothing,
// and removes nothing, not even a temporary file, because the directory it
// reads belongs to running processes that are writing it at the same time.
//
// # Concurrency
//
// A [Source] is SINGLE-OWNER. It carries the last good state of each record so
// that a torn read does not lose a session, and that state is a plain map with
// no lock on it: the goroutine that created the Source is the only one that
// may call Read. Nothing here starts a goroutine.
package registry

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/adapters"
)

// ProcessOracle answers whether a process is still alive.
//
// It returns [obs.Liveness] and not a bool because there is a third answer and
// it is the honest one more often than people expect: an oracle that was never
// asked, or that cannot tell, must not be coerced into "dead". A session
// reported dead because nobody could look is worse than a session reported
// undetermined, because only one of those two is visibly a non-answer.
//
// The port exists so the production probe stays OUT of this package. magmux
// already owns the errno classification this needs — `killErrMeansGone` in
// sockdir.go, which is ESRCH and ONLY ESRCH, because EPERM means the process
// exists and belongs to somebody else, and treating that as "gone" once made
// the socket reaper unlink other users' live sockets. Rather than copy that
// reasoning into internal/obs, where the test that pins it cannot follow, the
// signal oracle is built in package main and handed in here. That also keeps
// this package free of platform errno entirely.
type ProcessOracle interface {
	Alive(pid int) obs.Liveness
}

// DefaultRetryDelay is how long a torn read waits before its one retry.
//
// A record is a few hundred bytes written in place, so the window in which a
// reader can catch half of one is measured in microseconds; 20ms is three
// orders of magnitude more than the writer needs and still imperceptible to a
// human watching a table redraw. It is a one-shot wait, not a poll: there is
// exactly one retry, and a second failure is answered by keeping what we
// already knew.
const DefaultRetryDelay = 20 * time.Millisecond

// Config is how a [Source] is built. It is copied by value; see the package
// doc.
type Config struct {
	// Root is the directory holding the records, normally
	// $CLAUDE_CONFIG_DIR/sessions or ~/.claude/sessions. It is required.
	Root string

	// Now answers what the instant is, for [obs.Fleet.Epoch] — "when this
	// document was taken, from the reader's clock". nil takes time.Now.
	//
	// # Why this is not a method on Clock
	//
	// [obs.Clock] creates TIMERS. Its Now exists for one caller, arming a
	// cascade at birth on its constructing goroutine, and the fake answers it
	// from an immutable field — so a second caller reading it repeatedly gets
	// the same instant forever and every epoch in a test would be identical.
	//
	// The deeper reason is the rule the whole layer is built on: an instant is
	// carried BY the event that reports it, never asked for afterwards,
	// because the gap between something happening and a goroutine hearing
	// about it is exactly what these numbers measure. A Now() beside NewTimer
	// makes it one keystroke to violate that by accident. A separate function
	// makes the one legitimate case — an epoch, which is a fact about the READ
	// and not about any session — say so at its call site.
	Now func() time.Time

	// Oracle answers the liveness question. nil yields
	// [obs.LivenessUnknown] for every session, and that is deliberate: a test
	// that forgets to inject one gets "undetermined", which is honest, where a
	// default signal probe would have the suite sending signals to whatever
	// pids happen to exist on the machine running it — and the answers would
	// depend on what else was running.
	Oracle ProcessOracle

	// RetryDelay is how long a torn read waits before its single retry.
	// Non-positive takes [DefaultRetryDelay]. Zero exactly is honoured as "no
	// wait" only in the sense that the timer fires immediately.
	RetryDelay time.Duration

	// Clock is where the retry's one-shot timer comes from. nil takes
	// [obs.RealClock].
	Clock obs.Clock
}

// ProblemKind says what went wrong with one record.
type ProblemKind uint8

const (
	// ProblemUnknown is the zero value and is never reported.
	ProblemUnknown ProblemKind = iota
	// ProblemTorn is a record that would not parse twice running. The session
	// keeps the state it had at the previous read, and the problem is how a
	// caller learns that what it is looking at is a moment old.
	ProblemTorn
	// ProblemAbsent is a record whose VERY FIRST read was torn, so there is no
	// previous state to keep. The session is absent from the document — which
	// is not the same as dead, and no consumer may read it that way.
	ProblemAbsent
	// ProblemUnreadable is an I/O error that is not a torn read: a permission
	// problem, a directory that disappeared. Like ProblemTorn, the previous
	// state is kept.
	ProblemUnreadable
	// ProblemPIDMismatch is a record whose filename and whose pid field
	// disagree. The record wins; see [Source.Read].
	ProblemPIDMismatch
	// ProblemUndecoded is a value this version does not understand — a status
	// word it has never seen, an instant in an unexpected unit. The raw form is
	// kept on the session and the field it would have filled is undetermined.
	ProblemUndecoded
)

// String returns a short word for the kind.
func (k ProblemKind) String() string {
	switch k {
	case ProblemTorn:
		return "torn"
	case ProblemAbsent:
		return "absent"
	case ProblemUnreadable:
		return "unreadable"
	case ProblemPIDMismatch:
		return "pid-mismatch"
	case ProblemUndecoded:
		return "undecoded"
	default:
		return fmt.Sprintf("ProblemKind(%d)", uint8(k))
	}
}

// Problem is one thing that went wrong, named.
//
// Problems are RETURNED, never logged and forgotten. A source that silently
// skips a record it could not read is indistinguishable from a machine with
// fewer sessions on it, and the difference between those two is the entire
// value of the report.
type Problem struct {
	// Path is the record the problem is about, or the directory itself.
	Path string `json:"path"`
	// Kind is what went wrong.
	Kind ProblemKind `json:"kind"`
	// Detail is the human-readable explanation. Free text: it is shown to a
	// person, never switched on.
	Detail string `json:"detail"`
}

// Error renders a problem as one line.
func (p Problem) Error() string {
	return fmt.Sprintf("%s: %s: %s", p.Path, p.Kind, p.Detail)
}

// Source reads a registry directory. Build one with [New] and keep it: it
// remembers the last good state of each record, which is what lets a torn read
// keep a session instead of dropping it.
type Source struct {
	cfg Config
	// known is the last good session for each path. It is plain, unlocked
	// state and the Source is single-owner; see the package doc.
	known map[string]obs.Session
}

// New builds a Source. It touches no filesystem: an unreadable root is
// reported by [Source.Read], because the directory can appear and disappear
// between one read and the next and a constructor that refused would have to
// be rebuilt to recover.
func New(cfg Config) (*Source, error) {
	if cfg.Root == "" {
		return nil, errors.New("registry: no root directory")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = DefaultRetryDelay
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.RealClock()
	}
	return &Source{cfg: cfg, known: map[string]obs.Session{}}, nil
}

// Read builds one document from the directory as it stands.
//
// # The document is a snapshot and says so
//
// Every session in it was read within one pass and the whole document carries
// ONE epoch, because the records mutate between reads — a sixth of them were
// measured changing status inside five minutes — and a consumer diffing two
// documents has to know whether it is comparing two instants or an hour's
// drift.
//
// # No freshness verdict is computed here, and no timestamp could supply one
//
// The instant in a record says when the session ENTERED its status. It is
// EXACT: the record is written when something changes, an idle session is
// never written, and so an old timestamp means an old status rather than a
// neglected file. Measured 2026-09-18 — a session sat busy for 31.7 minutes of
// continuous work with exactly one write, the transition itself, and a 15
// minute probe at 3-second intervals saw no other. (One window, one machine.
// An earlier claim of restamping every 6-10 minutes rests on two observations
// that this did not reproduce and did not explain away, so the honest
// statement is "exact as measured" rather than "restamping is impossible".)
//
// The file's mtime cannot add anything either, and that was the tempting
// mistake: across 33 records it differs from the record's own written-at
// instant by more than a second exactly ZERO times, worst case 892ms, and the
// written-at instant equals the status instant in 34 of 34. All three are one
// timestamp wearing three names, so reading the mtime is asking the same
// source the same question.
//
// What follows is that NO timestamp distinguishes a 46-hour-old record whose
// session is alive and idle from a 46-hour-old record whose session died
// yesterday. Only liveness can, which is why the oracle exists and why a
// session's age is reported rather than judged. `60010.json` in the test data
// is exactly that case — a live pid with a three-day-old status — and it is
// reported like any other. [obs.Session] stores nothing time-derived by
// design, so there is no field here for a verdict, and inventing one to hold
// an answer nothing reads would be data with no reader.
//
// # When the filename and the record disagree about the pid
//
// The RECORD wins. The record is the document; the filename is an index into
// it, and an index that disagrees with its own document is a symptom rather
// than a second opinion. The disagreement is reported as a
// [ProblemPIDMismatch] and the session is still included, because dropping it
// would turn a naming oddity into a missing session.
//
// # A parse failure is TORN, not dead
//
// Claude writes these records in place — confirmed from its own
// `publishDiscipline:"inPlace"`, from its reader's torn-read guard, and from a
// captured before/after inode pair — so a half-written read is real, and it is
// most likely at precisely the moment a session changes state, which is the
// moment we most want to read it. Every parse failure is retried ONCE after
// the configured delay. A second failure keeps the previous known state and
// reports a problem; it never produces a "dead" verdict and never drops a
// session that was there a moment ago. If the very FIRST read of a path is
// torn there is nothing to keep, so the session is absent from the document —
// absent, which a consumer must not read as dead.
func (s *Source) Read() (obs.Fleet, []Problem) {
	fleet := obs.Fleet{Epoch: s.cfg.Now()}
	var problems []Problem

	entries, err := os.ReadDir(s.cfg.Root)
	if err != nil {
		return fleet, []Problem{{
			Path:   s.cfg.Root,
			Kind:   ProblemUnreadable,
			Detail: fmt.Sprintf("listing the registry: %v", err),
		}}
	}

	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != adapters.RecordSuffix {
			continue
		}
		path := filepath.Join(s.cfg.Root, e.Name())
		seen[path] = true

		sess, ps, ok := s.readRecord(path, e.Name())
		problems = append(problems, ps...)
		if !ok {
			continue
		}
		fleet.Sessions = append(fleet.Sessions, sess)
	}

	// Forget the records that are no longer there. Keeping them would make a
	// session that was deleted an hour ago reappear the first time a live one
	// tore, which is a worse answer than having nothing to fall back on.
	for path := range s.known {
		if !seen[path] {
			delete(s.known, path)
		}
	}

	fleet.Sort()
	return fleet, problems
}

// readRecord reads and decodes one record, with the single torn-read retry.
// ok is false when there is nothing to report for this path at all.
func (s *Source) readRecord(path, name string) (obs.Session, []Problem, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The record vanished between the listing and the read. That is
			// the registry doing exactly what it does — sessions appear and
			// disappear constantly — so it is not a problem, and reporting it
			// as one would make a healthy machine noisy in the one place a
			// reader is looking for real trouble.
			delete(s.known, path)
			return obs.Session{}, nil, false
		}
		return s.fallback(path, ProblemUnreadable, fmt.Sprintf("reading: %v", err))
	}

	sess, notes, perr := adapters.ParseRecord(data)
	if perr != nil {
		// ONE retry, after the injected delay, through the one-shot timer
		// port. Not a loop: if the second read is torn too, something other
		// than a rewrite in flight is wrong, and waiting longer would trade a
		// wrong answer for no answer.
		s.wait()
		data, err = os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				delete(s.known, path)
				return obs.Session{}, nil, false
			}
			return s.fallback(path, ProblemUnreadable, fmt.Sprintf("reading after a torn read: %v", err))
		}
		sess, notes, perr = adapters.ParseRecord(data)
	}
	if perr != nil {
		return s.fallback(path, ProblemTorn, fmt.Sprintf("unreadable twice running: %v", perr))
	}

	var problems []Problem
	for _, n := range notes {
		problems = append(problems, Problem{Path: path, Kind: ProblemUndecoded, Detail: n})
	}

	if filePID, ok := adapters.PIDFromRecordName(name); ok && sess.PID != 0 && filePID != sess.PID {
		problems = append(problems, Problem{
			Path: path,
			Kind: ProblemPIDMismatch,
			Detail: fmt.Sprintf("the filename says pid %d and the record says pid %d; "+
				"the record is the document and the filename is an index into it, so %d is used",
				filePID, sess.PID, sess.PID),
		})
	}

	sess.ProcessAlive = s.alive(sess.PID)

	// PaneID and PaneAlive are deliberately left undetermined. The second
	// liveness gate needs magmux's pane table, and a pane whose existence
	// nobody established is not thereby gone.

	s.known[path] = sess
	return sess, problems, true
}

// fallback keeps the previous known state for a path that could not be read,
// and reports why. A session that was there a moment ago does not stop
// existing because we failed to read its record.
func (s *Source) fallback(path string, kind ProblemKind, detail string) (obs.Session, []Problem, bool) {
	prev, had := s.known[path]
	if !had {
		return obs.Session{}, []Problem{{
			Path: path,
			Kind: ProblemAbsent,
			Detail: detail + " — and there is no earlier state for it, so this session is " +
				"ABSENT from the document. Absent is not dead.",
		}}, false
	}
	// The liveness answer is re-asked even though the record was not re-read:
	// the process is a fact about the machine, not about the file, and it is
	// the half of the previous state most likely to have changed.
	prev.ProcessAlive = s.alive(prev.PID)
	return prev, []Problem{{
		Path:   path,
		Kind:   kind,
		Detail: detail + " — keeping the state read previously",
	}}, true
}

func (s *Source) alive(pid int) obs.Liveness {
	if s.cfg.Oracle == nil || pid <= 0 {
		return obs.LivenessUnknown
	}
	return s.cfg.Oracle.Alive(pid)
}

// wait sleeps out the retry delay on a one-shot timer from the injected clock.
// It is the only waiting this package does.
func (s *Source) wait() {
	t := s.cfg.Clock.NewTimer(s.cfg.RetryDelay)
	defer t.Stop()
	<-t.C()
}
