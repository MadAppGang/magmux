package model_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/model"
)

func TestEnumsSpellTheirLowerCaseWords(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{model.StatusUnknown.String(), "unknown"},
		{model.StatusIdle.String(), "idle"},
		{model.StatusBlocked.String(), "blocked"},
		{model.StatusFailed.String(), "failed"},
		{model.WaitingDialog.String(), "dialog"},
		{model.KindBackground.String(), "background"},
		{model.LivenessNo.String(), "no"},
		{model.ConfidenceAuthoritative.String(), "authoritative"},
		{model.EvidenceNoTranscript.String(), "no-transcript"},
		{model.ShellMarkRunning.String(), "running"},
		{model.SourceWatch.String(), "watch"},
		{model.SourceDegraded.String(), "degraded"},
		{model.TargetJobTranscript.String(), "job-transcript"},
		{model.ScopeHost.String(), "host"},
		{model.ProblemAmbiguousTranscript.String(), "ambiguous-transcript"},
		{model.ProblemHost.String(), "host"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// WRONG: an out-of-range value printing as "unknown". A corrupt value must be
// visible as a corrupt value.
func TestAnOutOfRangeEnumPrintsItsOrdinal(t *testing.T) {
	if got := model.Status(200).String(); got != "Status(200)" {
		t.Errorf("Status(200) = %q", got)
	}
	if got := model.ProblemKind(250).String(); got != "ProblemKind(250)" {
		t.Errorf("ProblemKind(250) = %q", got)
	}
}

func TestStatusPredicates(t *testing.T) {
	for s := model.StatusUnknown; s <= model.StatusFailed; s++ {
		wantWaiting := s == model.StatusWaiting || s == model.StatusBlocked
		wantWorking := s == model.StatusBusy || s == model.StatusWorking
		wantTerminal := s == model.StatusDone || s == model.StatusFailed
		if s.IsWaiting() != wantWaiting || s.IsWorking() != wantWorking || s.IsTerminal() != wantTerminal {
			t.Errorf("%s: waiting=%v working=%v terminal=%v", s, s.IsWaiting(), s.IsWorking(), s.IsTerminal())
		}
		if (model.Session{Status: s}).WaitingApplies() != wantWaiting {
			t.Errorf("%s: WaitingApplies disagrees with IsWaiting", s)
		}
	}
}

func TestKeySpellings(t *testing.T) {
	cases := []struct {
		k    model.Key
		want string
	}{
		{model.Key{SessionID: "abc", PID: 42}, "abc/pid:42"},
		{model.Key{SessionID: "abc", Job: "j1"}, "abc/job:j1"},
		{model.Key{Orphan: "1828.json"}, "orphan:1828.json"},
		{model.Key{Orphan: "jobs/deadbeef"}, "orphan:jobs/deadbeef"},
		{model.Key{}, "/pid:0"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("%+v.String() = %q, want %q", c.k, got, c.want)
		}
	}
}

func TestKeyCompareIsSessionIDThenPIDThenJobThenOrphan(t *testing.T) {
	ordered := []model.Key{
		{Orphan: "a"},
		{Orphan: "b"},
		{SessionID: "a", PID: 1},
		{SessionID: "a", PID: 2},
		{SessionID: "a", PID: 2, Job: "x"},
		{SessionID: "b"},
	}
	for i := range ordered {
		for j := range ordered {
			got := ordered[i].Compare(ordered[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("Compare(%v, %v) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

// fill sets every pointer, slice and map reachable in v to a non-nil value, so
// a Clone that forgets a field shows up as shared memory below.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fill(v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	case reflect.Slice:
		if v.Len() == 0 {
			v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		}
		for i := 0; i < v.Len(); i++ {
			fill(v.Index(i))
		}
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	}
}

// shared reports the path of the first pointer or slice backing array a and b
// have in common.
func shared(a, b reflect.Value, path string) string {
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return ""
		}
		if a.Pointer() == b.Pointer() {
			return path
		}
		return shared(a.Elem(), b.Elem(), path)
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if !a.Type().Field(i).IsExported() {
				continue
			}
			if p := shared(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name); p != "" {
				return p
			}
		}
	case reflect.Slice:
		if a.Len() > 0 && b.Len() > 0 && a.Pointer() == b.Pointer() {
			return path
		}
		for i := 0; i < a.Len() && i < b.Len(); i++ {
			if p := shared(a.Index(i), b.Index(i), path+"[]"); p != "" {
				return p
			}
		}
	}
	return ""
}

// WRONG: a Clone that copies the struct and forgets a pointer field added
// later. Every field is filled by reflection, so a new field is covered the
// moment it exists.
func TestSessionCloneSharesNothing(t *testing.T) {
	var s model.Session
	fill(reflect.ValueOf(&s).Elem())
	c := s.Clone()
	if p := shared(reflect.ValueOf(s), reflect.ValueOf(c), "Session"); p != "" {
		t.Fatalf("Clone shares %s with the original", p)
	}
	if !reflect.DeepEqual(s, c) {
		t.Fatal("Clone is not equal to the original")
	}
}

func TestFleetCloneSharesNothing(t *testing.T) {
	var f model.Fleet
	fill(reflect.ValueOf(&f).Elem())
	c := f.Clone()
	if p := shared(reflect.ValueOf(f), reflect.ValueOf(c), "Fleet"); p != "" {
		t.Fatalf("Clone shares %s with the original", p)
	}
	if !reflect.DeepEqual(f, c) {
		t.Fatal("Clone is not equal to the original")
	}
}

func TestFleetSortIsPIDThenKey(t *testing.T) {
	f := model.Fleet{Sessions: []model.Session{
		{PID: 2, Key: model.Key{SessionID: "a", PID: 2}},
		{PID: 0, Key: model.Key{Orphan: "jobs/z"}},
		{PID: 1, Key: model.Key{SessionID: "b", PID: 1}},
		{PID: 0, Key: model.Key{SessionID: "a", Job: "k"}},
		{PID: 0, Key: model.Key{SessionID: "a", Job: "j"}},
	}}
	f.Sort()
	var got []string
	for _, s := range f.Sessions {
		got = append(got, s.Key.String())
	}
	want := "orphan:jobs/z a/job:j a/job:k b/pid:1 a/pid:2"
	if strings.Join(got, " ") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}

func TestProblemErrorNamesWhatItKnows(t *testing.T) {
	k := model.Key{SessionID: "abc", PID: 7}
	p := model.Problem{
		Scope: model.ScopeRecord, Kind: model.ProblemImplausibleInstant,
		Path: "7.json", Key: &k, Field: "updatedAt", Detail: "outside 1e12..1e14",
	}
	want := "record: implausible-instant: abc/pid:7: 7.json: field updatedAt: outside 1e12..1e14"
	if got := p.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got := (model.Problem{Scope: model.ScopeSource, Kind: model.ProblemTorn}).Error(); got != "source: torn" {
		t.Errorf("minimal Error() = %q", got)
	}
}

func TestAttachToolResultPairsByIDThenByTheOpenCall(t *testing.T) {
	turns := []model.Turn{
		{Role: model.TurnAssistant, Tools: []model.ToolCall{model.NewToolCall("Bash", "{}", "t1")}},
		{Role: model.TurnAssistant, Tools: []model.ToolCall{model.NewToolCall("Edit", "{}", "t2")}},
	}
	if i := model.AttachToolResult(turns, "t1", "ok"); i != 0 || turns[0].Tools[0].Result != "ok" {
		t.Fatalf("by id: landed in %d, result %q", i, turns[0].Tools[0].Result)
	}
	if i := model.AttachToolResult(turns, "", "done"); i != 1 || turns[1].Tools[0].Result != "done" {
		t.Fatalf("id-less: landed in %d, result %q", i, turns[1].Tools[0].Result)
	}
	if i := model.AttachToolResult(turns, "nope", "x"); i != -1 {
		t.Fatalf("an unknown id landed in %d", i)
	}
}
