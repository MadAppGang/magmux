package mux

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/MadAppGang/magmux/client"
	"github.com/MadAppGang/magmux/sockdir"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/obsview"
)

// The socket-backed pane table: what closes phase 6's gap, and what it refuses.
//
// Nothing here speaks to a real magmux, reads ~/.claude, or probes a live pid.
// The listener below is this test's own, in this test's own socket directory,
// and it is named with a WORD rather than a pid on purpose: discoverSessions
// asks the kernel about a pid-named socket's owner, and no test may.

// fakeSockMagmux is just enough of magmux's socket for a pane table: the
// guaranteed connect-time aggregate, and one reply per request that carries an
// id.
type fakeSockMagmux struct {
	path string

	mu     sync.Mutex
	panes  string
	focus  []int
	closed bool
}

// startFakeSockMagmux listens on <sockdir.Dir>/magmux-<name>.sock and serves panes.
//
// It points the PACKAGE's socket directory at a temporary one for the test's
// lifetime, which is what keeps discovery off the developer's own running
// magmuxes. Nothing in this package runs in parallel, so the swap is safe —
// the same argument shortProbes already makes.
func startFakeSockMagmux(t *testing.T, name, panes string) *fakeSockMagmux {
	t.Helper()
	// os.MkdirTemp rather than t.TempDir: a unix socket path is capped at
	// about 104 bytes on darwin (sun_path), and t.TempDir embeds the test's
	// own name, which is long enough here to make bind fail with EINVAL. The
	// same reason tempSockPath does it this way.
	dir, err := os.MkdirTemp("", "mmx")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	prev := sockdir.Dir
	sockdir.Dir = dir
	t.Cleanup(func() { sockdir.Dir = prev })

	return listenFakeSockMagmux(t, dir, name, panes)
}

// listenFakeSockMagmux adds one more fake magmux to a socket directory that
// startFakeSockMagmux has already pointed the package at — which is how a test
// stands up TWO magmuxes that discovery will find together.
func listenFakeSockMagmux(t *testing.T, dir, name, panes string) *fakeSockMagmux {
	t.Helper()
	path := filepath.Join(dir, "magmux-"+name+".sock")
	ln, lerr := net.Listen("unix", path)
	if lerr != nil {
		t.Fatalf("listen %s: %v", path, lerr)
	}
	f := &fakeSockMagmux{path: path, panes: panes}
	t.Cleanup(func() {
		f.mu.Lock()
		f.closed = true
		f.mu.Unlock()
		ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSockMagmux) serve(conn net.Conn) {
	defer conn.Close()
	// The connect-time aggregate, first and guaranteed. discoverSessions reads
	// exactly this one line and closes.
	fmt.Fprintf(conn, "{\"type\":\"snapshot\",\"panes\":[%s]}\n", f.panes)

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var msg map[string]any
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		verb, _ := client.EventString(msg, "type")
		id := msg["id"]
		result := map[string]any{}
		switch verb {
		case "capabilities":
			result = map[string]any{"protocol": 1}
		case "focus":
			pane, _ := client.EventInt(msg, "pane")
			f.mu.Lock()
			f.focus = append(f.focus, pane)
			f.mu.Unlock()
			result = map[string]any{"pane": pane, "focused": true}
		}
		if id == nil {
			continue
		}
		out, _ := json.Marshal(map[string]any{
			"type": "reply", "id": id, "ok": true, "result": result,
		})
		fmt.Fprintln(conn, string(out))
	}
}

func (f *fakeSockMagmux) focused() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.focus...)
}

// stagedPanes is the aggregate a two-pane magmux with a hidden panel sends.
// Pane 1 is the panel: no process, therefore no pid, therefore not an ancestor
// of anything.
const stagedPanes = `{"pane":0,"state":"running","cmd":"claude","pid":900001},` +
	`{"pane":1,"state":"panel","control":true},` +
	`{"pane":2,"state":"running","cmd":"npm run dev","pid":900002}`

