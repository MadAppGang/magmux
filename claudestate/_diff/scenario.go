package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/internal/difftest"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/proclive"
)

// corpusPrefix is the canonical projects path every scrubbed linkScanPath
// names (§8.1); the sandbox rewrites it to its own root.
const corpusPrefix = "/testdata/projects"

// advCorroborationWaitExpired is tt's advisory for a rebuild that answered
// without its agents listing (tt:internal/observer/advisories.go:120). With a
// listing in play the comparison is then not of like with like.
const advCorroborationWaitExpired = "corroboration-wait-expired"

// errRetry marks a failure that one re-run of the scenario may clear.
type errRetry struct{ why string }

func (e errRetry) Error() string { return e.why }

// runScenario is §8.2's testdata mode for one scenario, with its one re-run.
func runScenario(sb *sandbox, o options, s difftest.Scenario, rec *recorder) outcome {
	out := outcome{name: s.Name}
	var last errRetry
	for attempt := 1; attempt <= 2; attempt++ {
		rep, doc, recorded, err := attemptScenario(sb, o, s, attempt)
		var retry errRetry
		switch {
		case errors.As(err, &retry):
			last = retry
			fmt.Fprintf(os.Stderr, "_diff: %s attempt %d: %s\n", s.Name, attempt, retry.why)
			continue
		case err != nil:
			out.setup = err
			return out
		}
		out.report = rep
		// A reference is recorded only for a scenario whose every difference
		// is cited. The CI test replays it and requires zero uncited; an
		// uncited difference is a finding to fix or report, and recording it
		// would only move the red from this report into CI.
		if rec != nil && len(rep.Uncited) == 0 {
			if err := rec.write(s.Name+".json", doc); err != nil {
				out.setup = err
				return out
			}
			if err := rec.write(s.Name+".liveness.json", recorded.Encode()); err != nil {
				out.setup = err
				return out
			}
			out.recorded = true
		}
		return out
	}
	out.notVerified = last.why + " (twice)"
	return out
}

// attemptScenario runs one scenario once. It returns the report, the
// canonical tt document and its inputs record.
func attemptScenario(sb *sandbox, o options, s difftest.Scenario, attempt int) (difftest.Report, []byte, difftest.Recorded, error) {
	var zero difftest.Report
	srcRoot := filepath.Join(o.module, "testdata", "roots", s.Root)
	if _, err := os.Stat(srcRoot); err != nil {
		return zero, nil, difftest.Recorded{}, err
	}
	src := os.DirFS(srcRoot)
	registry, err := readRegistry(src)
	if err != nil {
		return zero, nil, difftest.Recorded{}, err
	}
	var listing []byte
	if s.Listing != "" {
		if listing, err = os.ReadFile(filepath.Join(o.module, "testdata", filepath.FromSlash(s.Listing))); err != nil {
			return zero, nil, difftest.Recorded{}, err
		}
	}
	canon := difftest.CanonicalPIDs(registry, listing)
	dead := map[int]bool{}
	for _, p := range s.Dead {
		dead[p] = true
	}
	recorded := difftest.NewRecorded(s, canon)

	kids, err := spawnChildren(canon)
	if errors.Is(err, errNotAscending) {
		return zero, nil, recorded, errRetry{"the kernel did not hand out ascending pids"}
	}
	if err != nil {
		return zero, nil, recorded, err
	}
	stuckErr := func() error {
		if stuck := kids.endAll(5 * time.Second); len(stuck) > 0 {
			return fmt.Errorf("children %v did not exit after their stdin closed; they were left running, unsignalled", stuck)
		}
		return nil
	}
	pidMap := kids.pidMap()
	if err := difftest.FilenameOrderKept(difftest.RegistryGroups(registry), pidMap); err != nil {
		if serr := stuckErr(); serr != nil {
			return zero, nil, recorded, serr
		}
		return zero, nil, recorded, errRetry{"filename order: " + err.Error()}
	}
	raced, err := kids.killDead(dead)
	if err != nil {
		_ = stuckErr()
		return zero, nil, recorded, err
	}
	if len(raced) > 0 {
		if serr := stuckErr(); serr != nil {
			return zero, nil, recorded, serr
		}
		return zero, nil, recorded, errRetry{fmt.Sprintf("RACED before tt ran: pids %v did not answer as the scenario says", raced)}
	}

	rep, doc, err := compareScenario(sb, s, attempt, src, registry, listing, kids, dead)
	if serr := stuckErr(); serr != nil {
		return zero, nil, recorded, serr
	}
	return rep, doc, recorded, err
}

