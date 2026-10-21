package termev

import (
	"strings"
	"testing"
)

// TestTheFirstObservationIsNotIdentical pins the one case with nothing to
// compare against.
//
// A WRONG IMPLEMENTATION comparing a first frame against a zero Frame and
// calling the result "identical" — which it would be, if both were empty —
// would tell a consumer that a pane which has just painted its whole screen
// did nothing. One that reported the first frame as identical whenever it
// happened to be empty would be right by accident and wrong the moment a pane
// is first observed with content on it.
func TestTheFirstObservationIsNotIdentical(t *testing.T) {
	d := NewDiffs(Limits{})

	got := d.Observe([]string{"one", "two"})
	if got.Identical {
		t.Fatal("the first observation reported identical")
	}
	if got.ChangedRows != 2 {
		t.Errorf("ChangedRows is %d, want 2", got.ChangedRows)
	}
	if got.ChangedRunes != 6 {
		t.Errorf("ChangedRunes is %d, want 6", got.ChangedRunes)
	}
	for _, rc := range got.Rows {
		if !rc.Added {
			t.Errorf("row %d is not marked Added", rc.Row)
		}
	}
	if !d.Prev().Empty() {
		t.Error("Prev is not empty after a single observation")
	}
}

// TestIdenticalFramesReportNothingChanged is the distinction magmux's dirty
// flag cannot make.
//
// A WRONG IMPLEMENTATION that reported a change whenever Observe was called —
// treating the call itself as evidence — would make every repaint look like
// activity, and the staleness heuristics built on top would never see a quiet
// screen. That is the same mistake as stamping every dirty site, one layer up.
func TestIdenticalFramesReportNothingChanged(t *testing.T) {
	d := NewDiffs(Limits{})
	rows := []string{"alpha", "beta", ""}

	d.Observe(rows)
	got := d.Observe([]string{"alpha", "beta", ""})

	if !got.Identical {
		t.Fatalf("a repeated frame reported %v", got)
	}
	if got.ChangedRows != 0 || got.ChangedRunes != 0 {
		t.Errorf("Identical diff carries %d rows / %d runes", got.ChangedRows, got.ChangedRunes)
	}
	if len(got.Rows) != 0 {
		t.Errorf("Identical diff records %d row changes", len(got.Rows))
	}
	if got.String() != "identical" {
		t.Errorf("String is %q", got.String())
	}
}

// TestASpinnerIsOneRowAndAHandfulOfRunes is the case the whole delta exists
// for: motion without progress.
//
// A WRONG IMPLEMENTATION reporting only a boolean "something changed" would
// make a spinner indistinguishable from a program printing a thousand lines,
// so every heuristic above would have to call a spinning pane busy. One
// counting BYTES rather than rune positions would score a one-character change
// between two multi-byte braille spinner frames as three, and a threshold set
// for "a handful" would start missing them.
func TestASpinnerIsOneRowAndAHandfulOfRunes(t *testing.T) {
	d := NewDiffs(Limits{})
	d.Observe([]string{"header", "⠋ working", "footer"})
	got := d.Observe([]string{"header", "⠙ working", "footer"})

	if got.Identical {
		t.Fatal("the spinner frame reported identical")
	}
	if got.ChangedRows != 1 {
		t.Fatalf("ChangedRows is %d, want 1", got.ChangedRows)
	}
	if got.ChangedRunes != 1 {
		t.Fatalf("ChangedRunes is %d, want 1 — a multi-byte rune is ONE position", got.ChangedRunes)
	}
	if !got.SpinnerLike(2) {
		t.Error("SpinnerLike(2) is false for a one-rune change on one row")
	}
	if len(got.Rows) != 1 || got.Rows[0].Row != 1 {
		t.Fatalf("recorded changes are %v", got.Rows)
	}
	if got.Rows[0].Added || got.Rows[0].Removed {
		t.Error("an edited row is marked Added or Removed")
	}

	// And the contrast: a real repaint is not spinner-like.
	busy := d.Observe([]string{"header", "⠙ working", "compiled 41 files", "linking", "done"})
	if busy.SpinnerLike(2) {
		t.Errorf("a three-row change reported as spinner-like: %v", busy)
	}
	// Row 2 was rewritten and rows 3 and 4 are new; the spinner row is the one
	// that did NOT move this time.
	if busy.ChangedRows != 3 {
		t.Errorf("ChangedRows is %d, want 3", busy.ChangedRows)
	}
}

