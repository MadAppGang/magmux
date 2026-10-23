// Package lint finds the places a HOST decodes Claude Code's formats itself,
// instead of going through claudestate/claude (FR12, NFR6; magmux's H1).
//
// The vocabulary is claude.Vocabulary(), the decoders' own table, so the set
// of literals cannot drift from what is actually decoded. A literal is flagged
// only in DECODE position, the four shapes a second decoder is written in:
//
//	"index"       m["sessionId"]
//	"struct-tag"  Field T `json:"waitingFor"`
//	"compare"     raw == "permission prompt", raw != "idle"
//	"case"        case "busy":
//
// Anywhere else a vocabulary word is the host's own output (magmux's status
// words are homographs of Claude's values), and none of this package's
// business. What it does not catch, stated so nobody mistakes it for a
// guarantee: a literal split in two, a word passed to a function, a constant
// declared under another name. A host that needs a site excused filters the
// returned findings by Literal and position and keeps the reason with it.
//
// lint parses source with go/parser. It writes nothing, runs nothing and keeps
// nothing open.
package lint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// The four decode shapes.
const (
	ShapeIndex     = "index"
	ShapeStructTag = "struct-tag"
	ShapeCompare   = "compare"
	ShapeCase      = "case"
)

// Finding is one decode site.
type Finding struct {
	Pos     token.Position
	Literal string
	Shape   string // ShapeIndex, ShapeStructTag, ShapeCompare or ShapeCase
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: %s decodes %q, which is Claude Code's; decode it through claudestate/claude", f.Pos, f.Shape, f.Literal)
}

// Literals is the vocabulary as a set: every key and value the decoders name.
func Literals(v claude.Vocab) map[string]bool {
	out := map[string]bool{}
	add := func(ss ...[]string) {
		for _, s := range ss {
			for _, w := range s {
				if w != "" {
					out[w] = true
				}
			}
		}
	}
	for _, k := range []claude.KeySet{v.Registry, v.Jobs, v.AgentsInteractive, v.AgentsBackground, v.SubagentMeta, v.Timeline} {
		add(k.Universal, k.Optional)
	}
	add(v.JobsNeverRead, v.TranscriptKeys, v.RegistryStatus, v.WaitingFor, v.JobState, v.Tempo, v.Kind,
		v.UntimestampedTypes, v.Homographs)
	return out
}

// ClaudeDecodeSites walks every directory in dirs, recursively, and returns
// every decode site of a vocabulary literal in a .go file, sorted by position.
// Directories named testdata or vendor, and those beginning with "." are not
// entered. skip, when non-nil, is asked about every directory and file path
// (as filepath.WalkDir gives it): true leaves a file unread and a directory
// unentered.
func ClaudeDecodeSites(dirs []string, v claude.Vocab, skip func(path string) bool) ([]Finding, error) {
	lits := Literals(v)
	if len(lits) == 0 {
		return nil, fmt.Errorf("lint: the vocabulary is empty; nothing would be found")
	}
	var out []Finding
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if p != dir && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".")) {
					return fs.SkipDir
				}
				if skip != nil && skip(p) {
					return fs.SkipDir
				}
				return nil
			}
			if filepath.Ext(p) != ".go" || (skip != nil && skip(p)) {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			out = append(out, Sites(fset, f, lits)...)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("lint: %w", err)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Pos, out[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Offset < b.Offset
	})
	return out, nil
}

// Sites is the walk over one parsed file, exported so a host can hold the
// detector itself to known-bad and known-good source.
func Sites(fset *token.FileSet, f *ast.File, lits map[string]bool) []Finding {
	var out []Finding
	seen := map[token.Pos]bool{}
	add := func(pos token.Pos, lit, shape string) {
		if !seen[pos] {
			seen[pos] = true
			out = append(out, Finding{Pos: fset.Position(pos), Literal: lit, Shape: shape})
		}
	}
	decoded := func(e ast.Expr, shape string) {
		if s, ok := stringLit(e); ok && lits[s] {
			add(e.Pos(), s, shape)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if x.Op == token.EQL || x.Op == token.NEQ {
				decoded(x.X, ShapeCompare)
				decoded(x.Y, ShapeCompare)
			}
		case *ast.CaseClause:
			for _, e := range x.List {
				decoded(e, ShapeCase)
			}
		case *ast.IndexExpr:
			decoded(x.Index, ShapeIndex)
		case *ast.Field:
			if x.Tag == nil {
				return true
			}
			tag, err := strconv.Unquote(x.Tag.Value)
			if err != nil {
				return true
			}
			name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
			if lits[name] {
				add(x.Tag.Pos(), name, ShapeStructTag)
			}
		}
		return true
	})
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = p.X
	}
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
