package mux

// `magmux agents focus` prints registry text, and a session id is registry
// text: the decoder accepts any string Claude Code (or anything else that can
// write ~/.claude/sessions) puts there, and claude.ValidSessionID is consulted
// only before a glob. So every line the command prints goes through
// obsview.DisplaySafe, the one rule (review r1 #7). Nothing here reads
// ~/.claude, probes a pid, or runs osascript.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/internal/obs/act"
)

// stageHostileRecord writes one registry record whose session id is s,
// encoded by encoding/json so that every control character survives into the
// file as Claude Code's own writer would leave it.
func stageHostileRecord(t *testing.T, dir string, pid int, id string) {
	t.Helper()
	ms := time.Now().Add(-time.Minute).UnixMilli()
	body, err := json.Marshal(map[string]any{
		"sessionId": id, "pid": pid, "name": "n", "cwd": "/tmp/project", "status": "idle",
		"statusUpdatedAt": ms, "updatedAt": ms,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgentsFocusPrintsRegistryTextThroughTheDisplayRule(t *testing.T) {
	check := func(t *testing.T, what, got string) {
		t.Helper()
		if found := controlsIn(got, true); len(found) != 0 {
			t.Errorf("%s carries %v raw to the terminal:\n%q", what, found, got)
		}
		if !strings.Contains(got, inertRemnant) {
			t.Errorf("%s dropped the text instead of neutralising it:\n%q", what, got)
		}
	}

	t.Run("the report", func(t *testing.T) {
		var b bytes.Buffer
		writeFocusReport(&b, model.Session{Key: model.Key{SessionID: hostileText, Job: hostileText}, PID: 7},
			act.Report{
				Decision: act.Decision{Rule: hostileText},
				Result:   act.Result{Done: true, Detail: hostileText},
				Records: []act.Record{{Seq: 1, Event: act.EventOutcome, Verb: act.VerbFocus, Tier: act.TierNavigate,
					Outcome: &act.Outcome{Error: hostileText}}},
			})
		check(t, "writeFocusReport", b.String())
		if n := strings.Count(b.String(), "\n"); n != 4 {
			t.Errorf("a hostile newline forged a line: %d lines, want 4:\n%s", n, b.String())
		}
	})

	t.Run("an ambiguous target", func(t *testing.T) {
		dir := t.TempDir()
		stageHostileRecord(t, dir, 4242, hostileText)
		stageHostileRecord(t, dir, 4243, hostileText)
		out, errOut := &strings.Builder{}, &strings.Builder{}
		code := obsFocus(obsFocusConfig{Root: dir, Target: hostileText, Policy: act.Default(), Out: out, Err: errOut})
		if code == 0 {
			t.Fatal("two sessions on one id were not refused as ambiguous")
		}
		check(t, "the ambiguity refusal", errOut.String())
	})
}

// printFindings reports every fmt.Fprint* in src outside the functions
// allowed to call it: focusLine, which applies the rule, and the help text,
// which is a literal.
func printFindings(t *testing.T, name string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"focusLine": true, "printObsFocusHelp": true}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || allowed[fn.Name.Name] {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" && strings.HasPrefix(sel.Sel.Name, "Fprint") {
				out = append(out, fmt.Sprintf("%s: fmt.%s in %s", fset.Position(sel.Pos()), sel.Sel.Name, fn.Name.Name))
			}
			return true
		})
	}
	return out
}

// TestEveryAgentsFocusLineGoesThroughTheDisplayRule holds obsfocus.go to one
// way of printing, so that a line added later cannot forget the rule.
func TestEveryAgentsFocusLineGoesThroughTheDisplayRule(t *testing.T) {
	src, err := os.ReadFile("obsfocus.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := printFindings(t, "obsfocus.go", src); len(got) != 0 {
		t.Fatalf("obsfocus.go prints around obsview.DisplaySafe; use focusLine:\n  %s", strings.Join(got, "\n  "))
	}
	if !bytes.Contains(src, []byte("obsview.DisplaySafe(")) {
		t.Fatal("obsfocus.go does not call obsview.DisplaySafe")
	}
}

func TestTheFocusPrintGuardCatchesAPlantedPrint(t *testing.T) {
	planted := []byte("package mux\nimport (\"fmt\"; \"io\")\n" +
		"func focusLine(w io.Writer, f string, a ...any) { fmt.Fprintln(w, f) }\n" +
		"func printObsFocusHelp(w io.Writer) { fmt.Fprint(w, \"help\") }\n" +
		"func report(w io.Writer, id string) { fmt.Fprintf(w, \"session %s\\n\", id) }\n")
	if got := printFindings(t, "planted.go", planted); len(got) != 1 || !strings.Contains(got[0], "report") {
		t.Fatalf("want exactly the planted Fprintf in report, got %v", got)
	}
}
