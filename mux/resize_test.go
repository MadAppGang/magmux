package mux

// The `resize` op, in process: the refusal table, the same-size no-op, manual
// mode against SIGWINCH and Ctrl-G r, the panel row and the advertising.
//
// Requests are decoded from JSON TEXT rather than built as sockMsg literals,
// because half of the contract is about what the wire carries — "rows":"40",
// 40.5, true — and a literal would type those values for the test instead of
// letting the parser meet them.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// sessionMux is n PTY-less SESSION panes (see asSessions) and no panel.
func sessionMux(t *testing.T, n int) *Magmux {
	t.Helper()
	return asSessions(newTestMux(t, ctrlPanes(n)...))
}

// resizeMsg decodes one socket line the way the socket reader does.
func resizeMsg(t *testing.T, line string) sockMsg {
	t.Helper()
	var msg sockMsg
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("the request %s did not decode as a sockMsg — a typed field would drop it on the wire: %v", line, err)
	}
	return msg
}

// resizeOn runs one resize request through the socket's dispatch table.
func resizeOn(t *testing.T, m *Magmux, line string) (map[string]any, error) {
	t.Helper()
	return m.dispatchSocketMsg(resizeMsg(t, line))
}

// mustResize runs a request that must succeed and returns its result.
func mustResize(t *testing.T, m *Magmux, line string) map[string]any {
	t.Helper()
	res, err := resizeOn(t, m, line)
	if err != nil {
		t.Fatalf("%s: refused %q: %v", line, verbErrCode(err), err)
	}
	return res
}

// wantResult compares a result map with the four contract fields.
func wantResult(t *testing.T, what string, res map[string]any, rows, cols int, changed, manual bool) {
	t.Helper()
	got := fmt.Sprintf("%v %v %v %v", res["rows"], res["cols"], res["changed"], res["manual"])
	want := fmt.Sprintf("%d %d %v %v", rows, cols, changed, manual)
	if got != want {
		t.Fatalf("%s: result rows/cols/changed/manual = %s, want %s (%v)", what, got, want, res)
	}
	if len(res) != 4 {
		t.Fatalf("%s: result carries %d fields, the contract has exactly four: %v", what, len(res), res)
	}
}

// screenSize reads the virtual screen under the lock.
func screenSize(m *Magmux) (int, int) {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.rows, m.cols
}

// lastSignal is the panel's newest row.
func lastSignal(t *testing.T, m *Magmux) ctrlSignal {
	t.Helper()
	m.control.mu.Lock()
	defer m.control.mu.Unlock()
	if len(m.control.signals) == 0 {
		t.Fatal("the panel recorded nothing")
	}
	return m.control.signals[len(m.control.signals)-1]
}

