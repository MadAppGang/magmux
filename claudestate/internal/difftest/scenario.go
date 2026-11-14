package difftest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
)

// Scenario is one differential case: a scenario root under testdata/roots,
// the agents listing tt's `claude` stub prints (and the library is fed), and
// which canonical pids are dead. Every other pid the scenario names is alive.
type Scenario struct {
	Name string
	Root string // under testdata/roots/
	// Listing is under testdata/, e.g. "agents/real-39.json". "" means the
	// claude stub exits 127, and the library's listing fails the same way.
	Listing string
	Dead    []int
}

// Corpus is every scenario `_diff -all-corpus` runs, in this order. The dead
// pids are PROVENANCE.md's scenario table, LIVE-PIDS.txt for the S-origin
// corpus, and tt's own dead-pid.json for tt's.
func Corpus() []Scenario {
	tt := []int{999001} // tt's registry/dead-pid.json
	out := []Scenario{
		{Name: "magmux-corpus", Root: "magmux-corpus", Dead: []int{60001, 60011}},
		{Name: "tt-corpus", Root: "tt-corpus", Dead: tt},
	}
	for _, l := range []string{
		"background-only", "cross-kind-sessionid", "empty", "interactive-no-registry",
		"malformed", "real-37-with-waitingfor", "real-39",
	} {
		out = append(out, Scenario{Name: "tt-corpus.agents-" + l, Root: "tt-corpus", Listing: "agents/" + l + ".json", Dead: tt})
	}
	for _, s := range []Scenario{
		{Name: "working-tempo-blocked"},
		{Name: "twin-dead-blocked", Dead: []int{70001}},
		{Name: "twin-waiting-done", Dead: []int{70002}},
		{Name: "bg-two-claimants", Dead: []int{70011}},
		{Name: "lone-bg"},
		{Name: "lone-bg-with-transcript"},
		{Name: "lone-bg-two-claimants", Dead: []int{70031}},
		{Name: "bg-spare-no-jobid"},
		{Name: "job-no-id-shared-short"},
		{Name: "k3-duplicate-both-mismatched"},
		{Name: "twin-dead-overlay", Dead: []int{70051}},
		{Name: "k2-duplicate-different-transcripts"},
		{Name: "k3-duplicate-99-100"},
	} {
		s.Root = s.Name
		out = append(out, s)
	}
	return out
}

// ScenarioNamed finds one scenario of the corpus.
func ScenarioNamed(name string) (Scenario, bool) {
	for _, s := range Corpus() {
		if s.Name == name {
			return s, true
		}
	}
	return Scenario{}, false
}

// Recorded is `<scenario>.liveness.json`: what a recorded reference was run
// against, in CANONICAL pids, so the CI test can feed the library the same
// inputs tt saw.
type Recorded struct {
	Scenario string `json:"scenario"`
	Root     string `json:"root"`
	Listing  string `json:"listing"`
	// Liveness: canonical pid -> "alive" | "dead", for every pid the
	// scenario names.
	Liveness map[string]string `json:"liveness"`
}

// Liveness words.
const (
	Alive = "alive"
	Dead  = "dead"
)

// NewRecorded builds the inputs record for a scenario over its pid set.
func NewRecorded(s Scenario, pids []int) Recorded {
	r := Recorded{Scenario: s.Name, Root: s.Root, Listing: s.Listing, Liveness: map[string]string{}}
	for _, p := range pids {
		w := Alive
		if slices.Contains(s.Dead, p) {
			w = Dead
		}
		r.Liveness[strconv.Itoa(p)] = w
	}
	return r
}

// Encode is the recorded bytes: indented, keys sorted, one trailing newline.
func (r Recorded) Encode() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
	return b.Bytes()
}

// DecodeRecorded reads `<scenario>.liveness.json`.
func DecodeRecorded(b []byte) (Recorded, error) {
	var r Recorded
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Recorded{}, fmt.Errorf("difftest: a recorded inputs file: %w", err)
	}
	for k, v := range r.Liveness {
		if _, err := strconv.Atoi(k); err != nil || (v != Alive && v != Dead) {
			return Recorded{}, fmt.Errorf("difftest: a recorded inputs file: liveness %q: %q", k, v)
		}
	}
	return r, nil
}

// DeadPIDs lists the recorded dead pids, ascending.
func (r Recorded) DeadPIDs() []int {
	var out []int
	for k, v := range r.Liveness {
		if v == Dead {
			n, _ := strconv.Atoi(k)
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}
