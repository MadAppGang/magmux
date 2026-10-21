package obsview

// wireChanges is every (session, field) pair whose JSON may differ from the
// recorded goldens under testdata/golden/magmux, keyed "<session id>/<field>"
// with the decision row that changed it (e.g. "D6"), or "*/<field>" for every
// session.
//
// It is EMPTY on purpose. The goldens were recorded from magmux's own
// observation layer before the claudestate library existed (claudestate
// architecture §10, C0.6), and TestWireGoldens fails on any difference not
// cited here. When the library changes the wire — a job row, a new member —
// the change is named in this table, in the same diff, by the decision that
// made it; a golden is never re-recorded to make a difference disappear, and a
// testdata change cannot hide one either, because each golden directory's
// MANIFEST pins every input byte.
var wireChanges = map[string]string{}
