package adapters

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/internal/obs"
)

// schemaFileName is the one file allowed to name Claude Code's strings.
const schemaFileName = "schema.go"

// testdataSessions is the copied registry records. See testdata/PROVENANCE.md
// for what each one is and whether it was measured or constructed.
func testdataSessions() string { return filepath.Join("..", "testdata", "sessions") }

// recordValue reads one string-valued key out of one registry record, BY NAME.
// ok is false when the key is absent, which is a different answer from an
// empty value and is the whole point of several records.
func recordValue(t *testing.T, file, key string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdataSessions(), file))
	if err != nil {
		t.Fatalf("reading test data %s: %v", file, err)
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("decoding test data %s: %v", file, err)
	}
	raw, ok := rec[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("test data %s: key %s is not a string: %v", file, key, err)
	}
	return s, true
}

// Every status value Claude actually writes decodes, and the one it does not
// write yet does not.
//
// The values come out of the test data rather than out of this file, so the
// test is asking "does the decoder handle what was measured" rather than "does
// the decoder agree with the constants next door", which it would do
// regardless of whether either was right.
//
// A wrong implementation: normalising shell to idle, which is the plausible
// simplification — one status fewer, and the rare one at that. It was 117 of
// 1920 observations, so it is frequent enough that the error would be visible
// and slow enough to notice that it would ship first.
func TestDecodeStatusHandlesEveryValueInTheTestData(t *testing.T) {
	cases := []struct {
		file string
		want obs.Status
		ok   bool
	}{
		{"98198.json", obs.StatusIdle, true},
		{"98693.json", obs.StatusBusy, true},
		{"15775.json", obs.StatusShell, true},
		{"1828.json", obs.StatusShell, true},
		{"1724.json", obs.StatusWaiting, true},
		{"60006.json", obs.StatusWaiting, true},
		{"60003.json", obs.StatusUnknown, false},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			raw, present := recordValue(t, c.file, KeyStatus)
			if !present {
				t.Fatalf("test data %s carries no status at all", c.file)
			}
			got, ok := DecodeStatus(raw)
			if ok != c.ok {
				t.Fatalf("DecodeStatus(%q) ok = %v, want %v", raw, ok, c.ok)
			}
			if got != c.want {
				t.Errorf("DecodeStatus(%q) = %v, want %v", raw, got, c.want)
			}
		})
	}
}

// The four values are four values. An implementation that collapsed any two of
// them would still pass every test above that looks at one record at a time.
func TestTheFourStatusesAreFourDistinctValues(t *testing.T) {
	seen := map[obs.Status]string{}
	for _, file := range []string{"98198.json", "98693.json", "15775.json", "1724.json"} {
		raw, _ := recordValue(t, file, KeyStatus)
		got, ok := DecodeStatus(raw)
		if !ok {
			t.Fatalf("DecodeStatus(%q) did not recognise a measured value", raw)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s both decoded to %v", prev, file, got)
		}
		seen[got] = file
	}
	if len(seen) != 4 {
		t.Errorf("four records decoded to %d distinct statuses, want 4", len(seen))
	}
}

// An unrecognised status is not guessed at. The value in this record is a real
// Claude Code activity, chosen precisely because it is plausible: an
// implementation tempted to guess will guess busy, and nothing downstream can
// tell that guess from an observation.
func TestUnrecognisedStatusIsNoOpinionAndNotAGuess(t *testing.T) {
	raw, present := recordValue(t, "60003.json", KeyStatus)
	if !present {
		t.Fatal("test data 60003.json carries no status")
	}
	got, ok := DecodeStatus(raw)
	if ok {
		t.Fatalf("DecodeStatus(%q) claimed to recognise a value this schema does not know", raw)
	}
	if got != obs.StatusUnknown {
		t.Errorf("DecodeStatus(%q) = %v, want obs.StatusUnknown", raw, got)
	}
	if got == obs.StatusBusy || got == obs.StatusIdle {
		t.Errorf("DecodeStatus(%q) guessed %v", raw, got)
	}
}

