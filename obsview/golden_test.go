package obsview

// H6: the wire is pinned. The JSON document `magmux agents --json` prints — the
// same bytes the socket `agents` verb and MCP's list_agents carry — is recorded
// over the magmux-origin corpus (internal/obs/testdata) before the claudestate
// library replaces the code that produces it. Every later difference must be
// cited by decision in wirechanges.go; nothing else may change.
//
// Two things make that a guarantee rather than a hope:
//
//   - each golden directory carries MANIFEST, the SHA-256 of every input file
//     at capture time, and the test fails if any input byte changed — so a
//     code change can never be hidden by a testdata change;
//   - the document is a pure function of the corpus here: a fixed epoch, no
//     liveness oracle, no pane table, no watcher. Nothing reads ~/.claude,
//     probes a pid or depends on when the test runs.
//
// Re-recording is deliberate and visible: go test ./obsview -run Golden -update
// rewrites the goldens AND the MANIFEST, and both show up in the diff.

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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/claudecode"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
)

var update = flag.Bool("update", false, "re-record the wire goldens and their MANIFEST from the current code")

const (
	goldenDir = "testdata/golden/magmux"
	// corpus is the magmux-origin testdata, read where it lives rather than
	// copied, so there is one corpus and the MANIFEST says which bytes of it
	// the goldens were recorded over.
	corpus = "../internal/obs/testdata"
)

// goldenNow is every golden's epoch: the corpus's own reference instant
// (1789545200000 ms, see internal/obs/adapters/instant_test.go), so the
// durations in the document are the ones the corpus was written to produce.
var goldenNow = time.UnixMilli(1789545200000).UTC()

// goldenInputs are the trees every scenario reads. The MANIFEST covers all of
// them, whichever scenario reads which.
var goldenInputs = []string{"sessions", "projects"}

// goldenScenario is one recorded document.
type goldenScenario struct {
	name string
	cfg  func() pipeline.Config
}

var goldenScenarios = []goldenScenario{
	{
		// The registry alone: what `magmux agents --root DIR --once --json`
		// prints with no transcript tree.
		name: "registry",
		cfg: func() pipeline.Config {
			return pipeline.Config{Root: filepath.Join(corpus, "sessions")}
		},
	},
	{
		// The registry and the transcripts, through the Claude Code
		// middleware: the harness-derived columns (subagents, API errors,
		// colour) are determined.
		name: "registry-and-transcripts",
		cfg: func() pipeline.Config {
			return pipeline.Config{
				Root:  filepath.Join(corpus, "sessions"),
				Chain: []pipeline.Factory{claudecode.Middleware(claudecode.Config{Root: filepath.Join(corpus, "projects")})},
			}
		},
	},
}

// firstDocument starts a pipeline with cfg (plus the fixed epoch and no
// watcher), takes its first document, and stops it.
func firstDocument(t *testing.T, cfg pipeline.Config) pipeline.Delivery {
	t.Helper()
	cfg.Watch = false
	cfg.Now = func() time.Time { return goldenNow }
	p, err := pipeline.Start(cfg)
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	defer p.Stop()
	sub, err := p.Subscribe("golden", 1)
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
	case <-time.After(20 * time.Second):
		t.Fatal("no document within 20s")
	}
	return pipeline.Delivery{}
}

// wireOf renders a document exactly as `magmux agents --json` does.
func wireOf(doc obs.Fleet) []byte {
	var b bytes.Buffer
	WriteFleetJSON(&b, doc)
	return b.Bytes()
}

// ── MANIFEST ────────────────────────────────────────────────────────────────