// TestRefusedResizeLeavesTreeByteIdentical is the contract's refusal half,
// and it is written first because it is the contract: every refusal names its
// exact code, and leaves the tree, every pane's geometry, every screen's cells
// and every frame generation exactly as they were. There is no rollback —
// Screen.resize destroys the cells that do not fit — so this can only hold if
// validation never touches a Pane, which is what the fingerprint proves.
//
// The one side effect a refusal is ALLOWED is the panel's OUT row: the
// controller asked, and the panel records what controllers ask.
func TestRefusedResizeLeavesTreeByteIdentical(t *testing.T) {
	// The mux shapes the table runs against.
	one := func(t *testing.T) *Magmux { return sessionMux(t, 1) }
	two := func(t *testing.T) *Magmux { return sessionMux(t, 2) }
	interactive := func(t *testing.T) *Magmux {
		m := sessionMux(t, 1)
		m.autoRows, m.autoCols = 40, 120 // a 40x120 terminal
		return m
	}
	withHiddenPanel := func(t *testing.T) *Magmux {
		m, _ := muxWithPanel(t, 1)
		return asSessions(m)
	}
	withVisiblePanel := func(t *testing.T) *Magmux {
		m, _ := muxWithVisiblePanel(t, 1)
		return asSessions(m)
	}

	cases := []struct {
		name string
		mux  func(*testing.T) *Magmux
		line string
		code string
	}{
		// too_small: a session leaf would get less than 3x20.
		{"two panes squeezed", two, `{"type":"resize","rows":4,"cols":39}`, sockCodeTooSmall},
		{"pane form two rows", one, `{"type":"resize","pane":0,"rows":2,"cols":60}`, sockCodeTooSmall},
		{"pane form 19 cols", one, `{"type":"resize","pane":0,"rows":3,"cols":19}`, sockCodeTooSmall},
		{"screen is all status row", one, `{"type":"resize","rows":3,"cols":80}`, sockCodeTooSmall},

		// too_large: a side, the product, the terminal, and the status row
		// pushing a legal pane over the side bound.
		{"side over 10000", one, `{"type":"resize","rows":10001,"cols":80}`, sockCodeTooLarge},
		{"product over the cap", one, `{"type":"resize","rows":2001,"cols":2000}`, sockCodeTooLarge},
		{"huge number saturates", one, `{"type":"resize","rows":1e300,"cols":80}`, sockCodeTooLarge},
		{"taller than the terminal", interactive, `{"type":"resize","rows":41,"cols":120}`, sockCodeTooLarge},
		{"wider than the terminal", interactive, `{"type":"resize","rows":40,"cols":121}`, sockCodeTooLarge},
		{"pane form plus status row", one, `{"type":"resize","pane":0,"rows":10000,"cols":80}`, sockCodeTooLarge},

		// bad_request: shape.
		{"no cols", one, `{"type":"resize","rows":40}`, sockCodeBadRequest},
		{"no rows", one, `{"type":"resize","cols":40}`, sockCodeBadRequest},
		{"nothing at all", one, `{"type":"resize"}`, sockCodeBadRequest},
		{"rows as a string", one, `{"type":"resize","rows":"40","cols":100}`, sockCodeBadRequest},
		{"fractional rows", one, `{"type":"resize","rows":40.5,"cols":100}`, sockCodeBadRequest},
		{"zero rows", one, `{"type":"resize","rows":0,"cols":100}`, sockCodeBadRequest},
		{"negative cols", one, `{"type":"resize","rows":40,"cols":-1}`, sockCodeBadRequest},
		{"boolean cols", one, `{"type":"resize","rows":40,"cols":true}`, sockCodeBadRequest},
		{"auto with a size", one, `{"type":"resize","auto":true,"rows":40,"cols":100}`, sockCodeBadRequest},
		{"auto with rows only", one, `{"type":"resize","auto":true,"rows":40}`, sockCodeBadRequest},
		{"auto as a string", one, `{"type":"resize","auto":"yes"}`, sockCodeBadRequest},
		{"pane form, two sessions", two, `{"type":"resize","pane":0,"rows":20,"cols":60}`, sockCodeBadRequest},
		{"pane fan-out", one, `{"type":"resize","pane":"*","rows":20,"cols":60}`, sockCodeBadRequest},
		{"pane not an index", one, `{"type":"resize","pane":"zero","rows":20,"cols":60}`, sockCodeBadRequest},
		{"fractional pane", one, `{"type":"resize","pane":0.5,"rows":20,"cols":60}`, sockCodeBadRequest},
		{"pane form, visible panel", withVisiblePanel, `{"type":"resize","pane":0,"rows":20,"cols":60}`, sockCodeBadRequest},

		// A bad `pane`.
		{"no such pane", one, `{"type":"resize","pane":99,"rows":20,"cols":60}`, sockCodeNoSuchPane},
		{"negative pane", one, `{"type":"resize","pane":-4,"rows":20,"cols":60}`, sockCodeBadRequest},
		// -1/-2/-3 are parsePaneIndex's own sentinels; -3 is "absent", and
		// reading it that way would turn a bad pane into the screen form.
		{"pane is a sentinel", one, `{"type":"resize","pane":-3,"rows":20,"cols":60}`, sockCodeBadRequest},
		{"the panel", withHiddenPanel, `{"type":"resize","pane":1,"rows":20,"cols":60}`, sockCodePaneIsControl},
		{"auto on no such pane", one, `{"type":"resize","pane":99,"auto":true}`, sockCodeNoSuchPane},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.mux(t)
			before := treeFingerprint(m)

			_, err := resizeOn(t, m, tc.line)
			if got := verbErrCode(err); got != tc.code {
				t.Fatalf("%s: code %q (%v), want %q", tc.line, got, err, tc.code)
			}
			if after := treeFingerprint(m); after != before {
				t.Fatalf("a refused resize changed the tree:\n--- before\n%s--- after\n%s", before, after)
			}
			sig := lastSignal(t, m)
			if sig.verb != "resize" || sig.dir != "out" || sig.pane != -1 {
				t.Fatalf("newest panel row is %+v, want the refused resize OUT row at run level", sig)
			}
			if sig.ok == nil || *sig.ok || sig.code != tc.code {
				t.Fatalf("the OUT row's ack is ok=%v code=%q, want refused with %q", sig.ok, sig.code, tc.code)
			}
		})
	}
}

