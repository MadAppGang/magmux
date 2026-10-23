package audit

// G14: every contract through which the pipeline runs HOST code states that
// the host code bounds itself (review r1 #2).
//
// The pipeline runs host stages on actor goroutines and host callbacks on the
// coordinator, and it waits for each: collect awaits every actor, Stop waits
// for every goroutine it started. It arms no timer of its own to cut a wait
// short (C2), and abandoning a goroutine mid-call would leave host code running
// after Stop returned. So a host that blocks forever freezes every session and
// hangs Stop, and the only defence is the contract: each one says, in its doc,
// that the implementation must return promptly. Config.OnEffects and
// notify.Port always did; the stage interfaces and most ports did not.
//
// The property is a sentence, so the guard reads the source: a doc comment is
// the only place a host author meets the obligation.

import (
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

// boundMarkers: a contract states its bound with one of these phrases.
var boundMarkers = []string{"must return promptly", "must not block", "must bound itself"}

// boundContracts: file -> the declarations in it that run host code. A dotted
// name is a field or method of a struct or interface type.
var boundContracts = map[string][]string{
	"stage/stage.go":       {"Stage", "Targeting", "Waker", "Effecting"},
	"ports/ports.go":       {"ProcessOracle", "PaneLocator", "CycleLocator", "PaneActivity", "PaneShell", "AgentsListing"},
	"pipeline/pipeline.go": {"Config.OnEffects", "Ops.Call", "Exec"},
}

func statesBound(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	text := strings.ToLower(strings.Join(strings.Fields(doc.Text()), " "))
	for _, m := range boundMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// declDocs maps every top-level type and function, and every field or method
// of a top-level struct or interface ("Type.Field"), to its doc comment.
func declDocs(f file) map[string]*ast.CommentGroup {
	out := map[string]*ast.CommentGroup{}
	for _, d := range f.AST.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			out[d.Name.Name] = d.Doc // a method by its bare name: Exec is (*Pipeline).Exec
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, s := range d.Specs {
				ts := s.(*ast.TypeSpec)
				doc := ts.Doc
				if doc == nil && len(d.Specs) == 1 {
					doc = d.Doc
				}
				out[ts.Name.Name] = doc
				var fields *ast.FieldList
				switch t := ts.Type.(type) {
				case *ast.StructType:
					fields = t.Fields
				case *ast.InterfaceType:
					fields = t.Methods
				}
				if fields == nil {
					continue
				}
				for _, fl := range fields.List {
					for _, n := range fl.Names {
						out[ts.Name.Name+"."+n.Name] = fl.Doc
					}
				}
			}
		}
	}
	return out
}

func boundFindings(files []file, contracts map[string][]string) []finding {
	var out []finding
	for _, f := range nonTest(files) {
		names, ok := contracts[f.Rel]
		if !ok {
			continue
		}
		docs := declDocs(f)
		for _, n := range names {
			doc, found := docs[n]
			switch {
			case !found:
				out = append(out, finding{File: f.Rel, Rule: "G14 bounded-host-code",
					Detail: n + " is not declared here: the contract table is stale"})
			case !statesBound(doc):
				line := 1
				if doc != nil {
					line = lineOf(f, doc.Pos())
				}
				out = append(out, finding{File: f.Rel, Line: line, Rule: "G14 bounded-host-code",
					Detail: n + "'s doc does not say the host code must return promptly"})
			}
		}
	}
	return out
}

func TestEveryHostFacingContractStatesItsBound(t *testing.T) {
	files := loadModule(t)
	for rel := range boundContracts {
		requireScanned(t, nonTest(files), rel)
	}
	if got := boundFindings(files, boundContracts); len(got) != 0 {
		t.Fatalf("a contract that runs host code does not state its bound:\n%s", format(got))
	}
}

func TestTheBoundGuardActuallyDetectsAnUnboundedContract(t *testing.T) {
	src := "package stage\n" +
		"// Bounded is a stage. Observe must return promptly.\ntype Bounded interface{ Observe() }\n" +
		"// Unbounded is a stage.\ntype Unbounded interface {\n\t// Observe observes.\n\tObserve()\n}\n"
	f := snippet(t, "stage/zz.go", src)
	got := boundFindings([]file{f}, map[string][]string{"stage/zz.go": {"Bounded", "Unbounded", "Unbounded.Observe", "Missing"}})
	if len(got) != 3 {
		t.Fatalf("want 3 findings (Unbounded, Unbounded.Observe, Missing), got:\n%s", format(got))
	}
}
