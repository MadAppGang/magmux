// Command _diff is the differential check against tt's UNMODIFIED observer
// (architecture §8.2). It is run by hand and is never a test: `./...` skips a
// directory whose name begins with "_".
//
//	go run ./_diff (-tt-repo DIR [-tt-rev REV] | -tt-tar FILE | -tt-src DIR)
//	               (-corpus SCENARIO | -all-corpus | -live) [-record] [-real-agents] [-keep]
//
// tt comes from git OBJECTS, never a working tree: -tt-repo runs
// `git archive REV:tools/tt` (GIT_OPTIONAL_LOCKS=0) on that repository;
// -tt-tar takes a tarball an operator made the same way; -tt-src takes a tree
// already extracted from one. Whichever, it is copied into a fresh sandbox and
// built there, and nothing is ever written into the tt repository.
//
// Testdata mode runs tt and the library over one scenario root copied into
// the sandbox, with every canonical pid rewritten to a /bin/cat child this
// driver owns (alive) or has already reaped (dead), and compares the two
// through internal/difftest. -record maps tt's document back to canonical form
// and writes it, with its inputs, into testdata/tt-reference/, which is what
// the CI test internal/difftest/reference_test.go replays.
//
// Live mode reads the real Claude root READ-ONLY (it is implemented here and
// run only as validation V2).
//
// Exit codes: 0 every difference is cited; 1 at least one uncited difference;
// 2 a setup error (archive, build, a child that would not exit); 3 no uncited
// difference, but at least one scenario NOT VERIFIED or RACED that its one
// re-run did not clear. NOT VERIFIED is never PASS.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MadAppGang/magmux/claudestate/internal/difftest"
)

const (
	exitPass        = 0
	exitUncited     = 1
	exitSetup       = 2
	exitNotVerified = 3

	referenceDir = "tt-reference"
	// canonicalRoot is what a sandbox root is recorded as: the corpus's
	// canonical prefix, so transcript.path records as /testdata/projects/….
	canonicalRoot = "/testdata"
	// canonicalSandbox stands for the scenario directory around the root in
	// a recorded document (tt's state directory and the like).
	canonicalSandbox = "/sandbox"
)

func main() { os.Exit(run(os.Args[1:])) }

type options struct {
	tt         ttSource
	corpus     string
	all, live  bool
	record     bool
	realAgents bool
	keep       bool
	verbose    bool
	module     string
}

func run(args []string) int {
	fs := flag.NewFlagSet("_diff", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.tt.repo, "tt-repo", "", "the repository holding tools/tt; read with git archive only")
	fs.StringVar(&o.tt.rev, "tt-rev", ttDefaultRev, "the tt commit")
	fs.StringVar(&o.tt.tar, "tt-tar", "", "a tarball of tools/tt made with git archive")
	fs.StringVar(&o.tt.src, "tt-src", "", "a tools/tt tree already extracted from git archive")
	fs.StringVar(&o.corpus, "corpus", "", "run one scenario")
	fs.BoolVar(&o.all, "all-corpus", false, "run every scenario")
	fs.BoolVar(&o.live, "live", false, "compare against the real Claude root, read-only")
	fs.BoolVar(&o.record, "record", false, "write the canonical tt references into testdata/tt-reference")
	fs.BoolVar(&o.realAgents, "real-agents", false, "live mode: let tt run the real `claude agents --json`")
	fs.BoolVar(&o.keep, "keep", false, "keep the sandbox for inspection")
	fs.BoolVar(&o.verbose, "v", false, "print every cited difference too")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	sources := 0
	for _, s := range []string{o.tt.repo, o.tt.tar, o.tt.src} {
		if s != "" {
			sources++
		}
	}
	modes := 0
	for _, b := range []bool{o.corpus != "", o.all, o.live} {
		if b {
			modes++
		}
	}
	if sources != 1 || modes != 1 || (o.live && o.record) || (o.realAgents && !o.live) {
		fmt.Fprintln(os.Stderr, "_diff: want exactly one of -tt-repo, -tt-tar, -tt-src and one of -corpus, -all-corpus, -live; -record is testdata mode only; -real-agents is live mode only")
		return exitSetup
	}
	mod, err := moduleDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "_diff:", err)
		return exitSetup
	}
	o.module = mod
	verbose = o.verbose

	var scenarios []difftest.Scenario
	switch {
	case o.all:
		scenarios = difftest.Corpus()
	case o.corpus != "":
		s, ok := difftest.ScenarioNamed(o.corpus)
		if !ok {
			fmt.Fprintf(os.Stderr, "_diff: no scenario %q\n", o.corpus)
			return exitSetup
		}
		scenarios = []difftest.Scenario{s}
	}

	code := exitPass
	err = withSandbox(o.keep, func(sb *sandbox) error {
		origin, err := o.tt.fetch(sb)
		if err != nil {
			return err
		}
		if err := build(sb); err != nil {
			return err
		}
		fmt.Printf("tt: %s", origin.How)
		if origin.Commit != "" {
			fmt.Printf(", commit %s", origin.Commit)
		}
		fmt.Println()
		if o.live {
			code = runLive(sb, o)
			return nil
		}
		code = runCorpus(sb, o, scenarios)
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "_diff:", err)
		return exitSetup
	}
	return code
}

