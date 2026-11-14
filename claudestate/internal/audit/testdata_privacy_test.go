package audit

// G9: testdata privacy and provenance (NFR12; tt:fixtures_privacy_test.go:38-46,
// 116, 133, reconciled with magmux's single PROVENANCE.md).
//
//   - Every file under testdata/ has a row in testdata/PROVENANCE.md with a
//     class (REAL, DERIVED, SYNTHETIC) and an origin (magmux, tt), and every
//     row names a file that exists.
//   - The field denylist, by origin. intent, output, suggestedReply and
//     lastPrompt are banned corpus-wide. message, content and text are banned
//     in tt-origin files, which is tt's own rule, and allowed in magmux-origin
//     SYNTHETIC transcripts, which need them.
//   - An identifier denylist stored as SHA-256 hashes of lowercased tokens,
//     corpus-wide and over file paths too, so this repository never spells the
//     names it bans.
//   - The string "/fixtures/" is banned corpus-wide: the scrub replaced tt's
//     prefix with the same-length "/testdata/projects".

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

const provenanceFile = "PROVENANCE.md"

// identifierDenylist is SHA-256(lowercase token) of every real person, client
// and product name the scrub removed. The names are not written here, on
// purpose: a denylist that spells its entries republishes them.
var identifierDenylist = hashSet(
	"31611159e7e6ff7843ea4627745e89225fc866621cfcfdbd40871af4413747cc",
	"2b4e4a442df9924ed2885cb5a493a627978dba4a3418199bfc89b90e0e5b5e3f",
	"36821a6f55f8238368d8e44d7d4e698ec73251b35d1fd5e0bec3d4d5cf104200",
	"bf1c395a22bb6224fa49a500d41ec44c3220e1f2d93a5d110a7d671ae5404125",
	"a1141e968c76f03d21a21f50aacc11cded1d7744da70ab8972564a200e396da7",
	"b07739b6c6f9449162eda95b86da4d60de664f325c7ef2e869a2bfbbd606faae",
	"e94de5698b3aee9a56efe3bbf908e46cbedea1cde580a7be4cf948bdd7064256",
	"3e47ef1da5e76313ee6bc8dc75c73b4cfdde142ed5901a711ae95a0f2ec1223c",
	"3d8ba62efd187cb8751cb90f89f410cb82d443d2eb1fca61d8b10d839386456e",
	"37ad8d79d8dd69bb0cdbd9153cdf85567fd098de1fe1dee2109c984fe2f9828a",
	"f1b7401bf3d4b49f15bedafd1620d177676ee171f1f478c0465b1cd7cc1dbdd5",
	"850f4e8602b77a2e1a8e51b68017361af8faff7b8f20c8f6100ae789d9f399fc",
	"3b6f68916865acbb2536797944ee5d1a572306de2fbabab6b0a3e277b2b04738",
	"14d0ddd00f5401d1bac02ba1070836e17f7212c3422d918d85d57e61dc52dd81",
	"63cac44ac1db41ad2d28d8237a5a5cf2e5c6b7928c95ff2d281eef43543b7016",
	"2828623523e60d73ce899a76b04c3921ec4b763030732ad7592b31eaa9418935",
	"b4208c181c62f9f5d9cc2a87232c0c2045c8f7605ec057317fedd0075b0f2a63",
	"03e34f40c142624df9261524e36094c747778d6535b0d33295f65247df5db966",
	"4b0149c38a1ca7e7c87ec2204d5d8cbd05af0e5940a224a00215a4b98dda2af7",
	"4399193522b0fece366c3397b90ea1ac78d3e755d26eda677c28d1efb74af036",
	"71ac4aee3ae32a326f3f2e31c60885aeb485f237ae33f020ab0337fc70492e51",
	"2e9593b5fa469c66bb4ed74fd93f0ffdb47f938dc6aca9d201ba2ef8b1e41a69",
)

// Keys that carry a user's prompt, an agent's messages or tool output.
var (
	bannedEverywhere = []string{`"intent"`, `"output"`, `"suggestedReply"`, `"lastPrompt"`}
	bannedInTT       = []string{`"message"`, `"content"`, `"text"`}
)

const bannedPrefix = "/fixtures/"

func hashSet(hexes ...string) map[[32]byte]bool {
	out := make(map[[32]byte]bool, len(hexes))
	for _, h := range hexes {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 32 {
			panic("identifierDenylist: bad hash " + h)
		}
		out[[32]byte(b)] = true
	}
	return out
}

