package termev

import (
	"fmt"
	"strings"
)

// Limits bounds what a [Diffs] retains. Every field is a hard cap applied at
// the moment a frame is observed, so the memory a Diffs holds is bounded by
// the limits and not by what the child chose to print.
//
// The bounds are not paranoia. A pane is a terminal and a terminal is fed by a
// program that can print anything: a single row can be a megabyte of one line
// if nothing wrapped it, and a child in the alternate screen can repaint every
// row of a very tall window at 60 Hz. Two frames plus a delta, unbounded, is
// unbounded per pane and magmux runs many.
type Limits struct {
	// MaxRows caps the rows retained per frame. Rows beyond it are dropped
	// from the retained copy and counted in [Diff.Truncated].
	MaxRows int
	// MaxRowRunes caps the runes retained per row. Longer rows are truncated;
	// comparison then happens on the truncated text, which is stated rather
	// than hidden because it means a change past the cap is invisible.
	MaxRowRunes int
	// MaxDeltaRows caps how many changed rows the delta itself records. The
	// COUNTS ([Diff.ChangedRows], [Diff.ChangedRunes]) are exact regardless;
	// only the recorded detail is capped, so "a spinner moved" stays
	// answerable on a screen that changed everywhere.
	MaxDeltaRows int
}

// DefaultLimits are the limits a caller gets when it expresses no opinion:
// 200 rows, 1000 runes per row, 64 recorded changes.
//
// 200 rows is a tall terminal with room to spare; 1000 runes is well past any
// real terminal width, so an ordinary pane is never truncated and only
// pathological output is; 64 recorded changes is enough to see the shape of a
// partial repaint without retaining a whole second screen a third time.
func DefaultLimits() Limits {
	return Limits{MaxRows: 200, MaxRowRunes: 1000, MaxDeltaRows: 64}
}

func (l Limits) normalise() Limits {
	d := DefaultLimits()
	if l.MaxRows <= 0 {
		l.MaxRows = d.MaxRows
	}
	if l.MaxRowRunes <= 0 {
		l.MaxRowRunes = d.MaxRowRunes
	}
	if l.MaxDeltaRows <= 0 {
		l.MaxDeltaRows = d.MaxDeltaRows
	}
	return l
}

// Frame is one observed screen: the pane's rows as text, already bounded by
// the [Limits] in force when it was observed.
//
// Text, not cells. Colour and attributes are magmux's to render and are not
// evidence about what a program is doing — a spinner that changes colour every
// frame is still a spinner. Keeping text also keeps this package free of the
// pane model, which is the rule the whole observation layer rests on.
type Frame struct {
	Rows []string
	// Truncated records that the observation lost something: more rows than
	// MaxRows, or a row longer than MaxRowRunes.
	Truncated bool
}

// Empty reports whether the frame has no rows at all — a pane observed before
// anything was printed into it.
func (f Frame) Empty() bool { return len(f.Rows) == 0 }

// Clone returns a deep copy. Strings are immutable, so only the slice header
// needs copying; the method exists so a caller handed a Frame cannot alias the
// tracker's own slice and mutate it underneath.
func (f Frame) Clone() Frame {
	out := Frame{Truncated: f.Truncated}
	if f.Rows != nil {
		out.Rows = make([]string, len(f.Rows))
		copy(out.Rows, f.Rows)
	}
	return out
}

// RowChange is one row that differs between two frames.
//
// A row that only exists on one side carries the empty string on the other,
// and [RowChange.Added] / [RowChange.Removed] say which — because an empty
// string is also a legitimate row, and "the row went blank" and "the screen
// got shorter" are different facts.
type RowChange struct {
	Row     int
	Prev    string
	Cur     string
	Added   bool
	Removed bool
}

// Diff is the delta between two frames.
//
// It exists so that a consumer doing heuristics can tell three cases apart
// that magmux's dirty flag cannot:
//
//   - Identical — nothing moved. The pane was repainted and said the same
//     thing, which is what a redraw after a resize looks like.
//   - One row, a handful of runes — a spinner, a clock, a progress percentage.
//     Motion without progress.
//   - Many rows — the program actually did something.
//
// The counts are exact even when the recorded detail is capped, because the
// distinction above is made on the counts.
type Diff struct {
	// Identical is true when the two frames are equal row for row.
	Identical bool
	// ChangedRows is how many row positions differ, counting rows present on
	// only one side. Exact.
	ChangedRows int
	// ChangedRunes is the total number of rune positions that differ across
	// all changed rows, with a row present on one side only counting its full
	// length. Exact. It is a POSITIONAL count, not an edit distance: a row
	// whose text shifted left by one reads as changed everywhere, which is the
	// honest answer for a screen rather than for a document.
	ChangedRunes int
	// Rows records the changes themselves, capped at Limits.MaxDeltaRows.
	Rows []RowChange
	// Truncated is true when more rows changed than Rows records, or when
	// either frame was itself truncated on observation.
	Truncated bool
}

// SpinnerLike reports the narrow case worth naming: exactly one row changed,
// and by at most maxRunes rune positions. It is a convenience over the counts,
// not a new fact — a consumer that wants a different threshold reads
// ChangedRows and ChangedRunes directly.
func (d Diff) SpinnerLike(maxRunes int) bool {
	return !d.Identical && d.ChangedRows == 1 && d.ChangedRunes <= maxRunes
}

