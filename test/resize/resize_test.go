package resize_test

// Black-box tests for the `resize` socket op. Requirement ids (REQ-n) refer
// to TEST-PLAN.md in this directory.

import (
	"fmt"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// onePane starts the claudish shape: headless, --no-status, one -e pane 0
// running the stty-size child, and waits until the child has reported 50x160.
func onePane(t *testing.T, extra ...string) (*magmux, *conn) {
	t.Helper()
	m := startMagmux(t, extra, sizeChild)
	cn := dial(t, m)
	cn.waitChildSees(0, startRows, startCols)
	return m, cn
}

// twoPanes starts two session panes and waits until both children report a size.
func twoPanes(t *testing.T) (*magmux, *conn, []int) {
	t.Helper()
	m := startMagmux(t, nil, sizeChild, sizeChild)
	cn := dial(t, m)
	var ids []int
	deadline := time.Now().Add(readyTimeout)
	for {
		ids = ids[:0]
		ready := 0
		for _, p := range cn.list() {
			if p.isControl() || p.State == "closed" {
				continue
			}
			ids = append(ids, p.Pane)
			if r := cn.request(map[string]any{"type": "capture", "pane": p.Pane}); r.OK {
				var res struct {
					Text string `json:"text"`
				}
				_ = jsonUnmarshal(r.Result, &res)
				if len(sizeLines(res.Text)) > 0 {
					ready++
				}
			}
		}
		if len(ids) == 2 && ready == 2 {
			return m, cn, slices.Clone(ids)
		}
		if time.Now().After(deadline) {
			t.Fatalf("two session panes never both reported a size; list ids=%v ready=%d", ids, ready)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ── REQ-1: advertisement ────────────────────────────────────────────────────

func TestREQ1_CapabilitiesListsResizeAndProtocolStaysOne(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	c := cn.capabilities()

	if !slices.Contains(c.Verbs, "resize") {
		t.Errorf("capabilities.verbs = %v; want it to contain \"resize\"", c.Verbs)
	}
	if c.Protocol != 1 {
		t.Errorf("capabilities.protocol = %d; want 1", c.Protocol)
	}
	if c.Rows != startRows || c.Cols != startCols {
		t.Errorf("capabilities size = %dx%d; want the startup %dx%d", c.Rows, c.Cols, startRows, startCols)
	}
}

func TestREQ1_OpsListsResizeAsBuiltinControlOp(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	r := cn.mustOK(map[string]any{"type": "ops"})
	var res struct {
		Ops []protocol.OpSpec `json:"ops"`
	}
	if err := jsonUnmarshal(r.Result, &res); err != nil {
		t.Fatalf("ops result: %v; line %s", err, r.raw)
	}
	i := slices.IndexFunc(res.Ops, func(o protocol.OpSpec) bool { return o.Name == "resize" })
	if i < 0 {
		t.Fatalf("ops does not list \"resize\"; line %s", r.raw)
	}
	op := res.Ops[i]
	if op.Class != protocol.ClassControl {
		t.Errorf("resize class = %q; want %q", op.Class, protocol.ClassControl)
	}
	if op.Source != protocol.SourceBuiltin {
		t.Errorf("resize source = %q; want %q", op.Source, protocol.SourceBuiltin)
	}
}

func TestREQ1_ResizeReachableThroughCallVerb(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	r := cn.mustOK(map[string]any{"type": "call", "op": "resize",
		"args": map[string]any{"pane": 0, "rows": 40, "cols": 100}})
	res := decodeResize(t, r)

	assertResize(t, res, 40, 100, true, true)
	if rows, cols := cn.capsSize(); rows != 40 || cols != 100 {
		t.Errorf("after call resize, capabilities = %dx%d; want 40x100", rows, cols)
	}
}

// ── REQ-2 (criterion 1): pane resize is real, reported, and seen by the child ──

func TestREQ2_PaneResizeIsExactReportedAndObservedByChild(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	res := cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})

	assertResize(t, res, 40, 100, true, true)
	if rows, cols := cn.paneSize(0); rows != 40 || cols != 100 {
		t.Errorf("pane 0 is %dx%d; want exactly 40x100", rows, cols)
	}
	// --no-status: the screen is exactly the pane.
	if rows, cols := cn.capsSize(); rows != 40 || cols != 100 {
		t.Errorf("capabilities = %dx%d; want 40x100", rows, cols)
	}
	cn.waitChildSees(0, 40, 100)
}

func TestREQ2_WholeScreenFormResizesTheScreen(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	res := cn.resizeOK(map[string]any{"rows": 30, "cols": 90})

	assertResize(t, res, 30, 90, true, true)
	if rows, cols := cn.capsSize(); rows != 30 || cols != 90 {
		t.Errorf("capabilities = %dx%d; want 30x90", rows, cols)
	}
	// One pane, no status row: the pane IS the screen.
	if rows, cols := cn.paneSize(0); rows != 30 || cols != 90 {
		t.Errorf("pane 0 is %dx%d; want 30x90", rows, cols)
	}
	cn.waitChildSees(0, 30, 90)
}

func TestREQ2_HeadlessAcceptsLargerThanStartupSize(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	res := cn.resizeOK(map[string]any{"pane": 0, "rows": 70, "cols": 220})

	assertResize(t, res, 70, 220, true, true)
	cn.waitChildSees(0, 70, 220)
}

func TestREQ2_IntegralFloatRowsAndColsAreAccepted(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	r := cn.sendLine(`{"type":"resize","pane":0,"rows":40.0,"cols":100.0}`)
	if !r.OK {
		t.Fatalf("rows:40.0 cols:100.0 refused code=%q error=%q; want accepted as 40x100", r.Code, r.Error)
	}
	assertResize(t, decodeResize(t, r), 40, 100, true, true)
}

func TestREQ2_MinimumPaneSizeIsAccepted(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	res := cn.resizeOK(map[string]any{"pane": 0, "rows": 3, "cols": 20})

	assertResize(t, res, 3, 20, true, true)
	if rows, cols := cn.paneSize(0); rows != 3 || cols != 20 {
		t.Errorf("pane 0 is %dx%d; want 3x20", rows, cols)
	}
}

func TestREQ2_UpperBoundsExactlyAreAccepted(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	// A side of exactly 10000 and exactly 4,000,000 cells: neither is "over".
	res := cn.resizeOK(map[string]any{"rows": 400, "cols": 10000})

	assertResize(t, res, 400, 10000, true, true)
}

func TestREQ2_TwoPanesWholeScreenResizeKeepsEveryPaneUsable(t *testing.T) {
	t.Parallel()
	_, cn, ids := twoPanes(t)

	res := cn.resizeOK(map[string]any{"rows": 40, "cols": 100})

	assertResize(t, res, 40, 100, true, true)
	if rows, cols := cn.capsSize(); rows != 40 || cols != 100 {
		t.Errorf("capabilities = %dx%d; want 40x100", rows, cols)
	}
	for _, id := range ids {
		rows, cols := cn.paneSize(id)
		if rows < 3 || cols < 20 || rows > 40 || cols > 100 {
			t.Errorf("pane %d is %dx%d; want within 40x100 and at least 3x20", id, rows, cols)
		}
		cn.waitChildSees(id, rows, cols)
	}
}

// ── REQ-3 (criterion 2): watchers get a keyframe at the new geometry ─────────

func TestREQ3_WatcherReceivesKeyframeWithNewGeometry(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)
	watcher := cn
	r := watcher.mustOK(map[string]any{"type": "watch", "pane": 0, "mode": string(protocol.WatchFrames), "fps": 30})
	var info protocol.WatchInfo
	if err := jsonUnmarshal(r.Result, &info); err != nil {
		t.Fatalf("watch reply: %v", err)
	}
	if info.Rows != startRows || info.Cols != startCols {
		t.Fatalf("watch reply geometry %dx%d; want %dx%d", info.Rows, info.Cols, startRows, startCols)
	}

	cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})

	f := watcher.waitKeyframe(0, 40, 100, 5*time.Second)
	if f.Type != protocol.EventFrame {
		t.Errorf("frame type = %q; want %q", f.Type, protocol.EventFrame)
	}
	if len(f.Lines) != 40 {
		t.Errorf("keyframe carries %d lines; a keyframe covers every row, want 40", len(f.Lines))
	}
	for _, l := range f.Lines {
		if l.Y < 0 || l.Y >= 40 {
			t.Errorf("keyframe line y=%d outside 0..39", l.Y)
		}
	}
}

