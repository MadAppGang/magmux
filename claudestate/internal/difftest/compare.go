package difftest

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/verdict"
)

// Diff is one difference between the two sides.
//
// A FIELD difference names the field and both values. A ROW present on one
// side only is OneSided, with Field "" and the absent side's Value null; the
// key fields of a PAIRED row (the lone-K1 rule) are reported under the
// pseudo-field "key".
//
// Cited is the one D-row whose predicate holds ("D1" … "D12d"), or "" when
// none does. Candidates lists every predicate that held when more than one
// did: each difference must match EXACTLY one rule, so an ambiguous one is
// uncited and says why.
type Diff struct {
	Key        RowKey
	Field      string
	TT, Lib    Value
	Cited      string
	OneSided   bool
	Candidates []string
}

// FieldKey is the pseudo-field a paired row's key difference is reported on.
const FieldKey = "key"

// The D-rows a difference can be cited by (§8.1's attribution table).
const (
	D1      = "D1"
	D4      = "D4"
	D6Lone  = "D6-lone"
	D6Kind  = "D6-kind"
	D9a     = "D9a"
	D9b     = "D9b"
	D10     = "D10"
	D11     = "D11"
	D12a    = "D12a"
	D12b    = "D12b"
	D12c    = "D12c"
	D12d    = "D12d"
	rowLibs = "lib-only"
	rowTTs  = "tt-only"
)

// speakerDerived is D12c's closed field set: exactly the fields a registry
// SPEAKER decides (review 3 Issue 8). job.*, recordUpdatedAt, recordAgeMs,
// transcript.*, listedBy and the key fields come from the job record or the
// session id, which both sides read identically, so they are not here.
var speakerDerived = map[string]bool{
	FieldPID: true, FieldProcAlive: true, FieldStatus: true, FieldStatusRaw: true, FieldStatusSince: true,
	FieldWaitReason: true, FieldRawWaitingFor: true, FieldStatusAgeMs: true, FieldBlockedForMs: true,
	FieldName: true, FieldNameSource: true, FieldFormerNames: true, FieldCWD: true, FieldStartedAt: true,
	FieldClaudeVersion: true, FieldMessagingSocketPath: true, FieldTmux: true, FieldPaneAlive: true,
	FieldAttention: true,
}

// windowFields are D10's: a fact's window. None is projected today (tt's
// sizeBytes, tailBytes and truncated, and the library's Window, are one side
// only), so D10 can only ever cite a field a future projection adds here.
var windowFields = map[string]bool{}

// claudeKeyOf names, for D4, the Claude key a projected instant decodes from.
var claudeKeyOf = map[string][]string{
	FieldStatusSince:     {claude.KeyStatusUpdatedAt, claude.KeyUpdatedAt},
	FieldStartedAt:       {claude.KeyStartedAt, claude.KeyCreatedAt},
	FieldRecordUpdatedAt: {claude.KeyUpdatedAt},
	FieldReapedMidWorkAt: {claude.KeyReapedMidWorkAt},
	FieldLastActivityAt:  {claude.KeyTimestamp},
}

// Compare matches the two keyed sets, then applies ONE pairing rule (the
// lone K1, §8.1) to the rows left unmatched, and attributes every difference.
// Input order never matters: the output is sorted.
//
// problems are the library delivery's; they are tied to rows exactly as
// FromModel ties them, and added to whatever FromModel already tied.
func Compare(tt, lib []Row, problems []model.Problem) []Diff {
	lib = slices.Clone(lib)
	for i := range lib {
		if lib[i].Lib == nil {
			lib[i].Lib = &LibFacts{}
		}
		facts := *lib[i].Lib
		facts.Problems = append(slices.Clone(facts.Problems), problemsFor(lib[i], problems)...)
		lib[i].Lib = &facts
	}
	c := comparison{tt: tt, lib: lib}
	c.run()
	slices.SortFunc(c.out, func(a, b Diff) int {
		if x := cmp.Compare(a.Key.String(), b.Key.String()); x != 0 {
			return x
		}
		return cmp.Compare(a.Field, b.Field)
	})
	return c.out
}

type comparison struct {
	tt, lib []Row
	out     []Diff
}