// TestTheSocketPaneTableIsBuiltFromTheConnectTimeAggregate is phase 6's
// finding, made true: the wire already carries every pane's id and its child's
// pid, so a client needs no new verb.
func TestTheSocketPaneTableIsBuiltFromTheConnectTimeAggregate(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()
	startFakeSockMagmux(t, "obstable", stagedPanes)

	loc := newSockPaneLocator(context.Background())
	if loc == nil {
		t.Fatal("no pane table was built from a reachable magmux")
	}
	// The SHELL's pid is what the aggregate carries; the session's pid is the
	// tool's, two hops up. That asymmetry is the whole reason this is a walk
	// and not a comparison.
	loc.ppid = stagedTree(map[int]int{
		700100: 700010, // the tool's pid -> a wrapper
		700010: 900001, // the wrapper   -> the pane's own shell
	})

	id, live := loc.paneForPID(700100)
	if live != model.LivenessYes || id != 0 {
		t.Fatalf("the walk resolved pane %d (%v), want pane 0", id, live)
	}
	if id, live := loc.paneForPID(900002); live != model.LivenessYes || id != 2 {
		t.Fatalf("a pane's own pid resolved to %d (%v), want pane 2", id, live)
	}
	// The panel has no process at all, so it contributes nothing and cannot be
	// walked to.
	if got := loc.PaneAlive(obsview.PaneRef(1)); got != model.LivenessNo {
		t.Errorf("the control panel is in the pane table as %v; it has no pid and cannot be "+
			"an ancestor of anything", got)
	}
	// A pid belonging to nothing in any magmux is a determined NO, not an
	// undetermined answer: the walk reached init without meeting one of ours.
	loc.ppid = stagedTree(map[int]int{700200: 1})
	if id, live := loc.paneForPID(700200); live != model.LivenessNo {
		t.Errorf("a session in no magmux resolved to %d (%v), want a determined no", id, live)
	}
}

// TestFocusOverTheSocketReachesTheRightPane is the other half: the `focus`
// verb, unchanged, reached by a client that resolved the pane itself.
func TestFocusOverTheSocketReachesTheRightPane(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()
	fake := startFakeSockMagmux(t, "obsfocus", stagedPanes)

	loc := newSockPaneLocator(context.Background())
	if loc == nil {
		t.Fatal("no pane table")
	}
	if _, err := loc.Focus(2); err != nil {
		t.Fatalf("Focus(2): %v", err)
	}
	waitUntilObs(t, func() bool { return len(fake.focused()) == 1 }, "the focus to arrive")
	if got := fake.focused(); got[0] != 2 {
		t.Errorf("magmux was asked to focus pane %d, want 2", got[0])
	}
}

// TestAnAmbiguousPaneIdIsRefusedRatherThanPicked.
//
// A pane id is unique within ONE magmux and not across several, and focusing
// the wrong one is exactly the class of silent error this layer exists to
// avoid — the same rule resolveObsTarget applies to an ambiguous session name.
//
// This is what an UNBOUND table does, and it stays a refusal. What changed in
// fix round 1 is that obsFocus no longer hands the pipeline an unbound table:
// the staging below — two magmuxes, two different pane pids, both pane 1 — is
// not an invented id, it is the ordinary shape of a machine running two
// magmuxes, and TestObsFocusLandsInTheMagmuxTheSessionIsIn is that shape
// focused correctly.
func TestAnAmbiguousPaneIdIsRefusedRatherThanPicked(t *testing.T) {
	loc := &sockPaneLocator{
		byPanePID: map[int]sockPaneRef{
			900001: {sock: "/tmp/magmux-a.sock", id: 1},
			900003: {sock: "/tmp/magmux-b.sock", id: 1},
		},
		byPaneID: map[int][]sockPaneRef{
			1: {{sock: "/tmp/magmux-a.sock", id: 1}, {sock: "/tmp/magmux-b.sock", id: 1}},
		},
		focus: func(ctx context.Context, sock string, paneID int) error {
			t.Errorf("a focus was sent to %s for an ambiguous pane id", sock)
			return nil
		},
	}
	_, err := loc.Focus(1)
	if err == nil {
		t.Fatal("an ambiguous pane id was focused")
	}
	for _, want := range []string{"magmux-a.sock", "magmux-b.sock", "ambiguous"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q and does not mention %q", err, want)
		}
	}
}

