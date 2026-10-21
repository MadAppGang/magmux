package obs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// NOW is the injected reference instant used by every time-derived expectation
// in this package, matching the instant the test data was built against. No
// test here reads a wall clock: a test that did would produce a different
// answer on every run and could only be written as an inequality, which proves
// nothing about the arithmetic.
var NOW = time.Unix(1789545200, 0).UTC()

// The single most important property of the model: a Session nobody has
// determined anything about says so in every field, and says it the same way.
//
// A wrong implementation makes the zero value a plausible LIVE session — idle,
// alive, zero subagents, zero errors, attention not needed — which is a
// complete and entirely fictional report, and one that no consumer can tell
// from an observed one.
func TestZeroSessionHasNoOpinionAboutAnything(t *testing.T) {
	var s Session

	if s.Status != StatusUnknown {
		t.Errorf("Status = %v, want StatusUnknown", s.Status)
	}
	if s.Waiting != WaitingReasonUnknown {
		t.Errorf("Waiting = %v, want WaitingReasonUnknown", s.Waiting)
	}
	if s.ProcessAlive != LivenessUnknown {
		t.Errorf("ProcessAlive = %v, want LivenessUnknown", s.ProcessAlive)
	}
	if s.PaneAlive != LivenessUnknown {
		t.Errorf("PaneAlive = %v, want LivenessUnknown", s.PaneAlive)
	}
	if s.SubagentCount != nil {
		t.Errorf("SubagentCount = %v, want nil (undetermined, not zero)", *s.SubagentCount)
	}
	if s.Color != nil {
		t.Errorf("Color = %q, want nil (no record, not the empty colour)", *s.Color)
	}
	if s.Attention.Needed != nil {
		t.Errorf("Attention.Needed = %v, want nil (undetermined, not false)", *s.Attention.Needed)
	}
	if s.Fault.APIErrors != nil {
		t.Errorf("Fault.APIErrors = %v, want nil (unexamined, not zero errors)", *s.Fault.APIErrors)
	}
	if s.StatusSince != nil {
		t.Errorf("StatusSince = %v, want nil", *s.StatusSince)
	}
	if _, ok := s.InStatusFor(NOW); ok {
		t.Error("InStatusFor reported a duration for a session with no status instant")
	}
}