// The contract on the caller, written out: on !ok, keep the raw string and
// report Unknown. The raw value is the only thing that makes a schema change
// discoverable — without it the first sign is a user noticing a blank column.
func TestCallerKeepsTheRawValueWhenDecodingFails(t *testing.T) {
	raw, _ := recordValue(t, "60003.json", KeyStatus)

	status, ok := DecodeStatus(raw)
	s := obs.Session{Status: status}
	if !ok {
		s.StatusRaw = raw
	}

	if s.Status != obs.StatusUnknown {
		t.Errorf("Status = %v, want obs.StatusUnknown", s.Status)
	}
	if s.StatusRaw != raw {
		t.Errorf("StatusRaw = %q, want the raw value %q preserved", s.StatusRaw, raw)
	}
	if s.StatusRaw == "" {
		t.Error("the raw value was dropped, so the new format is undiscoverable")
	}
}

// The two waiting reasons survive as two values.
//
// The wrong implementation is a bool meaning "blocked on the user". Both
// records answer true, and on live data that bug is invisible: one of these
// two values has never been observed at all, in 1920 observations.
func TestTheTwoWaitingReasonsStayDistinct(t *testing.T) {
	inputRaw, ok := recordValue(t, "1724.json", KeyWaitingFor)
	if !ok {
		t.Fatal("test data 1724.json carries no waiting reason")
	}
	permRaw, ok := recordValue(t, "60002.json", KeyWaitingFor)
	if !ok {
		t.Fatal("test data 60002.json carries no waiting reason")
	}
	if inputRaw == permRaw {
		t.Fatalf("the test data itself carries one value twice: %q", inputRaw)
	}

	gotInput, ok := DecodeWaitingFor(inputRaw)
	if !ok || gotInput != obs.WaitingReasonInput {
		t.Errorf("DecodeWaitingFor(%q) = %v, %v; want obs.WaitingReasonInput, true", inputRaw, gotInput, ok)
	}
	gotPerm, ok := DecodeWaitingFor(permRaw)
	if !ok || gotPerm != obs.WaitingReasonPermission {
		t.Errorf("DecodeWaitingFor(%q) = %v, %v; want obs.WaitingReasonPermission, true", permRaw, gotPerm, ok)
	}
	if gotInput == gotPerm {
		t.Errorf("both reasons decoded to %v — they have been merged", gotInput)
	}
}

// An absent waiting reason is no opinion, and is emphatically not the common
// one. Defaulting it would be right on 55 of 55 live observations and wrong in
// exactly the case a user depends on.
//
// The record is still waiting. What it waits for being unknown does not make
// it not waiting, which is why the two are separate axes.
func TestAbsentWaitingReasonIsNoOpinion(t *testing.T) {
	if _, present := recordValue(t, "60006.json", KeyWaitingFor); present {
		t.Fatal("test data 60006.json was supposed to omit the waiting-reason key")
	}

	got, ok := DecodeWaitingFor("")
	if ok {
		t.Error("DecodeWaitingFor(\"\") claimed to have recognised something")
	}
	if got != obs.WaitingReasonUnknown {
		t.Errorf("DecodeWaitingFor(\"\") = %v, want obs.WaitingReasonUnknown", got)
	}
	if got == obs.WaitingReasonInput {
		t.Error("an absent waiting reason was defaulted to the common value")
	}
	if got == obs.WaitingReasonNone {
		t.Error("an absent waiting reason was reported as a determined negative")
	}

	statusRaw, _ := recordValue(t, "60006.json", KeyStatus)
	if status, _ := DecodeStatus(statusRaw); status != obs.StatusWaiting {
		t.Errorf("status = %v, want the record to still be waiting", status)
	}
}