func (c *comparison) run() {
	ttBy := map[RowKey][]int{}
	for i, r := range c.tt {
		ttBy[r.Key] = append(ttBy[r.Key], i)
	}
	ttUsed := make([]bool, len(c.tt))
	libUsed := make([]bool, len(c.lib))

	// 1. Keyed matching. A key repeated on one side matches once; the
	// repeat is left for the pairing rule and then reported one-sided.
	for li, l := range c.lib {
		for _, ti := range ttBy[l.Key] {
			if !ttUsed[ti] {
				ttUsed[ti], libUsed[li] = true, true
				c.fields(c.tt[ti], l, false)
				break
			}
		}
	}

	// 2. The pairing rule, over what is left. A lone K1 lib row and a tt row
	// on its session id with no shortId and a pid among its claimants are
	// paired when each is the other's ONLY candidate.
	cands := map[int][]int{}   // lib index -> tt candidates
	reverse := map[int][]int{} // tt index -> lib candidates
	for li, l := range c.lib {
		if libUsed[li] || !loneK1(l) {
			continue
		}
		for ti, t := range c.tt {
			if ttUsed[ti] || t.Key.SessionID != l.Key.SessionID || t.Key.Job != "" || t.Key.Orphan != "" {
				continue
			}
			if !slices.Contains(l.Lib.Claimants, t.Key.PID) {
				continue
			}
			cands[li] = append(cands[li], ti)
			reverse[ti] = append(reverse[ti], li)
		}
	}
	for li, ts := range cands {
		if len(ts) != 1 || len(reverse[ts[0]]) != 1 {
			continue
		}
		ti := ts[0]
		ttUsed[ti], libUsed[li] = true, true
		t, l := c.tt[ti], c.lib[li]
		c.add(Diff{Key: l.Key, Field: FieldKey, TT: Value{S: t.Key.String()}, Lib: Value{S: l.Key.String()}}, []string{D6Lone})
		c.fields(t, l, true)
	}

	// 3. What is still unmatched is one side only.
	for li, l := range c.lib {
		if !libUsed[li] {
			c.add(Diff{Key: l.Key, OneSided: true, TT: null, Lib: Value{S: rowLibs}}, c.libOnly(l))
		}
	}
	for ti, t := range c.tt {
		if !ttUsed[ti] {
			c.add(Diff{Key: t.Key, OneSided: true, TT: Value{S: rowTTs}, Lib: null}, c.ttOnly(t))
		}
	}
}

// loneK1: a kind:"bg" record keyed by its job, with no job record (§8.1).
func loneK1(l Row) bool { return l.Key.Job != "" && !l.Lib.HasJob && l.Lib.HasRegistry }

// fields compares every projected field of a matched or paired row.
func (c *comparison) fields(t, l Row, paired bool) {
	names := map[string]bool{}
	for n := range t.Fields {
		names[n] = true
	}
	for n := range l.Fields {
		names[n] = true
	}
	for n := range names {
		tv, lv := t.field(n), l.field(n)
		if tv == lv {
			continue
		}
		d := Diff{Key: l.Key, Field: n, TT: tv, Lib: lv}
		c.add(d, fieldRules(d, t, l, paired))
	}
}

func (c *comparison) add(d Diff, matched []string) {
	switch len(matched) {
	case 1:
		d.Cited = matched[0]
	case 0:
	default:
		d.Candidates = matched
	}
	c.out = append(c.out, d)
}

// fieldRules returns every D-row whose predicate holds for one field
// difference on a matched (or paired) row.
func fieldRules(d Diff, t, l Row, paired bool) []string {
	var out []string
	lf := l.Lib

	// D1: tt null ∧ lib 0 ∧ EvidenceAbsent.
	if d.Field == FieldSubagents && d.TT.Null && d.Lib == (Value{S: "0"}) && lf.SubagentEvidence == model.EvidenceAbsent {
		out = append(out, D1)
	}
	// D4: lib nil ∧ a ProblemImplausibleInstant on that key naming that
	// field's Claude key.
	if keys, ok := claudeKeyOf[d.Field]; ok && d.Lib.Null {
		for _, p := range lf.Problems {
			if p.Kind == model.ProblemImplausibleInstant && slices.Contains(keys, p.Field) {
				out = append(out, D4)
				break
			}
		}
	}
	// D6-lone: a paired row, on kind or shortId (the key itself is cited
	// where it is reported).
	if paired && (d.Field == FieldKind || d.Field == FieldShortID) {
		out = append(out, D6Lone)
	}
	// D6-kind: a matched K3 row, on kind, that is a kind:"bg" record with no
	// jobId, which tt publishes as interactive.
	if !paired && d.Field == FieldKind && lf.Kind == model.KindBackground && lf.KindRaw == claude.ValueKindBG &&
		l.Key.Job == "" && l.Key.Orphan == "" && d.TT == (Value{S: "interactive"}) {
		out = append(out, D6Kind)
	}
	if d.Field == FieldAttention {
		ttStatus := t.field(FieldStatus)
		// D9a: lib undetermined ∧ tt false ∧ tt not waiting/blocked ∧ an
		// undetermined reason.
		if d.Lib.Null && d.TT == (Value{S: "false"}) && ttStatus != (Value{S: "waiting"}) && ttStatus != (Value{S: "blocked"}) &&
			verdict.IsUndetermined(lf.AttentionReason) {
			out = append(out, D9a)
		}
		// D9b: lib false ∧ tt true ∧ no job ∧ the process is gone ∧ Rule 1.
		if d.Lib == (Value{S: "false"}) && d.TT == (Value{S: "true"}) && !lf.HasJob && lf.ProcessAlive == model.LivenessNo &&
			verdict.IsLivenessDismissal(lf.AttentionReason) {
			out = append(out, D9b)
		}
	}
	// D11 on a matched row: the library joined an id-less record to the
	// listing entry naming its pid; tt dropped the record at parse and
	// admitted the entry alone. Every conjunct: a matched K3 row with an id,
	// a registry speaker and no job; the speaker's record has no sessionId;
	// tt's row was listed by the agents listing ALONE; and the difference is
	// either listedBy gaining exactly "registry", or a registry-speaker field
	// on which the library has the record's value.
	if !paired && joinedIDLess(l) && t.field(FieldListedBy) == listedByAgentsOnly {
		switch {
		case d.Field == FieldListedBy && d.Lib == listedByRegistryAndAgents:
			out = append(out, D11)
		case speakerDerived[d.Field] && !d.Lib.Null:
			out = append(out, D11)
		}
	}
	// D10: a window field.
	if windowFields[d.Field] {
		out = append(out, D10)
	}
	// D12c: a speaker-derived field where tt's pid is not the library's
	// speaker, both are claimants, and a duplicate job claim is on the key.
	if speakerDerived[d.Field] && hasProblem(lf.Problems, model.ProblemDuplicateJobClaim) {
		ttPID := atoi(t.field(FieldPID))
		if ttPID != lf.PID && ttPID > 0 && slices.Contains(lf.Claimants, ttPID) && slices.Contains(lf.Claimants, lf.PID) {
			out = append(out, D12c)
		}
	}
	// D12d: lastActivityAt on a matched K2 row whose key carries a
	// duplicate-key problem naming two or more job directories.
	if !paired && d.Field == FieldLastActivityAt && l.Key.Job != "" && lf.HasJob && !lf.HasRegistry {
		for _, p := range lf.Problems {
			if p.Kind == model.ProblemDuplicateKey && len(p.PIDs) == 0 && namesDirectories(p.Detail) >= 2 {
				out = append(out, D12d)
				break
			}
		}
	}
	return out
}

