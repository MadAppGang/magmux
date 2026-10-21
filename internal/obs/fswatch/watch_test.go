package fswatch

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// Every test here builds its own directory with t.TempDir(). NOTHING in this
// file watches internal/obs/testdata/sessions, and nothing goes anywhere near
// ~/.claude: a watch test has to WRITE to be worth anything, and a watch test
// pointed at either of those would be writing into the repository or into
// thirty live agents' state.

const (
	// armWait and eventWait are FAILURE deadlines, not waits for a threshold.
	// On a working watcher the timer is armed and the event delivered within
	// microseconds of the write; these exist so a broken one reports "nothing
	// was armed" instead of hanging until the test binary is killed.
	armWait   = 2 * time.Second
	eventWait = 2 * time.Second
	// quietWait is how long an ABSENCE is given to disprove itself. An absence
	// can only be shown by waiting, and this is the one thing in the file that
	// genuinely costs its own duration.
	quietWait = 200 * time.Millisecond
)

// record writes a session record in place, with the exact shape the real
// writer uses: open the existing file, truncate it, write. No temporary file
// and no rename, so the directory entry never changes — which on Darwin is
// precisely the event the kernel does not report and fsnotify does.
func record(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("writing %s: %v", filepath.Base(path), err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		t.Fatalf("writing %s: %v", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing %s: %v", filepath.Base(path), err)
	}
}

// fakeWatcher starts a watcher whose window is driven by a fake clock, so the
// test decides when the window closes instead of sleeping through it.
func fakeWatcher(t *testing.T, dir string) (*Watcher, *obs.FakeClock) {
	t.Helper()
	clk := obs.NewFakeClock(time.Unix(1789545200, 0))
	w, err := Start(Config{Dir: dir, Clock: clk, Window: DefaultWindow})
	if err != nil {
		t.Fatalf("Start(%s): %v", dir, err)
	}
	t.Cleanup(w.Stop)
	return w, clk
}

// flush waits for the window's alarm to be armed and fires it, which is the
// synchronisation barrier every deterministic test here is built on: the alarm
// cannot exist until the loop has processed the event that armed it.
func flush(t *testing.T, clk *obs.FakeClock) {
	t.Helper()
	ft, err := clk.NextTimer(armWait)
	if err != nil {
		t.Fatalf("%v — the watcher saw no change at all", err)
	}
	ft.Fire(time.Unix(1789545200, 0).Add(DefaultWindow))
}

// drain collects events until the stream has been quiet for quiet, or until
// eventWait has passed overall. It returns what the consumer would have seen,
// which is the only thing this package promises.
//
// quiet is a parameter rather than a constant because it has to outlast the
// window under test: a real-clock test whose window is 250ms and whose quiet
// period is 200ms would give up before the first flush and report a watcher
// that works as one that saw nothing.
func drain(t *testing.T, w *Watcher, quiet time.Duration) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(eventWait)
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case err := <-w.Errors():
			t.Fatalf("watcher error: %v", err)
		case <-time.After(quiet):
			return got
		case <-deadline:
			return got
		}
	}
}

func wantOne(t *testing.T, got []Event, path string, kind Kind) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("got %d events, want exactly 1: %+v", len(got), got)
	}
	if got[0].Path != path {
		t.Errorf("event names %s, want %s", got[0].Path, path)
	}
	if got[0].Kind != kind {
		t.Errorf("event is %s, want %s", got[0].Kind, kind)
	}
	if got[0].Dropped != 0 {
		t.Errorf("event reports %d drops, want 0", got[0].Dropped)
	}
}

// WRONG: a watcher that watches the directory the way the kernel does, and so
// never fires at all for the only write discipline Claude Code uses.
//
// This is TestFsnotifySeesInPlaceWrites one layer up: that one proves the
// library still does it, this one proves this package still asks it to. A
// watcher built on per-file descriptors instead would pass the first and fail
// here the moment a record it did not know about at Start was rewritten.
func TestAnInPlaceRewriteIsOneChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "98693.json")
	record(t, path, `{"status":"before"}`)

	w, clk := fakeWatcher(t, dir)
	record(t, path, `{"status":"after"}`)

	flush(t, clk)
	wantOne(t, drain(t, w, quietWait), path, KindChanged)
}