// ── REQ-4 (criterion 3): same size is a no-op ───────────────────────────────

func TestREQ4_SameSizeAtStartupAnswersChangedFalseAndPinsManual(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	res := cn.resizeOK(map[string]any{"pane": 0, "rows": startRows, "cols": startCols})

	assertResize(t, res, startRows, startCols, false, true)
}

func TestREQ4_SameSizeAgainIsNoReflowNoSigwinchNoKeyframe(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)
	cn.mustOK(map[string]any{"type": "watch", "pane": 0, "mode": string(protocol.WatchFrames), "fps": 30})

	first := cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})
	assertResize(t, first, 40, 100, true, true)
	cn.waitKeyframe(0, 40, 100, 5*time.Second)
	cn.waitChildSees(0, 40, 100)
	before := cn.world()
	beforeLines := len(sizeLines(before.text[0]))
	cn.drainFrames()

	again := cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})

	assertResize(t, again, 40, 100, false, true)
	cn.noKeyframeFor(0, quietWindow)
	after := cn.world()
	assertSameWorld(t, "same-size resize", before, after)
	if n := len(sizeLines(after.text[0])); n != beforeLines {
		t.Errorf("child printed %d size lines, had %d: a same-size resize delivered SIGWINCH", n, beforeLines)
	}

	// Positive control: the watcher is still live, so the silence above meant something.
	cn.resizeOK(map[string]any{"pane": 0, "rows": 30, "cols": 90})
	cn.waitKeyframe(0, 30, 90, 5*time.Second)
}