// libOnly attributes a row the library has and tt does not.
func (c *comparison) libOnly(l Row) []string {
	var out []string
	if hasProblem(l.Lib.Problems, model.ProblemMissingField) {
		out = append(out, D11)
	}
	// D12a: tt kept one record on this id and discarded the rest, so the
	// library has two or more rows on an id tt also has.
	if l.Key.SessionID != "" {
		onID := 0
		for _, o := range c.lib {
			if o.Key.SessionID == l.Key.SessionID {
				onID++
			}
		}
		ttHas := false
		for _, t := range c.tt {
			if t.Key.SessionID == l.Key.SessionID {
				ttHas = true
				break
			}
		}
		if onID >= 2 && ttHas {
			out = append(out, D12a)
		}
	}
	return out
}

// ttOnly attributes a row tt has and the library does not. D12b: a
// non-winning kind:"bg" claimant that tt publishes as a registry row and the
// library merged into its job's session.
func (c *comparison) ttOnly(t Row) []string {
	if t.Key.SessionID == "" || t.Key.Job != "" || t.Key.PID <= 0 {
		return nil
	}
	for _, l := range c.lib {
		if l.Key.SessionID != t.Key.SessionID || l.Key.Job == "" || !slices.Contains(l.Lib.Claimants, t.Key.PID) {
			continue
		}
		for _, p := range l.Lib.Problems {
			if p.Kind == model.ProblemDuplicateJobClaim && slices.Contains(p.PIDs, t.Key.PID) {
				return []string{D12b}
			}
		}
	}
	return nil
}

// The two listedBy values D11's matched-row predicate names.
var (
	listedByAgentsOnly        = list([]string{sourceWord(model.SourceAgents)})
	listedByRegistryAndAgents = list([]string{sourceWord(model.SourceRegistry), sourceWord(model.SourceAgents)})
)

// joinedIDLess: a K3 library row carrying a session id, spoken for by a
// registry record that has none (ProblemMissingField on sessionId), with no
// job. Only join's D11 x FR15 rule makes one.
func joinedIDLess(l Row) bool {
	if l.Key.SessionID == "" || l.Key.PID <= 0 || l.Key.Job != "" || l.Key.Orphan != "" ||
		!l.Lib.HasRegistry || l.Lib.HasJob {
		return false
	}
	for _, p := range l.Lib.Problems {
		if p.Kind == model.ProblemMissingField && p.Field == claude.KeySessionID && p.Path == l.Lib.RegistryFile {
			return true
		}
	}
	return false
}

func hasProblem(ps []model.Problem, k model.ProblemKind) bool {
	for _, p := range ps {
		if p.Kind == k {
			return true
		}
	}
	return false
}

func atoi(v Value) int {
	if v.Null {
		return 0
	}
	n, _ := strconv.Atoi(v.S)
	return n
}

// namesDirectories counts the directories a job duplicate-key problem names.
// join writes "<n> job directories describe one session: a, b; a speaks".
func namesDirectories(detail string) int {
	head, rest, ok := strings.Cut(detail, " job directories describe one session: ")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(head)
	if err != nil {
		return 0
	}
	list, _, _ := strings.Cut(rest, ";")
	if got := len(strings.Split(list, ", ")); got != n {
		return 0
	}
	return n
}
