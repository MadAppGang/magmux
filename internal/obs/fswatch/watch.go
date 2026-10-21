package fswatch

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MadAppGang/magmux/internal/obs"
)

// Kind is what happened to a record.
//
// # There are TWO kinds, and the research names three
//
// watcher-mechanism.md §4 describes "changed, appeared, vanished". Appeared is
// collapsed into [KindChanged] here, deliberately.
//
// To a consumer both mean the identical action — RE-READ THE PATH — and there
// is nothing a caller could do differently on one that it would not do on the
// other. That on its own would only be an argument for convenience. The
// argument for correctness is that the distinction is not observable: on
// Darwin the Create/Write ordering for a file that appears is fsnotify's own
// kqueue bookkeeping rather than a documented promise, and a record that is
// rewritten in place produces Write with no Create at all while one that a
// future Claude published by rename(2) would produce both. A consumer that
// switched on "appeared" would be switching on library timing, and no test
// could pin what it was switching on.
//
// The information is not lost, either: a path that a reader has no state for
// IS an appearance, and the reader is the only side that knows whether it had
// state. See [KindVanished] for the same argument from the other end.
type Kind uint8

const (
	// KindUnknown is the zero value and is never emitted. It exists because
	// the layer's rule is that a zero value is "no opinion" rather than a
	// silent default, and a Kind(0) arriving anywhere is a defect rather than
	// a change.
	KindUnknown Kind = iota
	// KindChanged means: re-read this path. The record was written, or it
	// appeared, or its metadata moved.
	KindChanged
	// KindVanished means: the path is gone. It is NOT "the session is dead" —
	// this package reports what happened to a FILE, and what a missing record
	// implies about a process is the registry's question, not this one's.
	KindVanished
)