// TestResizeSameSizeIsANoOp: the same size again touches nothing — no
// Pane.resize, so no Screen.resize, no keyframe and no SetWinSize, all of
// which frameGen is the witness for — and still pins manual mode, so a
// repeated identical call is idempotent.
func TestResizeSameSizeIsANoOp(t *testing.T) {
	m := sessionMux(t, 1)
	m.headless = true
	before := treeFingerprint(m)

	res := mustResize(t, m, `{"type":"resize","rows":40,"cols":120}`)
	wantResult(t, "same size", res, 40, 120, false, true)

	m.treeMu.Lock()
	if m.manualRows != 40 || m.manualCols != 120 {
		t.Fatalf("a same-size resize did not pin manual mode: %dx%d", m.manualRows, m.manualCols)
	}
	m.manualRows, m.manualCols = 0, 0 // the one permitted difference
	m.treeMu.Unlock()
	if after := treeFingerprint(m); after != before {
		t.Fatalf("a same-size resize touched the tree:\n--- before\n%s--- after\n%s", before, after)
	}

	// And a real change does move frameGen, or the assertion above proved
	// nothing.
	gen := m.paneByID(0).frameGen.Load()
	res = mustResize(t, m, `{"type":"resize","rows":30,"cols":100}`)
	wantResult(t, "real change", res, 30, 100, true, true)
	if m.paneByID(0).frameGen.Load() == gen {
		t.Fatal("a real resize did not bump frameGen: the no-op proof above is vacuous")
	}
	// Pane form at the current size is a no-op too.
	res = mustResize(t, m, `{"type":"resize","pane":0,"rows":29,"cols":100}`)
	wantResult(t, "pane form, same size", res, 29, 100, false, true)
}

// TestResizePaneFormSizesThePane: with `pane`, rows/cols are the PANE's, and
// the screen is that plus the status row — or exactly that under --no-status.
func TestResizePaneFormSizesThePane(t *testing.T) {
	for _, hide := range []bool{false, true} {
		m := sessionMux(t, 1)
		m.hideStatus = hide
		res := mustResize(t, m, `{"type":"resize","pane":0,"rows":20,"cols":60}`)
		wantResult(t, fmt.Sprintf("hideStatus=%v", hide), res, 20, 60, true, true)
		p := m.paneByID(0)
		m.treeMu.RLock()
		h, w, rows, cols := p.h, p.w, m.rows, m.cols
		m.treeMu.RUnlock()
		p.mu.Lock()
		sr, sc := p.screen.rows, p.screen.cols
		p.mu.Unlock()
		wantRows := 21
		if hide {
			wantRows = 20
		}
		if h != 20 || w != 60 || sr != 20 || sc != 60 {
			t.Fatalf("hideStatus=%v: pane is %dx%d with a %dx%d screen, want exactly 20x60", hide, h, w, sr, sc)
		}
		if rows != wantRows || cols != 60 {
			t.Fatalf("hideStatus=%v: screen is %dx%d, want %dx60", hide, rows, cols, wantRows)
		}
	}

	// A hidden panel does not count against the one-session rule.
	m, _ := muxWithPanel(t, 1)
	asSessions(m)
	res := mustResize(t, m, `{"type":"resize","pane":0,"rows":20,"cols":60}`)
	wantResult(t, "hidden panel", res, 20, 60, true, true)

	// Wire spelling: 40.0 is an integer.
	m = sessionMux(t, 1)
	res = mustResize(t, m, `{"type":"resize","rows":30.0,"cols":90}`)
	wantResult(t, "30.0", res, 30, 90, true, true)
}