// TestObsFocusLandsInTheMagmuxTheSessionIsIn — fix round 1, item 1, the HIGH.
//
// Two magmuxes, and BOTH have a pane 0, because every magmux numbers its panes
// from 0. The session's ancestry reaches magmux B's pane 0. The focus must land
// on B's pane 0, and magmux A must hear nothing at all.
//
// Before the fix the socket table flattened every magmux into pane pid -> bare
// pane id, so the walk found B's pane through a globally unique pid and handed
// back "0" with the magmux thrown away. Focus then saw pane 0 in two magmuxes
// and refused it as ambiguous — so on any machine running two magmuxes, `magmux
// obs focus` refused most sessions it could plainly see.
//
// End to end through the command: real socket discovery against this test's
// own listeners (named with words, never pids), a staged process tree, the real
// pipeline and gate, and a trail in a temporary directory.
func TestObsFocusLandsInTheMagmuxTheSessionIsIn(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	a := startFakeSockMagmux(t, "obsbinda",
		`{"pane":0,"state":"running","cmd":"claude","pid":900001},`+
			`{"pane":1,"state":"panel","control":true},`+
			`{"pane":2,"state":"running","cmd":"zsh","pid":900002}`)
	b := listenFakeSockMagmux(t, sockdir.Dir, "obsbindb",
		`{"pane":0,"state":"running","cmd":"claude","pid":900011},`+
			`{"pane":1,"state":"panel","control":true},`+
			`{"pane":2,"state":"running","cmd":"zsh","pid":900012}`)

	loc := newSockPaneLocator(context.Background())
	if loc == nil {
		t.Fatal("no pane table was built from two reachable magmuxes")
	}
	if got := len(loc.byPaneID[0]); got != 2 {
		t.Fatalf("pane 0 is in %d magmuxes, want 2 — without that this test stages no ambiguity", got)
	}
	// The session is the TOOL, two hops under magmux B's pane-0 shell.
	loc.ppid = stagedTree(map[int]int{
		700100: 700010, // the tool     -> a wrapper
		700010: 900011, // the wrapper  -> magmux B's pane 0 shell
	})

	dir := t.TempDir()
	stageRegistry(t, dir, 700100, "id-in-b", "session-in-b")
	trail := trailPath(t)

	out, errOut := &strings.Builder{}, &strings.Builder{}
	code := obsFocus(obsFocusConfig{
		Root:      dir,
		AuditPath: trail,
		Target:    "700100",
		Policy:    act.Default(),
		Panes:     loc,
		Out:       out,
		Err:       errOut,
	})
	if code != 0 {
		t.Fatalf("the focus exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	waitUntilObs(t, func() bool { return len(b.focused()) == 1 }, "the focus to reach magmux B")
	if got := b.focused(); got[0] != 0 {
		t.Errorf("magmux B was asked to focus pane %d, want 0", got[0])
	}
	if got := a.focused(); len(got) != 0 {
		t.Errorf("magmux A — which the session is NOT in — was asked to focus %v", got)
	}

	records := readTrail(t, trail)
	if len(records) != 2 || records[1].Outcome == nil || !records[1].Outcome.Done {
		t.Fatalf("the trail holds %+v, want an attempt and a done outcome", records)
	}
	if records[1].Target.PaneID == nil || *records[1].Target.PaneID != 0 {
		t.Errorf("the record aims at pane %v, want 0", records[1].Target.PaneID)
	}
	t.Logf("stdout:\n%s", out)
}

// TestObsFocusOfASessionInNoMagmuxFocusesNothing — the other branch of the
// binding: magmuxes are running, and the session is in none of them. Nothing
// is bound, the pipeline gets no focuser, and the gate records "cannot" —
// with neither magmux asked to do anything.
func TestObsFocusOfASessionInNoMagmuxFocusesNothing(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	a := startFakeSockMagmux(t, "obsnonea", `{"pane":0,"state":"running","cmd":"zsh","pid":900001}`)
	b := listenFakeSockMagmux(t, sockdir.Dir, "obsnoneb", `{"pane":0,"state":"running","cmd":"zsh","pid":900011}`)
	loc := newSockPaneLocator(context.Background())
	if loc == nil {
		t.Fatal("no pane table was built from two reachable magmuxes")
	}
	loc.ppid = stagedTree(map[int]int{700200: 1}) // straight to init

	dir := t.TempDir()
	stageRegistry(t, dir, 700200, "id-nowhere", "session-nowhere")
	trail := trailPath(t)

	out, errOut := &strings.Builder{}, &strings.Builder{}
	code := obsFocus(obsFocusConfig{
		Root: dir, AuditPath: trail, Target: "700200", Policy: act.Default(),
		Panes: loc, Out: out, Err: errOut,
	})
	if code == 0 {
		t.Fatalf("a focus of a session in no magmux exited 0\nstdout:\n%s", out)
	}
	if got := append(a.focused(), b.focused()...); len(got) != 0 {
		t.Errorf("a magmux was asked to focus %v for a session in neither", got)
	}
	records := readTrail(t, trail)
	if len(records) != 1 || records[0].Event != act.EventRefused {
		t.Fatalf("the trail holds %+v, want one refusal", records)
	}
	if !strings.Contains(records[0].Outcome.Error, "nothing declares") {
		t.Errorf("the refusal is %q, want the \"cannot\" answer", records[0].Outcome.Error)
	}
}

// TestForSessionNarrowsTheTableToOneMagmux — the unit under the test above.
// The narrowed table holds exactly the magmux the walk reached, so every id it
// can be asked about is unique; a session in no magmux, or one whose walk
// cannot be completed, narrows to nothing and says which.
func TestForSessionNarrowsTheTableToOneMagmux(t *testing.T) {
	loc := &sockPaneLocator{
		byPanePID: map[int]sockPaneRef{
			900001: {sock: "/nowhere/magmux-a.sock", id: 0},
			900002: {sock: "/nowhere/magmux-a.sock", id: 2},
			900011: {sock: "/nowhere/magmux-b.sock", id: 0},
			900012: {sock: "/nowhere/magmux-b.sock", id: 2},
		},
		byPaneID: map[int][]sockPaneRef{
			0: {{sock: "/nowhere/magmux-a.sock", id: 0}, {sock: "/nowhere/magmux-b.sock", id: 0}},
			2: {{sock: "/nowhere/magmux-a.sock", id: 2}, {sock: "/nowhere/magmux-b.sock", id: 2}},
		},
		ppid: stagedTree(map[int]int{700100: 900012, 700200: 1}),
	}
	var sent []string
	loc.focus = func(ctx context.Context, sock string, paneID int) error {
		sent = append(sent, fmt.Sprintf("%s#%d", sock, paneID))
		return nil
	}

	one, live := loc.forSession(700100)
	if live != model.LivenessYes || one == nil {
		t.Fatalf("forSession = %v, %v; want a table", one, live)
	}
	for id, refs := range one.byPaneID {
		for _, r := range refs {
			if r.sock != "/nowhere/magmux-b.sock" {
				t.Errorf("the narrowed table still holds pane %d of %s", id, r.sock)
			}
		}
	}
	if _, err := one.Focus(0); err != nil {
		t.Fatalf("pane 0 of the one magmux the session is in was refused: %v", err)
	}
	if len(sent) != 1 || sent[0] != "/nowhere/magmux-b.sock#0" {
		t.Errorf("the focus went to %v, want magmux B's pane 0", sent)
	}
	// The unbound table is untouched by narrowing and still refuses.
	if _, err := loc.Focus(0); err == nil {
		t.Error("narrowing changed the table it was built from")
	}

	if one, live := loc.forSession(700200); one != nil || live != model.LivenessNo {
		t.Errorf("a session in no magmux narrowed to %v, %v; want nil, a determined no", one, live)
	}
	if one, live := loc.forSession(700300); one != nil || live != model.LivenessUnknown {
		t.Errorf("an unwalkable session narrowed to %v, %v; want nil, undetermined", one, live)
	}
	if tbl := loc.bindToSession(700200); tbl != nil {
		t.Error("bindToSession returned a non-nil table (a typed nil?) for a session in no magmux")
	}
}

// TestNoMagmuxMeansNoPaneTable: nil is a REAL answer and the pipeline knows
// what to do with it — no locator, no focuser, a navigate refused with
// "nothing can do this". A machine with no magmux running is the ordinary case
// for this command, not a failure.
func TestNoMagmuxMeansNoPaneTable(t *testing.T) {
	dir := t.TempDir()
	prev := sockdir.Dir
	sockdir.Dir = dir
	t.Cleanup(func() { sockdir.Dir = prev })

	if loc := newSockPaneLocator(context.Background()); loc != nil {
		t.Fatalf("a pane table was built from an empty socket directory: %+v", loc)
	}
	// And the typed-nil trap, which would turn "no pane table" into a panic in
	// an actor goroutine.
	if tbl := obsPaneTableOrNil(nil); tbl != nil {
		t.Error("a nil *sockPaneLocator became a non-nil obsPaneTable")
	}
}

// TestObsFocusWithAPaneTableFocusesAndRecordsIt is deliverable 1's other half,
// end to end: a session named on the command line, resolved to a pane by
// ancestry, focused, and recorded.
//
// The pane table is staged rather than discovered, so that what is under test
// is the COMMAND rather than the socket — the socket is TestFocusOverTheSocket
// ReachesTheRightPane's subject, above.
func TestObsFocusWithAPaneTableFocusesAndRecordsIt(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")
	trail := trailPath(t)
	table := &stubPaneTable{pane: 3}

	out, errOut := &strings.Builder{}, &strings.Builder{}
	code := obsFocus(obsFocusConfig{
		Root:      dir,
		AuditPath: trail,
		Target:    "4242",
		Policy:    act.Default(),
		Panes:     table,
		Out:       out,
		Err:       errOut,
	})
	if code != 0 {
		t.Fatalf("the focus exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if len(table.focused) != 1 || table.focused[0] != 3 {
		t.Fatalf("the focuser was called %v, want exactly pane 3", table.focused)
	}

	records := readTrail(t, trail)
	if len(records) != 2 {
		t.Fatalf("the trail holds %d records, want an attempt and an outcome", len(records))
	}
	if records[0].Event != act.EventAttempt || records[0].Outcome != nil {
		t.Errorf("the first record is %+v, want an attempt with a null outcome", records[0])
	}
	if records[1].Outcome == nil || !records[1].Outcome.Done {
		t.Errorf("the outcome is %+v, want done", records[1].Outcome)
	}
	if records[1].Target.PaneID == nil || *records[1].Target.PaneID != 3 {
		t.Errorf("the record aims at pane %v, want 3 — a trail read months later has to say "+
			"what it acted on", records[1].Target.PaneID)
	}
	if records[1].By != act.RequesterCLI {
		t.Errorf("the record says it was requested by %v, want the CLI", records[1].By)
	}
}

// TestObsFocusWithNoPaneTableStillRefusesHonestly: the phase 6 behaviour is
// unchanged on a machine with no magmux running, and the refusal is still
// "cannot" rather than "will not".
func TestObsFocusWithNoPaneTableStillRefusesHonestly(t *testing.T) {
	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")
	trail := trailPath(t)

	out, errOut := &strings.Builder{}, &strings.Builder{}
	code := obsFocus(obsFocusConfig{
		Root: dir, AuditPath: trail, Target: "4242", Policy: act.Default(),
		Out: out, Err: errOut,
	})
	if code == 0 {
		t.Fatalf("a focus with no pane table exited 0\nstdout:\n%s", out)
	}
	records := readTrail(t, trail)
	if len(records) != 1 || records[0].Event != act.EventRefused {
		t.Fatalf("the trail holds %d records, want one refusal", len(records))
	}
}

// TestTheWalkHasOneImplementation is the rule that keeps two pane tables from
// drifting: the hard part was never the table, it was that a session's pid is
// the TOOL's and a pane's is the SHELL's.
func TestTheWalkHasOneImplementation(t *testing.T) {
	body, err := os.ReadFile("obssock.go")
	if err != nil {
		t.Fatalf("reading obssock.go: %v", err)
	}
	if !strings.Contains(string(body), "walkToPane(") {
		t.Error("the socket locator does not use walkToPane; a second copy of the ancestry walk " +
			"drifts the first time one of the two learns something the other does not")
	}
	if strings.Contains(string(body), "ppidOf(") {
		t.Error("the socket locator calls proc.PPIDOf directly, which means it is walking the tree " +
			"itself rather than through walkToPane")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// stagedTree is a process tree a test can stand up without a single live
// process. An unknown pid ends the chain, which is what proc.PPIDOf's own contract
// says an error means.
func stagedTree(parents map[int]int) func(int) (int, error) {
	return func(pid int) (int, error) {
		if parent, ok := parents[pid]; ok {
			return parent, nil
		}
		return 0, fmt.Errorf("no parent for %d", pid)
	}
}

// stubPaneTable is a pane table with no socket behind it.
type stubPaneTable struct {
	pane    int
	focused []int
}

func (s *stubPaneTable) PaneFor(model.Session) (model.PaneRef, model.Liveness) {
	return obsview.PaneRef(s.pane), model.LivenessYes
}
func (s *stubPaneTable) PaneAlive(model.PaneRef) model.Liveness { return model.LivenessYes }
func (s *stubPaneTable) Focus(paneID int) (int, error) {
	s.focused = append(s.focused, paneID)
	return -1, nil
}
