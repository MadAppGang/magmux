package main

// The palette, and the one place a colour is given a meaning.
//
// TWO BACKGROUNDS, NOT ONE. The operator this was rewritten for runs a CREAM
// terminal, and the driver it replaced was grey-on-cream — legible in the
// author's dark terminal and washed out in the reader's. So every token is a
// PAIR chosen through lipgloss.LightDark: the light member is darkened and
// saturated enough to read on paper-white, the dark member is brightened
// enough to read on near-black. There is no third case; a terminal that
// answers no background query is treated as dark, which is the safer guess
// because a dark-tuned palette on a light terminal is merely low contrast
// while the reverse is invisible.
//
// Colour is SEMANTIC here and nowhere decorative:
//
//	green   proven / ok / alive          — and it is what a 403 gets when a
//	                                       refusal was the point (see Verdict)
//	red     refused / failed / dead
//	amber   the READ-ONLY view token, and staleness
//	violet  the FULL session token — the credential that can type
//	blue    accent: focus, titles, the thing to look at next
//	grey    chrome: borders, labels, anything that must recede

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
)

// Theme is the resolved palette. Every style in the app is built from it, and
// nothing anywhere else names a hex value.
type Theme struct {
	Dark bool

	// Chrome — low contrast, recedes.
	Text   color.Color
	Dim    color.Color
	Faint  color.Color
	Border color.Color
	Accent color.Color
	Ink    color.Color // text ON a saturated badge

	// Signal — saturated, means one thing each.
	OK      color.Color
	Err     color.Color
	Warn    color.Color
	Info    color.Color
	Full    color.Color // the session token
	View    color.Color // the read-only token
	PanelBg color.Color // panel background, barely off the terminal's own
}

// NewTheme resolves the palette for one background.
func NewTheme(dark bool) Theme {
	ld := lipgloss.LightDark(dark)
	return Theme{
		Dark:    dark,
		Text:    ld(lipgloss.Color("#1C1B22"), lipgloss.Color("#CDD6F4")),
		Dim:     ld(lipgloss.Color("#5C5A6B"), lipgloss.Color("#9399B2")),
		Faint:   ld(lipgloss.Color("#8E8BA0"), lipgloss.Color("#6C7086")),
		Border:  ld(lipgloss.Color("#C3BFD4"), lipgloss.Color("#45475A")),
		Accent:  ld(lipgloss.Color("#1E66F5"), lipgloss.Color("#89B4FA")),
		Ink:     ld(lipgloss.Color("#FFFFFF"), lipgloss.Color("#11111B")),
		OK:      ld(lipgloss.Color("#1A7F37"), lipgloss.Color("#2ECC71")),
		Err:     ld(lipgloss.Color("#C4314B"), lipgloss.Color("#FF6B6B")),
		Warn:    ld(lipgloss.Color("#B5730B"), lipgloss.Color("#FFB454")),
		Info:    ld(lipgloss.Color("#0A6E9E"), lipgloss.Color("#5AC8FF")),
		Full:    ld(lipgloss.Color("#7A3AC4"), lipgloss.Color("#CBA6F7")),
		View:    ld(lipgloss.Color("#B5730B"), lipgloss.Color("#FFB454")),
		PanelBg: ld(lipgloss.Color("#F4F2F8"), lipgloss.Color("#1E1E2E")),
	}
}

// ── the visual vocabulary ───────────────────────────────────────────────────

// Badge is a chip: ink on a saturated background. Used for states, statuses and
// credentials — anything discrete. A discrete value never renders as bare
// coloured text, because a chip is findable at a glance and a word is not.
func (t Theme) Badge(label string, bg color.Color) string {
	return lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Ink).
		Background(bg).
		Padding(0, 1).
		Render(label)
}

// Tag is the quieter half-badge: coloured text in a bracket, for a label that
// must be readable but must not compete with a real badge beside it.
func (t Theme) Tag(label string, fg color.Color) string {
	return lipgloss.NewStyle().Foreground(fg).Bold(true).Render(label)
}