// TestAddedAndRemovedRowsAreNotTheSameAsBlankedRows keeps two different facts
// apart.
//
// A WRONG IMPLEMENTATION padding the shorter frame with empty strings and
// comparing would report a screen that got shorter as a screen whose last rows
// went blank — and then report NO change at all when a blank row is dropped,
// because "" equals "". A pane that shrank would be indistinguishable from one
// that did nothing.
func TestAddedAndRemovedRowsAreNotTheSameAsBlankedRows(t *testing.T) {
	t.Run("blanked", func(t *testing.T) {
		d := NewDiffs(Limits{})
		d.Observe([]string{"a", "b"})
		got := d.Observe([]string{"a", ""})
		if got.ChangedRows != 1 {
			t.Fatalf("ChangedRows is %d, want 1", got.ChangedRows)
		}
		rc := got.Rows[0]
		if rc.Added || rc.Removed {
			t.Errorf("a blanked row is marked Added=%v Removed=%v", rc.Added, rc.Removed)
		}
	})

	t.Run("removed", func(t *testing.T) {
		d := NewDiffs(Limits{})
		d.Observe([]string{"a", ""})
		got := d.Observe([]string{"a"})
		if got.ChangedRows != 1 {
			t.Fatalf("ChangedRows is %d, want 1 — the frame got shorter", got.ChangedRows)
		}
		rc := got.Rows[0]
		if !rc.Removed {
			t.Error("a dropped row is not marked Removed")
		}
		if rc.Prev != "" || rc.Cur != "" {
			t.Errorf("a dropped blank row records Prev=%q Cur=%q", rc.Prev, rc.Cur)
		}
	})

	t.Run("added", func(t *testing.T) {
		d := NewDiffs(Limits{})
		d.Observe([]string{"a"})
		got := d.Observe([]string{"a", "b"})
		if got.ChangedRows != 1 {
			t.Fatalf("ChangedRows is %d, want 1", got.ChangedRows)
		}
		if !got.Rows[0].Added {
			t.Error("a new row is not marked Added")
		}
		if got.ChangedRunes != 1 {
			t.Errorf("ChangedRunes is %d, want 1", got.ChangedRunes)
		}
	})
}

// TestLimitsBoundEveryRetainedThing is what stops one pane's output becoming
// one pane's memory leak.
//
// A WRONG IMPLEMENTATION retaining whatever it was handed would hold two full
// frames per pane with no ceiling — a child printing a megabyte on one
// unwrapped line puts two megabytes in the observer, per pane, forever. One
// that cut rows by BYTE index would split a multi-byte rune and hand a
// consumer text that cannot be printed.
func TestLimitsBoundEveryRetainedThing(t *testing.T) {
	d := NewDiffs(Limits{MaxRows: 3, MaxRowRunes: 4, MaxDeltaRows: 2})

	long := strings.Repeat("é", 100) // 100 runes, 200 bytes
	got := d.Observe([]string{long, "bbbbbbbb", "cccccccc", "dddddddd", "eeeeeeee"})

	cur := d.Cur()
	if len(cur.Rows) != 3 {
		t.Fatalf("retained %d rows, want the 3-row cap", len(cur.Rows))
	}
	if n := len([]rune(cur.Rows[0])); n != 4 {
		t.Fatalf("retained %d runes on row 0, want the 4-rune cap", n)
	}
	if cur.Rows[0] != "éééé" {
		t.Fatalf("row 0 is %q — the cut was made by byte, not by rune", cur.Rows[0])
	}
	if !cur.Truncated {
		t.Error("the frame does not record that it was truncated")
	}
	if !got.Truncated {
		t.Error("the diff does not record that its frames were truncated")
	}
	if got.ChangedRows != 3 {
		t.Errorf("ChangedRows is %d, want 3 — the counts are over the bounded frame", got.ChangedRows)
	}
	if len(got.Rows) != 2 {
		t.Errorf("recorded %d row changes, want the 2-row cap", len(got.Rows))
	}
}

// TestTheCountsStayExactWhenTheDetailIsCapped is the reason the cap is on the
// recorded detail and not on the counting.
//
// A WRONG IMPLEMENTATION that stopped counting when it stopped recording would
// report "64 rows changed" for every large repaint, so a consumer could no
// longer tell a 65-row change from a 200-row one — and, worse, a spinner test
// phrased as ChangedRows == 1 would keep working while every other threshold
// silently saturated.
func TestTheCountsStayExactWhenTheDetailIsCapped(t *testing.T) {
	d := NewDiffs(Limits{MaxRows: 50, MaxRowRunes: 80, MaxDeltaRows: 2})
	before := make([]string, 20)
	after := make([]string, 20)
	for i := range before {
		before[i] = "x"
		after[i] = "y"
	}
	d.Observe(before)
	got := d.Observe(after)

	if got.ChangedRows != 20 {
		t.Fatalf("ChangedRows is %d, want the exact 20", got.ChangedRows)
	}
	if got.ChangedRunes != 20 {
		t.Fatalf("ChangedRunes is %d, want the exact 20", got.ChangedRunes)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("recorded %d changes, want the cap of 2", len(got.Rows))
	}
	if !got.Truncated {
		t.Error("a capped record does not report itself truncated")
	}
}