func TestREQ4_SameSizeWholeScreenFormAnswersChangedFalse(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)

	cn.resizeOK(map[string]any{"rows": 30, "cols": 90})
	again := cn.resizeOK(map[string]any{"rows": 30, "cols": 90})

	assertResize(t, again, 30, 90, false, true)
}

// ── REQ-5 (criterion 4): SIGWINCH does not undo manual; auto restores ───────

func TestREQ5_SigwinchDoesNotReleaseManualSize(t *testing.T) {
	t.Parallel()
	m, cn := onePane(t)
	cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})
	cn.waitChildSees(0, 40, 100)

	if err := m.cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatalf("SIGWINCH: %v", err)
	}
	time.Sleep(quietWindow) // absence check: nothing may change in response

	if rows, cols := cn.capsSize(); rows != 40 || cols != 100 {
		t.Errorf("after SIGWINCH capabilities = %dx%d; want the manual 40x100", rows, cols)
	}
	if rows, cols := cn.paneSize(0); rows != 40 || cols != 100 {
		t.Errorf("after SIGWINCH pane 0 = %dx%d; want the manual 40x100", rows, cols)
	}
	// Still manual, still that size: a repeat is a no-op that says manual:true.
	again := cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})
	assertResize(t, again, 40, 100, false, true)
}

func TestREQ5_AutoTrueRestoresAutomaticSize(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)
	cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})
	cn.waitChildSees(0, 40, 100)

	res := cn.resizeOK(map[string]any{"auto": true})

	assertResize(t, res, startRows, startCols, true, false)
	if rows, cols := cn.capsSize(); rows != startRows || cols != startCols {
		t.Errorf("after auto, capabilities = %dx%d; want %dx%d", rows, cols, startRows, startCols)
	}
	cn.waitChildSees(0, startRows, startCols)

	// Releasing again changes nothing and stays automatic.
	again := cn.resizeOK(map[string]any{"auto": true})
	assertResize(t, again, startRows, startCols, false, false)
}

func TestREQ5_AutoWithPaneReportsThePaneSize(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)
	cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})

	res := cn.resizeOK(map[string]any{"auto": true, "pane": 0})

	assertResize(t, res, startRows, startCols, true, false)
}

