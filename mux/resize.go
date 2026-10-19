package mux

// ── The `resize` op: a client choosing the screen's size ─────────────────────
//
// One implementation, two entrances, exactly as open_pane: the socket verb
// (dispatchSocketVerb's `case "resize"`) and the hub op (builtinOps), which
// builds the same sockMsg and lands in the same case. Everything below is the
// verb.
//
// What a resize IS: a client's manual size, written into manualRows/manualCols
// and applied through the same applySizeLocked → reflowLocked path SIGWINCH
// takes, so every child gets a real TIOCSWINSZ and a real SIGWINCH, and every
// watcher gets a keyframe at the new geometry (Pane.resize → noteGeometryLocked).
// It stays until {"auto":true} or Ctrl-G r. SIGWINCH never releases it — that
// is the whole point of the mode — and on an interactive terminal only CLIPS it
// while the window is smaller (screenSizeLocked).
//
// What a resize must never do is half-happen. Screen.resize destroys the cells
// that do not fit and there is no rollback, so every refusal is decided BEFORE
// any Pane is touched, by a dry run (treeFitsLocked) under the same treeMu.Lock
// the apply runs under. A refused resize therefore leaves the tree
// byte-identical by construction, not by undo.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// resizeReq is a parsed resize request.
type resizeReq struct {
	pane       int // paneUnspecified when absent; otherwise a real index >= 0
	rows, cols int // as REQUESTED: the pane's size when pane is given
	auto       bool
}

// parseResize types the fields sockMsg keeps as `any`. Everything it refuses
// is bad_request — the SHAPE of the request. too_large is left to
// checkScreenSize so the bound has one owner.
func parseResize(msg sockMsg) (resizeReq, error) {
	req := resizeReq{pane: paneUnspecified}

	switch a := msg.Auto.(type) {
	case nil:
	case bool:
		req.auto = a
	default:
		return req, sockErrf(sockCodeBadRequest, "auto must be true or false (got %v)", msg.Auto)
	}

	// Not parsePaneIndex alone: its sentinels are ints, so a wire value of -3
	// would read as "absent" and quietly become the screen form, and 0.5 would
	// truncate to pane 0. Both are a client asking for something it did not get.
	switch v := msg.Pane.(type) {
	case nil:
	case float64:
		if v != math.Trunc(v) || v < 0 || v > math.MaxInt32 {
			return req, sockErrf(sockCodeBadRequest, "pane must be a pane index (got %v)", v)
		}
		req.pane = int(v)
	case string:
		s := strings.TrimSpace(v)
		if s == "*" {
			return req, sockErrf(sockCodeBadRequest, `resize has no fan-out: "*" is not a target; omit pane to size the whole screen`)
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return req, sockErrf(sockCodeBadRequest, "pane must be a pane index (got %q)", v)
		}
		req.pane = n
	default:
		return req, sockErrf(sockCodeBadRequest, "pane must be a pane index (got %v)", msg.Pane)
	}

	if req.auto {
		// Exclusive: "release, and also this size" has two answers and the
		// caller would not know which one it got.
		if msg.Rows != nil || msg.Cols != nil {
			return req, sockErrf(sockCodeBadRequest, "auto releases manual mode and takes no rows or cols")
		}
		return req, nil
	}

	var err error
	if req.rows, err = resizeDim("rows", msg.Rows); err != nil {
		return req, err
	}
	if req.cols, err = resizeDim("cols", msg.Cols); err != nil {
		return req, err
	}
	return req, nil
}

// resizeDim types one of rows/cols: a JSON number with an integral value of at
// least 1. 40.0 is 40; "40", 40.5 and true are not numbers of rows.
//
// It SATURATES rather than converting a huge float directly: float64 → int is
// implementation-defined out of range, and 1e300 must come out as a too_large
// from checkScreenSize, not as whatever the conversion happens to produce.
func resizeDim(name string, v any) (int, error) {
	if v == nil {
		return 0, sockErrf(sockCodeBadRequest, "resize needs both rows and cols (no %s), or auto:true", name)
	}
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.IsInf(f, 0) {
		return 0, sockErrf(sockCodeBadRequest, "%s must be a whole number (got %v)", name, v)
	}
	if f < 1 {
		return 0, sockErrf(sockCodeBadRequest, "%s must be positive (got %v)", name, v)
	}
	if f > math.MaxInt32 {
		f = math.MaxInt32
	}
	return int(f), nil
}

// sockResize is the verb: the panel's OUT row, the work, the ack.
//
// The row is filed at RUN level (-1) always, including for the pane form. A
// resize touches the SESSION, not a pane, and a route means "a pane this
// controller touched" (see refuseRequest) — filing it against pane 0 would
// also make targetPane see a route the controller never opened. Recording
// happens with no treeMu held: the order treeMu -> cp.mu is legal, but nothing
// here needs both at once.
func (m *Magmux) sockResize(msg sockMsg) (map[string]any, error) {
	seq := m.control.recordRequest(-1, "resize", resizeText(msg))
	res, err := m.resizeScreen(msg)
	if err != nil {
		m.control.recordAck(seq, false, verbErrCode(err), err.Error())
		return nil, err
	}
	m.control.recordAck(seq, true, "", resizeAckText(res))
	return res, nil
}

