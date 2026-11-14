package obsview

// H6: the wire is pinned. The JSON document `magmux agents --json` prints — the
// same bytes the socket `agents` verb and MCP's list_agents carry — was
// recorded before the claudestate library replaced the code that produced it.
// Every later difference must be cited by decision in wirechanges.go; nothing
// else may change.
//
// There are two golden SETS, both recorded by the PRE-MOVE code:
//
//   - testdata/golden/magmux (C0.6) over the magmux-origin corpus,
//     internal/obs/testdata, where it lives;
//   - testdata/golden/tt (M0) over every tt scenario root of the library's
//     corpus, ../claudestate/testdata/roots/<scenario>. The old code read no
//     jobs/ directory and no agents listing, so those goldens show no job
//     rows; the rows and members the library adds are D6 changes,
//     wirechanges.go cites them, and this test now reads each scenario's jobs/
//     tree as `magmux agents --root S --jobs J --projects P` does.
//
// Two things make that a guarantee rather than a hope:
//
//   - each golden directory carries MANIFEST, the SHA-256 of every input file
//     at capture time, and the test fails if any input byte changed — so a
//     code change can never be hidden by a testdata change. The tt MANIFEST
//     also names the commit whose code recorded it and every scenario -> file
//     pair, and the test fails if the scenario set moved;
//   - the document is a pure function of the corpus here: a fixed epoch, no
//     pane table, no watcher, and liveness from a FAKE oracle that answers
//     from the scenario's own recorded list. Nothing reads ~/.claude, probes a
//     pid or depends on when the test runs.
//
// Every difference is cited EXACTLY: by golden, by row and by field, with the
// decision row that made it (wirechanges.go). A citation that no longer names
// a real difference fails too, so the table cannot accumulate cover for
// differences that are gone — or for ones that have not happened yet.
//
// Re-recording is deliberate and visible: go test ./obsview -run Golden
// -update -golden-source <commit> rewrites the goldens AND the MANIFESTs, and
// both show up in the diff. Since the library landed it would also erase the
// baseline every citation is measured against, so it is not a way to make a
// difference disappear: a re-recorded golden is a reviewer's question.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

var (
	update       = flag.Bool("update", false, "re-record the wire goldens and their MANIFEST from the current code")
	goldenSource = flag.String("golden-source", "", "with -update: the full commit sha whose code records the tt goldens, written into their MANIFEST")
)

const (
	goldenDir = "testdata/golden/magmux"
	// corpus is the magmux-origin testdata, read where it lives rather than
	// copied, so there is one corpus and the MANIFEST says which bytes of it
	// the goldens were recorded over.
	corpus = "../internal/obs/testdata"

	ttGoldenDir = "testdata/golden/tt"
	// ttCorpus is the library's testdata, read where it lives, by a relative
	// path.
	ttCorpus = "../claudestate/testdata"
)

// goldenNow is the magmux set's epoch: that corpus's own reference instant
// (1789545200000 ms), so the durations in the document are the ones the corpus
// was written to produce.
var goldenNow = time.UnixMilli(1789545200000).UTC()

// ttGoldenNow is the tt set's epoch: the scenario roots' reference instant
// (1789699211204 ms, claudestate/testdata/PROVENANCE.md).
var ttGoldenNow = time.UnixMilli(1789699211204).UTC()

// goldenInputs are the trees every magmux scenario reads. The MANIFEST covers
// all of them, whichever scenario reads which.
var goldenInputs = []string{"sessions", "projects"}

// canonicalTree is one Claude tree of the corpus, read through os.DirFS and
// named by a CANONICAL path that exists nowhere on a real machine: the
// library compares a job's linkScanPath against Path lexically, and the
// corpus's records name /testdata/projects. Dir is "", so the tree can be read
// and never watched, which is all a one-shot document needs.
func canonicalTree(dir, name string) ports.Tree {
	return ports.Tree{Path: "/testdata/" + name, FS: os.DirFS(filepath.Join(dir, name))}
}

// goldenScenario is one recorded document.
type goldenScenario struct {
	name string
	cfg  func() pipeline.Config
	now  time.Time
}