// provRow is one row of PROVENANCE.md's table.
type provRow struct{ Class, Origin string }

var provRowRE = regexp.MustCompile("^\\| `([^`]+)` \\| ([A-Z]+) \\| ([a-z]+) \\|")

func parseProvenance(b []byte) (map[string]provRow, []string) {
	rows := map[string]provRow{}
	var problems []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		m := provRowRE.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		if _, dup := rows[m[1]]; dup {
			problems = append(problems, "PROVENANCE.md lists "+m[1]+" twice")
		}
		rows[m[1]] = provRow{Class: m[2], Origin: m[3]}
	}
	return rows, problems
}

// corpusFile is one testdata file, slash-separated relative to testdata/.
type corpusFile struct {
	Rel  string
	Data []byte
}

// privacyFindings is G9 as a pure function, so each rule can be shown to fail.
func privacyFindings(files []corpusFile, rows map[string]provRow, deny map[[32]byte]bool) []string {
	var out []string
	have := map[string]bool{}
	for _, f := range files {
		have[f.Rel] = true
		for _, tok := range tokens(f.Rel) {
			if deny[sha256.Sum256([]byte(tok))] {
				out = append(out, f.Rel+": its path carries a denylisted identifier")
			}
		}
		for _, tok := range tokens(string(f.Data)) {
			if deny[sha256.Sum256([]byte(tok))] {
				out = append(out, f.Rel+": carries a denylisted identifier")
				break
			}
		}
		if bytes.Contains(f.Data, []byte(bannedPrefix)) || strings.Contains("/"+f.Rel, bannedPrefix) {
			out = append(out, f.Rel+": carries "+bannedPrefix+"; the corpus prefix is /testdata/projects")
		}
		if f.Rel == provenanceFile {
			continue
		}
		row, listed := rows[f.Rel]
		switch {
		case !listed:
			out = append(out, f.Rel+": has no row in PROVENANCE.md")
			continue
		case row.Class != "REAL" && row.Class != "DERIVED" && row.Class != "SYNTHETIC":
			out = append(out, f.Rel+": class "+row.Class+" is not REAL, DERIVED or SYNTHETIC")
		}
		if row.Origin != "magmux" && row.Origin != "tt" {
			out = append(out, f.Rel+": origin "+row.Origin+" is not magmux or tt")
		}
		ext := path.Ext(f.Rel)
		if ext != claude.RecordSuffix && ext != claude.TranscriptSuffix {
			continue
		}
		for _, k := range bannedEverywhere {
			if bytes.Contains(f.Data, []byte(k)) {
				out = append(out, f.Rel+": carries the key "+k+", banned corpus-wide. Redacted means the key is gone")
			}
		}
		if row.Origin == "magmux" && row.Class == "SYNTHETIC" {
			continue
		}
		for _, k := range bannedInTT {
			if bytes.Contains(f.Data, []byte(k)) {
				out = append(out, f.Rel+": carries the key "+k+", allowed only in a magmux-origin SYNTHETIC file")
			}
		}
	}
	for rel := range rows {
		if !have[rel] {
			out = append(out, "PROVENANCE.md lists "+rel+", which does not exist")
		}
	}
	return out
}

// tokens splits text into lowercased maximal runs of [a-z0-9].
func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

func loadCorpus(t *testing.T) []corpusFile {
	t.Helper()
	root := filepath.Join(moduleRoot(t), "testdata")
	var out []corpusFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, corpusFile{Rel: filepath.ToSlash(rel), Data: b})
		return nil
	})
	if err != nil {
		t.Fatalf("walking testdata: %v", err)
	}
	return out
}

func TestEveryTestdataFileHasProvenanceAndCarriesNoPrivateData(t *testing.T) {
	files := loadCorpus(t)
	var prov []byte
	for _, f := range files {
		if f.Rel == provenanceFile {
			prov = f.Data
		}
	}
	if prov == nil {
		t.Fatal("testdata/PROVENANCE.md is missing")
	}
	rows, dup := parseProvenance(prov)
	for _, d := range dup {
		t.Error(d)
	}
	if len(rows) < 100 || len(files) < 100 {
		t.Fatalf("%d rows over %d files: the parser or the walk is not seeing the corpus", len(rows), len(files))
	}
	if got := privacyFindings(files, rows, identifierDenylist); len(got) != 0 {
		t.Fatalf("testdata privacy or provenance:\n  %s", strings.Join(got, "\n  "))
	}
	var tt, mm int
	for _, r := range rows {
		switch r.Origin {
		case "tt":
			tt++
		case "magmux":
			mm++
		}
	}
	if tt == 0 || mm == 0 {
		t.Fatalf("the corpus is a union: %d tt-origin and %d magmux-origin rows", tt, mm)
	}
	t.Logf("%d files, %d provenance rows (%d tt, %d magmux)", len(files), len(rows), tt, mm)
}