// String renders a diff the way a log line wants it.
func (d Diff) String() string {
	if d.Identical {
		return "identical"
	}
	s := fmt.Sprintf("%d rows, %d runes changed", d.ChangedRows, d.ChangedRunes)
	if d.Truncated {
		s += " (truncated)"
	}
	return s
}

// Diffs keeps the previous frame, the current frame and the delta between
// them. Nothing older is retained: two frames answer "what just moved", and
// answering "what moved over the last minute" is a consumer's job with the
// events it was given, not a buffer this package grows.
//
// A Diffs is single-owner, like [Cascade]: no locks, no goroutine, and the
// goroutine that made one is the only one that may touch it.
type Diffs struct {
	limits Limits
	prev   Frame
	cur    Frame
	delta  Diff
	seen   bool
}

// NewDiffs builds a tracker. A zero [Limits], or any zero field within one,
// takes the corresponding [DefaultLimits] value.
func NewDiffs(limits Limits) *Diffs {
	return &Diffs{limits: limits.normalise()}
}

// Limits returns the bounds in force.
func (d *Diffs) Limits() Limits { return d.limits }

// Prev is the frame before the current one.
func (d *Diffs) Prev() Frame { return d.prev.Clone() }

// Cur is the most recently observed frame.
func (d *Diffs) Cur() Frame { return d.cur.Clone() }

// Delta is the difference between [Diffs.Prev] and [Diffs.Cur].
func (d *Diffs) Delta() Diff { return d.delta }

// Observe records rows as the current frame and returns the delta against the
// previous one.
//
// The FIRST observation has nothing to compare against. It is reported as a
// change of every row it contains — not as Identical, which would claim the
// screen had been seen before and had not moved. A caller that needs to know
// it was the first asks [Diffs.Prev] whether it is empty, or keeps its own
// count; inventing a third state on Diff for a case that happens once per pane
// would put a branch in every consumer forever.
//
// rows is COPIED. The caller may reuse or mutate its slice immediately.
func (d *Diffs) Observe(rows []string) Diff {
	next := d.bound(rows)
	d.prev, d.cur = d.cur, next
	if !d.seen {
		// Nothing was ever observed before, so prev is a zero Frame rather
		// than an observed empty screen. Comparing against it is still the
		// right arithmetic — every row is new — and this flag only stops that
		// being read as "the screen used to be blank".
		d.seen = true
	}
	d.delta = d.compare(d.prev, d.cur)
	return d.delta
}

// bound applies the limits to an incoming screen.
func (d *Diffs) bound(rows []string) Frame {
	f := Frame{}
	n := len(rows)
	if n > d.limits.MaxRows {
		n = d.limits.MaxRows
		f.Truncated = true
	}
	if n > 0 {
		f.Rows = make([]string, n)
	}
	for i := 0; i < n; i++ {
		r := rows[i]
		// Count runes, not bytes: a row is a row of characters and cutting in
		// the middle of a multi-byte rune would produce text no consumer can
		// print. utf8.RuneCountInString would walk the row twice, so the cut
		// is done in one pass over the indices instead.
		if cut, over := cutRunes(r, d.limits.MaxRowRunes); over {
			r = cut
			f.Truncated = true
		}
		f.Rows[i] = r
	}
	return f
}

// cutRunes truncates s to at most max runes, reporting whether it had more.
func cutRunes(s string, max int) (string, bool) {
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}

func (d *Diffs) compare(prev, cur Frame) Diff {
	out := Diff{Truncated: prev.Truncated || cur.Truncated}
	rows := len(prev.Rows)
	if len(cur.Rows) > rows {
		rows = len(cur.Rows)
	}
	for i := 0; i < rows; i++ {
		var (
			p, c              string
			added, removed    bool
			havePrev, haveCur bool
		)
		if i < len(prev.Rows) {
			p, havePrev = prev.Rows[i], true
		}
		if i < len(cur.Rows) {
			c, haveCur = cur.Rows[i], true
		}
		added = !havePrev && haveCur
		removed = havePrev && !haveCur
		if p == c && !added && !removed {
			continue
		}
		out.ChangedRows++
		out.ChangedRunes += runeDelta(p, c)
		if len(out.Rows) < d.limits.MaxDeltaRows {
			out.Rows = append(out.Rows, RowChange{Row: i, Prev: p, Cur: c, Added: added, Removed: removed})
		} else {
			out.Truncated = true
		}
	}
	out.Identical = out.ChangedRows == 0
	return out
}

// runeDelta counts the rune POSITIONS at which a and b differ, plus the length
// difference. It is not an edit distance and is not meant to be: a terminal
// row is a fixed grid of cells, and cell 40 changing is a fact about cell 40
// whatever happened to cell 39.
func runeDelta(a, b string) int {
	if a == b {
		return 0
	}
	ar, br := []rune(a), []rune(b)
	n := len(ar)
	if len(br) < n {
		n = len(br)
	}
	diff := 0
	for i := 0; i < n; i++ {
		if ar[i] != br[i] {
			diff++
		}
	}
	diff += abs(len(ar) - len(br))
	return diff
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// RowsFromText splits a captured screen into rows. It is a convenience for
// callers holding a pane capture as one string; magmux's own capture already
// joins rows with "\n", and this is its inverse.
func RowsFromText(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