var goldenScenarios = []goldenScenario{
	{
		// The registry alone: what `magmux agents --root DIR --once --json`
		// prints with no transcript tree.
		name: "registry",
		now:  goldenNow,
		cfg: func() pipeline.Config {
			return pipeline.Config{Root: ports.Root{Sessions: canonicalTree(corpus, "sessions")}}
		},
	},
	{
		// The registry and the transcripts: the harness-derived columns
		// (subagents, API errors, colour) come from the library's transcript
		// stage.
		name: "registry-and-transcripts",
		now:  goldenNow,
		cfg: func() pipeline.Config {
			return pipeline.Config{Root: ports.Root{
				Sessions: canonicalTree(corpus, "sessions"),
				Projects: canonicalTree(corpus, "projects"),
			}}
		},
	},
}

// ── the tt set ──────────────────────────────────────────────────────────────

// ttScenarios is every tt scenario root: tt's own corpus and the §8.1 roots,
// each of which tt 51ec374 was run over to record a reference. It is a closed
// list, held against the directory by TestWireGoldens, so a scenario added to
// the library's corpus without a baseline here fails rather than goes
// unpinned.
var ttScenarios = []string{
	"bg-spare-no-jobid",
	"bg-two-claimants",
	"job-no-id-shared-short",
	"k2-duplicate-different-transcripts",
	"k3-duplicate-99-100",
	"k3-duplicate-both-mismatched",
	"lone-bg",
	"lone-bg-two-claimants",
	"lone-bg-with-transcript",
	"tt-corpus",
	"twin-dead-blocked",
	"twin-dead-overlay",
	"twin-waiting-done",
	"working-tempo-blocked",
}

// ttNotScenarios is every directory under roots/ that is deliberately NOT in
// the tt set, and why.
var ttNotScenarios = map[string]string{
	"magmux-corpus": "a byte-identical copy of internal/obs/testdata, which the magmux set (C0.6) already pins",
	"agent-color":   "D2's colour transcripts with no registry: not a tt scenario, and a fleet of nothing under the old code",
}

// ttLivenessWithoutFile answers for the one scenario tt has no reference for,
// and so no recorded .liveness.json (PROVENANCE.md: NOT VERIFIED by
// construction). Its scenario-table row names no dead pid, so every pid it
// names is alive: the two filenames and the body pid.
var ttLivenessWithoutFile = map[string]goldenOracle{
	"k3-duplicate-99-100": {99: model.LivenessYes, 100: model.LivenessYes, 200: model.LivenessYes},
}

// goldenOracle is the fake liveness oracle: it answers only for the pids the
// scenario's own list names, and undetermined for any other. It probes
// nothing.
type goldenOracle map[int]model.Liveness

func (o goldenOracle) Alive(pid int) model.Liveness {
	if l, ok := o[pid]; ok {
		return l
	}
	return model.LivenessUnknown
}

// livenessFile is the path, relative to ttCorpus, of a scenario's recorded
// pid list.
func livenessFile(scenario string) string {
	return "tt-reference/" + scenario + ".liveness.json"
}