// Records are parsed BY NAME. The record used here carries every key in a
// different order, and must decode to the same values as one in the usual
// order. A positional or streaming-by-index parse gives it another field's
// value and nothing complains.
func TestKeyOrderDoesNotAffectDecoding(t *testing.T) {
	shuffledStatus, _ := recordValue(t, "60008.json", KeyStatus)
	canonicalStatus, _ := recordValue(t, "1724.json", KeyStatus)
	if shuffledStatus != canonicalStatus {
		t.Errorf("status = %q from the shuffled record, %q from the canonical one",
			shuffledStatus, canonicalStatus)
	}
	shuffledWaiting, _ := recordValue(t, "60008.json", KeyWaitingFor)
	canonicalWaiting, _ := recordValue(t, "1724.json", KeyWaitingFor)
	if shuffledWaiting != canonicalWaiting {
		t.Errorf("waiting reason = %q from the shuffled record, %q from the canonical one",
			shuffledWaiting, canonicalWaiting)
	}
	if id, ok := recordValue(t, "60008.json", KeySessionID); !ok || id == "" {
		t.Errorf("session id = %q, %v; want the last key in the record to decode", id, ok)
	}
}

// ---------------------------------------------------------------------------
// NFR6, enforced against DECODE SITES rather than against spellings.
//
// The rule is that when Claude changes a format, exactly one file moves. The
// first version of this test asked for something narrower than that — it
// failed on any appearance of a Claude-owned literal anywhere under
// internal/obs — and the moment the core settled its own wire vocabulary the
// test fired on our own name tables:
//
//	../status.go:52:2: string literal "idle", which belongs to Claude Code.
//
// That is a false positive, and it is the interesting kind. "idle" in Claude's
// registry and "idle" in our document are HOMOGRAPHS: the same spelling,
// different owners, independent lifetimes. If Claude renames its value
// tomorrow, DecodeStatus in schema.go absorbs the rename and our wire word
// does not move — which is NFR6 working exactly as intended, on a word the old
// test said we were forbidden to write.
//
// So the literals split into two classes, and the classification is DERIVED
// rather than listed, because a hand-kept second list is the drift this whole
// rule exists to prevent:
//
//   - Claude-only — a string that could only have come from Claude
//     ("statusUpdatedAt", "input needed", "in_process_teammate"). Banned
//     everywhere outside schema.go, in any position. There is no legitimate
//     reason for our code to write one.
//   - Homograph — a Claude value that is also one of obs's own wire words
//     ("idle", "busy", "shell", "waiting"). Banned only where it is being
//     DECODED: compared against, switched on, or named by a struct tag.
//     Anywhere else it is our output vocabulary and is none of NFR6's
//     business.
//
// What it does not catch, stated so nobody mistakes it for a guarantee: a
// literal split in two, a homograph reached through a map index, and a
// homograph passed to a function. The first is an evader and this test defends
// against the accident; the other two are shapes our own code has no reason to
// write and that no Claude record is decoded by. Comparison, switch and struct
// tag are the three shapes a second decoder actually gets written in.
//
// One consequence worth knowing before it surprises someone: comparing our own
// status against its SPELLING — if s.String() == "idle" — is flagged too, and
// deservedly. At a comparison site the two owners are indistinguishable, and
// comparing the value (s == obs.StatusIdle) is the better code regardless.
// ---------------------------------------------------------------------------

// decodeSite is one flagged position: what was found, and the shape it was
// found in.
type decodeSite struct {
	pos token.Pos
	lit string
	how string
}

// ourWireVocabulary is every word obs writes for a determined value.
//
// It is read out of obs itself — marshal each value, see what comes back —
// rather than listed here, so the classification below cannot drift from the
// vocabulary it classifies. If obs ever stops spelling a status the way Claude
// does, that word stops being a homograph and becomes Claude-only, and this
// test tightens on it with no edit.
func ourWireVocabulary(t *testing.T) map[string]bool {
	t.Helper()
	vals := []json.Marshaler{
		obs.StatusIdle, obs.StatusBusy, obs.StatusShell, obs.StatusWaiting,
		obs.WaitingReasonNone, obs.WaitingReasonInput, obs.WaitingReasonPermission,
		obs.LivenessYes, obs.LivenessNo,
	}
	out := make(map[string]bool, len(vals))
	for _, v := range vals {
		b, err := v.MarshalJSON()
		if err != nil {
			t.Fatalf("marshalling %v: %v", v, err)
		}
		s, err := strconv.Unquote(string(b))
		if err != nil {
			t.Fatalf("obs marshalled a determined value as %s, which is not a JSON string", b)
		}
		out[s] = true
	}
	return out
}