// TestSIGWINCHDoesNotUndoManual: the human's window writes auto* and nothing
// else. A smaller window clips the manual size while it cannot be shown; a
// larger one gives it back; only {"auto":true} releases it.
func TestSIGWINCHDoesNotUndoManual(t *testing.T) {
	m := sessionMux(t, 1)
	m.autoRows, m.autoCols = 40, 120 // interactive, a 40x120 terminal

	wantResult(t, "manual", mustResize(t, m, `{"type":"resize","rows":30,"cols":100}`), 30, 100, true, true)

	check := func(what string, rows, cols int) {
		t.Helper()
		m.treeMu.RLock()
		defer m.treeMu.RUnlock()
		if m.rows != rows || m.cols != cols {
			t.Fatalf("%s: screen %dx%d, want %dx%d", what, m.rows, m.cols, rows, cols)
		}
		if m.manualRows != 30 || m.manualCols != 100 {
			t.Fatalf("%s: manual is %dx%d, SIGWINCH must never touch it", what, m.manualRows, m.manualCols)
		}
	}
	m.setAutoSize(50, 160)
	check("a bigger window", 30, 100)
	m.setAutoSize(20, 80)
	check("a smaller window clips", 20, 80)
	m.setAutoSize(25, 200)
	check("clipped on one side only", 25, 100)
	m.setAutoSize(50, 160)
	check("the window grows back", 30, 100)

	res := mustResize(t, m, `{"type":"resize","auto":true}`)
	wantResult(t, "auto", res, 50, 160, true, false)
	if r, c := screenSize(m); r != 50 || c != 160 {
		t.Fatalf("after auto the screen is %dx%d, want the terminal's 50x160", r, c)
	}
	// Releasing when already automatic changes nothing.
	wantResult(t, "auto again", mustResize(t, m, `{"type":"resize","auto":true}`), 50, 160, false, false)
	// auto with a pane reports the pane.
	wantResult(t, "auto with pane", mustResize(t, m, `{"type":"resize","auto":true,"pane":0}`), 49, 160, false, false)
	// And {"auto":false} is no auto at all: it needs a size.
	if _, err := resizeOn(t, m, `{"type":"resize","auto":false}`); verbErrCode(err) != sockCodeBadRequest {
		t.Fatalf(`{"auto":false} with no size: %v, want bad_request`, err)
	}
}

// TestCtrlGRReleasesManual: Ctrl-G r is the local way out of manual mode, and
// it is CLAIMED whether or not the mode is on — a chord whose meaning depends
// on a mode the human may not know they are in is worse than one that says
// "already automatic".
func TestCtrlGRReleasesManual(t *testing.T) {
	defer deadlineWatchdog(t, 20*time.Second)()

	m := sessionMux(t, 1)
	m.autoRows, m.autoCols = 40, 120
	mustResize(t, m, `{"type":"resize","rows":30,"cols":100}`)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pw.Close()
	m.stdin = pr
	done := make(chan struct{})
	go func() { m.inputLoop(); close(done) }()

	await := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			m.treeMu.RLock()
			ok := cond()
			m.treeMu.RUnlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("ctrl-g r did not %s", what)
	}

	if _, err := pw.Write([]byte{0x07, 'r'}); err != nil {
		t.Fatalf("write chord: %v", err)
	}
	await("release manual mode", func() bool {
		return m.manualRows == 0 && m.manualCols == 0 && m.rows == 40 && m.cols == 120
	})

	if _, err := pw.Write([]byte{0x07, 'r'}); err != nil {
		t.Fatalf("write chord: %v", err)
	}
	await("say the size is already automatic", func() bool {
		return strings.Contains(m.chromeNote, "already automatic")
	})

	m.quitOnce.Do(func() { close(m.quit) })
	<-done
}