// ttOracle loads a scenario's oracle from its .liveness.json, the dead/alive
// list tt was recorded against, or from ttLivenessWithoutFile.
func ttOracle(t *testing.T, scenario string) goldenOracle {
	t.Helper()
	if o, ok := ttLivenessWithoutFile[scenario]; ok {
		return o
	}
	raw, err := os.ReadFile(filepath.Join(ttCorpus, livenessFile(scenario)))
	if err != nil {
		t.Fatalf("scenario %s: %v (list it in ttLivenessWithoutFile, with the PROVENANCE row that says why)", scenario, err)
	}
	var rec struct {
		Liveness map[string]string `json:"liveness"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("scenario %s: %s does not decode: %v", scenario, livenessFile(scenario), err)
	}
	o := goldenOracle{}
	for k, v := range rec.Liveness {
		pid, err := strconv.Atoi(k)
		if err != nil {
			t.Fatalf("scenario %s: pid %q is not a number", scenario, k)
		}
		switch v {
		case "alive":
			o[pid] = model.LivenessYes
		case "dead":
			o[pid] = model.LivenessNo
		default:
			t.Fatalf("scenario %s: pid %d is %q, want alive or dead", scenario, pid, v)
		}
	}
	return o
}

// ttInputs is every path under ttCorpus the tt goldens read: each scenario's
// root and its recorded liveness list.
func ttInputs() []string {
	var out []string
	for _, s := range ttScenarios {
		out = append(out, "roots/"+s)
		if _, ok := ttLivenessWithoutFile[s]; !ok {
			out = append(out, livenessFile(s))
		}
	}
	return out
}

// ttGoldenScenarios builds the tt set's scenarios: the registry, the jobs and
// the transcripts, as `magmux agents --root <s>/sessions --jobs <s>/jobs
// --projects <s>/projects --once --json` reads them, with the fake oracle. A
// scenario with no sessions/, jobs/ or projects/ directory is read as exactly
// that: an absent source.
func ttGoldenScenarios(t *testing.T) []goldenScenario {
	t.Helper()
	out := make([]goldenScenario, 0, len(ttScenarios))
	for _, s := range ttScenarios {
		root := filepath.Join(ttCorpus, "roots", s)
		oracle := ttOracle(t, s)
		out = append(out, goldenScenario{
			name: s,
			now:  ttGoldenNow,
			cfg: func() pipeline.Config {
				return pipeline.Config{
					Root: ports.Root{
						Sessions: canonicalTree(root, "sessions"),
						Jobs:     canonicalTree(root, "jobs"),
						Projects: canonicalTree(root, "projects"),
					},
					Oracle: oracle,
				}
			},
		})
	}
	return out
}

// checkTTScenarioSet holds ttScenarios and ttNotScenarios against the
// directories that exist: every root is in exactly one of them, and every
// listed scenario exists.
func checkTTScenarioSet(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(ttCorpus, "roots"))
	if err != nil {
		t.Fatalf("listing the scenario roots: %v", err)
	}
	listed := map[string]bool{}
	for _, s := range ttScenarios {
		if _, both := ttNotScenarios[s]; both {
			t.Errorf("scenario %s is both pinned and excluded", s)
		}
		listed[s] = true
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		onDisk[e.Name()] = true
		if _, excluded := ttNotScenarios[e.Name()]; !listed[e.Name()] && !excluded {
			t.Errorf("scenario root %s has no tt golden: add it to ttScenarios and re-record, or say in ttNotScenarios why not", e.Name())
		}
	}
	for s := range listed {
		if !onDisk[s] {
			t.Errorf("scenario %s is in ttScenarios but roots/%s does not exist", s, s)
		}
	}
}

// ── the sets ────────────────────────────────────────────────────────────────

// goldenSet is one golden directory and what it was recorded over.
type goldenSet struct {
	name      string
	dir       string
	corpus    string
	inputs    []string // trees or files under corpus that MANIFEST covers
	header    []string // MANIFEST's comment lines
	scenarios func(t *testing.T) []goldenScenario
	// sourced: MANIFEST names the commit that recorded it and every
	// scenario -> file pair, and the test holds both.
	sourced bool
	// planted is the golden the guard self-tests edit, and victim the input
	// file they flip a byte of.
	planted, victim string
}

var magmuxSet = goldenSet{
	name:   "magmux",
	dir:    goldenDir,
	corpus: corpus,
	inputs: goldenInputs,
	header: []string{
		"SHA-256 of every input file the goldens in this directory were recorded over,",
		"relative to internal/obs/testdata. TestWireGoldens fails if any of them changes.",
	},
	scenarios: func(*testing.T) []goldenScenario { return goldenScenarios },
	planted:   "registry.json",
	victim:    "sessions/1724.json",
}

var ttSet = goldenSet{
	name:   "tt",
	dir:    ttGoldenDir,
	corpus: ttCorpus,
	inputs: ttInputs(),
	header: []string{
		"SHA-256 of every input file the goldens in this directory were recorded over,",
		"relative to claudestate/testdata: each tt scenario root and the liveness list",
		"its fake oracle answers from. TestWireGoldens fails if any of them changes,",
		"if the scenario set moves, or if a golden here is not listed below.",
		"Recorded by the PRE-MOVE code in internal/obs, which reads no jobs/ and no",
		"agents listing: no job rows. The rows and members the library adds are D6.",
	},
	scenarios: ttGoldenScenarios,
	sourced:   true,
	planted:   "tt-corpus.json",
	victim:    "roots/tt-corpus/sessions/live-busy.json",
}

var goldenSets = []goldenSet{magmuxSet, ttSet}

// goldenRetry is the torn-read retry the golden runs use: the corpus holds
// torn records on purpose, and the retry waits on the real clock (the
// pipeline blocks its cycle on it), so it is kept short.
const goldenRetry = time.Millisecond

// firstDocument starts a pipeline with cfg (plus the epoch now, the real
// clock for the torn-read retry, and no watcher), takes its first document,
// and stops it.
func firstDocument(t *testing.T, cfg pipeline.Config, now time.Time) pipeline.Delivery {
	t.Helper()
	cfg.Watch = false
	cfg.Now = func() time.Time { return now }
	cfg.Clock = ports.RealClock()
	cfg.RetryDelay = goldenRetry
	p, err := pipeline.Start(cfg)
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	defer p.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sub, err := p.Subscribe(ctx, "golden", 1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	select {
	case d, ok := <-sub.C():
		if !ok {
			t.Fatal("the pipeline stopped before it published")
		}
		return d
	case <-ctx.Done():
		t.Fatal("no document within 20s")
	}
	return pipeline.Delivery{}
}

// wireOf renders a document exactly as `magmux agents --json` does.
func wireOf(doc Fleet) []byte {
	var b bytes.Buffer
	WriteFleetJSON(&b, doc)
	return b.Bytes()
}

// ── MANIFEST ────────────────────────────────────────────────────────────────

// hashInputs returns path -> sha256 for every file under the inputs (each a
// tree or a single file), paths relative to root and slash-separated.
func hashInputs(t *testing.T, root string, inputs []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, in := range inputs {
		err := filepath.WalkDir(filepath.Join(root, in), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
			return nil
		})
		if err != nil {
			t.Fatalf("hashing %s: %v", in, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("hashed no input files; the corpus is not where the goldens expect it")
	}
	return out
}

// manifest is a parsed MANIFEST. source and scenarios are empty for a set
// that does not record them.
type manifest struct {
	hashes    map[string]string // input path -> sha256
	source    string            // the commit whose code recorded the goldens
	scenarios map[string]string // scenario -> golden file
}

const (
	manifestSource   = "source  "
	manifestScenario = "scenario  "
)

func renderManifest(header []string, m manifest) []byte {
	var b strings.Builder
	for _, h := range header {
		b.WriteString("# " + h + "\n")
	}
	if m.source != "" {
		b.WriteString(manifestSource + m.source + "\n")
	}
	names := make([]string, 0, len(m.scenarios))
	for n := range m.scenarios {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "%s%s  %s\n", manifestScenario, n, m.scenarios[n])
	}
	paths := make([]string, 0, len(m.hashes))
	for p := range m.hashes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(&b, "%s  %s\n", m.hashes[p], p)
	}
	return []byte(b.String())
}

func parseManifest(b []byte) (manifest, error) {
	m := manifest{hashes: map[string]string{}, scenarios: map[string]string{}}
	for i, line := range strings.Split(string(b), "\n") {
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, manifestSource):
			m.source = strings.TrimPrefix(line, manifestSource)
			continue
		case strings.HasPrefix(line, manifestScenario):
			name, file, ok := strings.Cut(strings.TrimPrefix(line, manifestScenario), "  ")
			if !ok || name == "" || file == "" {
				return manifest{}, fmt.Errorf("MANIFEST line %d is not \"scenario  <name>  <file>\": %q", i+1, line)
			}
			m.scenarios[name] = file
			continue
		}
		sum, path, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 || path == "" {
			return manifest{}, fmt.Errorf("MANIFEST line %d is not \"<sha256>  <path>\": %q", i+1, line)
		}
		m.hashes[path] = sum
	}
	return m, nil
}