// resizeScreen validates and applies one request. Order:
//
//  1. parse, and auto/size exclusivity          bad_request   (no lock)
//  2. the REQUESTED numbers against the bounds  too_large     (no lock)
//
// then ONE treeMu.Lock to the end, so validation and apply cannot be split by
// a concurrent open_pane, close_pane, Ctrl-G p or SIGWINCH:
//
//  3. resolve `pane`                            no_such_pane, pane_is_control,
//     bad_request (not the one session on screen)
//  4. the SCREEN size against the bounds        too_large     (pane + status row)
//  5. interactive: not larger than the tty      too_large
//  6. the dry run                               too_small
//  7. same size: pin manual, touch nothing      changed:false
//  8. apply
//
// Steps 3–6 write nothing, which is what makes every refusal byte-identical.
func (m *Magmux) resizeScreen(msg sockMsg) (map[string]any, error) {
	req, err := parseResize(msg)
	if err != nil {
		return nil, err
	}
	if !req.auto {
		if err := checkScreenSize(req.rows, req.cols); err != nil {
			return nil, err
		}
	}

	m.treeMu.Lock()
	res, changed, err := m.resizeLocked(req)
	m.treeMu.Unlock()
	if err != nil {
		return nil, err
	}
	if changed {
		// The panel has no child to redraw it after its Screen was rebuilt,
		// exactly as on SIGWINCH.
		m.control.markDirty()
	}
	return res, nil
}

// resizeLocked is steps 3–8. Caller holds treeMu.Lock.
func (m *Magmux) resizeLocked(req resizeReq) (res map[string]any, changed bool, err error) {
	var target *Pane
	if req.pane != paneUnspecified {
		if target, err = m.resizeTargetLocked(req.pane); err != nil {
			return nil, false, err
		}
	}

	if req.auto {
		changed = m.releaseManualLocked()
		return m.resizeResultLocked(target, changed), changed, nil
	}

	sr, sc := req.rows, req.cols
	if target != nil {
		sr += m.statusRowsLocked()
		// A legal pane plus the status row can cross the side bound.
		if err := checkScreenSize(sr, sc); err != nil {
			return nil, false, err
		}
	}
	// Phase B has no renderer clipping, so a screen larger than the terminal
	// would paint past its edge. Headless has no terminal to exceed, and
	// auto* == 0 (a struct literal, never a real run) knows of none.
	if !m.headless && m.autoRows > 0 && m.autoCols > 0 && (sr > m.autoRows || sc > m.autoCols) {
		return nil, false, sockErrf(sockCodeTooLarge,
			"%dx%d is larger than this terminal (%dx%d); a screen bigger than the terminal needs "+
				"renderer clipping, which is not built", sr, sc, m.autoRows, m.autoCols)
	}
	if bad, got, ok := m.treeFitsLocked(sr, sc); !ok {
		return nil, false, sockErrf(sockCodeTooSmall,
			"%dx%d leaves pane %d %dx%d; every session pane needs at least %dx%d",
			sr, sc, bad.id, got.h, got.w, minPaneRows, minPaneCols)
	}

	// Same size: Pane.resize is never a no-op (it rebuilds every Screen, forces
	// a keyframe and calls SetWinSize), so the comparison must come BEFORE
	// applySizeLocked. Manual mode is still pinned, so "manual" in the reply
	// has one meaning and a repeated identical call is idempotent.
	if sr == m.rows && sc == m.cols {
		if m.manualRows != sr || m.manualCols != sc {
			// Entering manual mode changes the status bar ("r auto"), though not
			// a cell of any pane: repaint so the hint appears now, not at the
			// next unrelated change.
			m.markAllDirtyLocked()
		}
		m.manualRows, m.manualCols = sr, sc
		return m.resizeResultLocked(target, false), false, nil
	}

	prevR, prevC := m.rows, m.cols
	m.manualRows, m.manualCols = sr, sc
	m.applySizeLocked()
	m.markAllDirtyLocked()
	// The renderer never clears, so the rows and columns a shrink abandons
	// would keep the old frame forever. Headless paints nothing to clear.
	if !m.headless && (m.rows < prevR || m.cols < prevC) {
		m.clearNext.Store(true)
	}
	return m.resizeResultLocked(target, true), true, nil
}