// claudeLiteralClasses splits the inventory into the two classes above.
func claudeLiteralClasses(t *testing.T) (claudeOnly, homograph map[string]bool) {
	t.Helper()
	ours := ourWireVocabulary(t)
	claudeOnly = make(map[string]bool, len(claudeOwnedLiterals))
	homograph = make(map[string]bool)
	for _, s := range claudeOwnedLiterals {
		if ours[s] {
			homograph[s] = true
		} else {
			claudeOnly[s] = true
		}
	}
	return claudeOnly, homograph
}

// claudeDecodeSites walks one parsed file and returns every site that decodes
// a Claude string. It is a pure function over an AST so that the detector
// itself can be tested against known-bad and known-good source; see
// TestTheDecodeDetectorCatchesTheShapesItMustCatch.
//
// AST rather than a regex, because the whole job is telling a comparison from
// a slice element and reading a struct tag as a tag. A regex sees the same
// characters in both, which is how the first version of this test got it
// wrong.
func claudeDecodeSites(file *ast.File, claudeOnly, homograph map[string]bool) []decodeSite {
	var found []decodeSite
	seen := map[token.Pos]bool{}

	add := func(pos token.Pos, lit, how string) {
		if seen[pos] {
			return
		}
		seen[pos] = true
		found = append(found, decodeSite{pos, lit, how})
	}
	// decoded flags a literal being read AS Claude's value. Either class
	// counts here, because this is decode position.
	decoded := func(e ast.Expr, how string) {
		lit, ok := stringLit(e)
		if !ok {
			return
		}
		if claudeOnly[lit] || homograph[lit] {
			add(e.Pos(), lit, how)
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			// raw == "waiting" — the plainest decode there is.
			if v.Op == token.EQL || v.Op == token.NEQ {
				decoded(v.X, "compares a value against")
				decoded(v.Y, "compares a value against")
			}
		case *ast.CaseClause:
			// switch raw { case "permission prompt": … }
			for _, e := range v.List {
				decoded(e, "switches on")
			}
		case *ast.Field:
			// `json:"statusUpdatedAt"` — the idiomatic second decoder, and the
			// one that never feels like a copy of anything as it is written.
			if v.Tag == nil {
				return true
			}
			tag, err := strconv.Unquote(v.Tag.Value)
			if err != nil {
				return true
			}
			name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
			if claudeOnly[name] || homograph[name] {
				add(v.Tag.Pos(), name, "carries a struct tag decoding")
			}
		case *ast.BasicLit:
			// A Claude-only string anywhere at all. Our code has no reason to
			// write one, so position does not matter for this class.
			if lit, ok := stringLit(v); ok && claudeOnly[lit] {
				add(v.Pos(), lit, "names")
			}
		}
		return true
	})
	return found
}