func TestREQ5_AutoReleaseAfterSigwinchStillRestores(t *testing.T) {
	t.Parallel()
	m, cn := onePane(t)
	cn.resizeOK(map[string]any{"rows": 40, "cols": 100})
	if err := m.cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatalf("SIGWINCH: %v", err)
	}

	res := cn.resizeOK(map[string]any{"auto": true})

	assertResize(t, res, startRows, startCols, true, false)
	cn.waitChildSees(0, startRows, startCols)
}

// ── REQ-6..10 (criterion 5): refusals carry the exact code and touch nothing ──

type refusal struct {
	name string
	line string // the request body, without "id"
	code string
}

var singlePaneRefusals = []refusal{
	// too_small: any session pane below 3 rows x 20 cols
	{"pane rows 2", `{"type":"resize","pane":0,"rows":2,"cols":100}`, protocol.CodeTooSmall},
	{"pane cols 19", `{"type":"resize","pane":0,"rows":40,"cols":19}`, protocol.CodeTooSmall},
	{"screen rows 1 cols 1", `{"type":"resize","rows":1,"cols":1}`, protocol.CodeTooSmall},
	// too_large: a side over 10000, or more than 4,000,000 cells
	{"cols 10001", `{"type":"resize","pane":0,"rows":40,"cols":10001}`, protocol.CodeTooLarge},
	{"rows 10001", `{"type":"resize","rows":10001,"cols":100}`, protocol.CodeTooLarge},
	{"cells 4,002,000", `{"type":"resize","rows":2001,"cols":2000}`, protocol.CodeTooLarge},
	// bad_request: missing / non-integer / non-positive, auto with a size
	{"no rows or cols", `{"type":"resize"}`, protocol.CodeBadRequest},
	{"missing rows", `{"type":"resize","pane":0,"cols":100}`, protocol.CodeBadRequest},
	{"missing cols", `{"type":"resize","pane":0,"rows":40}`, protocol.CodeBadRequest},
	{"rows as string", `{"type":"resize","pane":0,"rows":"40","cols":100}`, protocol.CodeBadRequest},
	{"rows fractional", `{"type":"resize","pane":0,"rows":40.5,"cols":100}`, protocol.CodeBadRequest},
	{"cols boolean", `{"type":"resize","pane":0,"rows":40,"cols":true}`, protocol.CodeBadRequest},
	{"rows null", `{"type":"resize","pane":0,"rows":null,"cols":100}`, protocol.CodeBadRequest},
	{"rows zero", `{"type":"resize","pane":0,"rows":0,"cols":100}`, protocol.CodeBadRequest},
	{"cols zero", `{"type":"resize","rows":40,"cols":0}`, protocol.CodeBadRequest},
	{"rows negative", `{"type":"resize","pane":0,"rows":-1,"cols":100}`, protocol.CodeBadRequest},
	{"auto with size", `{"type":"resize","auto":true,"rows":40,"cols":100}`, protocol.CodeBadRequest},
	{"auto false alone", `{"type":"resize","auto":false}`, protocol.CodeBadRequest},
	{"auto not boolean", `{"type":"resize","auto":"yes"}`, protocol.CodeBadRequest},
	// pane addressing
	{"pane does not exist", `{"type":"resize","pane":99,"rows":40,"cols":100}`, protocol.CodeNoSuchPane},
}

func assertRefused(t *testing.T, r reply, code string) {
	t.Helper()
	if r.OK {
		t.Fatalf("want refusal %q, got ok; line %s", code, r.raw)
	}
	if r.Code != code {
		t.Errorf("code = %q; want %q (error %q)", r.Code, code, r.Error)
	}
	if r.Error == "" {
		t.Errorf("refusal carries no human-readable error; line %s", r.raw)
	}
	if r.Type != protocol.EventReply {
		t.Errorf("reply type = %q; want %q", r.Type, protocol.EventReply)
	}
}

