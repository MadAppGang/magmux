package registry_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/adapters"
	"github.com/MadAppGang/magmux/internal/obs/registry"
)

// Everything here reads internal/obs/testdata/sessions, which is READ-ONLY to
// the suite: no test in this file writes into it, no test touches ~/.claude,
// no test probes a live pid, and no test opens a .key file. The liveness
// answers come from LIVE-PIDS.txt through fileOracle below, because a pid in
// this data could collide with a real process on whatever machine is running
// the suite and the answer would then depend on what else is running.
//
// The one test that needs to MUTATE a record copies it into t.TempDir() first.

const testdataDir = "../testdata/sessions"

// NOW is the corpus's reference instant. PROVENANCE.md states it in epoch
// SECONDS; the records are in epoch MILLISECONDS, and confusing the two is the
// bug that was found and fixed mid-phase — see adapters.DecodeInstant.
var NOW = time.Unix(1789545200, 0).UTC()

// fileOracle answers liveness from LIVE-PIDS.txt.
//
// A pid NOT in the file is LivenessUnknown rather than LivenessNo, which
// matters: the file's own header says "nothing else is live", but a test
// asserting "dead" must name the pid it means, and an oracle that answered
// "no" for everything unlisted would make a typo in a test look like a
// correctly detected dead process.
type fileOracle struct {
	answers map[int]obs.Liveness
}

func loadOracle(t *testing.T) *fileOracle {
	t.Helper()
	f, err := os.Open(filepath.Join(testdataDir, "LIVE-PIDS.txt"))
	if err != nil {
		t.Fatalf("opening the liveness oracle: %v", err)
	}
	defer f.Close()

	o := &fileOracle{answers: map[int]obs.Liveness{}}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed oracle line %q", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("malformed pid in oracle line %q", line)
		}
		switch fields[1] {
		case "alive":
			o.answers[pid] = obs.LivenessYes
		case "dead":
			o.answers[pid] = obs.LivenessNo
		default:
			t.Fatalf("malformed answer in oracle line %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading the oracle: %v", err)
	}
	if len(o.answers) < 10 {
		t.Fatalf("the oracle holds only %d answers; it is not being read", len(o.answers))
	}
	return o
}

func (o *fileOracle) Alive(pid int) obs.Liveness { return o.answers[pid] }