// WRONG: one event per fsnotify wake-up. Session 98693 was measured rewriting
// its record twice thirteen seconds apart, and a single rewrite can produce
// more than one underlying event; a consumer that rebuilds its whole document
// per event does that work twice for one change.
//
// The window here is deliberately LARGE, because what is being tested is that
// two writes inside one window collapse — not how long the window is. Five
// hundred milliseconds means the two writes have to be separated by half a
// second of scheduling delay before this test can lie, where 50ms is within
// reach of a loaded machine under -race.
func TestABurstInsideTheWindowIsOneChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "98693.json")
	record(t, path, `{"n":0}`)

	const window = 500 * time.Millisecond
	w, err := Start(Config{Dir: dir, Window: window})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	record(t, path, `{"n":1}`)
	record(t, path, `{"n":2}`)

	// The quiet period has to outlast the window twice over: once to reach the
	// first flush, and again afterwards to give a second flush — the failure
	// this test is looking for — room to arrive.
	wantOne(t, drain(t, w, 2*window), path, KindChanged)
}

// WRONG: a window that never closes, or one that swallows genuine successive
// changes. 98693's two rewrites were thirteen seconds apart and are two facts
// about the session, not one.
//
// The window here is SHORT and the gap is six times longer, so the only way to
// report one event is to have no window boundary at all.
func TestTwoWritesEitherSideOfTheWindowAreTwoChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "1724.json")
	record(t, path, `{"n":0}`)

	w, err := Start(Config{Dir: dir, Window: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	record(t, path, `{"n":1}`)
	time.Sleep(60 * time.Millisecond)
	record(t, path, `{"n":2}`)

	got := drain(t, w, quietWait)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	for i, ev := range got {
		if ev.Path != path || ev.Kind != KindChanged {
			t.Errorf("event %d is %s %s, want changed %s", i, ev.Kind, ev.Path, path)
		}
	}
}

// WRONG: no suffix filter. The registry directory holds 33 `.key` siblings
// against 32 records plus a LIVE-PIDS.txt, none of which a reader of session
// state has any use for — and every one of their writes would wake a scan that
// finds nothing changed, undoing the one property the no-polling design exists
// to buy.
//
// The `.json` write AFTER the two negatives is what makes this test mean
// something. Without it a watcher that sees nothing at all — a broken Add, a
// dead loop — would pass. With it, the flush must contain exactly one event
// and it must be the record: an unfiltered watcher has all three pending in
// the same window, because a fake clock means the window never closes until
// the test says so.
func TestAKeyFileWriteWakesNothing(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "56526.e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855.key")
	pids := filepath.Join(dir, "LIVE-PIDS.txt")
	rec := filepath.Join(dir, "98198.json")
	record(t, rec, `{"status":"before"}`)

	w, clk := fakeWatcher(t, dir)

	// Deliberately NOT the canary string the real .key files hold. This test
	// writes its own bytes into its own temp directory and never reads a .key
	// file, here or anywhere.
	record(t, key, "not key material")
	record(t, pids, "98198 alive\n")
	record(t, rec, `{"status":"after"}`)

	flush(t, clk)
	wantOne(t, drain(t, w, quietWait), rec, KindChanged)
}

// WRONG: reporting a removed record as a change, so a reader re-reads a path
// that is not there and has to work out from an ENOENT what happened; or
// reporting nothing at all, so a session that was cleaned up stays in the
// document forever.
func TestARemovedRecordVanishes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "60001.json")
	record(t, path, `{"status":"idle"}`)

	w, clk := fakeWatcher(t, dir)
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the record: %v", err)
	}

	flush(t, clk)
	wantOne(t, drain(t, w, quietWait), path, KindVanished)
}

// WRONG: treating a vanished path as terminal — closing its watch and never
// looking at it again. §4.3: handling a delete by re-arming on the PATH is
// correct under BOTH in-place writing and a future switch to rename(2), where
// every publish unlinks the old inode. That hedge costs nothing and it is the
// only defence against a write-discipline change we do not control.
func TestAPathThatComesBackChangesAgain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "60001.json")
	record(t, path, `{"status":"idle"}`)

	w, clk := fakeWatcher(t, dir)

	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the record: %v", err)
	}
	flush(t, clk)
	wantOne(t, drain(t, w, quietWait), path, KindVanished)

	record(t, path, `{"status":"busy"}`)
	flush(t, clk)
	wantOne(t, drain(t, w, quietWait), path, KindChanged)
}