// String returns the wire word, so a value in a failing test reads the same as
// one in a log.
func (k Kind) String() string {
	switch k {
	case KindChanged:
		return "changed"
	case KindVanished:
		return "vanished"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// Event is one coalesced report about one path.
type Event struct {
	// Path is the record's path, resolved against the watched directory so a
	// consumer can open it without knowing where the watch was pointed.
	Path string
	// Kind is what happened. See [Kind] for why there are two of them.
	Kind Kind
	// Dropped is how many events were thrown away before this one because the
	// consumer was not reading. It is carried ON the next delivered event
	// rather than reported separately, so that a consumer which fell behind
	// learns exactly how far and cannot mistake a gap for a quiet machine.
	Dropped int
}

// DefaultWindow is the coalescing window: 50ms.
//
// It comes from a measurement rather than from taste. Session 98693 was
// observed rewriting its record TWICE, thirteen seconds apart, and the
// registry-wide rate was 3 rewrites in 9 minutes across 32 sessions. The
// thirteen-second gap is what the window must NOT swallow — those are two
// genuine state changes — while a single rewrite that fsnotify reports as
// several events must produce ONE rebuild. 50ms is far below the former and
// far above the latter.
const DefaultWindow = 50 * time.Millisecond

// DefaultSuffix is the filename suffix a path must carry to be reported.
//
// The registry directory holds `<pid>.<sha256>.key` siblings — 33 of them
// against 32 records on the machine this was measured on — plus whatever else
// a user drops in. An unfiltered watch wakes a scan that finds nothing
// changed, which does not break correctness but does undo the "no measurable
// cost when nothing is happening" property the whole no-polling design exists
// to buy.
//
// The .key files are also never READ, for a second reason: they are named like
// key material, and the tree is read-only to magmux regardless.
const DefaultSuffix = ".json"

// Config is how a watcher is built. Every field has a default, and the zero
// Config is invalid only in that it names no directory.
//
// It is a plain struct COPIED BY VALUE into the watcher, per architecture v4
// §7.1: nothing mutates it in place, so nothing needs atomic access, and the
// question "who may change this while the loop is running" never arises.
type Config struct {
	// Dir is the directory to watch. It must exist at [Start].
	Dir string
	// Suffix filters which paths are reported. Empty takes [DefaultSuffix].
	// A caller that genuinely wants everything passes " " — there is no way
	// to spell "no filter", on purpose.
	Suffix string
	// Window is the coalescing window. Non-positive takes [DefaultWindow].
	Window time.Duration
	// Clock is the port the window's one-shot timer comes from. nil takes
	// [obs.RealClock]. This is the seam that lets a test drive the window
	// exactly rather than sleep through it.
	Clock obs.Clock
	// Buffer sizes the events channel. Non-positive takes a small default.
	// A consumer that falls behind loses events and is told how many; see
	// [Event.Dropped].
	Buffer int
}

const (
	defaultBuffer = 64
	// errorBuffer is small because errors are rare and a consumer that is not
	// reading them after sixteen has stopped reading them. The seventeenth is
	// counted, not lost: see the loop's fail().
	errorBuffer = 16
)

// Watcher reports that a session record changed or vanished. It never reports
// that a kqueue event arrived, and nothing outside this package ever sees an
// fsnotify value — that boundary is what lets the Darwin and Linux behaviours
// differ as much as they do (inotify's directory watch reports writes to
// children; kqueue's does not, and fsnotify compensates by watching each
// entry) without a single consumer knowing.
//
// # Ownership
//
// One goroutine owns everything: the fsnotify watcher, the pending set, the
// live timer and the drop counts. None of them is a field anything outside can
// reach, and the only ways in are [Watcher.Stop] and the two channels. That is
// why this package needs no mutex and no atomic — not because locking was
// avoided cleverly, but because there is nothing to lock.
//
// # It never blocks on a consumer
//
// Both channels are buffered and every send from the loop is non-blocking. A
// consumer that stops reading loses events and is TOLD how many. The
// alternative — a blocking send — would stall the loop, and a stalled loop
// stops draining fsnotify's own channel, which is how a watcher goes silent
// with no error anywhere: the exact failure mode this subsystem is written to
// make impossible.
type Watcher struct {
	events chan Event
	errs   chan error
	stop   chan struct{}
	done   chan struct{}
}

// Start begins watching cfg.Dir. The caller owns the result and must
// [Watcher.Stop] it.
//
// It watches the DIRECTORY, not each file. On Darwin a kqueue watch on a
// directory does not fire when a child's contents change, and fsnotify
// compensates by opening a descriptor on every entry itself — measured, and
// pinned by TestFsnotifySeesInPlaceWrites, which is the entire defence against
// that version coupling. On Linux inotify reports it directly. Either way the
// per-file bookkeeping is the library's problem and not this package's.
//
// Start does NOT emit an event for the records already present. A watch
// reports CHANGE; the initial state is the caller's to read, and a watcher
// that replayed the directory at birth would make "has this changed since I
// last looked" unanswerable for the caller's first look.
func Start(cfg Config) (*Watcher, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("fswatch: no directory to watch")
	}
	if cfg.Suffix == "" {
		cfg.Suffix = DefaultSuffix
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.RealClock()
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = defaultBuffer
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fswatch: %w", err)
	}
	if err := fsw.Add(cfg.Dir); err != nil {
		// Released here rather than left to the loop: there is no loop yet,
		// and a watcher returned as an error with a live kqueue behind it is
		// a descriptor nobody will ever close. Same drain as the teardown, for
		// the same measured reason.
		closeAndDrain(fsw)
		return nil, fmt.Errorf("fswatch: watching %s: %w", cfg.Dir, err)
	}

	w := &Watcher{
		events: make(chan Event, cfg.Buffer),
		errs:   make(chan error, errorBuffer),
		stop:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	go w.loop(cfg, fsw)
	return w, nil
}

// Events yields coalesced reports. It is CLOSED when the watcher stops, so a
// consumer may range over it.
func (w *Watcher) Events() <-chan Event { return w.events }

// Errors yields failures from the underlying watch. It is CLOSED when the
// watcher stops.
//
// An error here is never swallowed and never merely logged, because the shape
// this subsystem fails in is SILENCE: no event, no error, every session frozen
// at whatever it was when magmux started, and a machine that looks quiet. A
// consumer that ignores this channel is choosing that failure mode; a consumer
// that reads it can say so out loud.
func (w *Watcher) Errors() <-chan error { return w.errs }

// Stop shuts the watcher down and waits for its goroutine to finish. It is
// idempotent and may be called from any goroutine.
//
// Stopping is a SIGNAL rather than a closed channel on purpose: closing a
// channel twice panics, and making that safe needs a sync.Once, which is a
// mutex, which this layer does not have. A signal that races another merely
// queues, and one that arrives after the loop has gone is answered by done.
//
// Pending coalesced events are DROPPED, not flushed. Stop means stop: a
// consumer that has asked the watcher to go away has no use for one more
// report, and a flush on the way out would deliver an event after the caller
// believed the stream had ended.
func (w *Watcher) Stop() {
	select {
	case w.stop <- struct{}{}:
	case <-w.done:
	}
	<-w.done
}

// closeDrainWait bounds the teardown drain below. The drain normally finishes
// in microseconds; this exists so that a future fsnotify which never closes its
// channel costs a bounded pause on the way out instead of hanging [Watcher.Stop]
// — and magmux's own quit path calls Stop.
const closeDrainWait = 2 * time.Second

// closeAndDrain releases the underlying watch, and then DRAINS its event
// channel until it closes.
//
// The drain is not politeness, it is the descriptor release, and it is
// MEASURED. fsnotify v1.9.0's Close on Darwin returns before its reader
// goroutine has finished, and calling Close alone in a tight start/stop loop
// leaves about four descriptors behind per watcher on a three-record directory
// — a kqueue plus one per watched entry, which is the cost of the very
// compensation this package depends on (see fsnotify_regression_test.go).
// Draining until the channel closes releases every one of them: 20 cycles took
// the process's lowest free descriptor from 4 to 84 without the drain, and
// left it at 4 with it.
//
// That is a library behaviour rather than a documented promise, which is why
// TestRepeatedStartStopLeaksNoDescriptor exists beside it. magmux already
// holds a descriptor per PTY and the default soft limit on macOS is 256, so
// the failure this prevents is not a slow watcher — it is magmux failing to
// open something else entirely, much later, for no visible reason.
func closeAndDrain(fsw *fsnotify.Watcher) {
	fsw.Close()
	deadline := time.After(closeDrainWait)
	for {
		select {
		case _, ok := <-fsw.Events:
			if !ok {
				return
			}
		case <-fsw.Errors:
		case <-deadline:
			return
		}
	}
}

func (w *Watcher) loop(cfg Config, fsw *fsnotify.Watcher) {
	defer close(w.done)
	defer close(w.events)
	defer close(w.errs)
	defer closeAndDrain(fsw)

	var (
		timer   obs.Timer
		timerC  <-chan time.Time
		pending = map[string]Kind{}
		// order keeps the flush deterministic. A map's iteration order would
		// make two identical bursts produce two different event orders, and
		// this stream is meant to be compared between runs.
		order    []string
		dropped  int
		droppedE int
	)

	disarm := func() {
		if timer != nil {
			timer.Stop()
			timer, timerC = nil, nil
		}
	}

	// note records one path's latest kind and arms the window if it is not
	// already running.
	//
	// ONE timer for the whole window, not one per path. Per-path timers would
	// mean an unbounded number of live alarms and a dynamic select to wait on
	// them, and they would buy nothing a consumer can use: the consumer of
	// this stream rebuilds ONE document, so a burst that spans three records
	// wants one flush and not three. The coalescing is still per PATH in the
	// sense that matters — a path written five times inside the window
	// produces exactly one event.
	note := func(path string, kind Kind) {
		if _, seen := pending[path]; !seen {
			order = append(order, path)
		}
		// Last writer wins. A path that was written and then removed inside
		// one window is VANISHED, and one that was removed and then written
		// is CHANGED, which is what a reader re-reading the path would find
		// either way.
		pending[path] = kind
		if timer == nil {
			timer = cfg.Clock.NewTimer(cfg.Window)
			timerC = timer.C()
		}
	}

	publish := func(ev Event) {
		ev.Dropped = dropped
		select {
		case w.events <- ev:
			dropped = 0
		default:
			dropped++
		}
	}

	fail := func(err error) {
		if droppedE > 0 {
			err = fmt.Errorf("%w (and %d earlier errors were dropped: nothing is reading Errors)", err, droppedE)
		}
		select {
		case w.errs <- err:
			droppedE = 0
		default:
			droppedE++
		}
	}

	for {
		select {
		case <-w.stop:
			disarm()
			return

		case ev, ok := <-fsw.Events:
			if !ok {
				// fsnotify closed its own channel. That is not something the
				// library does while it is healthy, so it is reported rather
				// than treated as a quiet shutdown — a watcher that returns
				// silently here is indistinguishable from a quiet machine.
				fail(fmt.Errorf("fswatch: the underlying watch on %s closed unexpectedly", cfg.Dir))
				disarm()
				return
			}
			if !strings.HasSuffix(ev.Name, cfg.Suffix) {
				continue
			}
			if kind, ok := classify(ev.Op); ok {
				note(resolve(cfg.Dir, ev.Name), kind)
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				fail(fmt.Errorf("fswatch: the error channel for %s closed unexpectedly", cfg.Dir))
				disarm()
				return
			}
			fail(fmt.Errorf("fswatch: watching %s: %w", cfg.Dir, err))

		case <-timerC:
			// One-shot: the alarm has delivered and is finished with. A fresh
			// one is armed by the next event that finds the pending set empty.
			timer, timerC = nil, nil
			for _, path := range order {
				publish(Event{Path: path, Kind: pending[path]})
			}
			// Rebuilt rather than cleared, so the backing array of a burst
			// does not stay alive for the life of the watcher.
			pending = map[string]Kind{}
			order = nil
		}
	}
}

// classify maps an fsnotify operation onto the two kinds. It reports false for
// an operation that says nothing about the record's content.
func classify(op fsnotify.Op) (Kind, bool) {
	switch {
	case op.Has(fsnotify.Remove), op.Has(fsnotify.Rename):
		// A REMOVED record is Vanished. A RENAMED one is too: the path we were
		// asked about no longer holds that record, which is exactly what a
		// reader needs to know. If Claude ever switches from writing in place
		// to publishing by rename(2), the old inode is unlinked and this is
		// the event that arrives — and because nothing here suppresses a path
		// that has vanished, the Create that follows re-reports it as Changed
		// a moment later. Handling delete by re-arming on the PATH is correct
		// under both write disciplines, and it is the cheapest possible hedge
		// against a change we do not control.
		return KindVanished, true
	case op.Has(fsnotify.Create), op.Has(fsnotify.Write):
		return KindChanged, true
	case op.Has(fsnotify.Chmod):
		// Chmod is Darwin's NOTE_ATTRIB. It is INCLUDED, and the reason is an
		// open question in the research rather than a preference: it is not
		// established that NOTE_ATTRIB can never fire for one of these records
		// without NOTE_WRITE beside it. Excluding it risks missing a rewrite,
		// which is silence; including it risks one extra re-read, which is a
		// few microseconds of stat and parse and is idempotent. Between a
		// cheap false positive and a silent false negative there is no
		// contest.
		return KindChanged, true
	default:
		return KindUnknown, false
	}
}

// resolve joins the watched directory and the name fsnotify reported.
//
// fsnotify already reports a path built from the directory it was given, so
// this is normally a no-op — but "normally" is the library's business and not
// a promise, and a bare filename handed to a consumer would be opened against
// THAT consumer's working directory. The result is absolute exactly when the
// watched directory was.
func resolve(dir, name string) string {
	if filepath.IsAbs(name) || strings.HasPrefix(name, dir) {
		return name
	}
	return filepath.Join(dir, filepath.Base(name))
}
