package mux

// Encoding a screen for the wire.
//
// The whole of the frame format is here: how a Cell becomes a colour integer,
// how a row of cells becomes text plus style runs, and how a frame's header is
// built. The MERGE — which rows a subscriber gets and whether the result is a
// keyframe — belongs to the hub's slot (hub/watch.go), because it depends on
// what that one subscriber has not read yet.
//
// Two decisions are worth stating, because both look like premature
// compression and neither is:
//
//   - Text and style are SEPARATE. A row is one string plus a handful of runs,
//     not a cell array. A shell line is 80 cells and one or two styles; an
//     array of per-cell objects would be two orders of magnitude larger for the
//     same information, on every frame, at up to 30 frames a second.
//   - Columns in a run are CELL columns, never indexes into the text. A
//     double-width character is one code point in two cells, so the two
//     coordinate systems diverge at the first CJK glyph, and a client painting
//     runs by code point would tint the wrong half of every line after it. `wd`
//     exists so a client can convert without a width table of its own.

import (
	"encoding/json"

	"github.com/MadAppGang/magmux/protocol"
)

// colorInt encodes one Color as the single integer the wire carries.
//
// The three shapes collapse into one number rather than one object because a
// frame is mostly colour: -1 is the terminal's own default, 0..255 is the
// indexed palette, and anything at or above protocol.ColorTrue is truecolor
// with 0xRRGGBB in its low 24 bits. The high bit is what keeps index 255 and
// the colour #0000FF from being the same number.
func colorInt(c Color) int {
	if c.True {
		return protocol.ColorTrue | int(c.R)<<16 | int(c.G)<<8 | int(c.B)
	}
	if c.Index < 0 {
		return protocol.ColorDefault
	}
	return int(c.Index)
}

// rowStyle is what rowWalk records on its way across a row, for the caller that
// wants more than the text.
//
// It is an accumulator rather than a return value so the ONE cell walk can
// serve both readers: capture asks for text and passes nil, the framer asks for
// text plus style and passes one of these. A second walk would drift on the
// first wide-character or NUL-cell fix, and history, selection and the live
// stream disagreeing about what a row said is a bug nobody would look for.
//
// Reused between rows: reset() keeps the slices and drops their contents, so
// encoding a 24-row screen 15 times a second does not allocate 360 slices a
// second.
type rowStyle struct {
	runs []protocol.Run
	wide []int
	// open says the run at the end of runs is still being extended, and by
	// which style. A default-styled cell closes it: a run is only ever emitted
	// for cells that differ from the terminal's own defaults.
	open         bool
	fg, bg, attr int
	lastCol      int
}

func (st *rowStyle) reset() {
	st.runs = st.runs[:0]
	st.wide = st.wide[:0]
	st.open = false
}

// add folds one cell into the style runs. Merging happens here rather than in a
// second pass because a run is only ever extended by the cell to its right.
func (st *rowStyle) add(col int, c Cell) {
	fg, bg, attr := colorInt(c.Fg), colorInt(c.Bg), int(c.Attr)
	if fg == protocol.ColorDefault && bg == protocol.ColorDefault && attr == 0 {
		st.open = false
		return
	}
	if st.open && st.fg == fg && st.bg == bg && st.attr == attr && st.lastCol+1 == col {
		st.runs[len(st.runs)-1][1]++
		st.lastCol = col
		return
	}
	st.runs = append(st.runs, protocol.NewRun(col, 1, fg, bg, attr))
	st.open, st.fg, st.bg, st.attr, st.lastCol = true, fg, bg, attr, col
}

// encodeLine renders one row of cells as the JSON object a frame carries.
//
// cols bounds the walk, and rowWalk clamps it again to the row itself: a
// scrollback row keeps the width it was printed at and can be wider or narrower
// than the screen showing it, so every reader has to bound by len(row) as well.
func encodeLine(y int, row []Cell, cols int, st *rowStyle) ([]byte, error) {
	st.reset()
	text := rowWalk(row, 0, cols-1, st)
	line := protocol.Line{Y: y, T: text}
	if len(st.wide) > 0 {
		line.Wd = append([]int(nil), st.wide...)
	}
	if len(st.runs) > 0 {
		line.R = append([]protocol.Run(nil), st.runs...)
	}
	return json.Marshal(line)
}

// frameHeader builds the part of a frame that is fixed when the screen is READ.
//
// It hands back a JSON object that has been opened and NOT closed, with no
// trailing comma, because the hub appends the two fields it owns — `key` and
// `lines` — and only it knows what they are once a keyframe and the deltas
// behind it have merged into one message. json.Marshal of a struct always ends
// in '}', so dropping the last byte is exact rather than a parse.
func frameHeader(h protocol.FrameHeader) ([]byte, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	return b[:len(b)-1], nil
}

// changedLine is the notify mode's whole message: this pane moved, go and read
// it. It is a complete line, newline included, because it travels through the
// same slot a frame does and a slot's raw form is written verbatim.
func changedLine(pane int) []byte {
	b, err := json.Marshal(map[string]any{"type": protocol.EventChanged, "pane": pane})
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