// hashInputs returns path -> sha256 for every file under the input trees,
// paths relative to the corpus root and slash-separated.
func hashInputs(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tree := range goldenInputs {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d fs.DirEntry, err error) error {
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
			t.Fatalf("hashing %s: %v", tree, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("hashed no input files; the corpus is not where the goldens expect it")
	}
	return out
}

func renderManifest(hashes map[string]string) []byte {
	paths := make([]string, 0, len(hashes))
	for p := range hashes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString("# SHA-256 of every input file the goldens in this directory were recorded over,\n")
	b.WriteString("# relative to internal/obs/testdata. TestWireGoldens fails if any of them changes.\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "%s  %s\n", hashes[p], p)
	}
	return []byte(b.String())
}

func parseManifest(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sum, path, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 || path == "" {
			return nil, fmt.Errorf("MANIFEST line %d is not \"<sha256>  <path>\": %q", i+1, line)
		}
		out[path] = sum
	}
	return out, nil
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

// ── the field diff ──────────────────────────────────────────────────────────

// goldenDiff compares two wire documents field by field and returns every
// difference wireChanges does not cite. A session is identified by "id/pid",
// because the wire's id is not unique on its own (an id-less row has "").
func goldenDiff(want, got []byte, cited map[string]string) ([]string, error) {
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
	var out []string
	if !bytes.Equal(w.Epoch, g.Epoch) {
		out = append(out, fmt.Sprintf("epoch: %s -> %s", w.Epoch, g.Epoch))
	}
	key := func(s map[string]json.RawMessage) (string, string) {
		var id string
		_ = json.Unmarshal(s["id"], &id)
		return id + "/" + string(s["pid"]), id
	}
	byKey := func(list []map[string]json.RawMessage) map[string]map[string]json.RawMessage {
		m := map[string]map[string]json.RawMessage{}
		for _, s := range list {
			k, _ := key(s)
			m[k] = s
		}
		return m
	}
	wm, gm := byKey(w.Sessions), byKey(g.Sessions)
	isCited := func(id, field string) bool {
		_, one := cited[id+"/"+field]
		_, all := cited["*/"+field]
		return one || all
	}
	for k, ws := range wm {
		gs, ok := gm[k]
		_, id := key(ws)
		if !ok {
			if !isCited(id, "*row") {
				out = append(out, "session "+k+" is missing")
			}
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
			if bytes.Equal(ws[f], gs[f]) || isCited(id, f) {
				continue
			}
			out = append(out, fmt.Sprintf("session %s field %q: %s -> %s", k, f, orAbsent(ws[f]), orAbsent(gs[f])))
		}
	}
	for k, gs := range gm {
		if _, ok := wm[k]; !ok {
			if _, id := key(gs); !isCited(id, "*row") {
				out = append(out, "session "+k+" is new")
			}
		}
	}
	// The ORDER of the sessions is part of the wire too.
	if len(out) == 0 && !reflect.DeepEqual(order(w.Sessions, key), order(g.Sessions, key)) {
		out = append(out, "the sessions are in a different order")
	}
	sort.Strings(out)
	return out, nil
}

func order(list []map[string]json.RawMessage, key func(map[string]json.RawMessage) (string, string)) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i], _ = key(s)
	}
	return out
}

func orAbsent(b json.RawMessage) string {
	if b == nil {
		return "(absent)"
	}
	return string(b)
}

// ── the tests ───────────────────────────────────────────────────────────────

// TestWireGoldens is H6.
func TestWireGoldens(t *testing.T) {
	current := hashInputs(t, corpus)
	manifestPath := filepath.Join(goldenDir, "MANIFEST")
	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, renderManifest(current), 0o644); err != nil {
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
	if diff := manifestDiff(recorded, current); len(diff) > 0 {
		t.Fatalf("the goldens' inputs changed since they were recorded, so a difference below could "+
			"be the testdata's rather than the code's:\n  %s\nRe-record deliberately with -update, "+
			"and say why in the same diff.", strings.Join(diff, "\n  "))
	}

	for _, sc := range goldenScenarios {
		t.Run(sc.name, func(t *testing.T) {
			got := wireOf(firstDocument(t, sc.cfg()).Doc)
			// Deterministic or worthless: the same inputs twice must give the
			// same bytes, or the golden is pinning noise.
			if again := wireOf(firstDocument(t, sc.cfg()).Doc); !bytes.Equal(got, again) {
				t.Fatalf("two runs over the same corpus produced different documents:\n%s\n---\n%s", got, again)
			}
			path := filepath.Join(goldenDir, sc.name+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			if bytes.Equal(want, got) {
				return
			}
			diff, err := goldenDiff(want, got, wireChanges)
			if err != nil {
				t.Fatal(err)
			}
			if len(diff) > 0 {
				t.Errorf("the wire changed, and wirechanges.go cites none of it:\n  %s", strings.Join(diff, "\n  "))
			}
		})
	}
}

// TestTheGoldenGuardCatchesAPlantedChange: the field diff must see one field
// of one session move, and must let it through only when it is cited.
func TestTheGoldenGuardCatchesAPlantedChange(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(goldenDir, "registry.json"))
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	var doc obs.Fleet
	if err := json.Unmarshal(want, &doc); err != nil {
		t.Fatalf("the golden does not decode: %v", err)
	}
	if len(doc.Sessions) == 0 {
		t.Fatal("the golden has no sessions; there is nothing to plant a change in")
	}
	doc.Sessions[0].Name = doc.Sessions[0].Name + "-planted"
	planted := wireOf(doc)

	diff, err := goldenDiff(want, planted, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff) != 1 || !strings.Contains(diff[0], `"name"`) {
		t.Fatalf("a planted name change produced %v, want exactly one uncited difference on name", diff)
	}
	cited := map[string]string{doc.Sessions[0].ID + "/name": "D-test"}
	if diff, _ := goldenDiff(want, planted, cited); len(diff) != 0 {
		t.Errorf("a CITED change was still reported: %v", diff)
	}
	if diff, _ := goldenDiff(want, want, map[string]string{}); len(diff) != 0 {
		t.Errorf("the golden differs from itself: %v", diff)
	}
}