// TestResizeKeyHint: the bar is the only place manual mode can announce itself
// and Ctrl-G r is the only way out, so "r auto" joins the hint FLOOR, the part
// that survives the narrowest terminal next to "q quit".
func TestResizeKeyHint(t *testing.T) {
	has := func(items []hintItem, text string) bool {
		for _, it := range items {
			if it.text == text {
				return true
			}
		}
		return false
	}
	on := keyHintItems(false, true, true, true)
	if !has(on, "r auto") {
		t.Fatalf("manual mode on, hints %v carry no \"r auto\"", on)
	}
	if !strings.Contains(hintFloor(on), "r auto") {
		t.Fatalf("\"r auto\" is not in the hint floor %q", hintFloor(on))
	}
	if off := keyHintItems(false, true, true, false); has(off, "r auto") {
		t.Fatalf("manual mode off, hints %v still offer \"r auto\"", off)
	}

	m := sessionMux(t, 1)
	m.headless = true
	mustResize(t, m, `{"type":"resize","rows":30,"cols":100}`)
	m.treeMu.RLock()
	got := m.keyHintLocked()
	m.treeMu.RUnlock()
	if !has(got, "r auto") {
		t.Fatalf("keyHintLocked under manual mode: %v, no \"r auto\"", got)
	}
}