// WRONG: Stop implemented by closing a channel, which panics the second time —
// and a caller with both a quit path and a teardown path has exactly that
// shape. Or a Stop that does not wait, so the goroutine is still draining
// fsnotify when the test's directory is removed.
func TestStopIsIdempotentAndClosesBothChannels(t *testing.T) {
	dir := t.TempDir()
	w, err := Start(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	w.Stop()
	w.Stop()
	w.Stop()

	if _, ok := <-w.Events(); ok {
		t.Error("the events channel is still open after Stop; a consumer ranging it would hang")
	}
	if _, ok := <-w.Errors(); ok {
		t.Error("the errors channel is still open after Stop")
	}
}

// WRONG: a Stop that returns before its goroutine has, or one that leaves the
// fsnotify watcher open. magmux holds a descriptor per PTY already and the
// default soft limit on macOS is 256; a watcher that leaks one goroutine and
// N+1 descriptors per start is a process that dies of exhaustion somewhere
// else entirely, hours later.
//
// Goroutines are counted rather than descriptors because the count is exact
// and portable. The descriptor half is covered by
// TestRepeatedStartStopLeaksNoDescriptor.
func TestStopLeavesNoGoroutineBehind(t *testing.T) {
	dir := t.TempDir()
	record(t, filepath.Join(dir, "1.json"), `{}`)

	// One warm-up cycle first: the first fsnotify watcher in a process starts
	// machinery that is not per-watcher, and counting across it would measure
	// that instead.
	warm, err := Start(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	warm.Stop()

	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		w, err := Start(Config{Dir: dir})
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		record(t, filepath.Join(dir, "1.json"), `{"n":1}`)
		w.Stop()
	}

	// A goroutine that has been signalled still has to be scheduled before it
	// is gone, so this settles rather than asserting instantly. It is bounded,
	// and a real leak never settles.
	var after int
	for i := 0; i < 50; i++ {
		after = runtime.NumGoroutine()
		if after <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d goroutines before 20 start/stop cycles, %d after: the loop is not exiting", before, after)
}

// WRONG: not closing the fsnotify watcher on the way out. The goroutine test
// above cannot see that — fsnotify's kqueue backend would have gone with it —
// but the kqueue descriptor and the one it holds per directory ENTRY would
// still be open.
//
// Descriptors are counted by opening a file and reading the number the kernel
// gave it: Unix assigns the lowest free descriptor, so a fresh open after the
// cycles lands on the same number as one before them if and only if nothing
// was left open in between. That is exact, costs a microsecond, and needs no
// external tool — where lsof would need a subprocess and RLIMIT arithmetic
// would need a loop long enough to exhaust whatever the limit happens to be.
func TestRepeatedStartStopLeaksNoDescriptor(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"1.json", "2.json", "3.json"} {
		record(t, filepath.Join(dir, n), `{}`)
	}

	probe := func() uintptr {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatalf("probing the descriptor table: %v", err)
		}
		fd := f.Fd()
		f.Close()
		return fd
	}

	warm, err := Start(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	warm.Stop()

	before := probe()
	for i := 0; i < 20; i++ {
		w, err := Start(Config{Dir: dir})
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		w.Stop()
	}
	// Settled rather than asserted instantly, for the same reason the
	// goroutine count is: a descriptor closed by a goroutine on its way out is
	// closed when that goroutine runs, not when Stop returns. A real leak
	// never settles.
	var after uintptr
	for i := 0; i < 50; i++ {
		after = probe()
		if after <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("a fresh descriptor was %d before 20 start/stop cycles and %d after: "+
		"each cycle is leaving at least one open. The watcher on a directory of 3 "+
		"records holds a kqueue plus one descriptor per entry.", before, after)
}

// WRONG: taking the window from a package-level constant, so the seam exists
// on paper and every test that needs a different window waits out 50ms of real
// time — or worse, arming a ticker, which emits the same events under a fake
// clock and differs only in what it costs when nothing is happening.
//
// This is the deterministic half of the coalescing story: exactly ONE alarm is
// armed for a burst, it is armed for exactly the configured window, and no
// second alarm appears until the next change. The two tests above measure the
// behaviour in real time; this one measures the mechanism.
func TestTheWindowIsOneAlarmArmedForTheConfiguredDelay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "98693.json")
	record(t, path, `{"n":0}`)

	clk := obs.NewFakeClock(time.Unix(1789545200, 0))
	w, err := Start(Config{Dir: dir, Clock: clk, Window: 250 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	record(t, path, `{"n":1}`)
	ft, err := clk.NextTimer(armWait)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if ft.Delay() != 250*time.Millisecond {
		t.Errorf("the window alarm was armed for %s, want the configured 250ms", ft.Delay())
	}

	// A second write inside the same window must not arm a second alarm. The
	// wait is what makes the absence mean something.
	record(t, path, `{"n":2}`)
	if err := clk.NoTimerWithin(quietWait); err != nil {
		t.Errorf("a second alarm was armed inside one window: %v", err)
	}

	ft.Fire(time.Unix(1789545200, 0).Add(250 * time.Millisecond))
	wantOne(t, drain(t, w, quietWait), path, KindChanged)

	// And nothing is armed once the window has flushed: an idle directory must
	// cost nothing at all, which is the whole no-polling decision.
	if err := clk.NoTimerWithin(quietWait); err != nil {
		t.Errorf("an alarm is live with nothing pending: %v", err)
	}
}

// WRONG: reporting a missing directory as a watcher that simply never fires.
// The registry root is a path a user can mistype on the command line, and the
// difference between "no sessions" and "that directory does not exist" is the
// whole of the diagnosis.
func TestStartRefusesADirectoryItCannotWatch(t *testing.T) {
	if _, err := Start(Config{Dir: ""}); err == nil {
		t.Error("Start accepted an empty directory")
	}
	missing := filepath.Join(t.TempDir(), "not-there")
	if _, err := Start(Config{Dir: missing}); err == nil {
		t.Error("Start accepted a directory that does not exist")
	}
}

// WRONG: blocking on the consumer. A watcher that blocks stops draining
// fsnotify's channel, and a watcher that has stopped draining fsnotify is a
// watcher that is silent with no error anywhere — the one failure shape this
// whole subsystem is written to make impossible. Dropping silently is the
// other wrong answer: a consumer cannot tell a gap from a quiet machine unless
// it is told there was a gap.
// It asserts the PROPERTY and not a sequence of identities. An earlier version
// asserted exactly which record carried the count, and it was wrong twice over:
// how the underlying watch batches six writes into windows is not this
// package's behaviour to pin, and firing a window while a write is still in
// flight puts that write in the NEXT window — both of which move which event
// carries the drops without changing anything the design promises.
func TestASlowConsumerLosesEventsAndIsToldHowMany(t *testing.T) {
	dir := t.TempDir()
	base := time.Unix(1789545200, 0)
	clk := obs.NewFakeClock(base)
	w, err := Start(Config{Dir: dir, Clock: clk, Buffer: 1})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	// Six records against a buffer of one, with NOTHING reading. However they
	// fall into windows, at most one of them can be sitting in the buffer.
	const records = 6
	for i := 0; i < records; i++ {
		record(t, filepath.Join(dir, fmt.Sprintf("%d.json", i)), `{}`)
	}

	// Close every window the loop opens, until it stops opening them. That
	// this terminates at all is the first half of the test: a loop blocked on
	// a full events channel never processes another fs event, so it never arms
	// another alarm — and it would never reach the quiescent state below.
	windows := 0
	for {
		ft, err := clk.NextTimer(500 * time.Millisecond)
		if err != nil {
			break
		}
		windows++
		ft.Fire(base.Add(DefaultWindow))
	}
	if windows == 0 {
		t.Fatal("the watcher opened no window at all for six records")
	}

	// One event survived, and it was published before anything was dropped.
	var first Event
	select {
	case first = <-w.Events():
	case <-time.After(eventWait):
		t.Fatal("nothing was delivered at all")
	}
	if first.Dropped != 0 {
		t.Errorf("the first event reports %d drops, but nothing had been dropped when it was published", first.Dropped)
	}

	// The loop is quiescent and the buffer has a slot again, so one more
	// change gets through — and it is the one that has to carry the count of
	// what was lost, because a consumer cannot tell a gap from a quiet machine
	// unless it is told there was a gap.
	record(t, filepath.Join(dir, "last.json"), `{}`)
	ft, err := clk.NextTimer(armWait)
	if err != nil {
		t.Fatalf("%v — the loop stopped processing events, which is what blocking on a consumer looks like", err)
	}
	ft.Fire(base.Add(2 * DefaultWindow))

	select {
	case ev := <-w.Events():
		if ev.Dropped < records-1 {
			t.Errorf("it reports %d drops; at least %d of the %d records could not fit a buffer of one",
				ev.Dropped, records-1, records)
		}
	case <-time.After(eventWait):
		t.Fatal("no further event: the loop blocked on the consumer instead of dropping")
	}
}