// manifestDiff names every input that was added, removed or changed since the
// MANIFEST was recorded. Empty means the inputs are byte-identical.
func manifestDiff(recorded, current map[string]string) []string {
	var out []string
	for p, sum := range recorded {
		switch now, ok := current[p]; {
		case !ok:
			out = append(out, "removed: "+p)
		case now != sum:
			out = append(out, "changed: "+p)
		}
	}
	for p := range current {
		if _, ok := recorded[p]; !ok {
			out = append(out, "added: "+p)
		}
	}
	sort.Strings(out)
	return out
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// scenarioFiles is the scenario -> golden file map a set's scenarios give.
func scenarioFiles(scs []goldenScenario) map[string]string {
	out := make(map[string]string, len(scs))
	for _, sc := range scs {
		out[sc.name] = sc.name + ".json"
	}
	return out
}

// ── the field diff ──────────────────────────────────────────────────────────

// wireDiff is one difference between a golden and the current document.
type wireDiff struct {
	row   string // the row's key (rowKey), or "" for the document itself
	field string // a member name, or rowAdded / rowRemoved / rowOrder / "epoch"
	text  string // what changed, for a person
}

// The pseudo-fields a whole-row difference is cited under.
const (
	rowAdded   = "+row" // a row the golden does not have
	rowRemoved = "-row" // a golden row the document no longer has
	rowOrder   = "*order"
)

// citationKey is how a difference is named in wirechanges.go:
// "<golden>|<row>|<field>", <golden> being the golden file's name without
// .json (the scenario), which is unique across both sets.
func citationKey(golden string, d wireDiff) string {
	return golden + "|" + d.row + "|" + d.field
}

// rowKeys names every row of a document, in order.
//
// A row is "<id>/<pid>", plus "/job:<job>" when it carries the job member, and
// "#<n>" for the n-th later row with the same name. The wire's id is not
// unique on its own: an id-less row has "", a resumed job shares its id with
// the job it resumed, and two id-less job rows share everything but the
// directory, which the wire does not carry. The occurrence suffix is
// deterministic because the document's order is (Fleet.Sort).
func rowKeys(list []map[string]json.RawMessage) []string {
	seen := map[string]int{}
	out := make([]string, len(list))
	for i, s := range list {
		var id, job string
		_ = json.Unmarshal(s["id"], &id)
		_ = json.Unmarshal(s["job"], &job)
		k := id + "/" + string(s["pid"])
		if job != "" {
			k += "/job:" + job
		}
		if n := seen[k]; n > 0 {
			seen[k] = n + 1
			k += "#" + strconv.Itoa(n)
		} else {
			seen[k] = 1
		}
		out[i] = k
	}
	return out
}

// goldenDiff compares two wire documents row by row and field by field.
//
// The order of the rows is part of the wire too: the rows present in both
// must keep their relative order. A row added or removed is one difference,
// cited as a row; its fields are not compared, because there is nothing to
// compare them with.
func goldenDiff(want, got []byte) ([]wireDiff, error) {
	type doc struct {
		Epoch    json.RawMessage              `json:"epoch"`
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	var w, g doc
	if err := json.Unmarshal(want, &w); err != nil {
		return nil, fmt.Errorf("the golden is not a document: %w", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		return nil, fmt.Errorf("the output is not a document: %w", err)
	}
	var out []wireDiff
	if !bytes.Equal(w.Epoch, g.Epoch) {
		out = append(out, wireDiff{field: "epoch", text: fmt.Sprintf("epoch: %s -> %s", w.Epoch, g.Epoch)})
	}
	wk, gk := rowKeys(w.Sessions), rowKeys(g.Sessions)
	wm, gm := map[string]map[string]json.RawMessage{}, map[string]map[string]json.RawMessage{}
	for i, k := range wk {
		wm[k] = w.Sessions[i]
	}
	for i, k := range gk {
		gm[k] = g.Sessions[i]
	}
	for _, k := range wk {
		ws := wm[k]
		gs, ok := gm[k]
		if !ok {
			out = append(out, wireDiff{row: k, field: rowRemoved, text: "session " + k + " is missing"})
			continue
		}
		fields := map[string]bool{}
		for f := range ws {
			fields[f] = true
		}
		for f := range gs {
			fields[f] = true
		}
		for f := range fields {
			if bytes.Equal(ws[f], gs[f]) {
				continue
			}
			out = append(out, wireDiff{row: k, field: f,
				text: fmt.Sprintf("session %s field %q: %s -> %s", k, f, orAbsent(ws[f]), orAbsent(gs[f]))})
		}
	}
	for _, k := range gk {
		if _, ok := wm[k]; !ok {
			out = append(out, wireDiff{row: k, field: rowAdded, text: "session " + k + " is new"})
		}
	}
	var wOrder, gOrder []string
	for _, k := range wk {
		if _, ok := gm[k]; ok {
			wOrder = append(wOrder, k)
		}
	}
	for _, k := range gk {
		if _, ok := wm[k]; ok {
			gOrder = append(gOrder, k)
		}
	}
	if !reflect.DeepEqual(wOrder, gOrder) {
		out = append(out, wireDiff{field: rowOrder, text: fmt.Sprintf("the sessions are in a different order: %v -> %v", wOrder, gOrder)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].row != out[j].row {
			return out[i].row < out[j].row
		}
		return out[i].field < out[j].field
	})
	return out, nil
}

// uncited is every difference wirechanges.go does not cite for this golden,
// and the citations it used.
func uncited(golden string, diffs []wireDiff, cited map[string]string) (missing []string, used []string) {
	for _, d := range diffs {
		k := citationKey(golden, d)
		if _, ok := cited[k]; ok {
			used = append(used, k)
			continue
		}
		missing = append(missing, d.text+"  [cite as "+strconv.Quote(k)+"]")
	}
	return missing, used
}

func orAbsent(b json.RawMessage) string {
	if b == nil {
		return "(absent)"
	}
	return string(b)
}

// ── the tests ───────────────────────────────────────────────────────────────

// TestWireGoldens is H6, over both golden sets. Every difference must be cited,
// and every citation must name a difference that exists.
func TestWireGoldens(t *testing.T) {
	checkTTScenarioSet(t)
	used := map[string]bool{}
	for _, set := range goldenSets {
		t.Run(set.name, func(t *testing.T) {
			for _, k := range testGoldenSet(t, set) {
				used[k] = true
			}
		})
	}
	if t.Failed() {
		return // a stale citation is only meaningful once every golden ran clean
	}
	for k, why := range wireChanges {
		if !used[k] {
			t.Errorf("wirechanges.go cites %q (%s), and no golden differs there: a citation must name a real difference, so remove it", k, why)
		}
		if !citedRow.MatchString(why) {
			t.Errorf("wirechanges.go cites %q as %q, which names no decision row (want it to begin \"D<n>\")", k, why)
		}
	}
}

// citedRow is what every citation begins with: the decision row that made the
// difference, D1..D15 with an optional suffix (D6-lone, D12c).
var citedRow = regexp.MustCompile(`^D(1[0-5]|[1-9])[a-z]?(-[a-z]+)?\b`)

func testGoldenSet(t *testing.T, set goldenSet) []string {
	scenarios := set.scenarios(t)
	current := manifest{hashes: hashInputs(t, set.corpus, set.inputs)}
	if set.sourced {
		current.scenarios = scenarioFiles(scenarios)
	}
	manifestPath := filepath.Join(set.dir, "MANIFEST")
	if *update {
		if set.sourced {
			if !fullSHA.MatchString(*goldenSource) {
				t.Fatalf("re-recording the %s goldens needs -golden-source <full commit sha> naming the code that records them; got %q", set.name, *goldenSource)
			}
			current.source = *goldenSource
		}
		if err := os.MkdirAll(set.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, renderManifest(set.header, current), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v (record the goldens with -update)", manifestPath, err)
	}
	recorded, err := parseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if diff := manifestDiff(recorded.hashes, current.hashes); len(diff) > 0 {
		t.Fatalf("the goldens' inputs changed since they were recorded, so a difference below could "+
			"be the testdata's rather than the code's:\n  %s\nRe-record deliberately with -update, "+
			"and say why in the same diff.", strings.Join(diff, "\n  "))
	}
	if set.sourced {
		if !fullSHA.MatchString(recorded.source) {
			t.Fatalf("%s names no recording commit (want a %q line with a full sha), got %q", manifestPath, strings.TrimSpace(manifestSource), recorded.source)
		}
		if !reflect.DeepEqual(recorded.scenarios, current.scenarios) {
			t.Fatalf("the scenario set moved since the goldens were recorded:\n MANIFEST %v\n now      %v", recorded.scenarios, current.scenarios)
		}
		// Every golden in the directory is one MANIFEST lists, so a stale
		// file cannot sit there looking pinned.
		files, err := filepath.Glob(filepath.Join(set.dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		listed := map[string]bool{}
		for _, f := range recorded.scenarios {
			listed[f] = true
		}
		for _, f := range files {
			if !listed[filepath.Base(f)] {
				t.Errorf("%s is not a scenario MANIFEST lists", f)
			}
		}
	} else if recorded.source != "" || len(recorded.scenarios) != 0 {
		t.Fatalf("%s carries source or scenario lines, which this set does not record", manifestPath)
	}

	var used []string
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			got := wireOf(FromModel(firstDocument(t, sc.cfg(), sc.now).Doc))
			// Deterministic or worthless: the same inputs twice must give the
			// same bytes, or the golden is pinning noise.
			if again := wireOf(FromModel(firstDocument(t, sc.cfg(), sc.now).Doc)); !bytes.Equal(got, again) {
				t.Fatalf("two runs over the same corpus produced different documents:\n%s\n---\n%s", got, again)
			}
			path := filepath.Join(set.dir, sc.name+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			diffs, err := goldenDiff(want, got)
			if err != nil {
				t.Fatal(err)
			}
			missing, u := uncited(sc.name, diffs, wireChanges)
			used = append(used, u...)
			if len(missing) > 0 {
				t.Errorf("the wire changed, and wirechanges.go cites none of it:\n  %s", strings.Join(missing, "\n  "))
			}
			if len(diffs) == 0 && !bytes.Equal(want, got) {
				t.Errorf("the document differs from %s in its bytes and in no field, row or order; "+
					"the field diff is missing a case", path)
			}
		})
	}
	return used
}

// TestTheGoldenGuardCatchesAPlantedChange: for each set, the field diff must
// see one field of one session move, and must let it through only when that
// exact (golden, row, field) is cited.
func TestTheGoldenGuardCatchesAPlantedChange(t *testing.T) {
	for _, set := range goldenSets {
		t.Run(set.name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(set.dir, set.planted))
			if err != nil {
				t.Fatalf("reading the golden: %v", err)
			}
			var doc Fleet
			if err := json.Unmarshal(want, &doc); err != nil {
				t.Fatalf("the golden does not decode: %v", err)
			}
			if len(doc.Sessions) == 0 {
				t.Fatal("the golden has no sessions; there is nothing to plant a change in")
			}
			if !bytes.Equal(wireOf(doc), want) {
				t.Fatal("the golden does not survive a decode and re-encode, so a plant in it would move more than one field")
			}
			golden := strings.TrimSuffix(set.planted, ".json")
			doc.Sessions[0].Name = doc.Sessions[0].Name + "-planted"
			planted := wireOf(doc)

			diffs, err := goldenDiff(want, planted)
			if err != nil {
				t.Fatal(err)
			}
			missing, _ := uncited(golden, diffs, map[string]string{})
			if len(missing) != 1 || !strings.Contains(missing[0], `"name"`) {
				t.Fatalf("a planted name change produced %v, want exactly one uncited difference on name", missing)
			}
			exact := map[string]string{citationKey(golden, diffs[0]): "D-test"}
			if missing, _ := uncited(golden, diffs, exact); len(missing) != 0 {
				t.Errorf("a CITED change was still reported: %v", missing)
			}
			// A citation for another golden, row or field covers nothing.
			for _, wrong := range []wireDiff{
				{row: diffs[0].row, field: "status"},
				{row: diffs[0].row + "x", field: diffs[0].field},
			} {
				if missing, _ := uncited(golden, diffs, map[string]string{citationKey(golden, wrong): "D-test"}); len(missing) != 1 {
					t.Errorf("a citation of %+v covered the name change", wrong)
				}
			}
			if missing, _ := uncited("another-golden", diffs, exact); len(missing) != 1 {
				t.Error("a citation for one golden covered the same change in another")
			}
			if diffs, _ := goldenDiff(want, want); len(diffs) != 0 {
				t.Errorf("the golden differs from itself: %v", diffs)
			}
		})
	}
}

// TestTheGoldenGuardSeesRowsAndOrder: an added row, a removed row and a moved
// row are each a difference of their own, and a row's identity includes its
// job so two rows on one id are two rows.
func TestTheGoldenGuardSeesRowsAndOrder(t *testing.T) {
	one := func(id string, pid int, job string) Session { return Session{ID: id, PID: pid, Job: job} }
	doc := func(ss ...Session) []byte { return wireOf(Fleet{Epoch: goldenNow, Sessions: ss}) }
	base := doc(one("a", 1, ""), one("b", 2, ""))

	kinds := func(d []wireDiff) []string {
		var out []string
		for _, x := range d {
			out = append(out, x.row+" "+x.field)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		got  []byte
		want []string
	}{
		{"an added job row on an existing id", doc(one("a", 1, ""), one("a", 0, "j1"), one("b", 2, "")), []string{"a/0/job:j1 " + rowAdded}},
		{"a removed row", doc(one("b", 2, "")), []string{"a/1 " + rowRemoved}},
		{"a moved row", doc(one("b", 2, ""), one("a", 1, "")), []string{" " + rowOrder}},
		{"two id-less rows are two rows", doc(one("a", 1, ""), one("b", 2, ""), one("", 0, ""), one("", 0, "")), []string{"/0 " + rowAdded, "/0#1 " + rowAdded}},
	} {
		d, err := goldenDiff(base, tc.got)
		if err != nil {
			t.Fatal(err)
		}
		if got := kinds(d); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestTheGoldenGuardCatchesAPlantedInputChange: for each set, one changed
// byte in a copy of its inputs must fail the MANIFEST check, naming the file.
func TestTheGoldenGuardCatchesAPlantedInputChange(t *testing.T) {
	for _, set := range goldenSets {
		t.Run(set.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(set.dir, "MANIFEST"))
			if err != nil {
				t.Fatalf("reading MANIFEST: %v", err)
			}
			recorded, err := parseManifest(raw)
			if err != nil {
				t.Fatal(err)
			}

			tmp := t.TempDir()
			for _, in := range set.inputs {
				src, dst := filepath.Join(set.corpus, in), filepath.Join(tmp, in)
				st, err := os.Stat(src)
				if err != nil {
					t.Fatal(err)
				}
				if st.IsDir() {
					err = os.CopyFS(dst, os.DirFS(src))
				} else {
					var b []byte
					if b, err = os.ReadFile(src); err == nil {
						if err = os.MkdirAll(filepath.Dir(dst), 0o755); err == nil {
							err = os.WriteFile(dst, b, 0o644)
						}
					}
				}
				if err != nil {
					t.Fatalf("copying %s: %v", in, err)
				}
			}
			if diff := manifestDiff(recorded.hashes, hashInputs(t, tmp, set.inputs)); len(diff) != 0 {
				t.Fatalf("an untouched copy of the inputs already differs from MANIFEST: %v", diff)
			}

			victim := filepath.Join(tmp, filepath.FromSlash(set.victim))
			b, err := os.ReadFile(victim)
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)/2] ^= 0x01
			if err := os.WriteFile(victim, b, 0o644); err != nil {
				t.Fatal(err)
			}
			diff := manifestDiff(recorded.hashes, hashInputs(t, tmp, set.inputs))
			if len(diff) != 1 || diff[0] != "changed: "+set.victim {
				t.Errorf("a one-byte change to %s produced %v", set.victim, diff)
			}
		})
	}
}