// TestTheGoldenGuardCatchesAPlantedInputChange: one changed byte in a copy of
// the corpus must fail the MANIFEST check, naming the file.
func TestTheGoldenGuardCatchesAPlantedInputChange(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(goldenDir, "MANIFEST"))
	if err != nil {
		t.Fatalf("reading MANIFEST: %v", err)
	}
	recorded, err := parseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	for _, tree := range goldenInputs {
		if err := os.CopyFS(filepath.Join(tmp, tree), os.DirFS(filepath.Join(corpus, tree))); err != nil {
			t.Fatalf("copying %s: %v", tree, err)
		}
	}
	if diff := manifestDiff(recorded, hashInputs(t, tmp)); len(diff) != 0 {
		t.Fatalf("an untouched copy of the corpus already differs from MANIFEST: %v", diff)
	}

	victim := filepath.Join(tmp, "sessions", "1724.json")
	b, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x01
	if err := os.WriteFile(victim, b, 0o644); err != nil {
		t.Fatal(err)
	}
	diff := manifestDiff(recorded, hashInputs(t, tmp))
	if len(diff) != 1 || diff[0] != "changed: sessions/1724.json" {
		t.Errorf("a one-byte change to sessions/1724.json produced %v", diff)
	}
}

// ── the audit line ──────────────────────────────────────────────────────────

// goldenPanes is the pane table the recorded focus runs against: the target
// session's pid is in pane 3, and pane 1 held focus before. Nothing else is in
// any pane. No process is involved: the pids are the corpus's.
type goldenPanes struct{ pid int }

func (g goldenPanes) PaneFor(pid int) (int, obs.Liveness) {
	if pid == g.pid {
		return 3, obs.LivenessYes
	}
	return 0, obs.LivenessNo
}

func (goldenPanes) PaneAlive(id int) obs.Liveness {
	if id == 3 {
		return obs.LivenessYes
	}
	return obs.LivenessNo
}

func (goldenPanes) Focus(id int) (int, error) {
	if id != 3 {
		return -1, fmt.Errorf("no pane %d", id)
	}
	return 1, nil
}

// TestAnAuditLineIsByteIdenticalAcrossTheMove records the audit trail of one
// focus — the attempt line and its outcome — through the real pipeline, gate
// and writer, over the corpus, with a fixed clock and pid. The library move
// re-points act at the library's session type; this golden is what says the
// trail it writes is the same bytes as before (claudestate architecture §2.2).
func TestAnAuditLineIsByteIdenticalAcrossTheMove(t *testing.T) {
	const targetPID = 1724
	const targetID = "8e2d4b17-0c93-4a6f-b512-77c3ad9e4f60"

	trail := filepath.Join(t.TempDir(), "trail.jsonl")
	w, err := act.NewWriter(act.WriterConfig{Path: trail, Now: func() time.Time { return goldenNow }, PID: 4242})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	panes := goldenPanes{pid: targetPID}
	cfg := pipeline.Config{
		Root:    filepath.Join(corpus, "sessions"),
		Chain:   []pipeline.Factory{claudecode.Middleware(claudecode.Config{Root: filepath.Join(corpus, "projects")})},
		Locator: panes,
		Focuser: panes,
		Policy:  act.Default(),
		Audit:   w,
		Watch:   false,
		Now:     func() time.Time { return goldenNow },
	}
	p, err := pipeline.Start(cfg)
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	defer p.Stop()
	sub, err := p.Subscribe("golden", 1)
	if err != nil {
		t.Fatal(err)
	}
	p.Refresh()
	select {
	case <-sub.C():
	case <-time.After(20 * time.Second):
		t.Fatal("no document within 20s")
	}
	if _, err := p.Do(context.Background(), act.Request{
		Action: act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate, Session: targetID},
		By:     act.RequesterCLI,
	}); err != nil {
		t.Fatalf("the recorded focus was refused: %v", err)
	}
	got, err := os.ReadFile(trail)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	if n := bytes.Count(got, []byte("\n")); n != 2 {
		t.Fatalf("the trail has %d lines, want the attempt and its outcome:\n%s", n, got)
	}

	path := filepath.Join(goldenDir, "audit-focus.jsonl")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("the audit trail of a recorded focus changed:\n got %s\nwant %s", got, want)
	}
}