// moduleDir finds claudestate's go.mod from the working directory up.
func moduleDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(b), "module github.com/MadAppGang/magmux/claudestate\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run _diff from inside the claudestate module")
		}
		dir = parent
	}
}

// outcome is one scenario's result.
type outcome struct {
	name        string
	report      difftest.Report
	notVerified string // why, when the scenario is NOT VERIFIED
	recorded    bool   // -record wrote its reference
	setup       error
}

func runCorpus(sb *sandbox, o options, scenarios []difftest.Scenario) int {
	var rec *recorder
	if o.record {
		r, err := openRecorder(filepath.Join(o.module, "testdata"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "_diff:", err)
			return exitSetup
		}
		defer r.close()
		rec = r
	}
	var outs []outcome
	for _, s := range scenarios {
		out := runScenario(sb, o, s, rec)
		outs = append(outs, out)
		printOutcome(out)
	}
	return summarise(outs)
}

func printOutcome(out outcome) {
	fmt.Printf("\n== %s\n", out.name)
	switch {
	case out.setup != nil:
		fmt.Printf("   SETUP ERROR: %v\n", out.setup)
		return
	case out.notVerified != "":
		fmt.Printf("   NOT VERIFIED: %s\n", out.notVerified)
		return
	}
	printReport(out.report)
	if out.recorded {
		fmt.Println("   recorded")
	} else if len(out.report.Uncited) > 0 {
		fmt.Println("   NOT recorded: an uncited difference is a finding, not a reference")
	}
}

var verbose bool

func printReport(r difftest.Report) {
	if verbose {
		for _, d := range r.Cited {
			fmt.Printf("   . %s\n", d.Format())
		}
	}
	var rows []string
	for k := range r.PerRow {
		rows = append(rows, k)
	}
	sort.Strings(rows)
	if len(rows) == 0 {
		fmt.Println("   no cited difference")
	}
	for _, k := range rows {
		fmt.Printf("   %-8s %d\n", k, r.PerRow[k])
	}
	fmt.Printf("   one-sided rows: %d\n", r.OneSided)
	for _, k := range r.Raced {
		fmt.Printf("   RACED %s\n", k)
	}
	for _, d := range r.Uncited {
		fmt.Printf("   %s\n", d.Format())
	}
	if len(r.Uncited) == 0 {
		fmt.Println("   uncited: 0")
	}
}

func summarise(outs []outcome) int {
	total := map[string]int{}
	var uncited, setup int
	var nv []string
	for _, o := range outs {
		switch {
		case o.setup != nil:
			setup++
		case o.notVerified != "":
			nv = append(nv, o.name+" ("+o.notVerified+")")
		default:
			for k, v := range o.report.PerRow {
				total[k] += v
			}
			uncited += len(o.report.Uncited)
		}
	}
	fmt.Println("\n== summary")
	var rows []string
	for k := range total {
		rows = append(rows, k)
	}
	sort.Strings(rows)
	for _, k := range rows {
		fmt.Printf("   %-8s %d\n", k, total[k])
	}
	fmt.Printf("   uncited: %d\n", uncited)
	for _, n := range nv {
		fmt.Printf("   NOT VERIFIED: %s\n", n)
	}
	if setup > 0 {
		fmt.Printf("   setup errors: %d\n", setup)
	}
	switch {
	case setup > 0:
		return exitSetup
	case uncited > 0:
		return exitUncited
	case len(nv) > 0:
		return exitNotVerified
	}
	return exitPass
}