// TestManualShrinkClearsTheTerminal: the renderer never clears, so a manual
// size smaller than the terminal would leave the old frame standing in the
// rows and columns it abandoned. The next FULL frame after a shrink clears
// first; the one after that does not.
func TestManualShrinkClearsTheTerminal(t *testing.T) {
	m := sessionMux(t, 1)
	m.autoRows, m.autoCols = 40, 120

	frame := func() string {
		m.treeMu.RLock()
		defer m.treeMu.RUnlock()
		_, out, _ := m.renderLocked()
		return out
	}
	frame() // the boot frame
	mustResize(t, m, `{"type":"resize","rows":30,"cols":100}`)
	out := frame()
	if !strings.HasPrefix(out, "\x1b[0m\x1b[2J") {
		t.Fatalf("the first frame after a shrink does not start with a clear: %q", trunc(out, 40))
	}
	m.treeMu.Lock()
	m.markAllDirtyLocked()
	m.treeMu.Unlock()
	if out := frame(); strings.Contains(out, "\x1b[2J") {
		t.Fatalf("the clear is one-shot, but the next frame cleared again: %q", trunc(out, 40))
	}
	// Growing back needs no clear: the bigger frame paints every cell.
	mustResize(t, m, `{"type":"resize","rows":40,"cols":120}`)
	if out := frame(); strings.Contains(out, "\x1b[2J") {
		t.Fatalf("a grow cleared the terminal: %q", trunc(out, 40))
	}

	// Headless: no clear is armed, and nothing is written anyway.
	h := sessionMux(t, 1)
	h.headless = true
	var buf strings.Builder
	h.out = &buf
	mustResize(t, h, `{"type":"resize","rows":30,"cols":100}`)
	if h.clearNext.Load() {
		t.Fatal("headless armed a terminal clear for a terminal it does not have")
	}
	h.render()
	if buf.Len() != 0 {
		t.Fatalf("headless wrote %d bytes", buf.Len())
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// TestResizeRecordsRunLevelOutRow: a resize touches the SESSION, not a pane,
// so it is filed at run level and opens no route — a route means "a pane this
// controller touched", and pane 0 gaining one would make targetPane see a
// route the controller never opened.
func TestResizeRecordsRunLevelOutRow(t *testing.T) {
	m := sessionMux(t, 1)
	m.headless = true
	mustResize(t, m, `{"type":"resize","pane":0,"rows":20,"cols":60}`)
	sig := lastSignal(t, m)
	if sig.verb != "resize" || sig.dir != "out" || sig.pane != -1 {
		t.Fatalf("newest panel row is %+v, want a run-level resize OUT row", sig)
	}
	if sig.ok == nil || !*sig.ok {
		t.Fatalf("the OUT row's ack is %v, want ok", sig.ok)
	}
	if !strings.Contains(sig.text, "20x60") || !strings.Contains(sig.ackText, "20x60") {
		t.Fatalf("the row says %q / %q; it should name the size asked for and got", sig.text, sig.ackText)
	}
	m.control.mu.Lock()
	routes := len(m.control.routes)
	m.control.mu.Unlock()
	if routes != 0 {
		t.Fatalf("a resize opened %d route(s); it touches no pane", routes)
	}
}

// TestResizeIsAdvertised: capabilities lists the verb, the op is registered as
// control, the protocol did not move, and the view token cannot reach it —
// changing everybody's screen is not a viewer's act.
func TestResizeIsAdvertised(t *testing.T) {
	m := sessionMux(t, 1)
	caps, err := m.sockCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	verbs, _ := caps["verbs"].([]string)
	found := false
	for _, v := range verbs {
		found = found || v == "resize"
	}
	if !found {
		t.Fatalf("capabilities.verbs %v has no resize", verbs)
	}
	if caps["protocol"] != 1 {
		t.Fatalf("protocol is %v; resize is additive and must not move it", caps["protocol"])
	}
	spec, ok := m.bus().Spec("resize")
	if !ok {
		t.Fatal("resize is not a registered op")
	}
	if spec.Class != protocol.ClassControl || spec.Source != opSource {
		t.Fatalf("resize is %s/%s, want control/%s", spec.Class, spec.Source, opSource)
	}

	viewer := hub.Caller{Transport: "ws", Conn: "ws#1", ReadOnly: true}
	if _, err := m.bus().Call(t.Context(), viewer, "resize", json.RawMessage(`{"rows":30,"cols":100}`)); verbErrCode(err) != sockCodeForbidden {
		t.Fatalf("a viewer calling resize got %v, want forbidden", err)
	}
	if r, c := screenSize(m); r != 40 || c != 120 {
		t.Fatalf("the refused viewer call resized the screen to %dx%d", r, c)
	}

	// The op path is the verb: same parse, same result.
	owner := hub.Caller{Transport: "http", Conn: "http#1"}
	res, err := m.bus().Call(t.Context(), owner, "resize", json.RawMessage(`{"rows":30,"cols":100}`))
	if err != nil {
		t.Fatalf("resize through the hub: %v", err)
	}
	wantResult(t, "hub call", res, 30, 100, true, true)
	if _, err := m.bus().Call(t.Context(), owner, "resize", json.RawMessage(`{"rows":"30","cols":100}`)); verbErrCode(err) != sockCodeBadRequest {
		t.Fatalf(`"rows":"30" through the hub: %v, want bad_request`, err)
	}
}

// TestManualModeChangeRepaintsTheHint: entering or leaving manual mode without
// moving the size changes only the status bar's "r auto" hint, and the renderer
// paints only dirty panes — so both edges must mark the screen dirty, or the
// hint lags until some unrelated output arrives.
func TestManualModeChangeRepaintsTheHint(t *testing.T) {
	m := sessionMux(t, 1)
	m.headless = true
	clean := func() {
		m.treeMu.RLock()
		for _, p := range m.livePanesLocked(nil) {
			p.mu.Lock()
			p.dirty = false
			p.mu.Unlock()
		}
		m.treeMu.RUnlock()
	}
	dirty := func() bool {
		p := m.paneByID(0)
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.dirty
	}

	r, c := screenSize(m)
	clean()
	wantResult(t, "same size", mustResize(t, m, fmt.Sprintf(`{"type":"resize","rows":%d,"cols":%d}`, r, c)), r, c, false, true)
	if !dirty() {
		t.Fatal("a same-size resize entered manual mode without a repaint: \"r auto\" would not appear")
	}

	clean()
	wantResult(t, "auto", mustResize(t, m, `{"type":"resize","auto":true}`), r, c, false, false)
	if !dirty() {
		t.Fatal("an auto release that kept the size left manual mode without a repaint: \"r auto\" would linger")
	}

	// And a release with nothing to release stays quiet: no flicker for a no-op.
	clean()
	mustResize(t, m, `{"type":"resize","auto":true}`)
	if dirty() {
		t.Fatal("releasing an already-automatic size repainted the screen")
	}
}
