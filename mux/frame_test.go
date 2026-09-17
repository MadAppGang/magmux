package mux

// The wire format, pinned byte for byte.
//
// These are GOLDEN tests on purpose. Everything else in this repo asserts on
// behaviour, but a frame is a contract with clients magmux does not ship — a
// browser, a plugin, an MCP host — and a field that quietly changes shape is a
// break nobody here would notice. The expected JSON below is what those clients
// parse, so a diff in this file is a diff in the protocol and has to be read as
// one.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/protocol"
)

// styled builds one row of cells from a string, with a styler applied to each
// cell by column. Nothing clever: a row is the unit under test, and building it
// through the VT parser would be testing the parser.
func styled(text string, style func(i int, c *Cell)) []Cell {
	runes := []rune(text)
	row := make([]Cell, 0, len(runes))
	for i, r := range runes {
		c := Cell{Ch: r, Fg: defaultColor, Bg: defaultColor}
		if style != nil {
			style(i, &c)
		}
		row = append(row, c)
	}
	return row
}

func encodeRow(t *testing.T, row []Cell, y, cols int) string {
	t.Helper()
	var st rowStyle
	b, err := encodeLine(y, row, cols, &st)
	if err != nil {
		t.Fatalf("encodeLine: %v", err)
	}
	return string(b)
}

