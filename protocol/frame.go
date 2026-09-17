package protocol

// The live screen on the wire.
//
// A frame is one pane's screen as a set of WHOLE ROWS. That is the single
// decision everything else follows from: a delta row replaces the row it names,
// so deltas merge by row index, a merged delta applies on any earlier state,
// and a gap in `seq` is harmless. A client that misses three frames and gets
// the fourth is correct about every row the fourth carries and stale about no
// row it does not — which is what makes the latest-wins slot (one pending frame
// per subscriber per pane, later rows overwriting earlier ones) a lossless
// COALESCE rather than a lossy drop.
//
// The alternative — a cell-level or scroll-aware diff — would make every frame
// depend on the client having applied the one before it, and the first dropped
// frame would corrupt the screen until the next keyframe. magmux would then
// need per-client acknowledgement, which is exactly the queue the slot exists
// to avoid.

import "encoding/json"

// WatchMode is what a watcher wants: the screen itself, or only the news that
// it changed.
//
// WatchNotify exists for a subscriber that has its own way to read a pane —
// an MCP client that will call `capture` — and needs to know WHEN, not WHAT.
// It is rate-limited to one `changed` per pane per second, because its whole
// purpose is to be cheap.
type WatchMode string

const (
	WatchFrames WatchMode = "frames"
	WatchNotify WatchMode = "notify"
)

// ValidWatchMode reports whether m is one of the two modes. An empty mode is
// NOT valid here; callers that want a default resolve it with ResolveWatchMode
// first, so "" cannot mean one thing in one adapter and another elsewhere.
func ValidWatchMode(m WatchMode) bool {
	return m == WatchFrames || m == WatchNotify
}

// ResolveWatchMode fills in the default. Frames is the default because a
// watcher that named no mode asked to see the pane.
func ResolveWatchMode(m WatchMode) WatchMode {
	if m == "" {
		return WatchFrames
	}
	return m
}

// Frame rate bounds, per watcher.
//
// The floor is 1 rather than 0 because a watcher at 0 fps is not watching, and
// answering its `watch` with a subscription that can never deliver is worse
// than clamping. The ceiling is 30 because the framer's cost is one screen diff
// per tick per WATCHED pane, and past 30 that is spent on frames no human eye
// and no browser paint loop can use.
const (
	FPSMin     = 1
	FPSMax     = 30
	FPSDefault = 15
)

// ClampFPS resolves a requested rate: 0 (absent) takes the default, anything
// else is clamped rather than refused. A rate is a preference, not a claim that
// can be wrong, so a caller asking for 120 gets 30 and is told so in WatchInfo.
func ClampFPS(fps int) int {
	switch {
	case fps <= 0:
		return FPSDefault
	case fps < FPSMin:
		return FPSMin
	case fps > FPSMax:
		return FPSMax
	}
	return fps
}

// WatchInfo answers a `watch`: the pane's geometry, so a client can size its
// display before the first frame arrives, and the mode and rate magmux actually
// settled on after clamping.
type WatchInfo struct {
	Pane int       `json:"pane"`
	Rows int       `json:"rows"`
	Cols int       `json:"cols"`
	Mode WatchMode `json:"mode"`
	FPS  int       `json:"fps"`
}

// Colour ints. A cell's foreground and background cross the wire as ONE
// integer, because a colour has three shapes (default, indexed, truecolor) and
// an object per cell-run would triple the size of a frame that is mostly
// colour.
//
//	-1            default (the terminal's own)
//	0..255        indexed
//	>= ColorTrue  truecolor; c & ColorRGBMask is 0xRRGGBB
const (
	ColorDefault = -1
	ColorTrue    = 1 << 24
	ColorRGBMask = 0xFFFFFF
)

// Run is one stretch of identically-styled cells: [col, len, fg, bg, attr].
//
// The columns are CELL columns, not indexes into the row's text — a
// double-width character occupies two cells and one code point, and a client
// that painted by code point would drift one column right of every CJK glyph on
// the line. Runs may extend past the end of the text, which is how a styled
// blank (a selection, a filled status bar) survives the right-trim.
//
// An array rather than a struct so it marshals as [0,1,2,-1,1]: five small
// integers are the whole of a run, and five field names per run would be most
// of a frame.
type Run [5]int

// NewRun builds a run. attr is the Attr bitmask verbatim.
func NewRun(col, length, fg, bg, attr int) Run { return Run{col, length, fg, bg, attr} }

func (r Run) Col() int    { return r[0] }
func (r Run) Len() int    { return r[1] }
func (r Run) Fg() int     { return r[2] }
func (r Run) Bg() int     { return r[3] }
func (r Run) Attr() int   { return r[4] }
func (r Run) Empty() bool { return r[1] <= 0 }

// Line is one row of a frame.
//
// T is the row's text, one code point per non-continuation cell, NUL rendered
// as a space and the whole right-trimmed. Wd lists the code-point indexes in T
// that occupy TWO cells, so a client can lay the text out on a grid without
// carrying a width table of its own. R is the style, which is why T can be
// trimmed at all: everything past the text that still has colour is described
// by a run.
type Line struct {
	Y  int    `json:"y"`
	T  string `json:"t"`
	Wd []int  `json:"wd,omitempty"`
	R  []Run  `json:"r,omitempty"`
}

// Cursor is where the caret is and whether it is visible. Vis is DECTCEM, which
// magmux tracks per PANE rather than per screen: xterm keeps cursor visibility
// across an alternate-screen switch, so a TUI that hid the cursor and exited
// must not leave the shell behind it with a cursor magmux thinks is hidden.
type Cursor struct {
	Y   int  `json:"y"`
	X   int  `json:"x"`
	Vis bool `json:"vis"`
}

// FrameHeader is every field of a frame except `key` and `lines`.
//
// It is a separate type because those two are decided at the moment the frame
// is WRITTEN, not when it is built: a keyframe and the deltas that merge into
// it behind a slow subscriber become one message, whose `key` is true and whose
// `lines` are the merged set. Everything here is a property of the last
// observation and simply takes the newest value.
//
// Alt true means the pane is on its alternate screen, where Sb is 0 by
// construction: an alternate screen records no history (that is what stops vim
// flushing a shell's scrollback), so history for such a pane lives in the
// session's own transcript and not in magmux. Scrolled reports that the LOCAL
// human has scrolled this pane back; the stream itself always shows the live
// screen, because two viewers must not fight over one viewport.
type FrameHeader struct {
	Type     string `json:"type"`
	Pane     int    `json:"pane"`
	Seq      uint64 `json:"seq"`
	Rows     int    `json:"rows"`
	Cols     int    `json:"cols"`
	Alt      bool   `json:"alt"`
	Sb       int    `json:"sb"`
	Scrolled bool   `json:"scrolled"`
	Cur      Cursor `json:"cur"`
}

// Frame is the whole message. Key true means Lines covers every row 0..Rows-1
// and the client clears first; key false means Lines carries only the rows that
// changed, each replacing its whole row.
//
// It exists for DECODERS — clients, and magmux's own tests. The encoder builds
// the header and each line separately, because the merge happens between the
// two.
type Frame struct {
	FrameHeader
	Key   bool   `json:"key"`
	Lines []Line `json:"lines"`
}

// PaneArg is the one field of an op's arguments that something other than the
// op itself decodes: the hub reads it to know which pane's lane an `input` or a
// `send` belongs on. Everything else in an op's args stays raw.
//
// Pane is left as raw JSON because the wire accepts an index or a string, and
// which strings are legal ("*", "3", a label) is magmux's question rather than
// the hub's.
type PaneArg struct {
	Pane json.RawMessage `json:"pane"`
}
