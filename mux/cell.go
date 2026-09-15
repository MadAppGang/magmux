package mux

import "github.com/MadAppGang/magmux/theme"

// ── Cell & Attributes ─────────────────────────────────────────────────────────

type Attr uint16

const (
	AttrBold      Attr = 1 << 0
	AttrDim       Attr = 1 << 1
	AttrItalic    Attr = 1 << 2
	AttrBlink     Attr = 1 << 3
	AttrReverse   Attr = 1 << 4
	AttrInvis     Attr = 1 << 5
	AttrUnderline Attr = 1 << 6
	AttrStrike    Attr = 1 << 7
	AttrOverline  Attr = 1 << 8
)

// Color represents a terminal color: default (-1), 256-color (0-255), or truecolor
type Color struct {
	Index   int16 // -1=default, 0-255=indexed
	R, G, B uint8
	True    bool // if true, use R/G/B instead of Index
}

var defaultColor = Color{Index: -1}

type Cell struct {
	Ch   rune
	Fg   Color
	Bg   Color
	Attr Attr
	Wide bool // is this cell the left half of a wide char?
	Cont bool // is this a continuation (right half) of a wide char?
}

// toColor converts a palette entry to the renderer's Color, so main.go's own
// chrome can be driven from the same palette as the panel.
func toColor(c theme.RGB) Color { return Color{R: c.R, G: c.G, B: c.B, True: true} }
