package source

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

var base = time.Date(2026, 9, 18, 2, 40, 11, 0, time.UTC)

func rec(pid int, id, name string) []byte {
	return []byte(fmt.Sprintf(`{%q:%d,%q:%q,%q:%q}`, claude.KeyPID, pid, claude.KeySessionID, id, claude.KeyName, name))
}

func has(ps []model.Problem, k model.ProblemKind) bool {
	for _, p := range ps {
		if p.Kind == k {
			return true
		}
	}
	return false
}

// C8: a torn record is retried once after the delay (on the injected clock),
// then its last good value is kept with a ProblemTorn; a record with no
// earlier value is absent, and the Problem says so. The real clock waits a
// millisecond here; the pipeline's tests drive the same path on a FakeClock.
func TestATornRecordIsRetriedOnceThenKeptOrAbsent(t *testing.T) {
	fsys := fstest.MapFS{"1.json": {Data: rec(1, "s", "good")}}
	r := NewRegistry(ports.Tree{Path: "/s", FS: fsys}, ports.RealClock(), time.Millisecond)
	if got := r.Read(); len(got.Records) != 1 || got.State != model.SourceOK {
		t.Fatalf("%+v", got)
	}
	fsys["1.json"] = &fstest.MapFile{Data: []byte(`{"pid":1,`)}
	fsys["2.json"] = &fstest.MapFile{Data: []byte(`{`)}
	got := r.Read()
	if len(got.Records) != 1 || got.Records[0].Name != "good" || got.State != model.SourceDegraded {
		t.Fatalf("%+v", got)
	}
	n := 0
	for _, p := range got.Problems {
		if p.Kind == model.ProblemTorn {
			n++
			if p.Path == "2.json" && !strings.Contains(p.Detail, "absent") {
				t.Errorf("a record with no earlier value says it is absent: %q", p.Detail)
			}
		}
	}
	if n != 2 {
		t.Fatalf("%v", got.Problems)
	}
	delete(fsys, "1.json")
	if got := r.Read(); len(got.Records) != 0 {
		t.Fatal("a removed file's last good value is forgotten")
	}
}

func TestAnOversizedRecordIsNeverReadWhole(t *testing.T) {
	big := make([]byte, claude.MaxRecordBytes+1)
	r := NewRegistry(ports.Tree{Path: "/s", FS: fstest.MapFS{"1.json": {Data: big}}}, ports.RealClock(), time.Millisecond)
	if got := r.Read(); got.State != model.SourceDegraded || !has(got.Problems, model.ProblemUnreadable) {
		t.Fatalf("%+v", got)
	}
}

func TestAnAbsentTreeIsOKAndAZeroTreeIsOff(t *testing.T) {
	if got := NewRegistry(ports.Tree{}, nil, 0).Read(); got.State != model.SourceOff {
		t.Fatalf("%+v", got)
	}
	if got := NewRegistry(ports.Tree{Path: "/x", FS: fstest.MapFS{"x": {Data: []byte("{}")}}}, nil, 0).Read(); got.State != model.SourceOK {
		t.Fatalf("%+v", got)
	}
	l := NewJobs(ports.Tree{Path: "/j", FS: fstest.MapFS{}}).List()
	if l.State != model.SourceOK {
		t.Fatalf("%+v", l)
	}
}

// C17 on the jobs tree: only permitted directories are read; the rest are
// refused with ProblemNotGranted, keeping any last good value. Root files and
// deeper state.json decoys are never records.
func TestJobsReadsOnlyWhatTheCyclePermits(t *testing.T) {
	job := func(id string) []byte {
		return []byte(fmt.Sprintf(`{%q:%q,%q:%q}`, claude.KeySessionID, id, claude.KeyState, claude.ValueStateWorking))
	}
	fsys := fstest.MapFS{
		"a/state.json":           {Data: job("a")},
		"b/state.json":           {Data: job("b")},
		"c/tmp/x/state.json":     {Data: job("decoy")},
		"pins.json":              {Data: []byte("[]")},
		"empty/" + "placeholder": {Data: nil},
	}
	j := NewJobs(ports.Tree{Path: "/j", FS: fsys})
	l := j.List()
	if strings.Join(l.Dirs, ",") != "a,b,c,empty" {
		t.Fatalf("%v", l.Dirs)
	}
	got := j.Read(l.Dirs, func(string) bool { return true })
	if len(got.Jobs) != 2 || got.State != model.SourceOK {
		t.Fatalf("%+v", got)
	}
	got = j.Read(l.Dirs, func(d string) bool { return d != "b" })
	if len(got.Jobs) != 2 || !has(got.Problems, model.ProblemNotGranted) || got.State != model.SourceDegraded {
		t.Fatalf("a refused directory keeps its last good value and is reported: %+v", got)
	}
}

// §3.3: the gates are timestamps. A second Begin while in flight joins; a
// success within MinInterval is skipped; failures back off and double.
func TestTheAgentsGatesAreTimestamps(t *testing.T) {
	a := NewAgents(nil)
	if a.On() || a.Report().State != model.SourceOff {
		t.Fatal("a nil port is off")
	}
	a = NewAgents(port{})
	if a.Report().State != model.SourceStateUnknown {
		t.Fatalf("%+v", a.Report())
	}
	now := base
	if start, _, _ := a.Begin(now); !start {
		t.Fatal("the first refresh starts")
	}
	if _, joined, _ := a.Begin(now); !joined {
		t.Fatal("a second request joins the one in flight")
	}
	if l, _, err := a.Finish(now, ports.Listing{Raw: []byte("[]"), At: now}, nil); err != nil || l == nil {
		t.Fatal(err)
	}
	if _, _, skip := a.Begin(now.Add(time.Second)); skip != SkippedMinInterval {
		t.Fatalf("%q", skip)
	}
	now = now.Add(time.Minute)
	a.Begin(now)
	if _, ps, err := a.Finish(now, ports.Listing{}, errors.New("down")); err == nil || !has(ps, model.ProblemAgentsListing) {
		t.Fatal("a failure is an error and a problem")
	}
	if a.Report().State != model.SourceDegraded || a.Listing() == nil {
		t.Fatalf("the older listing is still applied: %+v", a.Report())
	}
	if _, _, skip := a.Begin(now.Add(time.Second)); skip != SkippedBackoff {
		t.Fatalf("%q", skip)
	}
	if start, _, _ := a.Begin(now.Add(3 * time.Second)); !start {
		t.Fatal("after the 2 s backoff")
	}
	a.Finish(now.Add(3*time.Second), ports.Listing{Raw: []byte("nope")}, nil)
	if _, _, skip := a.Begin(now.Add(5 * time.Second)); skip != SkippedBackoff {
		t.Fatalf("a malformed listing is a failure too, and the backoff doubles: %q", skip)
	}
}

type port struct{}

func (port) List(context.Context) (ports.Listing, error) { return ports.Listing{}, nil }

// Tail is ONE read of the last n bytes, and the size it read at.
func TestTailIsOneBoundedRead(t *testing.T) {
	fsys := fstest.MapFS{"f": {Data: []byte("0123456789")}}
	b, size, err := Tail(fsys, "f", 4)
	if err != nil || string(b) != "6789" || size != 10 {
		t.Fatalf("%q %d %v", b, size, err)
	}
	b, _, _ = Tail(fsys, "f", 40)
	if string(b) != "0123456789" {
		t.Fatalf("%q", b)
	}
	if _, _, err := Tail(fsys, "missing", 4); err == nil {
		t.Fatal("a missing file is an error")
	}
}
