package difftest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// The sandbox's pid map (§8.2 step 4) lives here, as pure functions over
// bytes, so CI tests the rewrite the driver performs. One bijection covers
// every canonical pid a scenario names ANYWHERE: registry file names, `.key`
// file names, record bodies and the agents listing. It is applied to every
// occurrence, so a file whose name and body agree still agree after it, and
// one whose name and body disagree on purpose still disagrees.

// pidMember matches the pid member of a JSON object, whitespace and all.
// The key is claude's constant: this is a byte rewrite inside a record, and it
// decodes nothing.
var pidMember = regexp.MustCompile(regexp.QuoteMeta(`"`+claude.KeyPID+`"`) + `(\s*:\s*)(-?[0-9]+)`)

// BodyPIDs lists every pid member's value in b.
func BodyPIDs(b []byte) []int {
	var out []int
	for _, m := range pidMember.FindAllSubmatch(b, -1) {
		if n, err := strconv.Atoi(string(m[2])); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// NamePID is the pid a registry-directory entry's NAME carries: the first
// dot-separated element when it is a decimal number ("98198.json",
// "98198.<hash>.key"), else 0.
func NamePID(name string) int {
	head, _, _ := strings.Cut(name, ".")
	if head == "" || head[0] < '0' || head[0] > '9' {
		return 0
	}
	n, err := strconv.Atoi(head)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// CanonicalPIDs is the sorted, de-duplicated set of pids in the registry
// entry names, their bodies and the listing.
func CanonicalPIDs(registry map[string][]byte, listing []byte) []int {
	set := map[int]bool{}
	for name, body := range registry {
		if n := NamePID(name); n > 0 {
			set[n] = true
		}
		for _, n := range BodyPIDs(body) {
			set[n] = true
		}
	}
	for _, n := range BodyPIDs(listing) {
		set[n] = true
	}
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// ErrUnmappedPID is a pid the map does not cover: the driver refuses to run a
// scenario it would only partly rewrite.
var ErrUnmappedPID = errors.New("difftest: a pid the map does not cover")

// RewritePIDs replaces every pid member's value through m.
func RewritePIDs(b []byte, m map[int]int) ([]byte, error) {
	var err error
	out := pidMember.ReplaceAllFunc(b, func(match []byte) []byte {
		sub := pidMember.FindSubmatch(match)
		n, perr := strconv.Atoi(string(sub[2]))
		if perr != nil || n <= 0 {
			return match
		}
		to, ok := m[n]
		if !ok {
			err = fmt.Errorf("%w: %d", ErrUnmappedPID, n)
			return match
		}
		return []byte(`"` + claude.KeyPID + `"` + string(sub[1]) + strconv.Itoa(to))
	})
	return out, err
}

// ReplacePrefix is §8.2 step 3's rewrite: inside JSON strings, the byte
// sequence `"<from>` becomes `"<to>`. It names no Claude key (G1). to must not
// need escaping in JSON, which the driver checks before it gets here.
func ReplacePrefix(b []byte, from, to string) []byte {
	return bytes.ReplaceAll(b, []byte(`"`+from), []byte(`"`+to))
}

// SafeForJSON reports whether s can be spliced into a JSON string verbatim,
// and quoted into a /bin/sh script: no quote of either kind, no backslash and
// no control character.
func SafeForJSON(s string) bool {
	for _, r := range s {
		if r == '"' || r == '\'' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// RegistryGroups lists, for FilenameOrderKept, every group of two or more
// registry files that share a sessionId and a Record.PID (after the
// body-else-filename fallback). A record that does not decode is in no group.
func RegistryGroups(registry map[string][]byte) [][]string {
	type gk struct {
		id  string
		pid int
	}
	by := map[gk][]string{}
	for name, body := range registry {
		if !claude.IsRecordName(name) {
			continue
		}
		r, _, err := claude.DecodeRecord(name, body)
		if err != nil {
			continue
		}
		k := gk{r.SessionID, r.PID}
		by[k] = append(by[k], name)
	}
	var out [][]string
	for _, names := range by {
		if len(names) >= 2 {
			sort.Strings(names)
			out = append(out, names)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// Rewrite is one prefix substitution Canonicalise applies to strings.
type Rewrite struct{ From, To string }

// Canonicalise is §8.2 step 10: tt's document mapped back to canonical form
// before it is recorded. Every "pid" member whose value the inverse map
// covers is mapped back, and every string equal to a Rewrite's From, or
// starting with From + "/", gets To instead; the first matching Rewrite wins,
// so the scenario's root ("…/root" -> "/testdata", making transcript.path
// record as "/testdata/projects/…") goes before the directory around it. The
// output is indented JSON with keys sorted.
//
// It fails when any string still contains forbid afterwards (the sandbox's
// own directory): a recorded reference never names a sandbox path.
func Canonicalise(doc []byte, inverse map[int]int, rewrites []Rewrite, forbid string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotATTDocument, err)
	}
	c := canon{inverse: inverse, rewrites: rewrites, forbid: forbid}
	v = c.walk(v)
	if c.leak != nil {
		return nil, c.leak
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

type canon struct {
	inverse  map[int]int
	rewrites []Rewrite
	forbid   string
	leak     error
}

func (c *canon) walk(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == claude.KeyPID {
				if n, ok := e.(json.Number); ok {
					if p, err := strconv.Atoi(string(n)); err == nil {
						if to, ok := c.inverse[p]; ok {
							x[k] = json.Number(strconv.Itoa(to))
							continue
						}
					}
				}
			}
			x[k] = c.walk(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = c.walk(e)
		}
		return x
	case string:
		for _, r := range c.rewrites {
			if x == r.From || strings.HasPrefix(x, r.From+"/") {
				x = r.To + x[len(r.From):]
				break
			}
		}
		if c.forbid != "" && strings.Contains(x, c.forbid) && c.leak == nil {
			c.leak = fmt.Errorf("difftest: a recorded string still names the sandbox: %q", x)
		}
		return x
	}
	return v
}