// read builds a document from the test data with the oracle installed.
func read(t *testing.T) (obs.Fleet, []registry.Problem) {
	t.Helper()
	src, err := registry.New(registry.Config{
		Root:   testdataDir,
		Oracle: loadOracle(t),
		Now:    func() time.Time { return NOW },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return src.Read()
}

func session(t *testing.T, f obs.Fleet, pid int) obs.Session {
	t.Helper()
	for _, s := range f.Sessions {
		if s.PID == pid {
			return s
		}
	}
	t.Fatalf("no session with pid %d in the document; it has %d sessions", pid, len(f.Sessions))
	return obs.Session{}
}

func problemsFor(ps []registry.Problem, base string) []registry.Problem {
	var out []registry.Problem
	for _, p := range ps {
		if filepath.Base(p.Path) == base {
			out = append(out, p)
		}
	}
	return out
}

// WRONG: a source that reads no records at all, or one that reads the .key
// siblings and the oracle file as if they were records. Every other test here
// would be vacuous if this one were not true.
func TestTheDirectoryYieldsEveryReadableRecord(t *testing.T) {
	fleet, _ := read(t)

	// 21 records on disk; 60004 is torn and 60005 is empty, and both are
	// first-read failures with no previous state, so they are ABSENT. The 21st
	// is 76136.json, added in fix round 2 for the object-shaped former names.
	if len(fleet.Sessions) != 19 {
		var got []int
		for _, s := range fleet.Sessions {
			got = append(got, s.PID)
		}
		t.Fatalf("the document has %d sessions, want 19: %v", len(fleet.Sessions), got)
	}
	if !fleet.Epoch.Equal(NOW) {
		t.Errorf("the document's epoch is %s, want the injected %s", fleet.Epoch, NOW)
	}
	for _, s := range fleet.Sessions {
		if s.ID == "" {
			t.Errorf("session with pid %d has no id", s.PID)
		}
	}
}

// WRONG: reading the `.key` siblings, whose contents are a canary precisely so
// that this failure is visible. 33 of them against 32 records on the machine
// this data came from, and one has no matching record at all.
func TestNoKeyFileIsEverRead(t *testing.T) {
	fleet, problems := read(t)

	var all []string
	for _, s := range fleet.Sessions {
		all = append(all, s.ID, s.Name, s.StatusRaw, s.WaitingRaw, s.LaunchDir)
		all = append(all, s.PreviousNames...)
	}
	for _, p := range problems {
		all = append(all, p.Path, p.Detail)
	}
	for _, v := range all {
		if strings.Contains(v, "TESTDATA-CANARY-") {
			t.Fatalf("the canary string from a .key file reached the document: %q", v)
		}
	}
	// 56526 has a .key file and no record. A source that enumerated by key
	// file, or that stripped any extension rather than filtering on .json,
	// would have invented a session for it.
	for _, s := range fleet.Sessions {
		if s.PID == 56526 {
			t.Error("a session was invented for 56526, which has a .key file and no record")
		}
	}
}

// WRONG: guessing that an unrecognised status means busy. `compacting` was
// chosen for the test data precisely because it is plausible enough that an
// implementation tempted to guess will guess busy — and a plausible guess is
// indistinguishable from an observation, which is the whole failure.
func TestAnUnrecognisedStatusIsUnknownAndKeepsItsRawValue(t *testing.T) {
	fleet, problems := read(t)
	s := session(t, fleet, 60003)

	if s.Status != obs.StatusUnknown {
		t.Errorf("status decoded as %s, want unknown: this version has never seen the word", s.Status)
	}
	if s.StatusRaw != "compacting" {
		t.Errorf("statusRaw is %q, want the word the tool actually wrote — it is the ONLY "+
			"source of truth when status is null", s.StatusRaw)
	}
	if got := problemsFor(problems, "60003.json"); len(got) != 1 || got[0].Kind != registry.ProblemUndecoded {
		t.Errorf("an unrecognised status produced %v, want exactly one undecoded problem: "+
			"a format change has to be discoverable, not merely absent", got)
	}
}

// WRONG: defaulting an absent waiting reason to "input needed". That default
// is right 55 times out of 55 on live data and wrong in exactly the case a
// user depends on it — a session blocked on something else entirely, reported
// as waiting for them to type.
func TestWaitingWithNoReasonIsUndeterminedNotInput(t *testing.T) {
	fleet, _ := read(t)
	s := session(t, fleet, 60006)

	if s.Status != obs.StatusWaiting {
		t.Fatalf("status is %s, want waiting", s.Status)
	}
	if s.Waiting != obs.WaitingReasonUnknown {
		t.Errorf("waiting reason is %s, want unknown. The key is ABSENT, and absence is not "+
			"one of the two values", s.Waiting)
	}
	if s.WaitingRaw != "" {
		t.Errorf("waitingOnRaw is %q, want empty — there was nothing to keep", s.WaitingRaw)
	}
	// And the measured case, for contrast: 1724 carries the one value that
	// occurs in the wild, all 55 observations of it.
	if got := session(t, fleet, 1724); got.Waiting != obs.WaitingReasonInput {
		t.Errorf("1724's waiting reason is %s, want input", got.Waiting)
	}
}

// WRONG: merging the two waiting values into a boolean. Input and permission
// are different interruptions with different urgencies, and the one that
// matters most is the one that is hardest to observe.
//
// SYNTHETIC: `permission prompt` was never observed on this machine, in 1920
// observations over 304 seconds. This record is a declared SUBSTITUTION and
// not a proof, and a report that counts it as live verification is wrong.
//
// The raw value is checked by decoding it rather than by naming it, because a
// test that spelled Claude's string out would be a second decode site and
// TestOnlySchemaFileDecodesClaudeStrings scans this file too.
func TestAPermissionPromptIsItsOwnWaitingReason(t *testing.T) {
	fleet, _ := read(t)
	s := session(t, fleet, 60002)

	if s.Waiting != obs.WaitingReasonPermission {
		t.Errorf("waiting reason is %s, want permission", s.Waiting)
	}
	if s.WaitingRaw == "" {
		t.Fatal("waitingOnRaw is empty; the string the tool wrote was not kept")
	}
	if got, ok := adapters.DecodeWaitingFor(s.WaitingRaw); !ok || got != obs.WaitingReasonPermission {
		t.Errorf("the kept raw value does not decode back to permission (%s, ok=%v)", got, ok)
	}
}

// WRONG: flattening previous names to the latest, so a session that was
// renamed is no longer findable under what a human last called it. Two names
// is the minimum that can detect flattening, which is why this record has two.
func TestPreviousNamesAreKeptInOrder(t *testing.T) {
	fleet, _ := read(t)
	s := session(t, fleet, 45682)

	want := []string{"magmux-release", "release-prep"}
	if len(s.PreviousNames) != len(want) {
		t.Fatalf("previousNames is %v, want %v", s.PreviousNames, want)
	}
	for i := range want {
		if s.PreviousNames[i] != want[i] {
			t.Errorf("previousNames[%d] is %q, want %q — the order is the trail back", i, s.PreviousNames[i], want[i])
		}
	}
}

// WRONG: reading former names as strings only. Fix round 2, item 1, through
// the source the live finding came through: on 2026-09-19 Claude Code 2.1.276
// wrote each former name as an object, the record reached the document with
// previousNames null, and `magmux agents` printed a problem for it on stderr.
// 76136.json is that live record, copied byte for byte.
func TestFormerNamesWrittenAsObjectsReachTheDocumentWithoutAProblem(t *testing.T) {
	fleet, problems := read(t)
	s := session(t, fleet, 76136)

	if len(s.PreviousNames) != 1 || s.PreviousNames[0] != "agent-fleet-sessions" {
		t.Errorf("previousNames is %q, want [agent-fleet-sessions] — Claude Code's current "+
			"format, and the session is unfindable by its former name without it", s.PreviousNames)
	}
	if got := problemsFor(problems, "76136.json"); len(got) != 0 {
		t.Errorf("a well-formed record from Claude Code 2.1.276 produced problems: %v", got)
	}
}

// WRONG: failing on a record from a newer Claude. The formats are undocumented
// and they change; a source that errors on an unknown key stops reporting the
// whole fleet the day Claude ships a field.
func TestAFutureRecordDecodesAndItsUnknownKeysAreIgnored(t *testing.T) {
	fleet, problems := read(t)
	s := session(t, fleet, 60007)

	if s.Status != obs.StatusIdle {
		t.Errorf("status is %s, want idle: the keys this version knows still decode", s.Status)
	}
	if s.Name != "future-schema-session" {
		t.Errorf("name is %q, want future-schema-session", s.Name)
	}
	if got := problemsFor(problems, "60007.json"); len(got) != 0 {
		t.Errorf("five unknown keys produced %v; an unknown key is not an error", got)
	}
}

// WRONG: parsing by position, or decoding into a struct whose field order is
// assumed. Key order is UNSTABLE between real records — measured — and a
// positional read gives one record's session id out of another record's field.
func TestKeyOrderDoesNotChangeTheResult(t *testing.T) {
	fleet, _ := read(t)
	shuffled := session(t, fleet, 60008)
	canonical := session(t, fleet, 1724)

	// 60008 is 1724's content with every key in a different order, and with
	// its own id, pid and name. Everything else must match exactly.
	if shuffled.Status != canonical.Status {
		t.Errorf("status %s against %s", shuffled.Status, canonical.Status)
	}
	if shuffled.Waiting != canonical.Waiting {
		t.Errorf("waiting %s against %s", shuffled.Waiting, canonical.Waiting)
	}
	if shuffled.StatusRaw != canonical.StatusRaw || shuffled.WaitingRaw != canonical.WaitingRaw {
		t.Errorf("raw values %q/%q against %q/%q",
			shuffled.StatusRaw, shuffled.WaitingRaw, canonical.StatusRaw, canonical.WaitingRaw)
	}
	if shuffled.StatusSince == nil || canonical.StatusSince == nil {
		t.Fatal("one of the two has no status instant")
	}
	if !shuffled.StatusSince.Equal(*canonical.StatusSince) {
		t.Errorf("statusSince %s against %s", shuffled.StatusSince, canonical.StatusSince)
	}
}

// WRONG: reporting a session whose process is gone as merely unknown, or
// excluding it from the document so a consumer cannot tell "gone" from "never
// existed".
//
// SYNTHETIC: 32 of 32 live records were live on this machine, so the dead-pid
// case cannot be validated against live data at all. This record is a declared
// substitution.
func TestADeadProcessIsReportedAsDead(t *testing.T) {
	fleet, _ := read(t)

	if got := session(t, fleet, 60001).ProcessAlive; got != obs.LivenessNo {
		t.Errorf("processAlive is %s, want no", got)
	}
	if got := session(t, fleet, 98198).ProcessAlive; got != obs.LivenessYes {
		t.Errorf("a live session's processAlive is %s, want yes", got)
	}
}

// WRONG: a nil oracle defaulting to a real signal probe. A test that forgets
// to inject one would then be sending signals to whatever pids happen to exist
// on the machine running the suite, and its answers would depend on what else
// was running — including, on a developer's machine, thirty live agents.
func TestWithNoOracleEveryLivenessIsUndetermined(t *testing.T) {
	src, err := registry.New(registry.Config{Root: testdataDir, Now: func() time.Time { return NOW }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fleet, _ := src.Read()
	if len(fleet.Sessions) == 0 {
		t.Fatal("no sessions were read at all")
	}
	for _, s := range fleet.Sessions {
		if s.ProcessAlive != obs.LivenessUnknown {
			t.Fatalf("session %d reports processAlive %s with no oracle installed; "+
				"undetermined is the only honest answer", s.PID, s.ProcessAlive)
		}
	}
}

// WRONG: excluding a session because its status is old. R1.3's stale record is
// one whose PID is dead, not one whose status has not changed — and a status
// that has not moved for three days is a fact about the session, which is
// often the single most useful fact in the document.
//
// The class is measured even though this instance is synthetic: 23 of 32 live
// sessions carried a status instant from the first 17 seconds of their life.
func TestALongIdleSessionIsReportedNotExcluded(t *testing.T) {
	fleet, _ := read(t)
	s := session(t, fleet, 60010)

	if s.ProcessAlive != obs.LivenessYes {
		t.Fatalf("processAlive is %s, want yes — this record's pid is live", s.ProcessAlive)
	}
	if s.StatusSince == nil {
		t.Fatal("statusSince is nil; the instant was not decoded")
	}
	for_, ok := s.InStatusFor(NOW)
	if !ok {
		t.Fatal("InStatusFor reports undetermined for a session that has an instant")
	}
	if for_ < 72*time.Hour {
		t.Errorf("the session has been idle for %s, want about three days — the instant is "+
			"epoch MILLISECONDS and reading it as seconds gives an answer 1000x wrong", for_)
	}
}

// WRONG: using the pid in the FILENAME. The oracle gives the two pids opposite
// answers precisely so that this test can tell which one was used: the
// filename says 60011 (dead) and the record says 60099 (alive).
//
// The record wins. The record is the document; the filename is an index into
// it, and an index that disagrees with its own document is a symptom, not a
// second opinion.
func TestTheRecordsOwnPIDWinsOverTheFilename(t *testing.T) {
	fleet, problems := read(t)

	s := session(t, fleet, 60099)
	if s.ProcessAlive != obs.LivenessYes {
		t.Errorf("processAlive is %s, want yes. The filename pid 60011 is DEAD in the oracle "+
			"and the record's pid 60099 is alive, so this is reading the filename", s.ProcessAlive)
	}
	for _, other := range fleet.Sessions {
		if other.PID == 60011 {
			t.Error("a session was reported with the filename's pid 60011")
		}
	}

	got := problemsFor(problems, "60011.json")
	if len(got) != 1 || got[0].Kind != registry.ProblemPIDMismatch {
		t.Fatalf("the disagreement produced %v, want exactly one pid-mismatch problem: "+
			"the session is still reported, and the oddity is still said out loud", got)
	}
	if !strings.Contains(got[0].Detail, "60011") || !strings.Contains(got[0].Detail, "60099") {
		t.Errorf("the problem detail %q names neither pid; a mismatch nobody can identify is "+
			"not a report", got[0].Detail)
	}
}

// WRONG: treating a parse failure as a dead session, or dropping it. Claude
// rewrites these records IN PLACE, so a half-written read is real and is most
// likely at exactly the moment a session changes state — the moment we most
// want to read it.
//
// A first-read failure has nothing to fall back on, so the session is ABSENT
// and the problem says so. Absent is not dead, and the two records here fail
// differently on purpose: 60004 stops mid-string and 60005 is zero bytes,
// which most decoders treat as different kinds of failure.
func TestATornFirstReadIsAbsentAndNotDead(t *testing.T) {
	fleet, problems := read(t)

	for _, base := range []string{"60004.json", "60005.json"} {
		got := problemsFor(problems, base)
		if len(got) != 1 {
			t.Fatalf("%s produced %d problems, want 1: %v", base, len(got), got)
		}
		if got[0].Kind != registry.ProblemAbsent {
			t.Errorf("%s is reported as %s, want absent", base, got[0].Kind)
		}
		if !strings.Contains(got[0].Detail, "Absent is not dead") {
			t.Errorf("%s's problem does not say absent is not dead: %q", base, got[0].Detail)
		}
	}
	for _, s := range fleet.Sessions {
		if s.PID == 60004 || s.PID == 60005 {
			t.Errorf("a torn record produced a session: %+v", s)
		}
		if s.ProcessAlive == obs.LivenessNo && s.PID != 60001 {
			t.Errorf("session %d is reported DEAD; the only dead pid in the oracle is 60001", s.PID)
		}
	}
}

// WRONG: a second torn read dropping the session, or reporting it dead. The
// session was there a moment ago and one unreadable file is not evidence that
// it stopped existing.
//
// This is the only test here that mutates a record, so it works on a COPY in
// t.TempDir(). Nothing under internal/obs/testdata is ever written.
func TestASecondTornReadKeepsThePreviousStateAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "98693.json")
	copyRecord(t, filepath.Join(testdataDir, "98693.json"), path)

	src, err := registry.New(registry.Config{
		Root:       dir,
		Oracle:     loadOracle(t),
		Now:        func() time.Time { return NOW },
		RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, problems := src.Read()
	if len(problems) != 0 {
		t.Fatalf("the intact record produced problems: %v", problems)
	}
	before := session(t, first, 98693)
	if before.Status != obs.StatusBusy {
		t.Fatalf("status is %s, want busy", before.Status)
	}

	// Tear it, the way an in-place rewrite caught mid-flight looks: the file
	// is opened, truncated, and half the bytes are there.
	writeFile(t, path, `{"sessionId":"a1478d55-6e02-4bb9-93cf-01d2e8f47a6b","pid":98693,"sta`)

	second, problems := src.Read()
	after := session(t, second, 98693)
	if after.Status != obs.StatusBusy || after.ID != before.ID {
		t.Errorf("the torn read changed the session: %+v against %+v", after, before)
	}
	if after.ProcessAlive != obs.LivenessYes {
		t.Errorf("processAlive is %s, want yes: a torn read is not a dead process", after.ProcessAlive)
	}
	got := problemsFor(problems, "98693.json")
	if len(got) != 1 || got[0].Kind != registry.ProblemTorn {
		t.Fatalf("a twice-torn record produced %v, want exactly one torn problem", got)
	}
	if !strings.Contains(got[0].Detail, "keeping the state read previously") {
		t.Errorf("the problem does not say the state was kept: %q", got[0].Detail)
	}

	// And it recovers: a whole record replaces the remembered one. The record
	// is the same session with a new name, because what has to be shown is
	// that the remembered state is a FALLBACK and not a cache — restoring the
	// identical bytes would pass either way.
	original, err := os.ReadFile(filepath.Join(testdataDir, "98693.json"))
	if err != nil {
		t.Fatalf("reading the original record: %v", err)
	}
	renamed := strings.Replace(string(original), before.Name, "renamed-after-recovery", 1)
	if renamed == string(original) {
		t.Fatalf("the record does not contain the name %q it decoded to", before.Name)
	}
	writeFile(t, path, renamed)

	third, problems := src.Read()
	if len(problems) != 0 {
		t.Fatalf("the restored record produced problems: %v", problems)
	}
	if got := session(t, third, 98693); got.Name != "renamed-after-recovery" {
		t.Errorf("after recovery the name is %q, want the newly written one — the remembered "+
			"state is a fallback, not a cache", got.Name)
	}
}

// WRONG: retrying in a busy loop, or not retrying at all. A record is a few
// hundred bytes written in place; the window in which a reader catches half of
// one is microseconds wide, so ONE retry after a short wait recovers nearly
// every torn read, and a loop would turn a rewrite in flight into a spin.
//
// The delay is driven through the injected clock, so this test proves the wait
// happened and how long it was armed for without waiting for it.
func TestTheTornReadRetryUsesTheInjectedClockOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "98693.json")
	writeFile(t, path, `{"sessionId":"a1478`)

	clk := obs.NewFakeClock(NOW)
	src, err := registry.New(registry.Config{
		Root:       dir,
		Now:        func() time.Time { return NOW },
		Clock:      clk,
		RetryDelay: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan []registry.Problem, 1)
	go func() {
		_, problems := src.Read()
		done <- problems
	}()

	ft, err := clk.NextTimer(2 * time.Second)
	if err != nil {
		t.Fatalf("%v — the torn read did not wait at all before retrying", err)
	}
	if ft.Delay() != 250*time.Millisecond {
		t.Errorf("the retry was armed for %s, want the configured 250ms", ft.Delay())
	}
	ft.Fire(NOW.Add(250 * time.Millisecond))

	select {
	case problems := <-done:
		if len(problems) != 1 || problems[0].Kind != registry.ProblemAbsent {
			t.Fatalf("got %v, want one absent problem", problems)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not finish after the retry timer fired")
	}

	// Exactly one retry. A second armed timer would be a second wait, which is
	// a loop with a small trip count rather than a single retry.
	if err := clk.NoTimerWithin(200 * time.Millisecond); err != nil {
		t.Errorf("a second retry was armed: %v", err)
	}
}

// WRONG: reporting an unreadable directory as an empty machine. "No sessions"
// and "that path does not exist" are the whole of the diagnosis, and the root
// is a path a user can mistype on the command line.
func TestAnUnreadableRootIsAProblemAndNotAnEmptyFleet(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	src, err := registry.New(registry.Config{Root: missing, Now: func() time.Time { return NOW }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fleet, problems := src.Read()
	if len(fleet.Sessions) != 0 {
		t.Errorf("a missing root produced %d sessions", len(fleet.Sessions))
	}
	if len(problems) != 1 || problems[0].Kind != registry.ProblemUnreadable {
		t.Fatalf("a missing root produced %v, want one unreadable problem", problems)
	}
	if !fleet.Epoch.Equal(NOW) {
		t.Error("even an empty document must say when it was taken")
	}

	if _, err := registry.New(registry.Config{}); err == nil {
		t.Error("New accepted a Config with no root")
	}
}

// WRONG: a document whose order depends on map iteration or on readdir order,
// so every diff between two reads is noise — and this document exists to be
// diffed.
func TestTheDocumentIsOrderedAndRepeatable(t *testing.T) {
	first, _ := read(t)
	second, _ := read(t)

	if len(first.Sessions) != len(second.Sessions) {
		t.Fatalf("two reads of an unchanging directory gave %d and %d sessions",
			len(first.Sessions), len(second.Sessions))
	}
	for i := range first.Sessions {
		if first.Sessions[i].PID != second.Sessions[i].PID {
			t.Fatalf("session %d is pid %d in one read and %d in the other",
				i, first.Sessions[i].PID, second.Sessions[i].PID)
		}
	}
	for i := 1; i < len(first.Sessions); i++ {
		if first.Sessions[i-1].PID > first.Sessions[i].PID {
			t.Errorf("sessions are not ordered by pid: %d before %d",
				first.Sessions[i-1].PID, first.Sessions[i].PID)
		}
	}
}

// WRONG: filling LaunchDir from the record's `cwd`.
//
// They are usually equal, which is what makes this dangerous: `cwd` is the
// session's LIVE working directory and LaunchDir is the directory it was
// started in, and the 4% where they differ resolve to a project directory that
// EXISTS and holds another session's transcripts. The failure is "read the
// wrong session and believe it", not "file not found". magmux spawns the
// sessions it observes, so it holds the real launch directory; that is where
// this field comes from, in a later phase, and nothing else will do.
func TestLaunchDirIsNotGuessedFromTheLiveWorkingDirectory(t *testing.T) {
	fleet, _ := read(t)

	// 98693's cwd is a worktree, which is exactly the shape that derives to a
	// different, existing project directory.
	for _, s := range fleet.Sessions {
		if s.LaunchDir != "" {
			t.Errorf("session %d has launchDir %q; phase 2 has no source for it, and `cwd` "+
				"is the silent 4%%-wrong answer", s.PID, s.LaunchDir)
		}
	}
}

// WRONG: inventing a pane gate now. The second liveness gate needs magmux's
// pane table, and a pane whose existence nobody established is not thereby
// gone. `60009.json` (an unparseable terminal coordinate) and `60012.json`
// (live process, dead pane) are unused this phase, which is correct.
func TestThePaneGateIsUndeterminedRatherThanGuessed(t *testing.T) {
	fleet, _ := read(t)
	for _, s := range fleet.Sessions {
		if s.PaneAlive != obs.LivenessUnknown {
			t.Errorf("session %d reports paneAlive %s; nothing has asked a pane oracle", s.PID, s.PaneAlive)
		}
		if s.PaneID != nil {
			t.Errorf("session %d has a pane id %d; magmux's pane table is phase 3", s.PID, *s.PaneID)
		}
	}
	// And a record with an unparseable coordinate is still a session: excluding
	// it would make a malformed field indistinguishable from a dead process.
	if got := session(t, fleet, 60009); got.ID == "" {
		t.Error("60009 was dropped for having a malformed terminal coordinate")
	}
}

// WRONG: filling in the fields later phases own. nil means "we did not
// determine this", and a determined zero is a different answer — the
// difference between a session that has finished its work and one nobody has
// counted.
func TestFieldsLaterPhasesOwnAreLeftUndetermined(t *testing.T) {
	fleet, _ := read(t)
	for _, s := range fleet.Sessions {
		if s.SubagentCount != nil {
			t.Errorf("session %d has a subagent count; nothing has read a transcript", s.PID)
		}
		if s.Color != nil {
			t.Errorf("session %d has a colour; nothing has read a transcript", s.PID)
		}
		if s.Fault.APIErrors != nil {
			t.Errorf("session %d has an API error count; nothing has read a transcript", s.PID)
		}
		if s.Attention.Needed != nil {
			t.Errorf("session %d has an attention verdict; attention resolution is phase 5", s.PID)
		}
	}
}

// WRONG: a record whose instant is in the wrong unit decoding to a date in
// 1970 or in the year 58600, silently. Both marshal cleanly and neither errors.
func TestEveryStatusInstantIsWithinLivingMemory(t *testing.T) {
	fleet, _ := read(t)

	lo := NOW.AddDate(-5, 0, 0)
	hi := NOW.AddDate(5, 0, 0)
	var checked int
	for _, s := range fleet.Sessions {
		if s.StatusSince == nil {
			continue
		}
		checked++
		if s.StatusSince.Before(lo) || s.StatusSince.After(hi) {
			t.Errorf("session %d entered its status at %s, which is not within five years of "+
				"the reference instant. These values are epoch MILLISECONDS: read as seconds "+
				"they land in the year 58600, and divided twice they land in 1970.",
				s.PID, s.StatusSince.Format(time.RFC3339))
		}
	}
	if checked < 15 {
		t.Fatalf("only %d sessions had an instant to check", checked)
	}
}

func copyRecord(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("reading %s: %v", from, err)
	}
	writeFile(t, to, string(b))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	// O_TRUNC, which is how the real writer publishes: in place, no temporary
	// file, no rename. It is the reason a torn read is possible at all.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing %s: %v", path, err)
	}
}

// WRONG: a reader that writes. This whole feature is read-only to ~/.claude
// because thirty live Claude Code processes own that tree, and a source that
// created so much as a temp file next to their records would be doing it under
// them while they wrote.
//
// It compares a full stat of the directory before and after a read: every name,
// every size, every mtime. os.Stat is re-issued rather than reusing the
// DirEntry, because ReadDir's DirEntry.Info is CACHED on Unix — comparing an
// entry against itself would pass against a source that rewrote every file in
// the directory.
func TestTheTestDataDirectoryIsNotWritten(t *testing.T) {
	before := statDir(t)
	read(t)
	after := statDir(t)

	if len(before) != len(after) {
		t.Fatalf("the directory held %d entries before the read and %d after", len(before), len(after))
	}
	for name, was := range before {
		is, still := after[name]
		if !still {
			t.Errorf("%s disappeared during a read", name)
			continue
		}
		if was != is {
			t.Errorf("%s changed during a read: %s -> %s. This directory is READ-ONLY to the "+
				"suite, and the production code is read-only to ~/.claude for the same reason.",
				name, was, is)
		}
	}
}

// statDir fingerprints every entry: size and modification time, freshly
// stat'ed.
func statDir(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("reading %s: %v", testdataDir, err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(testdataDir, e.Name()))
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		out[e.Name()] = fmt.Sprintf("%d bytes, %s", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
	}
	if len(out) < 20 {
		t.Fatalf("only %d entries under %s; the walk is not reaching the test data", len(out), testdataDir)
	}
	return out
}