// resizeTargetLocked resolves the pane form's `pane`, in the order a caller can
// act on: a pane that does not exist, then one that is not a session, then a
// layout in which "this pane's size" does not determine the screen's.
//
// The pane form is only defined when the session leaf IS the whole layout:
// screen = (rows + status row, cols) is false the moment anything else shares
// the screen, and the reply would then report success and a pane that is not
// the size asked for — the invisible failure this op refuses to produce. A
// visible control panel shares it too (-c, Ctrl-G p), so that is refused as
// well, with the same advice.
//
// Caller holds treeMu.Lock.
func (m *Magmux) resizeTargetLocked(idx int) (*Pane, error) {
	p := m.paneByIDLocked(idx)
	if p == nil {
		return nil, sockErrf(sockCodeNoSuchPane, "no pane %d (it may have been closed)", idx)
	}
	if p.isControl {
		return nil, sockErrf(sockCodePaneIsControl, "pane %d is the control panel; resize a session pane, or omit pane", idx)
	}
	sessions := 0
	m.planLayoutLocked(m.rows, m.cols, func(leaf *Pane, _ box) {
		if !leaf.isControl {
			sessions++
		}
	})
	if sessions != 1 {
		return nil, sockErrf(sockCodeBadRequest,
			"the pane form needs exactly one session pane on screen; there are %d — send rows/cols without pane to size the whole screen", sessions)
	}
	if p.hidden {
		return nil, sockErrf(sockCodePaneHidden, "pane %d is not on screen", idx)
	}
	if m.root != p {
		return nil, sockErrf(sockCodeBadRequest,
			"pane %d shares the screen with the control panel, so its size does not decide the screen's — hide the panel (Ctrl-G p) or send rows/cols without pane", idx)
	}
	return p, nil
}

// releaseManualLocked returns to the automatic size, reflowing only if that
// changes anything.
//
// No tty bound and no fit check: the auto size is what SIGWINCH would apply,
// and that is a RESHAPE, which clamps. Refusing a release would trap a human in
// a mode they cannot leave. No clearNext either: an interactive manual size is
// never larger than the terminal (refused at request time, clipped after), so
// returning to auto never shrinks the screen.
//
// Caller holds treeMu.Lock.
func (m *Magmux) releaseManualLocked() (changed bool) {
	wasManual := m.manualRows != 0 || m.manualCols != 0
	m.manualRows, m.manualCols = 0, 0
	r, c := m.screenSizeLocked()
	if r == m.rows && c == m.cols {
		if wasManual {
			m.markAllDirtyLocked() // the hint drops "r auto" though the size did not move
		}
		return false
	}
	m.applySizeLocked()
	m.markAllDirtyLocked()
	return true
}

// releaseManualSize is Ctrl-G r. A local key, not a controller request, so it
// files no panel row: the panel taps the controller↔session exchange only, as
// for Ctrl-G p.
//
// Caller must NOT hold treeMu.
func (m *Magmux) releaseManualSize() {
	m.treeMu.Lock()
	if m.manualRows == 0 && m.manualCols == 0 {
		// A key that does nothing must say so, or it is indistinguishable
		// from a broken one.
		m.noteChromeLocked("size is already automatic")
		m.markAllDirtyLocked()
		m.treeMu.Unlock()
		return
	}
	m.releaseManualLocked() // repaints either way: the hint drops "r auto"
	m.treeMu.Unlock()
	m.control.markDirty()
}

// resizeResultLocked is the contract's reply: the pane's size for the pane
// form, else the screen's; whether anything was reflowed; whether manual mode
// is on after the call. Caller holds at least treeMu.RLock.
func (m *Magmux) resizeResultLocked(target *Pane, changed bool) map[string]any {
	rows, cols := m.rows, m.cols
	if target != nil {
		// Structural, treeMu's; equal to the leaf's Screen because Pane.resize
		// sets both.
		rows, cols = target.h, target.w
	}
	return map[string]any{
		"rows":    rows,
		"cols":    cols,
		"changed": changed,
		"manual":  m.manualRows > 0 && m.manualCols > 0,
	}
}

// resizeText is the OUT row: what was asked for, as it arrived. It reads the
// raw fields, because a refused request has to be shown too.
func resizeText(msg sockMsg) string {
	if a, ok := msg.Auto.(bool); ok && a && msg.Rows == nil && msg.Cols == nil {
		if msg.Pane != nil {
			return fmt.Sprintf("auto · pane %v", msg.Pane)
		}
		return "auto"
	}
	s := fmt.Sprintf("%vx%v", dimText(msg.Rows), dimText(msg.Cols))
	if msg.Pane != nil {
		s += fmt.Sprintf(" pane %v", msg.Pane)
	}
	return s
}

func dimText(v any) string {
	switch d := v.(type) {
	case nil:
		return "?"
	case float64:
		return strconv.FormatFloat(d, 'f', -1, 64)
	}
	return fmt.Sprintf("%q", fmt.Sprint(v))
}

// resizeAckText is the inline ack: the size it came out at, and whether that
// moved anything.
func resizeAckText(res map[string]any) string {
	s := fmt.Sprintf("%vx%v", res["rows"], res["cols"])
	if changed, _ := res["changed"].(bool); changed {
		s += " · changed"
	} else {
		s += " · unchanged"
	}
	if manual, _ := res["manual"].(bool); !manual {
		s += " · auto"
	}
	return s
}
