package audit

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// modulePath is this module's import path.
const modulePath = "github.com/MadAppGang/magmux/claudestate"

// finding is one rule violation, located precisely enough to fix.
type finding struct {
	File   string // relative to the module root, slash-separated
	Line   int
	Func   string // enclosing function, "" at package scope
	Rule   string
	Detail string
}

func (f finding) String() string {
	where := f.File + ":" + strconv.Itoa(f.Line)
	if f.Func != "" {
		where += " (in " + f.Func + ")"
	}
	return fmt.Sprintf("%s: %s: %s", where, f.Rule, f.Detail)
}

func format(fs []finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  ")
		b.WriteString(f.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// file is one parsed Go source file.
type file struct {
	Rel    string // slash-separated, relative to the module root
	Dir    string // Rel's directory: the package key
	IsTest bool
	AST    *ast.File
	Fset   *token.FileSet
	// Build is the file's //go:build expression, nil when it has none.
	Build constraint.Expr
}

// moduleRoot walks up from the test's working directory to this module's
// go.mod. It refuses any other module, so a guard can never silently scan
// the root module instead.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			if !bytes.HasPrefix(b, []byte("module "+modulePath+"\n")) {
				t.Fatalf("the nearest go.mod (%s) is not %s", dir, modulePath)
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above the test directory")
		}
		dir = parent
	}
}

// loadModule parses every .go file in the module, test files included.
//
// It skips testdata, vendor, dot-directories and nested modules. It does NOT
// skip "_" directories: _probe and _diff are commands the guards must see.
func loadModule(t *testing.T) []file {
	t.Helper()
	root := moduleRoot(t)
	var out []file
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p == root {
				return nil
			}
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return fs.SkipDir // a nested module is not this module
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		f, err := parseFile(fset, filepath.ToSlash(rel), src)
		if err != nil {
			return err
		}
		out = append(out, f)
		return nil
	})
	if err != nil {
		t.Fatalf("loading the module: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the loader found no Go file at all")
	}
	return out
}

// snippet parses one in-memory file under a module-relative path. It is how
// each guard proves it can fail.
func snippet(t *testing.T, rel, src string) file {
	t.Helper()
	f, err := parseFile(token.NewFileSet(), rel, []byte(src))
	if err != nil {
		t.Fatalf("parsing snippet %s: %v", rel, err)
	}
	return f
}

func parseFile(fset *token.FileSet, rel string, src []byte) (file, error) {
	a, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
	if err != nil {
		return file{}, fmt.Errorf("parsing %s: %w", rel, err)
	}
	build, err := buildLine(src)
	if err != nil {
		return file{}, fmt.Errorf("%s: %w", rel, err)
	}
	return file{
		Rel:    rel,
		Dir:    path.Dir(rel),
		IsTest: strings.HasSuffix(rel, "_test.go"),
		AST:    a,
		Fset:   fset,
		Build:  build,
	}, nil
}

// buildLine returns the //go:build expression in the file's header, before
// the package clause.
func buildLine(src []byte) (constraint.Expr, error) {
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "package ") {
			return nil, nil
		}
		if constraint.IsGoBuild(line) {
			return constraint.Parse(line)
		}
	}
	return nil, sc.Err()
}

// supportedOS is where the library is built and tested (CI's matrix).
var supportedOS = []string{"darwin", "linux"}

// inDefaultBuild reports whether a file is compiled by a plain `go test` on
// at least one supported OS: GOOS, GOARCH, unix, cgo, gc and go1.N are true,
// and every custom tag is false.
func inDefaultBuild(f file) bool {
	if f.Build == nil {
		return true
	}
	for _, goos := range supportedOS {
		ok := f.Build.Eval(func(tag string) bool {
			switch tag {
			case goos, "unix", "cgo", "gc", "amd64", "arm64":
				return true
			}
			return strings.HasPrefix(tag, "go1.")
		})
		if ok {
			return true
		}
	}
	return false
}

func nonTest(files []file) []file { return filterFiles(files, false) }
func tests(files []file) []file   { return filterFiles(files, true) }

func filterFiles(files []file, wantTest bool) []file {
	var out []file
	for _, f := range files {
		if f.IsTest == wantTest {
			out = append(out, f)
		}
	}
	return out
}

// importNames returns every identifier under which f refers to the package
// at importPath. A renamed import is included; a dot or blank import is not a
// usable qualifier and is reported as "." or "_".
func importNames(f file, importPath string) map[string]bool {
	out := map[string]bool{}
	for _, imp := range f.AST.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		if imp.Name != nil {
			out[imp.Name.Name] = true
			continue
		}
		out[path.Base(p)] = true
	}
	return out
}

// walkWithFuncs visits every node, telling the callback which function it is
// in. Package-level declarations report "": `var sleep = time.Sleep` is a
// reference at package scope, and an audit that only walked function bodies
// would miss it.
func walkWithFuncs(f file, visit func(fnName string, n ast.Node)) {
	for _, decl := range f.AST.Decls {
		name := ""
		if fn, ok := decl.(*ast.FuncDecl); ok {
			name = fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				name = receiverTypeName(fn.Recv.List[0].Type) + "." + name
			}
		}
		captured := name
		ast.Inspect(decl, func(n ast.Node) bool {
			if n != nil {
				visit(captured, n)
			}
			return true
		})
	}
}

func receiverTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return receiverTypeName(v.X)
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr:
		return receiverTypeName(v.X)
	default:
		return ""
	}
}

// qualified returns (qualifier, name) for a selector whose X is a plain
// identifier, and ok=false otherwise.
func qualified(n ast.Node) (pkg, name string, sel *ast.SelectorExpr, ok bool) {
	sel, ok = n.(*ast.SelectorExpr)
	if !ok {
		return "", "", nil, false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", "", nil, false
	}
	return id.Name, sel.Sel.Name, sel, true
}

func lineOf(f file, pos token.Pos) int { return f.Fset.Position(pos).Line }

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// requireScanned fails when none of the named files was in a scanned set: the
// guard is then auditing nothing it claims to audit.
func requireScanned(t *testing.T, files []file, rels ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, f := range files {
		have[f.Rel] = true
	}
	for _, r := range rels {
		if !have[r] {
			t.Errorf("the scan did not include %s; the walker is not seeing the tree it guards", r)
		}
	}
}
