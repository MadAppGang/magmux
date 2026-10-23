package claudecode

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
)

func line(s string) string { return s + "\n" }

func build(t *testing.T, fsys fs.FS, dir string, tail, timing int) *Stage {
	t.Helper()
	return Factory(Config{Projects: ports.Tree{Path: "/p", Dir: dir, FS: fsys}, TailBytes: tail, TimingTailBytes: timing})(model.Key{SessionID: "s"}).(*Stage)
}

var located = model.Location{Path: "p/s.jsonl", Matches: 1}

// Targets names the transcript as a FILE, by kind, and nothing for an
// unlocated or ambiguous session; subagents/ is never named (O9, C11).
func TestTargetsAreFilesOnly(t *testing.T) {
	st := build(t, fstest.MapFS{}, "/root/projects", 0, 0)
	got := st.Targets(model.Session{}, located)
	if len(got) != 1 || got[0].Dir || got[0].Kind != model.TargetTranscript || got[0].Path != "/root/projects/p/s.jsonl" {
		t.Fatalf("%+v", got)
	}
	got = st.Targets(model.Session{Job: &model.JobFacts{}}, located)
	if got[0].Kind != model.TargetJobTranscript {
		t.Fatalf("a job's transcript: %+v", got)
	}
	for _, loc := range []model.Location{{}, {Matches: 2}} {
		if got := st.Targets(model.Session{}, loc); len(got) != 0 {
			t.Fatalf("%+v: %+v", loc, got)
		}
	}
}

// C17: a located transcript is read only under the grant, and refused with
// ProblemNotGranted otherwise, leaving the facts nil.
func TestTheReadPermitIsEnforced(t *testing.T) {
	fsys := fstest.MapFS{"p/s.jsonl": {Data: []byte(line(`{"` + claude.KeyRecordType + `":"` + claude.ValueRecordAgentColor + `","` + claude.KeyAgentColor + `":"teal"}`))}}
	st := build(t, fsys, "/root/projects", 0, 0)
	targets := st.Targets(model.Session{}, located)
	st.Granted(stage.Grant{})
	s := st.Observe(model.Session{Key: model.Key{SessionID: "s"}})
	ps := st.Problems()
	if s.Transcript == nil || s.Transcript.Color != nil || s.Transcript.APIErrors != nil || st.Reads() != 0 ||
		len(ps) != 1 || ps[0].Kind != model.ProblemNotGranted || ps[0].Key == nil {
		t.Fatalf("%+v %v reads %d", s.Transcript, ps, st.Reads())
	}
	for _, g := range []stage.Grant{{Armed: targets}, {Pull: targets}, {Unwatched: true}} {
		st.Targets(model.Session{}, located)
		st.Granted(g)
		s := st.Observe(model.Session{})
		if s.Transcript == nil || s.Transcript.Color == nil || *s.Transcript.Color != "teal" {
			t.Fatalf("%+v: %+v", g, s.Transcript)
		}
	}
	if st.Reads() != 3 {
		t.Fatalf("%d reads for three cycles", st.Reads())
	}
}

// C20: ONE read serves both windows, and each window is disclosed against the
// file's size (D10).
func TestOneReadServesBothWindows(t *testing.T) {
	pad := strings.Repeat(line(`{"`+claude.KeyRecordType+`":"`+claude.ValueRecordAssistant+`","`+claude.KeyIsAPIErrorMessage+`":true}`), 40)
	tail := line(`{"` + claude.KeyRecordType + `":"` + claude.ValueRecordUser + `","` + claude.KeyTimestamp + `":"2026-09-18T02:00:00Z"}`)
	data := pad + tail
	st := build(t, fstest.MapFS{"p/s.jsonl": {Data: []byte(data)}}, "", 1000, 200)
	st.Targets(model.Session{}, located)
	st.Granted(stage.Grant{Unwatched: true})
	s := st.Observe(model.Session{})
	tr := s.Transcript
	if st.Reads() != 1 {
		t.Fatalf("%d reads", st.Reads())
	}
	if tr.Window != (model.Window{TailBytes: 1000, FileSize: int64(len(data)), Partial: true}) ||
		tr.TimingWindow != (model.Window{TailBytes: 200, FileSize: int64(len(data)), Partial: true}) {
		t.Fatalf("windows %+v %+v", tr.Window, tr.TimingWindow)
	}
	if tr.APIErrors == nil || *tr.APIErrors == 0 || *tr.APIErrors >= 40 || tr.LastActivityAt == nil {
		t.Fatalf("the facts window is a tail of the file: %v %v", tr.APIErrors, tr.LastActivityAt)
	}
	if s.Fault.APIErrors == nil || *s.Fault.APIErrors != *tr.APIErrors || s.Fault.Window != tr.Window {
		t.Fatalf("fault %+v", s.Fault)
	}
	if tr.Subagents == nil || *tr.Subagents != 0 || tr.SubagentEvidence != model.EvidenceAbsent {
		t.Fatalf("D1: %v %v", tr.Subagents, tr.SubagentEvidence)
	}
}

// C6: an ambiguous location opens nothing and says so in the evidence.
func TestAnAmbiguousLocationOpensNothing(t *testing.T) {
	st := build(t, fstest.MapFS{}, "", 0, 0)
	st.Targets(model.Session{}, model.Location{Matches: 2})
	st.Granted(stage.Grant{Unwatched: true})
	s := st.Observe(model.Session{})
	if s.Transcript == nil || s.Transcript.SubagentEvidence != model.EvidenceAmbiguous || s.Transcript.Color != nil || st.Reads() != 0 {
		t.Fatalf("%+v", s.Transcript)
	}
	st.Targets(model.Session{}, model.Location{})
	if s := st.Observe(model.Session{}); s.Transcript != nil {
		t.Fatalf("not located: %+v", s.Transcript)
	}
	if s := st.Observe(model.Session{}); s.Transcript != nil || st.Reads() != 0 {
		t.Fatal("an observation without a prepare reads nothing")
	}
}