// TestFrameLineEncodingIsGolden is the format itself.
func TestFrameLineEncodingIsGolden(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  []Cell
		y    int
		cols int
		want string
	}{
		{
			// The ordinary case: a shell line with nothing on it. Trailing
			// blanks are trimmed and there is no `r` and no `wd` at all, which
			// is what keeps a mostly-empty screen cheap.
			name: "plain text right-trims and carries no style",
			row:  styled("$ ls      ", nil),
			y:    0,
			cols: 10,
			want: `{"y":0,"t":"$ ls"}`,
		},
		{
			// One run, merged across three identically-styled cells. Bold is
			// Attr 1; the colours are both default, and a run exists at all
			// only because the attribute is not.
			name: "an attribute alone makes a run",
			row: styled("abc", func(i int, c *Cell) {
				c.Attr = AttrBold
			}),
			y:    1,
			cols: 3,
			want: `{"y":1,"t":"abc","r":[[0,3,-1,-1,1]]}`,
		},
		{
			// Two styles side by side do NOT merge, and each run's columns are
			// cell columns from the start of the walk.
			name: "adjacent styles stay separate runs",
			row: styled("abcd", func(i int, c *Cell) {
				if i < 2 {
					c.Fg = Color{Index: 2}
				} else {
					c.Fg = Color{Index: 4}
				}
			}),
			y:    2,
			cols: 4,
			want: `{"y":2,"t":"abcd","r":[[0,2,2,-1,0],[2,2,4,-1,0]]}`,
		},
		{
			// Truecolor: one integer with the high bit set, 0xRRGGBB below it.
			// 0xFF8800 | 1<<24 is 33523712.
			name: "truecolor is one integer with the marker bit",
			row: styled("x", func(i int, c *Cell) {
				c.Fg = Color{True: true, R: 0xFF, G: 0x88, B: 0x00}
			}),
			y:    3,
			cols: 1,
			want: `{"y":3,"t":"x","r":[[0,1,33523712,-1,0]]}`,
		},
		{
			// A STYLED BLANK: the text is trimmed away and the run is not. This
			// is a selection, a filled status bar, a cleared line with a
			// background — real screen content with no character in it.
			name: "a styled blank survives the right-trim as a run",
			row: styled("hi    ", func(i int, c *Cell) {
				if i >= 2 {
					c.Bg = Color{Index: 4}
				}
			}),
			y:    4,
			cols: 6,
			want: `{"y":4,"t":"hi","r":[[2,4,-1,4,0]]}`,
		},
		{
			// A wide character is ONE code point in TWO cells. `wd` indexes the
			// TEXT (code point 1), the run indexes the CELLS (columns 1 and 2),
			// and the two diverging is exactly why both exist.
			name: "a double-width char is one code point in two cells",
			row: []Cell{
				{Ch: 'a', Fg: defaultColor, Bg: defaultColor},
				{Ch: '世', Fg: Color{Index: 5}, Bg: defaultColor, Wide: true},
				{Ch: 0, Fg: Color{Index: 5}, Bg: defaultColor, Cont: true},
				{Ch: 'b', Fg: defaultColor, Bg: defaultColor},
			},
			y:    5,
			cols: 4,
			want: `{"y":5,"t":"a世b","wd":[1],"r":[[1,2,5,-1,0]]}`,
		},
		{
			// A NUL cell — one a child never wrote — renders as a space, so a
			// client's column arithmetic is never off by the holes in a screen.
			name: "a NUL cell is a space",
			row:  []Cell{{Ch: 0, Fg: defaultColor, Bg: defaultColor}, {Ch: 'z', Fg: defaultColor, Bg: defaultColor}},
			y:    6,
			cols: 2,
			want: `{"y":6,"t":" z"}`,
		},
		{
			// A SCROLLBACK-WIDTH row: history keeps the width it was printed at
			// and nothing reflows it, so a row can be wider than the screen
			// showing it. The walk is bounded by cols AND by len(row), and it is
			// the first of those that applies here.
			name: "a row wider than the screen is cut at the screen",
			row:  styled("0123456789", nil),
			y:    7,
			cols: 4,
			want: `{"y":7,"t":"0123"}`,
		},
		{
			// …and the other way round: a row NARROWER than the screen is not
			// padded, and the walk stops at the row rather than indexing past it.
			name: "a row narrower than the screen stops at the row",
			row:  styled("ab", nil),
			y:    8,
			cols: 40,
			want: `{"y":8,"t":"ab"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeRow(t, tc.row, tc.y, tc.cols)
			if got != tc.want {
				t.Errorf("encodeLine =\n  %s\nwant\n  %s", got, tc.want)
			}
			// Whatever it is, it must decode as the type clients use.
			var line protocol.Line
			if err := json.Unmarshal([]byte(got), &line); err != nil {
				t.Fatalf("a frame line must decode as protocol.Line: %v", err)
			}
		})
	}
}

// TestFrameHeaderIsAnOpenObject: the encoder produces a header the hub can
// finish, and the two halves together must decode as one frame. The seam is the
// whole reason a keyframe and the deltas merged behind it can become one
// message, so it is worth a test of its own rather than only being exercised
// end to end.
func TestFrameHeaderIsAnOpenObject(t *testing.T) {
	hdr, err := frameHeader(protocol.FrameHeader{
		Type: protocol.EventFrame, Pane: 3, Seq: 118, Rows: 24, Cols: 80,
		Alt: false, Sb: 312, Scrolled: false,
		Cur: protocol.Cursor{Y: 23, X: 14, Vis: true},
	})
	if err != nil {
		t.Fatalf("frameHeader: %v", err)
	}
	// Open means it does not parse on its own: the hub has to finish it. (A
	// suffix check would not do — the header ends in the cursor object's own
	// brace.)
	if json.Valid(hdr) {
		t.Fatalf("the header is a complete object (%s); the hub cannot append key and lines to it", hdr)
	}
	if strings.HasSuffix(string(hdr), ",") {
		t.Fatalf("the header ends in a comma (%s); the hub appends its own", hdr)
	}
	whole := string(hdr) + `,"key":false,"lines":[{"y":22,"t":"$ echo"}]}`

	var f protocol.Frame
	if err := json.Unmarshal([]byte(whole), &f); err != nil {
		t.Fatalf("header + lines must decode as one frame: %v\n%s", err, whole)
	}
	if f.Type != "frame" || f.Pane != 3 || f.Seq != 118 || f.Rows != 24 || f.Cols != 80 || f.Sb != 312 {
		t.Errorf("header round-trip lost a field: %+v", f)
	}
	if !f.Cur.Vis || f.Cur.Y != 23 || f.Cur.X != 14 {
		t.Errorf("cursor round-trip = %+v, want {23 14 true}", f.Cur)
	}
	if f.Key || len(f.Lines) != 1 || f.Lines[0].Y != 22 {
		t.Errorf("lines round-trip = key %v %+v", f.Key, f.Lines)
	}
}

// TestColorIntDistinguishesTheThreeShapes. One integer carries three kinds of
// colour, and the marker bit is what keeps indexed 255 and the RGB colour
// #0000FF from being the same number — which, without it, they would be.
func TestColorIntDistinguishesTheThreeShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Color
		want int
	}{
		{"default", defaultColor, -1},
		{"indexed 0", Color{Index: 0}, 0},
		{"indexed 255", Color{Index: 255}, 255},
		{"truecolor black", Color{True: true}, protocol.ColorTrue},
		{"truecolor 0000ff", Color{True: true, B: 0xFF}, protocol.ColorTrue | 0xFF},
		{"truecolor ffffff", Color{True: true, R: 0xFF, G: 0xFF, B: 0xFF}, protocol.ColorTrue | 0xFFFFFF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := colorInt(tc.in); got != tc.want {
				t.Errorf("colorInt(%+v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
	if colorInt(Color{Index: 255}) == colorInt(Color{True: true, B: 0xFF}) {
		t.Error("indexed 255 and truecolor #0000ff encode to the same integer")
	}
}

// TestRowWalkIsStillTheOneCellWalk. rowText is now rowWalk with nothing
// recorded, and the claim that matters is that passing an accumulator does not
// change the TEXT: capture, selection and the stream must never disagree about
// what a row said.
func TestRowWalkIsStillTheOneCellWalk(t *testing.T) {
	rows := [][]Cell{
		styled("plain", nil),
		styled("trailing   ", nil),
		{{Ch: 0}, {Ch: 'x'}},
		{
			{Ch: 'a'},
			{Ch: '世', Wide: true},
			{Ch: 0, Cont: true},
			{Ch: 'b'},
		},
	}
	for i, row := range rows {
		var st rowStyle
		plain := rowText(row, 0, len(row)-1)
		withStyle := rowWalk(row, 0, len(row)-1, &st)
		if plain != withStyle {
			t.Errorf("row %d: rowText = %q but rowWalk with an accumulator = %q", i, plain, withStyle)
		}
	}
}

// TestRowWalkRunColumnsAreRelativeToTheWindow: a caller that walks a window
// gets runs in that window's coordinates, which is what lets selection and the
// framer share the walk without one of them subtracting an origin.
func TestRowWalkRunColumnsAreRelativeToTheWindow(t *testing.T) {
	row := styled("abcdef", func(i int, c *Cell) {
		if i == 4 {
			c.Attr = AttrUnderline
		}
	})
	var st rowStyle
	if got := rowWalk(row, 2, 5, &st); got != "cdef" {
		t.Fatalf("text = %q, want %q", got, "cdef")
	}
	if len(st.runs) != 1 || st.runs[0].Col() != 2 || st.runs[0].Len() != 1 {
		t.Fatalf("runs = %v, want one run at column 2 (cell 4 minus the window's origin)", st.runs)
	}
}