// compareScenario builds the scenario's sandbox, runs both sides and compares.
func compareScenario(sb *sandbox, s difftest.Scenario, attempt int, src fs.FS, registry map[string][]byte,
	listing []byte, kids *children, dead map[int]bool) (difftest.Report, []byte, error) {
	var zero difftest.Report
	dir := fmt.Sprintf("s/%s-%d", s.Name, attempt)
	rootRel := dir + "/root"
	rootAbs := sb.path(rootRel)
	pidMap := kids.pidMap()

	if err := sb.copyFS(rootRel, src, func(name string, data []byte) (string, []byte, error) {
		switch {
		case strings.HasPrefix(name, "sessions/"):
			base := path.Base(name)
			to := "sessions/" + difftest.RenameByPID(base, pidMap)
			if strings.HasSuffix(base, ".json") {
				b, err := difftest.RewritePIDs(data, pidMap)
				return to, b, err
			}
			return to, data, nil
		case strings.HasPrefix(name, "jobs/") && path.Base(name) == "state.json":
			return name, difftest.ReplacePrefix(data, corpusPrefix, rootAbs+"/projects"), nil
		}
		return name, data, nil
	}); err != nil {
		return zero, nil, err
	}
	for _, d := range []string{"/home", "/state", "/tmp"} {
		if err := sb.mkdir(dir + d); err != nil {
			return zero, nil, err
		}
	}
	listingAbs := ""
	var rewritten []byte
	if listing != nil {
		b, err := difftest.RewritePIDs(listing, pidMap)
		if err != nil {
			return zero, nil, err
		}
		rewritten = b
		if err := sb.writeFile(dir+"/listing.json", b, 0o600); err != nil {
			return zero, nil, err
		}
		listingAbs = sb.path(dir + "/listing.json")
	}
	if err := stubs(sb, dir, listingAbs); err != nil {
		return zero, nil, err
	}

	env := []string{
		"PATH=" + sb.path(dir+"/bin"),
		"HOME=" + sb.path(dir+"/home"),
		"XDG_STATE_HOME=" + sb.path(dir+"/state"),
		"TMPDIR=" + sb.path(dir+"/tmp"),
		"CLAUDE_CONFIG_DIR=" + rootAbs,
	}
	doc, _, err := runTT(sb, env)
	if err != nil {
		return zero, nil, err
	}
	// Kept beside the scenario for -keep, so a difference can be read at its
	// source.
	if err := sb.writeFile(dir+"/tt.json", doc, 0o600); err != nil {
		return zero, nil, err
	}
	if listing != nil && strings.Contains(string(doc), advCorroborationWaitExpired) {
		return zero, nil, errRetry{"tt answered without its agents listing (" + advCorroborationWaitExpired + ")"}
	}
	ttRows, observedAt, err := difftest.FromTT(doc, rootAbs+"/projects")
	if err != nil {
		return zero, nil, err
	}

	root, err := ports.RootAt(rootAbs)
	if err != nil {
		return zero, nil, err
	}
	var lst fixedListing
	if rewritten != nil {
		lst = fixedListing{raw: rewritten, at: observedAt}
	} else {
		lst = fixedListing{err: errNoClaude}
	}
	d, err := runLibrary(root, proclive.New(), lst, observedAt)
	if err != nil {
		return zero, nil, err
	}

	// The reuse guard (§8.2 step 4): after both sides ran, every dead pid
	// must still be dead.
	if raced := kids.check(dead); len(raced) > 0 {
		return zero, nil, errRetry{fmt.Sprintf("RACED: pids %v changed liveness while tt and the library ran", raced)}
	}

	libRows := difftest.FromModel(d.Doc, d.Problems)
	if b, err := json.MarshalIndent(struct {
		TT, Lib  []difftest.Row
		Problems []string
	}{ttRows, libRows, problemTexts(d.Problems)}, "", "  "); err == nil {
		if err := sb.writeFile(dir+"/rows.json", b, 0o600); err != nil {
			return zero, nil, err
		}
	}
	diffs := difftest.Compare(ttRows, libRows, d.Problems)
	inverse := kids.inverse()
	for i := range diffs {
		diffs[i] = canonicalDiff(diffs[i], inverse)
	}
	rep := difftest.Summarise(diffs)

	canonical, err := difftest.Canonicalise(doc, inverse, []difftest.Rewrite{
		{From: rootAbs, To: canonicalRoot},
		{From: sb.path(dir), To: canonicalSandbox},
	}, sb.dir)
	if err != nil {
		return zero, nil, err
	}
	return rep, canonical, nil
}

// readRegistry reads a scenario's sessions/ entries by base name.
func readRegistry(src fs.FS) (map[string][]byte, error) {
	out := map[string][]byte{}
	entries, err := fs.ReadDir(src, "sessions")
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := fs.ReadFile(src, "sessions/"+e.Name())
		if err != nil {
			return nil, err
		}
		out[e.Name()] = b
	}
	return out, nil
}

// canonicalDiff maps a difference's sandbox pids back for the report.
func canonicalDiff(d difftest.Diff, inverse map[int]int) difftest.Diff {
	if c, ok := inverse[d.Key.PID]; ok {
		d.Key.PID = c
	}
	if d.Field == difftest.FieldPID {
		d.TT, d.Lib = canonicalPIDValue(d.TT, inverse), canonicalPIDValue(d.Lib, inverse)
	}
	if d.Field == difftest.FieldKey {
		d.TT.S, d.Lib.S = canonicalKeyText(d.TT.S, inverse), canonicalKeyText(d.Lib.S, inverse)
	}
	return d
}

func canonicalPIDValue(v difftest.Value, inverse map[int]int) difftest.Value {
	var n int
	if _, err := fmt.Sscanf(v.S, "%d", &n); err == nil {
		if c, ok := inverse[n]; ok {
			v.S = fmt.Sprint(c)
		}
	}
	return v
}

func canonicalKeyText(s string, inverse map[int]int) string {
	head, pid, ok := strings.Cut(s, "/pid:")
	if !ok {
		return s
	}
	var n int
	if _, err := fmt.Sscanf(pid, "%d", &n); err == nil {
		if c, ok := inverse[n]; ok {
			return fmt.Sprintf("%s/pid:%d", head, c)
		}
	}
	return s
}

func problemTexts(ps []model.Problem) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Error()
	}
	return out
}
