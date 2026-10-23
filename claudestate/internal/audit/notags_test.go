package audit

// G6: no json tags, and no JSON method, in the library's packages. The wire
// belongs to each host; a json tag here would be a second wire nobody owns.
//
// Skipped, with the citation: internal/difftest, _diff and _probe decode
// tt's and magmux's WIRE, which is JSON by definition (architecture §6 G6).

import (
	"go/ast"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

var tagsSkip = map[string]string{
	"internal/difftest": "decodes tt's and magmux's wire (§6 G6)",
	"_diff":             "decodes tt's and magmux's wire (§6 G6)",
	"_probe":            "an independent reader of magmux's wire (§6 G6)",
}

var jsonMethods = map[string]bool{"MarshalJSON": true, "UnmarshalJSON": true}

func skippedForTags(dir string) bool {
	for d := range tagsSkip {
		if dir == d || strings.HasPrefix(dir, d+"/") {
			return true
		}
	}
	return false
}

func tagFindings(files []file) []finding {
	var out []finding
	for _, f := range nonTest(files) {
		if skippedForTags(f.Dir) {
			continue
		}
		walkWithFuncs(f, func(fn string, n ast.Node) {
			switch v := n.(type) {
			case *ast.StructType:
				for _, field := range v.Fields.List {
					if field.Tag == nil {
						continue
					}
					tag, err := strconv.Unquote(field.Tag.Value)
					if err != nil {
						continue
					}
					if _, ok := reflect.StructTag(tag).Lookup("json"); ok {
						out = append(out, finding{File: f.Rel, Line: lineOf(f, field.Tag.Pos()), Func: fn, Rule: "G6 no-json-tags",
							Detail: "a json struct tag: the wire belongs to the host"})
					}
				}
			case *ast.FuncDecl:
				if v.Recv != nil && jsonMethods[v.Name.Name] {
					out = append(out, finding{File: f.Rel, Line: lineOf(f, v.Pos()), Func: fn, Rule: "G6 no-json-tags",
						Detail: v.Name.Name + ": no library type has a JSON method"})
				}
			}
		})
	}
	return out
}

func TestNoJSONTagsInTheLibrary(t *testing.T) {
	files := loadModule(t)
	requireScanned(t, nonTest(files), "model/session.go", "model/enum.go")
	if got := tagFindings(files); len(got) != 0 {
		t.Fatalf("json in the library's types:\n%s", format(got))
	}
}

func TestTheTagAuditActuallyDetectsTags(t *testing.T) {
	const planted = "package model\ntype S struct {\n\tA string `json:\"a\"`\n}\nfunc (S) MarshalJSON() ([]byte, error) { return nil, nil }\n"
	if got := tagFindings([]file{snippet(t, "model/zz.go", planted)}); len(got) != 2 {
		t.Fatalf("want 2 findings (a tag and a method), got:\n%s", format(got))
	}
	if got := tagFindings([]file{snippet(t, "_probe/zz.go", planted)}); len(got) != 0 {
		t.Fatalf("the cited skip for _probe did not apply:\n%s", format(got))
	}
}
