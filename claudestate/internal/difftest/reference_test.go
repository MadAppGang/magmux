package difftest_test

// The differential's CI half (architecture §8.1): every reference _diff
// recorded from tt's UNMODIFIED observer is replayed against the library,
// in-process and deterministic, and every difference must be cited by a D-row.
//
// For each testdata/tt-reference/<scenario>.json the library runs over
// testdata/roots/<root> with:
//   - pipeline.Start, Watch false;
//   - every tree at its canonical Path ("/testdata/<tree>", which is what makes
//     the scrubbed linkScanPaths resolve under tt's own lexical rule) over
//     os.DirFS, with Dir "" (readable, unwatchable);
//   - a fake oracle answering from <scenario>.liveness.json;
//   - no pane locator;
//   - a fake AgentsListing returning the listing bytes tt's stub printed, or
//     the failure a missing claude is;
//   - verdict.Rules{IgnoreAPIErrors: true}, tt's attention policy (D9);
//   - Now and the FakeClock both pinned to the document's observedAt;
//   - RefreshSources for the agents source before Refresh.
//
// Nothing here reads the real Claude root, probes a pid, speaks to tmux or
// runs claude: the liveness is the recorded file's, and the clock is fake.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/internal/difftest"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/verdict"
)

const (
	testdataDir  = "../../testdata"
	referenceDir = testdataDir + "/tt-reference"
	projectsPath = "/testdata/projects"
	// retryDelay is the torn-read retry. The test fires exactly the timers of
	// this delay, so an actor's wake alarm never runs a cycle of its own.
	retryDelay = 7 * time.Millisecond
	bound      = 30 * time.Second
)

// notRecorded is every corpus scenario that has NO reference, and why. A
// scenario is recorded or listed here, never both and never neither, so the
// set CI checks cannot shrink quietly.
var notRecorded = map[string]string{
	"k3-duplicate-99-100": "NOT VERIFIED by construction: an ascending pid map cannot keep 99.json/100.json's byte order (§8.2 step 4, §11)",
}

type livenessOracle map[int]model.Liveness

func (o livenessOracle) Alive(pid int) model.Liveness { return o[pid] }

type listing struct {
	raw []byte
	at  time.Time
}

var errNoClaude = errors.New("claude agents --json: exit status 127")

func (l listing) List(context.Context) (ports.Listing, error) {
	if l.raw == nil {
		return ports.Listing{}, errNoClaude
	}
	return ports.Listing{Raw: l.raw, At: l.at}, nil
}

type reference struct {
	name     string
	doc      []byte
	recorded difftest.Recorded
}