// nil must reach the wire as null. This is the field the decision was made
// about by name: a UI that is handed false cannot tell "this session does not
// need you" from "nobody worked out whether it does", and it is the second
// case that loses a user an hour.
//
// The wrong implementation is a plain bool, which encoding/json renders as
// false with no way to say otherwise — so this test fails on the TYPE, not
// merely on a value.
func TestAttentionNeededMarshalsAsNullNeverFalse(t *testing.T) {
	cases := []struct {
		name string
		att  Attention
		want string
	}{
		{"undetermined", Attention{}, `{"needed":null}`},
		{"determined no", Attention{Needed: Ptr(false)}, `{"needed":false}`},
		{"determined yes", Attention{Needed: Ptr(true)}, `{"needed":true}`},
		{
			"undetermined with a reason",
			Attention{Reason: "no transcript was found"},
			`{"needed":null,"reason":"no transcript was found"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.att)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != c.want {
				t.Errorf("Marshal = %s, want %s", b, c.want)
			}
		})
	}
}

// Every undetermined value in a zero Session reaches the wire as null, not as
// a zero, an empty string or a false. Written as one assertion over the whole
// document because the failure it guards against is a single field being
// added later without a pointer.
func TestZeroSessionMarshalsEveryUndeterminedFieldAsNull(t *testing.T) {
	b, err := json.Marshal(Session{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		`"status":null`,
		`"statusSince":null`,
		`"waitingOn":null`,
		`"processAlive":null`,
		`"paneAlive":null`,
		`"subagentCount":null`,
		`"color":null`,
		`"paneId":null`,
		`"needed":null`,
		`"apiErrors":null`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("zero Session marshalled without %s:\n%s", want, b)
		}
	}
}

// A determined zero and an undetermined value are different bytes. This is the
// distinction that AC4 makes a pass condition, and it cannot be expressed by a
// number alone.
func TestDeterminedZeroIsNotUndetermined(t *testing.T) {
	determined, err := json.Marshal(Session{SubagentCount: Ptr(0), Fault: Fault{APIErrors: Ptr(0)}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	undetermined, err := json.Marshal(Session{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(determined) == string(undetermined) {
		t.Fatalf("a determined zero and an undetermined value marshalled identically:\n%s", determined)
	}
	if !strings.Contains(string(determined), `"subagentCount":0`) {
		t.Errorf("determined zero subagent count did not marshal as 0:\n%s", determined)
	}
	if !strings.Contains(string(determined), `"apiErrors":0`) {
		t.Errorf("determined zero errors did not marshal as 0:\n%s", determined)
	}
}

// Fault sits beside Status, not inside it. A session that has errored still
// reports what it is doing, and a session with no faults is not thereby idle.
//
// The wrong implementation folds the two together — a StatusError value, or a
// bool that makes any faulted session "not ok" — and then cannot answer the
// question a user actually has, which is whether the thing that errored is
// still working.
func TestFaultIsOrthogonalToStatus(t *testing.T) {
	s := Session{
		Status: StatusWaiting,
		Fault:  Fault{APIErrors: Ptr(3), Detail: "rate limited"},
	}
	if s.Status != StatusWaiting {
		t.Errorf("Status = %v, want a faulted session to keep its status", s.Status)
	}
	if s.Fault.APIErrors == nil || *s.Fault.APIErrors != 3 {
		t.Errorf("Fault.APIErrors = %v, want 3", s.Fault.APIErrors)
	}

	healthy := Session{Status: StatusBusy, Fault: Fault{APIErrors: Ptr(0)}}
	if healthy.Status != StatusBusy {
		t.Errorf("Status = %v, want a fault-free session to keep its status", healthy.Status)
	}
}

// The duration is computed from the reader's clock, never stored. A stored
// duration is wrong the moment after it is written.
//
// The second case is the one worth having: an instant read as MILLISECONDS
// when the source is seconds gives a duration of about 56,000 years, and an
// implementation that stored a duration instead would have baked that in
// before anyone could see it.
func TestInStatusForIsComputedFromTheReadersClock(t *testing.T) {
	cases := []struct {
		name  string
		since time.Time
		want  time.Duration
	}{
		{"two hours and a bit", time.Unix(1789536692, 0), 8508 * time.Second},
		{"one turn ago", time.Unix(1789545137, 0), 63 * time.Second},
		{"three days", time.Unix(1789286000, 0), 259200 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Session{StatusSince: Ptr(c.since)}
			got, ok := s.InStatusFor(NOW)
			if !ok {
				t.Fatal("InStatusFor reported no opinion for a determined instant")
			}
			if got != c.want {
				t.Errorf("InStatusFor(NOW) = %v, want %v", got, c.want)
			}
		})
	}
}

// A long time in one status is a FACT, not an anomaly, and the model has
// nowhere to record a correction even if someone wanted to apply one. Three
// days idle means the session has been idle for three days.
func TestLongDurationsAreReportedNotFlagged(t *testing.T) {
	s := Session{Status: StatusIdle, StatusSince: Ptr(time.Unix(1789286000, 0))}
	got, ok := s.InStatusFor(NOW)
	if !ok {
		t.Fatal("InStatusFor reported no opinion")
	}
	if got != 72*time.Hour {
		t.Errorf("InStatusFor(NOW) = %v, want 72h", got)
	}
	if s.Status != StatusIdle {
		t.Errorf("Status = %v, want the status left alone", s.Status)
	}
}

// A clock behind the instant it was handed gives a negative duration, and that
// is reported rather than clamped. Clamping would hide the only symptom of a
// misread unit or a skewed clock.
func TestAClockBehindTheInstantReportsANegativeDuration(t *testing.T) {
	s := Session{StatusSince: Ptr(NOW.Add(time.Minute))}
	got, ok := s.InStatusFor(NOW)
	if !ok {
		t.Fatal("InStatusFor reported no opinion")
	}
	if got != -time.Minute {
		t.Errorf("InStatusFor(NOW) = %v, want -1m", got)
	}
}

// Clone must share nothing. This is the publish boundary: a document is copied
// once on the way out and then read by something holding no lock, so a pointer
// still shared with the original is a race that only shows up under load.
func TestCloneSharesNothingWithTheOriginal(t *testing.T) {
	since := time.Unix(1789536692, 0)
	orig := Session{
		ID:            "c874de20",
		PID:           98198,
		PaneID:        Ptr(3),
		PreviousNames: []string{"one", "two"},
		StatusSince:   Ptr(since),
		SubagentCount: Ptr(23),
		Color:         Ptr("cyan"),
		Attention:     Attention{Needed: Ptr(true)},
		Fault:         Fault{APIErrors: Ptr(3)},
	}
	c := orig.Clone()

	*c.PaneID = 99
	c.PreviousNames[0] = "mutated"
	*c.StatusSince = NOW
	*c.SubagentCount = 0
	*c.Color = "red"
	*c.Attention.Needed = false
	*c.Fault.APIErrors = 0

	if *orig.PaneID != 3 {
		t.Errorf("PaneID leaked: original is now %d", *orig.PaneID)
	}
	if orig.PreviousNames[0] != "one" {
		t.Errorf("PreviousNames leaked: original is now %q", orig.PreviousNames[0])
	}
	if !orig.StatusSince.Equal(since) {
		t.Errorf("StatusSince leaked: original is now %v", *orig.StatusSince)
	}
	if *orig.SubagentCount != 23 {
		t.Errorf("SubagentCount leaked: original is now %d", *orig.SubagentCount)
	}
	if *orig.Color != "cyan" {
		t.Errorf("Color leaked: original is now %q", *orig.Color)
	}
	if *orig.Attention.Needed != true {
		t.Errorf("Attention.Needed leaked: original is now %v", *orig.Attention.Needed)
	}
	if *orig.Fault.APIErrors != 3 {
		t.Errorf("Fault.APIErrors leaked: original is now %d", *orig.Fault.APIErrors)
	}
}

// PreviousNameDetail is a slice of structs that each carry a pointer, so a
// shallow slices.Clone would pass the element test in
// TestCloneSharesNothingWithTheOriginal's style and still share every Until
// with the original. Fix round 2 added the field; this pins its copy.
func TestCloneSharesNoPreviousNameDetail(t *testing.T) {
	until := time.UnixMilli(1789745531896).UTC()
	orig := Session{PreviousNameDetail: []PreviousName{{Name: "one", Until: Ptr(until), Session: "s"}}}
	c := orig.Clone()

	c.PreviousNameDetail[0].Name = "mutated"
	*c.PreviousNameDetail[0].Until = NOW

	if orig.PreviousNameDetail[0].Name != "one" {
		t.Errorf("PreviousNameDetail's slice is shared: original name is now %q", orig.PreviousNameDetail[0].Name)
	}
	if !orig.PreviousNameDetail[0].Until.Equal(until) {
		t.Errorf("PreviousNameDetail's Until is shared: original is now %v", *orig.PreviousNameDetail[0].Until)
	}
	if (Session{}).Clone().PreviousNameDetail != nil {
		t.Error("Clone of a zero Session invented PreviousNameDetail")
	}
}

// Cloning a session that has determined nothing must not invent anything: a
// nil stays nil rather than becoming a pointer to a zero.
func TestCloningAnEmptySessionKeepsItEmpty(t *testing.T) {
	c := Session{}.Clone()
	if c.PaneID != nil || c.StatusSince != nil || c.SubagentCount != nil ||
		c.Color != nil || c.Attention.Needed != nil || c.Fault.APIErrors != nil {
		t.Errorf("Clone of a zero Session invented a value: %+v", c)
	}
	if c.PreviousNames != nil {
		t.Errorf("Clone of a zero Session invented PreviousNames: %v", c.PreviousNames)
	}
}

// One document, one epoch. A per-session timestamp cannot tell a consumer
// whether two sessions were seen together or an hour apart, and these records
// change often enough — a sixth of them inside five minutes — that the
// question is a real one.
func TestFleetCarriesOneEpochForTheWholeDocument(t *testing.T) {
	f := Fleet{
		Epoch: NOW,
		Sessions: []Session{
			{PID: 98198, StatusSince: Ptr(time.Unix(1789536692, 0))},
			{PID: 98693, StatusSince: Ptr(time.Unix(1789545137, 0))},
		},
	}
	if !f.Epoch.Equal(NOW) {
		t.Errorf("Epoch = %v, want the injected instant", f.Epoch)
	}
	// Both sessions are measured against that one epoch, not against their
	// own.
	for _, s := range f.Sessions {
		if _, ok := s.InStatusFor(f.Epoch); !ok {
			t.Errorf("session %d had no duration against the document epoch", s.PID)
		}
	}

	later := Fleet{Epoch: NOW.Add(time.Minute)}
	if later.Epoch.Equal(f.Epoch) {
		t.Error("two documents taken at two instants carried the same epoch")
	}
}

// Deep-copying the document deep-copies its sessions. The freeze this is for
// is the whole document, not each session separately.
func TestFleetCloneIsDeep(t *testing.T) {
	f := Fleet{Epoch: NOW, Sessions: []Session{{PID: 1, SubagentCount: Ptr(2)}}}
	c := f.Clone()
	*c.Sessions[0].SubagentCount = 99
	c.Sessions[0].PID = 7

	if *f.Sessions[0].SubagentCount != 2 {
		t.Errorf("SubagentCount leaked through Fleet.Clone: original is now %d", *f.Sessions[0].SubagentCount)
	}
	if f.Sessions[0].PID != 1 {
		t.Errorf("PID leaked through Fleet.Clone: original is now %d", f.Sessions[0].PID)
	}
}

// Sort is deterministic and total, so two documents over the same sessions can
// be diffed. Go map iteration order is the wrong implementation here, and it
// makes every diff noise.
func TestSortIsDeterministic(t *testing.T) {
	build := func() Fleet {
		return Fleet{Sessions: []Session{
			{PID: 98693, ID: "b"},
			{PID: 1724, ID: "z"},
			{PID: 98693, ID: "a"},
			{PID: 15775, ID: "m"},
		}}
	}
	a, b := build(), build()
	b.Sessions[0], b.Sessions[3] = b.Sessions[3], b.Sessions[0]

	a.Sort()
	b.Sort()

	if len(a.Sessions) != len(b.Sessions) {
		t.Fatalf("lengths differ after Sort: %d vs %d", len(a.Sessions), len(b.Sessions))
	}
	for i := range a.Sessions {
		if a.Sessions[i].PID != b.Sessions[i].PID || a.Sessions[i].ID != b.Sessions[i].ID {
			t.Fatalf("Sort is not deterministic at %d: %v vs %v", i, a.Sessions[i], b.Sessions[i])
		}
	}
	want := []struct {
		pid int
		id  string
	}{{1724, "z"}, {15775, "m"}, {98693, "a"}, {98693, "b"}}
	for i, w := range want {
		if a.Sessions[i].PID != w.pid || a.Sessions[i].ID != w.id {
			t.Errorf("Sort[%d] = %d/%q, want %d/%q",
				i, a.Sessions[i].PID, a.Sessions[i].ID, w.pid, w.id)
		}
	}
}

// A document survives a round trip through its own JSON with every
// undetermined value still undetermined. The failure this catches is a null
// coming back as a determined zero.
func TestFleetRoundTripsWithoutInventingValues(t *testing.T) {
	want := Fleet{
		Epoch: NOW,
		Sessions: []Session{
			{
				ID: "c874de20", PID: 98198,
				Status: StatusIdle, StatusRaw: "?",
				StatusSince:   Ptr(time.Unix(1789536692, 0).UTC()),
				ProcessAlive:  LivenessYes,
				SubagentCount: Ptr(0),
			},
			{ID: "3b90f6cc", PID: 15775},
		},
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Fleet
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Epoch.Equal(want.Epoch) {
		t.Errorf("Epoch = %v, want %v", got.Epoch, want.Epoch)
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got.Sessions))
	}
	if got.Sessions[0].Status != StatusIdle {
		t.Errorf("Status = %v, want StatusIdle", got.Sessions[0].Status)
	}
	if got.Sessions[0].SubagentCount == nil || *got.Sessions[0].SubagentCount != 0 {
		t.Errorf("a determined zero came back as %v", got.Sessions[0].SubagentCount)
	}
	if !got.Sessions[0].StatusSince.Equal(*want.Sessions[0].StatusSince) {
		t.Errorf("StatusSince = %v, want %v", got.Sessions[0].StatusSince, want.Sessions[0].StatusSince)
	}
	if got.Sessions[1].SubagentCount != nil {
		t.Errorf("an undetermined count came back determined: %v", *got.Sessions[1].SubagentCount)
	}
	if got.Sessions[1].Status != StatusUnknown {
		t.Errorf("an undetermined status came back as %v", got.Sessions[1].Status)
	}
}

// Marshalling one document twice produces identical bytes. The document is
// meant to be diffed by a consumer, and an unstable encoding makes every diff
// noise even when nothing changed.
func TestMarshallingIsByteIdenticalAcrossRepeatedBuilds(t *testing.T) {
	f := Fleet{
		Epoch: NOW,
		Sessions: []Session{
			{ID: "a", PID: 2, Status: StatusBusy, Attention: Attention{Needed: Ptr(true)}},
			{ID: "b", PID: 1, Color: Ptr("cyan")},
		},
	}
	f.Sort()
	first, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for i := range 5 {
		again, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("build %d differed:\n%s\n%s", i, first, again)
		}
	}
}
