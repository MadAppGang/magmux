package claude

import (
	"errors"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// Every status value Claude writes decodes, out of the testdata rather than
// out of the constants next door. Re-expressed from magmux's
// TestDecodeStatusHandlesEveryValueInTheTestData. The plausible wrong answer is
// shell folded into idle: 117 of 1920 observations, frequent enough to ship.
func TestEveryRegistryStatusInTheCorpusDecodes(t *testing.T) {
	cases := []struct {
		file string
		want model.Status
	}{
		{"98198.json", model.StatusIdle},
		{"98693.json", model.StatusBusy},
		{"15775.json", model.StatusShell},
		{"1828.json", model.StatusShell},
		{"1724.json", model.StatusWaiting},
		{"60006.json", model.StatusWaiting},
		{"60003.json", model.StatusUnknown},
	}
	seen := map[model.Status]bool{}
	for _, c := range cases {
		r, _, err := mmRecord(t, c.file)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if r.Status != c.want {
			t.Errorf("%s: status %v (raw %q), want %v", c.file, r.Status, r.StatusRaw, c.want)
		}
		seen[r.Status] = true
	}
	if len(seen) != 5 {
		t.Errorf("the corpus decoded to %d distinct statuses, want idle, busy, shell, waiting and unknown", len(seen))
	}
}

// An unrecognised status is no opinion with the raw word kept, never a guess.
// "compacting" is chosen because a guessing decoder guesses busy (C4).
func TestAnUnrecognisedStatusIsUnknownWithTheRawKept(t *testing.T) {
	r, _, _ := mmRecord(t, "60003.json")
	if r.Status != model.StatusUnknown || r.StatusRaw == "" {
		t.Fatalf("status %v raw %q, want Unknown with the raw word", r.Status, r.StatusRaw)
	}
	r, _, _ = ttRecord(t, "unknown-status.json")
	if r.Status != model.StatusUnknown || r.StatusRaw != "reticulating" {
		t.Fatalf("tt's reticulating: %v %q", r.Status, r.StatusRaw)
	}
	r, _, _ = DecodeRecord("1.json", []byte(`{"pid":1,"sessionId":"s","status":"dialog open","waitingFor":"dialog open"}`))
	if r.Status != model.StatusUnknown || r.StatusRaw != ValueWaitingDialog {
		t.Errorf("dialog open as a status: %v %q, want Unknown with the raw kept", r.Status, r.StatusRaw)
	}
	r, _, _ = DecodeRecord("1.json", []byte(`{"pid":1,"sessionId":"s","status":"waiting","waitingFor":"dialog open"}`))
	if r.Waiting != model.WaitingDialog || r.WaitingRaw != ValueWaitingDialog {
		t.Errorf("dialog open as a waitingFor: %v %q, want Dialog with the raw kept", r.Waiting, r.WaitingRaw)
	}
}

// The two waiting reasons stay two values, and an absent one is no opinion:
// defaulting it to the common value is right 55 of 55 times live and wrong in
// exactly the case a user depends on.
func TestTheWaitingReasonsStayDistinctAndAbsenceIsNoOpinion(t *testing.T) {
	in, _, _ := mmRecord(t, "1724.json")
	perm, _, _ := mmRecord(t, "60002.json")
	none, _, _ := mmRecord(t, "60006.json")
	if in.Waiting != model.WaitingInput || perm.Waiting != model.WaitingPermission {
		t.Fatalf("input %v, permission %v", in.Waiting, perm.Waiting)
	}
	if none.Status != model.StatusWaiting || none.Waiting != model.WaitingUnknown || none.WaitingRaw != "" {
		t.Fatalf("absent waitingFor: status %v waiting %v raw %q", none.Status, none.Waiting, none.WaitingRaw)
	}
	idle, _, _ := DecodeRecord("2.json", []byte(`{"pid":2,"sessionId":"s","status":"idle","waitingFor":"input needed"}`))
	if idle.Waiting != model.WaitingUnknown || idle.WaitingRaw != ValueWaitingInput {
		t.Errorf("a reason on a non-waiting record is not a waiting reason, but the raw is kept: %v %q", idle.Waiting, idle.WaitingRaw)
	}
}

// Records are read BY NAME: a reordered record decodes to the same values.
func TestKeyOrderDoesNotAffectDecoding(t *testing.T) {
	shuffled, _, _ := mmRecord(t, "60008.json")
	canonical, _, _ := mmRecord(t, "1724.json")
	if shuffled.Status != canonical.Status || shuffled.WaitingRaw != canonical.WaitingRaw || shuffled.SessionID == "" {
		t.Fatalf("shuffled %+v, canonical %+v", shuffled, canonical)
	}
	tt, _, _ := ttRecord(t, "key-order-shuffled.json")
	if tt.PID != 1926 || tt.Status != model.StatusBusy || tt.SessionID == "" {
		t.Fatalf("tt's shuffled record: %+v", tt)
	}
}

// A torn read is ErrTorn, an empty file too, and so is any JSON that is not an
// object (C8): torn, not dead.
func TestATornRecordIsErrTornNotAnEmptySession(t *testing.T) {
	for _, name := range []string{"60004.json", "60005.json"} {
		if _, _, err := mmRecord(t, name); !errors.Is(err, ErrTorn) {
			t.Errorf("%s: err %v, want ErrTorn", name, err)
		}
	}
	if _, _, err := ttRecord(t, "torn.json"); !errors.Is(err, ErrTorn) {
		t.Errorf("tt's torn.json: %v", err)
	}
	// Trailing bytes after the object are torn too, WHATEVER they are: a closing
	// brace or bracket is the shape an in-place rewrite that shrank a record
	// leaves behind, and json.Decoder.More reports no further element before
	// one, so a More-only check accepted `{...} }` as a whole record.
	for _, b := range []string{`null`, `[]`, `"x"`, `42`, `{"pid":1}{"pid":2}`, `{"pid":1} trailing`,
		`{"pid":1} }`, `{"pid":1}]`, `{"pid":1} ,`, "{\"pid\":1}\n}\n"} {
		if _, _, err := DecodeRecord("1.json", []byte(b)); !errors.Is(err, ErrTorn) {
			t.Errorf("%s: err %v, want ErrTorn", b, err)
		}
	}
}

// D11: a record missing its pid takes the filename's, and one missing its id is
// KEPT with a Problem, never dropped and never given an invented id.
func TestAMissingPIDOrIDIsKeptWithAProblem(t *testing.T) {
	r, ps, err := DecodeRecord("4242.json", []byte(`{"sessionId":"s","status":"idle"}`))
	if err != nil || r.PID != 4242 || !r.PIDFromName || r.NamePID != 4242 {
		t.Fatalf("no body pid: %+v %v", r, err)
	}
	if !hasProblem(ps, model.ProblemMissingField, KeyPID) {
		t.Errorf("no ProblemMissingField for the pid: %v", ps)
	}
	r, ps, _ = ttRecord(t, "no-session-id.json")
	if r.SessionID != "" || r.PID != 2196 || !hasProblem(ps, model.ProblemMissingField, KeySessionID) {
		t.Fatalf("tt's no-session-id: %+v %v", r, ps)
	}
	r, ps, _ = DecodeRecord("orphan.json", []byte(`{"status":"idle"}`))
	if r.PID != 0 || r.SessionID != "" || !hasProblem(ps, model.ProblemMissingField, KeyPID) {
		t.Fatalf("neither: %+v %v", r, ps)
	}
}

// The body wins over the filename, and the disagreement is reported.
func TestABodyPIDThatDisagreesWithTheFilenameWinsAndIsReported(t *testing.T) {
	r, ps, _ := mmRecord(t, "60011.json")
	if r.PID != 60099 || r.NamePID != 60011 || r.PIDFromName {
		t.Fatalf("%+v", r)
	}
	if !hasProblem(ps, model.ProblemPIDMismatch, KeyPID) {
		t.Fatalf("no ProblemPIDMismatch: %v", ps)
	}
}

// A value of the wrong JSON type is ProblemUndecoded on that key, and the field
// is left zero (§2.2's controller mapping reads exactly this).
func TestAWrongTypedValueIsUndecodedAndLeftZero(t *testing.T) {
	r, ps, err := DecodeRecord("7.json", []byte(`{"pid":7,"sessionId":42,"status":["busy"],"waitingFor":{},"name":"n"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{KeySessionID, KeyStatus, KeyWaitingFor} {
		if !hasProblem(ps, model.ProblemUndecoded, f) {
			t.Errorf("no ProblemUndecoded on %s: %v", f, ps)
		}
	}
	if r.SessionID != "" || r.StatusRaw != "" || r.WaitingRaw != "" || r.Name != "n" {
		t.Errorf("the bad fields were not left zero, or a good one was lost: %+v", r)
	}
	if hasProblem(ps, model.ProblemMissingField, KeySessionID) {
		t.Error("a wrong-typed id is undecoded, not missing")
	}
}

// Every registry instant is epoch MILLISECONDS; a seconds-shaped one is refused
// with a Problem naming the key, never rescaled (D4, C4).
func TestRegistryInstantsAreMillisecondsAndASecondsValueIsRefused(t *testing.T) {
	r, ps, _ := mmRecord(t, "98198.json")
	want := time.UnixMilli(1789536692000).UTC()
	if r.StatusUpdatedAt == nil || !r.StatusUpdatedAt.Equal(want) || r.UpdatedAt == nil || len(ps) != 0 {
		t.Fatalf("statusUpdatedAt %v updatedAt %v problems %v", r.StatusUpdatedAt, r.UpdatedAt, ps)
	}
	r, ps, _ = DecodeRecord("9.json", []byte(`{"pid":9,"sessionId":"s","statusUpdatedAt":1789536692,"startedAt":0}`))
	if r.StatusUpdatedAt != nil || r.StartedAt != nil {
		t.Fatalf("a seconds value or a zero produced an instant: %v %v", r.StatusUpdatedAt, r.StartedAt)
	}
	if !hasProblem(ps, model.ProblemImplausibleInstant, KeyStatusUpdatedAt) || hasProblem(ps, model.ProblemImplausibleInstant, KeyStartedAt) {
		t.Fatalf("want one implausible-instant problem on statusUpdatedAt and none for a zero: %v", ps)
	}
}

// Former names in both shapes, in order; one bad element costs only itself.
func TestFormerNamesInBothShapesKeepTheirOrder(t *testing.T) {
	r, _, _ := mmRecord(t, "45682.json")
	if len(r.FormerNames) != 2 || r.FormerNames[0].Name != "magmux-release" || r.FormerNames[1].Name != "release-prep" {
		t.Fatalf("strings: %+v", r.FormerNames)
	}
	r, _, _ = mmRecord(t, "76136.json")
	if len(r.FormerNames) != 1 || r.FormerNames[0].Name != "agent-fleet-sessions" ||
		r.FormerNames[0].Until == nil || r.FormerNames[0].Until.UnixMilli() != 1789745531896 ||
		r.FormerNames[0].SessionID != r.SessionID {
		t.Fatalf("objects: %+v", r.FormerNames)
	}
	r, ps, _ := DecodeRecord("3.json", []byte(`{"pid":3,"sessionId":"s","formerNames":["a",7,{"until":1},{"name":"b","until":12},{"name":"c","sessionId":5},{"name":"d"}]}`))
	var got []string
	for _, p := range r.FormerNames {
		got = append(got, p.Name)
	}
	if len(got) != 4 || got[0] != "a" || got[1] != "b" || got[2] != "c" || got[3] != "d" {
		t.Fatalf("names %v", got)
	}
	if r.FormerNames[1].Until != nil || r.FormerNames[2].SessionID != "" {
		t.Errorf("a bad until or session id survived: %+v", r.FormerNames)
	}
	if len(ps) != 4 {
		t.Errorf("want 4 problems (a number, a nameless object, a bad until, a bad id), got %v", ps)
	}
}

// The tmux value keeps its sigils, a colon in the session name is safe, and an
// unparseable value keeps only its raw form.
func TestTheTmuxValueIsParsedWithItsSigils(t *testing.T) {
	r, _, _ := mmRecord(t, "98198.json")
	if r.Tmux == nil || *r.Tmux != (model.TmuxRef{Raw: "devmx:@191.%359", Session: "devmx", Window: "@191", Pane: "%359"}) {
		t.Fatalf("%+v", r.Tmux)
	}
	r, _, _ = mmRecord(t, "60009.json")
	if r.Tmux == nil || *r.Tmux != (model.TmuxRef{Raw: "devmx"}) {
		t.Fatalf("unparseable: %+v", r.Tmux)
	}
	if got := ParseTmux("a:b:c:@1.%2"); got.Session != "a:b:c" || got.Window != "@1" || got.Pane != "%2" {
		t.Fatalf("colon-safe: %+v", got)
	}
	r, _, _ = mmRecord(t, "96632.json")
	if r.Tmux != nil {
		t.Fatalf("no tmux key: %+v", r.Tmux)
	}
}

// A future record with keys nobody named decodes exactly as today's.
func TestUnknownKeysAreIgnored(t *testing.T) {
	r, ps, err := mmRecord(t, "60007.json")
	if err != nil || len(ps) != 0 || r.Status != model.StatusIdle || r.PeerProtocol == nil || *r.PeerProtocol != 2 {
		t.Fatalf("%+v %v %v", r, ps, err)
	}
}

// The background process's own record: kind bg and the job it serves.
func TestABackgroundProcessRecordCarriesItsKindAndJob(t *testing.T) {
	r, _, _ := ttRecord(t, "background-process.json")
	if r.Kind != model.KindBackground || r.KindRaw != ValueKindBG || r.JobID != "857ef435" || r.Tmux != nil {
		t.Fatalf("%+v", r)
	}
	if r.MessagingSocketPath == nil || r.Version != "2.1.278" || r.StartedAt == nil {
		t.Fatalf("%+v", r)
	}
	r, _, _ = mmRecord(t, "98198.json")
	if r.Kind != model.KindInteractive || r.KindRaw != "" {
		t.Fatalf("no kind key is interactive: %v %q", r.Kind, r.KindRaw)
	}
	r, _, _ = DecodeRecord("5.json", []byte(`{"pid":5,"sessionId":"s","kind":"daemon"}`))
	if r.Kind != model.KindUnknown || r.KindRaw != "daemon" {
		t.Fatalf("an unknown kind: %v %q", r.Kind, r.KindRaw)
	}
}

func TestRecordNamesAndFilenamePIDs(t *testing.T) {
	for name, want := range map[string]bool{
		"98198.json": true, "live-idle.json": true, ".json": false, "98198.b94d.key": false,
		"LIVE-PIDS.txt": false, "a/b.json": false,
	} {
		if got := IsRecordName(name); got != want {
			t.Errorf("IsRecordName(%q) = %v", name, got)
		}
	}
	for name, want := range map[string]int{"98198.json": 98198, "0.json": 0, "-1.json": 0, "007.json": 0, "x.json": 0, "98198.key": 0} {
		got, ok := PIDFromName(name)
		if got != want || ok != (want > 0) {
			t.Errorf("PIDFromName(%q) = %d, %v", name, got, ok)
		}
	}
}

func TestLooksLikePermissionIsAContainsTest(t *testing.T) {
	for raw, want := range map[string]bool{
		ValueWaitingPermission: true, "needs permission to run": true, ValueWaitingInput: false, "": false,
	} {
		if got := LooksLikePermission(raw); got != want {
			t.Errorf("LooksLikePermission(%q) = %v", raw, got)
		}
	}
}

func TestTopLevelKeysListsWhatIsThere(t *testing.T) {
	keys, ok := TopLevelKeys(readCorpus(t, "roots", "magmux-corpus", "sessions", "60007.json"))
	if !ok || len(keys) != 15 || keys[0] != "contextWindowPct" {
		t.Fatalf("%v %v", keys, ok)
	}
	if _, ok := TopLevelKeys([]byte(`[1]`)); ok {
		t.Fatal("a list is not an object")
	}
}