func references(t *testing.T) []reference {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(referenceDir, "*.liveness.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no recorded reference: run `go run ./_diff -tt-repo … -all-corpus -record`")
	}
	sort.Strings(matches)
	var out []reference
	for _, m := range matches {
		name := strings.TrimSuffix(filepath.Base(m), ".liveness.json")
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := difftest.DecodeRecorded(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rec.Scenario != name {
			t.Fatalf("%s records scenario %q", m, rec.Scenario)
		}
		doc, err := os.ReadFile(filepath.Join(referenceDir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, reference{name: name, doc: doc, recorded: rec})
	}
	return out
}

// runLibrary replays one scenario through the library and returns its last
// delivery. RefreshSources runs on its own goroutine because its cycle may
// wait on the torn-read timer, which only this goroutine fires.
func runLibrary(t *testing.T, rec difftest.Recorded, observedAt time.Time) pipeline.Delivery {
	t.Helper()
	root := filepath.Join(testdataDir, "roots", rec.Root)
	tree := func(name string) ports.Tree {
		return ports.Tree{Path: "/testdata/" + name, FS: os.DirFS(filepath.Join(root, name))}
	}
	oracle := livenessOracle{}
	for pid, w := range rec.Liveness {
		n, err := strconv.Atoi(pid)
		if err != nil {
			t.Fatal(err)
		}
		oracle[n] = model.LivenessYes
		if w == difftest.Dead {
			oracle[n] = model.LivenessNo
		}
	}
	lst := listing{at: observedAt}
	if rec.Listing != "" {
		b, err := os.ReadFile(filepath.Join(testdataDir, filepath.FromSlash(rec.Listing)))
		if err != nil {
			t.Fatal(err)
		}
		lst.raw = b
	}
	clk := ports.NewFakeClock(observedAt)
	p, err := pipeline.Start(pipeline.Config{
		Root:       ports.Root{Sessions: tree("sessions"), Jobs: tree("jobs"), Projects: tree("projects")},
		Oracle:     oracle,
		Agents:     lst,
		Verdict:    verdict.Rules{IgnoreAPIErrors: true},
		Watch:      false,
		Clock:      clk,
		Now:        func() time.Time { return observedAt },
		RetryDelay: retryDelay,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	sub, err := p.Subscribe(ctx, "reference", 16)
	if err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan error, 1)
	go func() {
		_, err := p.RefreshSources(ctx, []model.SourceID{model.SourceAgents})
		refreshed <- err
	}()
	next := func() pipeline.Delivery {
		for {
			select {
			case d, ok := <-sub.C():
				if !ok {
					t.Fatal("the subscription closed")
				}
				return d
			case tm := <-clk.Timers():
				if tm.Delay() == retryDelay {
					tm.Fire(observedAt)
				}
			case <-ctx.Done():
				t.Fatal("the library published nothing")
			}
		}
	}
	next()
	for {
		select {
		case err := <-refreshed:
			if err != nil {
				t.Fatal(err)
			}
		case tm := <-clk.Timers():
			if tm.Delay() == retryDelay {
				tm.Fire(observedAt)
			}
			continue
		case <-ctx.Done():
			t.Fatal("RefreshSources did not return")
		}
		break
	}
	if !p.Refresh() {
		t.Fatal("Refresh refused")
	}
	return next()
}

func compare(t *testing.T, ref reference, doc []byte) []difftest.Diff {
	t.Helper()
	ttRows, observedAt, err := difftest.FromTT(doc, projectsPath)
	if err != nil {
		t.Fatal(err)
	}
	d := runLibrary(t, ref.recorded, observedAt)
	return difftest.Compare(ttRows, difftest.FromModel(d.Doc, d.Problems), d.Problems)
}

// TestTheLibraryAgreesWithTTsRecordedReferences is §8.1's gate: zero uncited
// differences in every recorded scenario.
func TestTheLibraryAgreesWithTTsRecordedReferences(t *testing.T) {
	total := map[string]int{}
	for _, ref := range references(t) {
		t.Run(ref.name, func(t *testing.T) {
			r := difftest.Summarise(compare(t, ref, ref.doc))
			for _, d := range r.Uncited {
				t.Errorf("UNCITED %s", d.Format())
			}
			var rows []string
			for k, v := range r.PerRow {
				total[k] += v
				rows = append(rows, k+"="+strconv.Itoa(v))
			}
			sort.Strings(rows)
			t.Logf("cited %v; one-sided rows %d", rows, r.OneSided)
		})
	}
	t.Logf("all scenarios: %v", total)
}

// A recorded reference never names a sandbox path, and every corpus scenario
// is either recorded or listed in notRecorded with its reason.
func TestTheReferencesAreCanonicalAndComplete(t *testing.T) {
	recorded := map[string]bool{}
	for _, ref := range references(t) {
		recorded[ref.name] = true
		for _, bad := range []string{"claudestate-diff-", "/var/folders/", "/private/var/"} {
			if bytes.Contains(ref.doc, []byte(bad)) {
				t.Errorf("%s names a sandbox path (%q)", ref.name, bad)
			}
		}
		if !bytes.Contains(ref.doc, []byte(`"observedAt"`)) {
			t.Errorf("%s is not a tt sessions document", ref.name)
		}
	}
	for _, s := range difftest.Corpus() {
		why, listed := notRecorded[s.Name]
		switch {
		case recorded[s.Name] && listed:
			t.Errorf("%s is recorded AND listed as not recorded (%s): drop the entry", s.Name, why)
		case !recorded[s.Name] && !listed:
			t.Errorf("%s has no reference and no reason", s.Name)
		}
	}
	for name := range notRecorded {
		if _, ok := difftest.ScenarioNamed(name); !ok {
			t.Errorf("notRecorded names %s, which is not a corpus scenario", name)
		}
	}
}

func find(t *testing.T, name string) reference {
	t.Helper()
	for _, r := range references(t) {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("no reference for %s", name)
	return reference{}
}

// mutate sets one member of the session whose shortId (else sessionId) is
// key, in a copy of doc, and re-encodes it: the mutated document is still
// valid JSON, so the only change is the one field.
func mutate(t *testing.T, doc []byte, key, member string, value any) []byte {
	t.Helper()
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	sessions, _ := v["sessions"].([]any)
	for _, s := range sessions {
		m := s.(map[string]any)
		if m["shortId"] == key || (m["shortId"] == nil && m["sessionId"] == key) {
			if _, ok := m[member]; !ok {
				t.Fatalf("the session %s has no member %s", key, member)
			}
			m[member] = value
			out, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatalf("no session %s in the reference", key)
	return nil
}

// The self-test: one mutated field in a copy of a recorded document is
// exactly one uncited difference. Without it, a comparison that cited
// everything, or compared nothing, would pass the gate above.
func TestOneMutatedFieldIsOneUncitedDifference(t *testing.T) {
	ref := find(t, "twin-dead-overlay")
	d := difftest.Summarise(compare(t, ref, mutate(t, ref.doc, "tdo00001", "claudeVersion", "MUTATED")))
	if len(d.Uncited) != 1 || d.Uncited[0].Field != difftest.FieldClaudeVersion {
		t.Fatalf("want exactly one uncited claudeVersion difference, got %d: %v", len(d.Uncited), d.Uncited)
	}
}

// The D12c negative control (review 3 Issue 8): job.tokens comes from the job
// record, which both sides read identically, so a mutated token count is
// uncited while the legitimate pid difference stays D12c.
func TestTheD12cNegativeControl(t *testing.T) {
	ref := find(t, "bg-two-claimants")
	diffs := compare(t, ref, mutate(t, ref.doc, "btc00001", "tokens", 424242))
	r := difftest.Summarise(diffs)
	if len(r.Uncited) != 1 || r.Uncited[0].Field != difftest.FieldTokens {
		t.Fatalf("want exactly one uncited tokens difference, got %v", r.Uncited)
	}
	pidCited := false
	for _, d := range diffs {
		if d.Field == difftest.FieldPID && d.Cited == difftest.D12c {
			pidCited = true
		}
	}
	if !pidCited {
		t.Fatal("the pid difference is no longer cited D12c")
	}
}

// The D12d negative control (review 4 Issue 6): the same mutation on the K2
// duplicate is uncited, while lastActivityAt stays D12d.
func TestTheD12dNegativeControl(t *testing.T) {
	ref := find(t, "k2-duplicate-different-transcripts")
	diffs := compare(t, ref, mutate(t, ref.doc, "k2dup001", "tokens", 424242))
	r := difftest.Summarise(diffs)
	if len(r.Uncited) != 1 || r.Uncited[0].Field != difftest.FieldTokens {
		t.Fatalf("want exactly one uncited tokens difference, got %v", r.Uncited)
	}
	if r.PerRow[difftest.D12d] != 1 {
		t.Fatalf("lastActivityAt is no longer cited D12d: %v", r.PerRow)
	}
}

// tt's interactive-no-registry case: the agents-only session keeps the
// listing's pid and its liveness on BOTH sides, not only that the oracle was
// called (review 3 Issue 4).
func TestAnAgentsOnlySessionKeepsItsPIDOnBothSides(t *testing.T) {
	ref := find(t, "tt-corpus.agents-interactive-no-registry")
	ttRows, observedAt, err := difftest.FromTT(ref.doc, projectsPath)
	if err != nil {
		t.Fatal(err)
	}
	d := runLibrary(t, ref.recorded, observedAt)
	const pid = 987654
	var ttSeen, libSeen bool
	for _, r := range ttRows {
		if r.Key.PID == pid {
			ttSeen = true
			if r.Fields[difftest.FieldProcAlive] != (difftest.Value{S: "true"}) {
				t.Errorf("tt: %v", r.Fields[difftest.FieldProcAlive])
			}
		}
	}
	for _, s := range d.Doc.Sessions {
		if s.PID == pid {
			libSeen = true
			if s.ProcessAlive != model.LivenessYes || s.Key.PID != pid {
				t.Errorf("lib: %+v", s)
			}
		}
	}
	if !ttSeen || !libSeen {
		t.Fatalf("pid %d on tt: %v, on the library: %v", pid, ttSeen, libSeen)
	}
}