// stringLit unquotes e when it is a string literal.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// TestOnlySchemaFileDecodesClaudeStrings is NFR6 made enforceable: no file
// under internal/obs except schema.go may decode a string Claude Code owns.
//
// It replaces TestOnlySchemaFileNamesClaudeFields, which asked whether a word
// appeared rather than who owned the value being read, and so could not tell
// our wire vocabulary from Claude's registry. See the commentary above.
func TestOnlySchemaFileDecodesClaudeStrings(t *testing.T) {
	claudeOnly, homograph := claudeLiteralClasses(t)

	root := ".."
	var scanned, skipped int

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		if filepath.Base(path) == schemaFileName {
			skipped++
			return nil
		}
		scanned++

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, s := range claudeDecodeSites(file, claudeOnly, homograph) {
			t.Errorf("%s: %s %q, which is Claude Code's. Claude's format is decoded in %s and nowhere else.",
				fset.Position(s.pos), s.how, s.lit, schemaFileName)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// Without these the test passes when the walk finds nothing, which is
	// exactly what a moved package or a renamed schema file would produce.
	if skipped != 1 {
		t.Errorf("found %d files named %s under %s, want exactly 1", skipped, schemaFileName, root)
	}
	if scanned < 4 {
		t.Errorf("scanned only %d Go files under %s — the walk is not reaching the package tree", scanned, root)
	}
}

// The detector is tested against source, which is the only way to know it
// catches anything: the test above passes on a clean tree whether the detector
// works or is a stub returning nil.
//
// Every snippet is built from the constants in schema.go rather than from
// words typed here, so a rename on Claude's side moves this test with it and
// cannot leave it quietly checking a string nobody writes any more.
func TestTheDecodeDetectorCatchesTheShapesItMustCatch(t *testing.T) {
	claudeOnly, homograph := claudeLiteralClasses(t)

	// The classification has to have split something, or the two rules
	// collapse into one and the false positive comes back.
	if len(homograph) == 0 {
		t.Error("no Claude literal is also one of our wire words — the homograph rule is checking nothing")
	}
	if len(claudeOnly) == 0 {
		t.Error("every Claude literal was classed as a homograph — the strict rule is checking nothing")
	}

	cases := []struct {
		name string
		src  string
		want bool // want a violation
	}{
		{
			name: "comparing a raw value against Claude's status",
			src:  "package p\nfunc f(raw string) bool { return raw == %[1]q }\n",
			want: true,
		},
		{
			name: "switching on Claude's waiting reason",
			src:  "package p\nfunc f(raw string) int {\nswitch raw {\ncase %[2]q:\nreturn 1\n}\nreturn 0\n}\n",
			want: true,
		},
		{
			name: "a struct tag naming Claude's key",
			src:  "package p\ntype record struct {\nAt int64 `json:%[3]q`\n}\n",
			want: true,
		},
		{
			name: "a Claude-only key as a bare literal",
			src:  "package p\nvar keys = []string{%[4]q}\n",
			want: true,
		},
		{
			name: "a map read by Claude's key",
			src:  "package p\nfunc f(m map[string]any) any { return m[%[5]q] }\n",
			want: true,
		},
		{
			name: "our own output vocabulary in a name table",
			src:  "package p\nvar names = []string{%[1]q, %[6]q}\n",
			want: false,
		},
		{
			name: "a comment mentioning Claude's value",
			src:  "package p\n// Claude writes %[1]s here.\nvar x int\n",
			want: false,
		},
		{
			name: "our own wire word written out, not read in",
			src:  "package p\nfunc name() string { return %[6]q }\n",
			want: false,
		},
		{
			name: "our own field named with our own word",
			src:  "package p\n// Our %[6]s status marshals into this field.\ntype doc struct {\nStatusRaw string `json:\"statusRaw\"`\n}\n",
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := fmt.Sprintf(c.src,
				statusWaiting,        // 1: a homograph value
				waitingForPermission, // 2: a Claude-only value
				KeyStatusUpdatedAt,   // 3: a Claude-only key
				KeyAgentColor,        // 4: a Claude-only key
				KeySessionID,         // 5: a Claude-only key
				statusIdle,           // 6: another homograph value
			)
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing the snippet: %v\n%s", err, src)
			}
			sites := claudeDecodeSites(file, claudeOnly, homograph)
			if got := len(sites) > 0; got != c.want {
				t.Errorf("detector found %d sites (%v), want a violation = %v, in:\n%s",
					len(sites), sites, c.want, src)
			}
		})
	}
}

// The inventory has to be non-trivial, or the test above is a ceremony. If
// someone empties it to make a failure go away, this is what objects.
func TestClaudeLiteralInventoryIsPopulated(t *testing.T) {
	if len(claudeOwnedLiterals) < 20 {
		t.Errorf("the inventory lists %d literals, which is fewer than the schema names",
			len(claudeOwnedLiterals))
	}
	for i, s := range claudeOwnedLiterals {
		if s == "" {
			t.Errorf("inventory entry %d is empty", i)
		}
	}
}