// TestObserveCopiesTheCallersRows and the accessors hand out copies.
//
// A WRONG IMPLEMENTATION retaining the caller's slice would alias whatever
// buffer the capture path reuses, so "the previous frame" would silently
// become the current one and every diff would come out identical. The failure
// looks exactly like a working quiet screen, which is the worst possible
// disguise for it.
func TestObserveCopiesTheCallersRows(t *testing.T) {
	d := NewDiffs(Limits{})
	rows := []string{"a", "b"}
	d.Observe(rows)

	rows[0] = "MUTATED"
	got := d.Observe([]string{"a", "b"})
	if !got.Identical {
		t.Fatalf("mutating the caller's slice changed the retained frame: %v", got)
	}

	cur := d.Cur()
	cur.Rows[0] = "MUTATED"
	if d.Cur().Rows[0] != "a" {
		t.Error("Cur hands out the tracker's own slice")
	}
	prev := d.Prev()
	prev.Rows[0] = "MUTATED"
	if d.Prev().Rows[0] != "a" {
		t.Error("Prev hands out the tracker's own slice")
	}
}

// TestOnlyTwoFramesAreEverRetained pins the memory shape: prev, cur, delta.
//
// A WRONG IMPLEMENTATION accumulating history — a ring of frames "in case a
// consumer wants it" — would grow per pane without bound and without anybody
// asking for it. Two frames answer "what just moved"; anything longer is the
// consumer's to keep from the events it was given.
func TestOnlyTwoFramesAreEverRetained(t *testing.T) {
	d := NewDiffs(Limits{})
	d.Observe([]string{"one"})
	d.Observe([]string{"two"})
	d.Observe([]string{"three"})

	if got := d.Cur().Rows[0]; got != "three" {
		t.Errorf("Cur is %q, want the third frame", got)
	}
	if got := d.Prev().Rows[0]; got != "two" {
		t.Errorf("Prev is %q, want the second frame — the first must be gone", got)
	}
	if got := d.Delta(); got.ChangedRows != 1 || got.Rows[0].Prev != "two" {
		t.Errorf("Delta is %v against %v, want the second-to-third change", got, got.Rows)
	}
}

// TestZeroLimitsTakeTheDefaults keeps the zero value usable.
//
// A WRONG IMPLEMENTATION honouring a zero cap literally would retain zero rows
// and zero runes, so every diff would be empty and every screen identical —
// for the caller who expressed no opinion, which is most of them.
func TestZeroLimitsTakeTheDefaults(t *testing.T) {
	d := NewDiffs(Limits{})
	if got, want := d.Limits(), DefaultLimits(); got != want {
		t.Fatalf("zero Limits normalised to %+v, want %+v", got, want)
	}
	partial := NewDiffs(Limits{MaxRows: 7})
	if got := partial.Limits(); got.MaxRows != 7 || got.MaxRowRunes != DefaultLimits().MaxRowRunes {
		t.Fatalf("a partly-specified Limits normalised to %+v", got)
	}
	if diff := d.Observe([]string{"content"}); diff.ChangedRows != 1 {
		t.Fatalf("a default tracker recorded %d changed rows for one row of content", diff.ChangedRows)
	}
}

// TestRowsFromTextIsTheInverseOfACapturedScreen covers the convenience.
//
// A WRONG IMPLEMENTATION using strings.Fields, or trimming, would silently
// drop the blank rows that make up most of a terminal — and the row INDICES in
// every RowChange would then refer to nothing a consumer could point at on
// screen.
func TestRowsFromTextIsTheInverseOfACapturedScreen(t *testing.T) {
	rows := RowsFromText("a\n\nb")
	if len(rows) != 3 || rows[0] != "a" || rows[1] != "" || rows[2] != "b" {
		t.Fatalf("RowsFromText gave %q", rows)
	}
	if RowsFromText("") != nil {
		t.Error("an empty capture produced a non-nil row slice")
	}
}