func TestTheDenylistCanActuallyFail(t *testing.T) {
	// A test-only hash of a planted token: the real list is never spelled.
	planted := sha256.Sum256([]byte("zzplantedclient"))
	deny := map[[32]byte]bool{planted: true}
	rows := map[string]provRow{"roots/x/sessions/1.json": {"SYNTHETIC", "magmux"}}
	files := []corpusFile{{Rel: "roots/x/sessions/1.json", Data: []byte(`{"cwd":"/Users/user/ZZPlantedClient/app"}`)}}
	if got := privacyFindings(files, rows, deny); len(got) != 1 {
		t.Fatalf("a planted identifier: %d findings, want 1: %v", len(got), got)
	}
	files[0].Rel = "roots/zzplantedclient/sessions/1.json"
	rows = map[string]provRow{files[0].Rel: {"SYNTHETIC", "magmux"}}
	files[0].Data = []byte(`{}`)
	if got := privacyFindings(files, rows, deny); len(got) != 1 {
		t.Fatalf("a planted identifier in a path: %d findings, want 1: %v", len(got), got)
	}
}

func TestTheFieldDenylistIsByOrigin(t *testing.T) {
	msg := []byte(`{"type":"user","message":{"content":"hi"}}`)
	f := []corpusFile{{Rel: "roots/a/projects/p/x.jsonl", Data: msg}}
	if got := privacyFindings(f, map[string]provRow{f[0].Rel: {"DERIVED", "tt"}}, nil); len(got) != 2 {
		t.Errorf("message and content in a tt-origin file: %d findings, want 2: %v", len(got), got)
	}
	if got := privacyFindings(f, map[string]provRow{f[0].Rel: {"SYNTHETIC", "magmux"}}, nil); len(got) != 0 {
		t.Errorf("the same keys in a magmux-origin SYNTHETIC file must pass: %v", got)
	}
	if got := privacyFindings(f, map[string]provRow{f[0].Rel: {"DERIVED", "magmux"}}, nil); len(got) != 2 {
		t.Errorf("a magmux-origin DERIVED file has no exemption: %d findings: %v", len(got), got)
	}
	everywhere := []corpusFile{{Rel: "roots/a/jobs/j/state.json", Data: []byte(`{"intent":"x"}`)}}
	if got := privacyFindings(everywhere, map[string]provRow{everywhere[0].Rel: {"SYNTHETIC", "magmux"}}, nil); len(got) != 1 {
		t.Errorf("intent is banned corpus-wide: %d findings: %v", len(got), got)
	}
}

func TestTheOldPrefixAndMissingProvenanceFail(t *testing.T) {
	f := []corpusFile{{Rel: "roots/a/jobs/j/state.json", Data: []byte(`{"linkScanPath":"` + bannedPrefix + `projects/x.jsonl"}`)}}
	if got := privacyFindings(f, map[string]provRow{f[0].Rel: {"SYNTHETIC", "tt"}}, nil); len(got) != 1 {
		t.Errorf("a planted %s: %d findings, want 1: %v", bannedPrefix, len(got), got)
	}
	f[0].Data = []byte(`{}`)
	if got := privacyFindings(f, map[string]provRow{}, nil); len(got) != 1 {
		t.Errorf("an unlisted file: %d findings, want 1: %v", len(got), got)
	}
	if got := privacyFindings(nil, map[string]provRow{"gone.json": {"REAL", "tt"}}, nil); len(got) != 1 {
		t.Errorf("a stale row: %d findings, want 1: %v", len(got), got)
	}
	if got := privacyFindings(f, map[string]provRow{f[0].Rel: {"MAYBE", "elsewhere"}}, nil); len(got) != 2 {
		t.Errorf("a bad class and origin: %d findings, want 2: %v", len(got), got)
	}
}