// C4: implausible instants in the window become ONE Problem per key per cycle.
func TestImplausibleInstantsAreOneProblemPerCycle(t *testing.T) {
	bad := line(`{"` + claude.KeyRecordType + `":"` + claude.ValueRecordUser + `","` + claude.KeyTimestamp + `":"1970-01-02T00:00:00Z"}`)
	st := build(t, fstest.MapFS{"p/s.jsonl": {Data: []byte(bad + bad)}}, "", 0, 0)
	st.Targets(model.Session{}, located)
	st.Granted(stage.Grant{Unwatched: true})
	st.Observe(model.Session{Key: model.Key{SessionID: "s"}})
	ps := st.Problems()
	if len(ps) != 1 || ps[0].Kind != model.ProblemImplausibleInstant || ps[0].Field != claude.KeyTimestamp {
		t.Fatalf("%v", ps)
	}
	if len(st.Problems()) != 0 {
		t.Fatal("Problems drains")
	}
}

// A wake re-reads only what changed. A transcript whose size and mtime are
// what they were at the last read is not read or decoded again: its facts are
// the ones that read produced (ADR-D7: mtime and size are cache keys only).
// A change to either, or a different location, is read; a zero mtime is never
// a key, because it says nothing, and nor is an mtime too close to Now to be
// trusted (review r1 #4a).
func TestAnUnchangedTranscriptIsNotReadAgain(t *testing.T) {
	colour := func(c string) string {
		return line(`{"` + claude.KeyRecordType + `":"` + claude.ValueRecordAgentColor + `","` + claude.KeyAgentColor + `":"` + c + `"}`)
	}
	at := time.Date(2026, 9, 18, 2, 0, 0, 0, time.UTC)
	fsys := fstest.MapFS{
		"p/s.jsonl": {Data: []byte(colour("teal")), ModTime: at},
		"q/s.jsonl": {Data: []byte(colour("teal")), ModTime: at},
	}
	now := at.Add(time.Hour)
	st := Factory(Config{Projects: ports.Tree{Path: "/p", FS: fsys}, Now: func() time.Time { return now }})(model.Key{SessionID: "s"}).(*Stage)
	observe := func(loc model.Location) model.Session {
		t.Helper()
		st.Targets(model.Session{}, loc)
		st.Granted(stage.Grant{Unwatched: true})
		s := st.Observe(model.Session{})
		if s.Transcript == nil || s.Transcript.Color == nil {
			t.Fatalf("no facts: %+v", s.Transcript)
		}
		return s
	}
	observe(located)
	if s := observe(located); st.Reads() != 1 || *s.Transcript.Color != "teal" || s.Transcript.Subagents == nil {
		t.Fatalf("an unchanged transcript: %d reads, %+v", st.Reads(), s.Transcript)
	}
	// Same size, new mtime: an in-place rewrite.
	fsys["p/s.jsonl"] = &fstest.MapFile{Data: []byte(colour("ruby")), ModTime: at.Add(time.Second)}
	if s := observe(located); st.Reads() != 2 || *s.Transcript.Color != "ruby" {
		t.Fatalf("a rewritten transcript: %d reads, colour %s", st.Reads(), *s.Transcript.Color)
	}
	// Same mtime, new size: an append.
	fsys["p/s.jsonl"] = &fstest.MapFile{Data: []byte(colour("ruby") + colour("jade")), ModTime: at.Add(time.Second)}
	if s := observe(located); st.Reads() != 3 || *s.Transcript.Color != "jade" {
		t.Fatalf("an appended transcript: %d reads, colour %s", st.Reads(), *s.Transcript.Color)
	}
	// A different location with the same size and mtime is a different file.
	if observe(model.Location{Path: "q/s.jsonl", Matches: 1}); st.Reads() != 4 {
		t.Fatalf("a new location was not read: %d reads", st.Reads())
	}
	// A zero mtime is never a key.
	fsys["q/s.jsonl"] = &fstest.MapFile{Data: []byte(colour("teal"))}
	loc := model.Location{Path: "q/s.jsonl", Matches: 1}
	observe(loc)
	observe(loc)
	if st.Reads() != 6 {
		t.Fatalf("a file with no mtime was cached: %d reads", st.Reads())
	}
	// The racy-file rule: an mtime this close to Now may share its tick with a
	// later same-size rewrite, so it is not a key.
	fsys["q/s.jsonl"] = &fstest.MapFile{Data: []byte(colour("teal")), ModTime: now.Add(-time.Second)}
	observe(loc)
	observe(loc)
	if st.Reads() != 8 {
		t.Fatalf("a racy mtime was cached: %d reads", st.Reads())
	}
	// And no Now, no key.
	noNow := build(t, fstest.MapFS{"p/s.jsonl": {Data: []byte(colour("teal")), ModTime: at}}, "", 0, 0)
	for range 2 {
		noNow.Targets(model.Session{}, located)
		noNow.Granted(stage.Grant{Unwatched: true})
		noNow.Observe(model.Session{})
	}
	if noNow.Reads() != 2 {
		t.Fatalf("a stage with no Now cached: %d reads", noNow.Reads())
	}
}