func TestREQ6to10_RefusedResizeHasExactCodeAndLeavesEverythingIdentical(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)
	start := cn.world()

	for _, tc := range singlePaneRefusals {
		t.Run(tc.name, func(t *testing.T) {
			cn.t = t
			defer func() { cn.t = nil }()
			before := cn.world()

			r := cn.sendLine(tc.line)

			assertRefused(t, r, tc.code)
			assertSameWorld(t, tc.name, before, cn.world())
		})
	}

	cn.t = t
	// No refused request may have reached the child as a SIGWINCH, late or not.
	time.Sleep(quietWindow)
	assertSameWorld(t, "after every refusal", start, cn.world())
	// And the size really is unchanged: asking for it is a no-op.
	again := cn.resizeOK(map[string]any{"pane": 0, "rows": startRows, "cols": startCols})
	assertResize(t, again, startRows, startCols, false, true)
}

func TestREQ6to10_RefusalWhileManualKeepsManualSize(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)
	cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})
	cn.waitChildSees(0, 40, 100)
	before := cn.world()

	assertRefused(t, cn.sendLine(`{"type":"resize","pane":0,"rows":2,"cols":100}`), protocol.CodeTooSmall)
	assertRefused(t, cn.sendLine(`{"type":"resize","rows":10001,"cols":100}`), protocol.CodeTooLarge)
	assertRefused(t, cn.sendLine(`{"type":"resize","auto":true,"rows":50,"cols":160}`), protocol.CodeBadRequest)

	time.Sleep(quietWindow)
	assertSameWorld(t, "refusals in manual mode", before, cn.world())
	again := cn.resizeOK(map[string]any{"pane": 0, "rows": 40, "cols": 100})
	assertResize(t, again, 40, 100, false, true)
}

func TestREQ9_PaneNamingTheControlPanelIsPaneIsControl(t *testing.T) {
	t.Parallel()
	_, cn := onePane(t)
	panel := -1
	for _, p := range cn.list() {
		if p.isControl() {
			panel = p.Pane
		}
	}
	if panel < 0 {
		t.Skip("`list` reports no control pane in this session, so there is no id to address it by")
	}
	before := cn.world()

	r := cn.sendLine(fmt.Sprintf(`{"type":"resize","pane":%d,"rows":40,"cols":100}`, panel))

	assertRefused(t, r, protocol.CodePaneIsControl)
	assertSameWorld(t, "pane_is_control", before, cn.world())
}

func TestREQ8_PaneFormWithTwoSessionPanesIsBadRequest(t *testing.T) {
	t.Parallel()
	_, cn, ids := twoPanes(t)
	before := cn.world()

	for _, id := range ids {
		r := cn.sendLine(fmt.Sprintf(`{"type":"resize","pane":%d,"rows":40,"cols":100}`, id))
		assertRefused(t, r, protocol.CodeBadRequest)
	}

	time.Sleep(quietWindow)
	assertSameWorld(t, "pane form with two panes", before, cn.world())
}

func TestREQ6_TwoPanesScreenTooSmallForBothIsTooSmall(t *testing.T) {
	t.Parallel()
	_, cn, _ := twoPanes(t)
	before := cn.world()

	// 5x30 holds one 3x20 pane but not two, whichever way they are split.
	r := cn.sendLine(`{"type":"resize","rows":5,"cols":30}`)

	assertRefused(t, r, protocol.CodeTooSmall)
	time.Sleep(quietWindow)
	assertSameWorld(t, "too_small with two panes", before, cn.world())
}

func TestREQ8_PaneFormWithControlPanelVisibleIsBadRequest(t *testing.T) {
	t.Parallel()
	m := startMagmux(t, []string{"-c"}, sizeChild)
	cn := dial(t, m)
	session := -1
	deadline := time.Now().Add(readyTimeout)
	for session < 0 || len(sizeLines(cn.capture(session))) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("session pane never reported a size with -c")
		}
		for _, p := range cn.list() {
			if !p.isControl() && p.State != "closed" {
				session = p.Pane
			}
		}
		if session < 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		time.Sleep(50 * time.Millisecond)
	}
	before := cn.world()

	r := cn.sendLine(fmt.Sprintf(`{"type":"resize","pane":%d,"rows":40,"cols":100}`, session))

	assertRefused(t, r, protocol.CodeBadRequest)
	assertSameWorld(t, "pane form with visible panel", before, cn.world())
}