// Meter is the canonical bounded value: a gradient across the FILL, one colour
// per cell, so the colour and the length both carry the magnitude. Never a flat
// bar and never a bare number — the number goes beside it as a label.
func (t Theme) Meter(frac float64, width int, from, to color.Color) string {
	if width < 1 {
		return ""
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	cols := lipgloss.Blend1D(width, from, to)
	filled := int(frac*float64(width) + 0.5)
	var b strings.Builder
	for i := range width {
		ch := "░"
		if i < filled {
			ch = "█"
		}
		fg := cols[i]
		if i >= filled {
			fg = t.Faint
		}
		b.WriteString(lipgloss.NewStyle().Foreground(fg).Render(ch))
	}
	return b.String()
}

var sparkRunes = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// Spark is a time series in one row: height AND colour both track magnitude, so
// a burst is visible even in a screenshot that lost its colour. `max` is passed
// in rather than derived, because a sparkline that renormalises every tick
// shows a flat line for a quiet pane and a flat line for a busy one.
func (t Theme) Spark(values []float64, max float64, from, to color.Color) string {
	if len(values) == 0 {
		return ""
	}
	if max <= 0 {
		max = 1
	}
	ramp := lipgloss.Blend1D(len(sparkRunes), from, to)
	var b strings.Builder
	for _, v := range values {
		f := v / max
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		idx := int(f * float64(len(sparkRunes)-1))
		fg := ramp[idx]
		if v <= 0 {
			// A second with no frames still draws its baseline, in the chrome
			// grey: a sparkline that vanished when the pane went quiet would be
			// indistinguishable from one that was never wired up, and an idle
			// pane producing no frames is the NORMAL case here. Faint rather
			// than Border because Faint is the token that keeps a usable
			// contrast on BOTH backgrounds — Border is deliberately the
			// lowest-contrast colour there is, and on cream it disappears.
			fg = t.Faint
		}
		b.WriteString(lipgloss.NewStyle().Foreground(fg).Render(string(sparkRunes[idx])))
	}
	return b.String()
}

// Panel draws one box. The focused one takes the accent border and a bold
// title; everything else recedes into the border grey. That contrast is what
// makes a dense screen navigable.
func (t Theme) Panel(title, body string, w, h int, focused bool) string {
	bc := t.Border
	ts := lipgloss.NewStyle().Foreground(t.Dim)
	if focused {
		bc = t.Accent
		ts = ts.Foreground(t.Accent).Bold(true)
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(bc).
		Padding(0, 1).
		Width(w).
		Height(h)
	fw, fh := box.GetFrameSize()
	innerH := max(1, h-fh)
	// CLIP THE BODY. lipgloss's Height is a MINIMUM, not a maximum, so a panel
	// handed one line more than it has room for renders one row taller — and
	// every panel below it slides down, which in a full-height layout means the
	// footer falls off the bottom of the screen. That is exactly what the
	// ntcharts bar chart plus its `+N plugin` badge did the first time a plugin
	// registered, and it is invisible until something actually overflows. The
	// caller's arithmetic should be right; this is the guard that makes being
	// wrong about it cost a truncated panel instead of a broken frame.
	lines := strings.Split(body, "\n")
	if len(lines) > innerH-1 {
		lines = lines[:max(0, innerH-1)]
	}
	inner := lipgloss.Place(
		max(1, w-fw), innerH,
		lipgloss.Left, lipgloss.Top,
		lipgloss.JoinVertical(lipgloss.Left, ts.Render(title), strings.Join(lines, "\n")),
	)
	return box.Render(inner)
}

// Clip cuts a line to a display width, measured in CELLS. len() would count
// bytes and every badge in this app carries escape sequences, so len() here is
// a layout bug waiting for the first non-ASCII rune.
func Clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}
