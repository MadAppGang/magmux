// Package theme is the palette magmux paints its own chrome in, the order that
// picks it (--theme, MAGMUX_THEME, TERM_THEME, the OSC 11 probe, COLORFGBG,
// dark), and the OSC colour helpers magmux answers a child's colour query with.
//
// It is an implementation detail of magmux, with no API stability before v1.
package theme
