package obsview

// wireChanges is every difference between the recorded goldens and the
// current document, cited by the decision row that made it.
//
// The goldens under testdata/golden/magmux (C0.6) and testdata/golden/tt (M0)
// were recorded by magmux's own observation layer before the claudestate
// library replaced it (claudestate architecture §10). Each key is
// "<golden>|<row>|<field>": the golden file's name without .json, the row as
// golden_test.go's rowKeys names it ("<id>/<pid>", "/job:<job>" when the row
// carries the job member, "#<n>" for a repeat of the same name), and the
// member — or "+row" for a row the library adds, "-row" for one it no longer
// publishes, "*order" for a move. The value begins with the D-row and says,
// in one line, why.
//
// TestWireGoldens fails on a difference that is not here AND on an entry that
// names no difference. A golden is never re-recorded to make a difference
// disappear, and a testdata change cannot hide one either, because each golden
// directory's MANIFEST pins every input byte.
var wireChanges = map[string]string{
	// ── magmux set: registry-and-transcripts ───────────────────────────────
	//
	// D2. Colour is the last type:"agent-color" record's agentColor, and only
	// that. Each of these six transcripts carries a top-level agentColor on
	// some other record and no agent-color record at all (counted: 0 of each),
	// which the old reader took and tt measured to be wrong.
	"registry-and-transcripts|2f7a8e14-9c63-4051-b7d8-3e1a6f90c4b7/96632|color": "D2: the transcript has a top-level agentColor and no agent-color record",
	"registry-and-transcripts|5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/98299|color": "D2: the transcript has a top-level agentColor and no agent-color record",
	"registry-and-transcripts|8e2d4b17-0c93-4a6f-b512-77c3ad9e4f60/1724|color":  "D2: the transcript has a top-level agentColor and no agent-color record",
	"registry-and-transcripts|a1478d55-6e02-4bb9-93cf-01d2e8f47a6b/98693|color": "D2: the transcript has a top-level agentColor and no agent-color record",
	"registry-and-transcripts|c874de20-33c1-480a-ba90-8ae3f2f332c4/98198|color": "D2: the transcript has a top-level agentColor and no agent-color record",
	"registry-and-transcripts|d0c31e88-4f17-4a52-8e6d-b9a70c55f213/45682|color": "D2: the transcript has four top-level agentColor lines and no agent-color record",

	// ── tt set: bg-two-claimants ───────────────────────────────────────────
	"bg-two-claimants|20000000-0000-4000-8000-000000000011/70011|-row":              "D12: the dead kind:bg claimant 70011 is not a row of its own; two processes claiming one job are ONE session, keyed by the job, with 70011 among its Claimants (§3.4 rule 2, C5)",
	"bg-two-claimants|20000000-0000-4000-8000-000000000011/70012|-row":              "D6: the live claimant's (id, pid) row is now the job's row (K1 = K2, a twin)",
	"bg-two-claimants|20000000-0000-4000-8000-000000000011/70012/job:btc00001|+row": "D6: job btc00001's row, new on the wire with the job member, spoken by its live claimant 70012 (D12c)",

	// ── tt set: job-no-id-shared-short ─────────────────────────────────────
	"job-no-id-shared-short|/0|+row":   "D11: a job record with no sessionId is kept as its own orphan row (id \"\", pid 0, no job member); a D6 job row",
	"job-no-id-shared-short|/0#1|+row": "D11: the second id-less job directory is a second orphan row, never merged with the first on their shared daemonShort (§3.4 row 15); a D6 job row",

	// ── tt set: k2-duplicate-different-transcripts ─────────────────────────
	"k2-duplicate-different-transcripts|50000000-0000-4000-8000-0000000000a1/0/job:k2dup001|+row": "D6: a job row new to the wire; its two directories name one sessionId and one daemonShort, so they are ONE session spoken by the lower directory (D12d)",

	// ── tt set: the K3 duplicates ──────────────────────────────────────────
	"k3-duplicate-99-100|60000000-0000-4000-8000-000000000099/200#1|-row":          "D12: 99.json and 100.json name one sessionId and one pid, so they are ONE session (§3.4 rule 2) where the old code published two identical rows; 100.json, the lower name byte-wise, speaks",
	"k3-duplicate-both-mismatched|40000000-0000-4000-8000-000000000100/200#1|-row": "D12: 100.json and 150.json both say pid 200 and one sessionId, so they are ONE session (§3.4 rule 2) where the old code published two identical rows",

	// ── tt set: the lone kind:bg records (no job record) ───────────────────
	"lone-bg|857ef435-aa77-46f9-bb28-ca9c5374be9d/65863|-row":                              "D6: a kind:bg record naming a job that has no job record is keyed by that job (a lone K1), so its (id, pid) row becomes the (id, job) row",
	"lone-bg|857ef435-aa77-46f9-bb28-ca9c5374be9d/65863/job:857ef435|+row":                 "D6: the lone K1's row, now carrying the job member; its transcript is still located by session id",
	"lone-bg-with-transcript|30000000-0000-4000-8000-000000000021/70021|-row":              "D6: a lone K1 is keyed by its job, so its (id, pid) row becomes the (id, job) row",
	"lone-bg-with-transcript|30000000-0000-4000-8000-000000000021/70021/job:lbt00001|+row": "D6: the lone K1's row, with the job member; located by session id, as before",
	"lone-bg-two-claimants|30000000-0000-4000-8000-000000000031/70031|-row":                "D12: the dead claimant 70031 of one job is not a row of its own; it is one of the session's Claimants (§3.4 rule 2)",
	"lone-bg-two-claimants|30000000-0000-4000-8000-000000000031/70032|-row":                "D6: a lone K1 is keyed by its job, so the live claimant's (id, pid) row becomes the (id, job) row",
	"lone-bg-two-claimants|30000000-0000-4000-8000-000000000031/70032/job:lbc00001|+row":   "D6: the lone K1's row, spoken by its live claimant 70032 (D12c)",

	// ── tt set: the twins (one kind:bg record and its job record) ──────────
	"twin-dead-blocked|20000000-0000-4000-8000-000000000002/70001|-row":              "D6: a kind:bg record and its job record are one session keyed by the job (K1 = K2), so the (id, pid) row becomes the (id, job) row",
	"twin-dead-blocked|20000000-0000-4000-8000-000000000002/70001/job:tdb00001|+row": "D6: the twin's row: the job is blocked and its serving process is dead, so it is waiting and needs a human (twinStatus; a blocked job is announced)",
	"twin-dead-overlay|20000000-0000-4000-8000-000000000051/70051|-row":              "D6: a kind:bg record and its job record are one session keyed by the job (K1 = K2)",
	"twin-dead-overlay|20000000-0000-4000-8000-000000000051/70051/job:tdo00001|+row": "D6: the twin's row; the dead registry speaker still supplies pid, name and the other facts (§3.4 rule 2)",
	"twin-waiting-done|20000000-0000-4000-8000-000000000003/70002|-row":              "D6: a kind:bg record and its job record are one session keyed by the job (K1 = K2)",
	"twin-waiting-done|20000000-0000-4000-8000-000000000003/70002/job:twd00001|+row": "D6: the twin's row: the job is done, so status is null with statusRaw \"done\" and confidence authoritative — a pair no pre-library row had (§4.9)",

	// ── tt set: working-tempo-blocked ──────────────────────────────────────
	"working-tempo-blocked|10000000-0000-4000-8000-000000000001/0/job:wtb00001|+row": "D6: a job-only row (pid 0): working with tempo blocked is blocked, so it is waiting and needs a human (C19)",

	// ── tt set: tt-corpus ──────────────────────────────────────────────────
	"tt-corpus|/0|+row": "D11: jobs/no-session-id is kept as an orphan row (id \"\", pid 0, no job member) with a ProblemMissingField; a D6 job row",
	"tt-corpus|857ef435-aa77-46f9-bb28-ca9c5374be9d/65863|-row":                             "D6: background-process.json is a lone K1 (kind:bg, a jobId, no job record), keyed by its job",
	"tt-corpus|857ef435-aa77-46f9-bb28-ca9c5374be9d/65863/job:857ef435|+row":                "D6: the lone K1's row, with the job member",
	"tt-corpus|aaaaaaaa-1111-2222-3333-444444444444/2000|confidence":                        "D12: the second interactive process on one session id is its own (id, pid) session with its own actor, so it is now enriched; the old layer keyed actors by id and left it bare",
	"tt-corpus|aaaaaaaa-1111-2222-3333-444444444444/2000|attention":                         "D12: the second process on the id now has a verdict of its own (see confidence)",
	"tt-corpus|093c8e29-d6e3-47d9-9634-35686cc52c13/0/job:record-later-than-activity|+row":  "D6: a job-only row, blocked, new to the wire",
	"tt-corpus|11111111-1111-4111-8111-111111111111/0/job:working-fresh|+row":               "D6: a job-only row, working (busy on magmux's wire), new to the wire",
	"tt-corpus|22222222-2222-4222-8222-222222222222/0/job:working-transcript-stale|+row":    "D6: a job-only row, working (busy on magmux's wire), new to the wire",
	"tt-corpus|33333333-3333-4333-8333-333333333333/0/job:done|+row":                        "D6: a finished job-only row: status null with statusRaw \"done\" and confidence authoritative, a pair no pre-library row had (§4.9)",
	"tt-corpus|44444444-4444-4444-8444-444444444444/0/job:failed|+row":                      "D6: a failed job-only row: status null with statusRaw \"failed\" and confidence authoritative (§4.9)",
	"tt-corpus|55555555-5555-4555-8555-555555555555/0/job:deadbeef|+row":                    "D6: a job-only row keyed by its daemonShort deadbeef, not its directory shortid-mismatch (K2)",
	"tt-corpus|66666666-6666-4666-8666-666666666666/0/job:linkscan-outside-root|+row":       "D6: a job-only row, blocked; its linkScanPath is outside the projects tree, so no transcript is read",
	"tt-corpus|77777777-7777-4777-8777-777777777777/0/job:dup-a|+row":                       "D6: one of two jobs on one session id with different shorts: two sessions, both published (D12a)",
	"tt-corpus|77777777-7777-4777-8777-777777777777/0/job:dup-b|+row":                       "D6: the other job on the same session id (D12a)",
	"tt-corpus|7a324a6d-27ec-4431-b17b-70ab8ac764c7/0/job:blocked-beyond-retention|+row":    "D6: a job-only row, blocked, new to the wire",
	"tt-corpus|88888888-8888-4888-8888-888888888888/0/job:unknown-state|+row":               "D6: a job-only row whose state magmux has no word for: status null, the raw word kept",
	"tt-corpus|97030e53-3ae6-4b0e-9f5a-1c5d4b2a8e01/0/job:blocked-with-needs|+row":          "D6: a job-only row, blocked, new to the wire",
	"tt-corpus|d3ed2cc8-420f-4a35-be7b-5d374345faae/0/job:reaped-mid-work|+row":             "D6: a job-only row, working (busy on magmux's wire), new to the wire",
	"tt-corpus|ea780079-5a4c-444f-a80c-c6a7b29bf62c/0/job:blocked-no-needs-tempo-idle|+row": "D6: a job-only row, blocked, new to the wire",
	"tt-corpus|f350c44d-d22a-4c47-b3ef-00021713db7f/0/job:cross-kind|+row":                  "D6: a job-only row, blocked, new to the wire; an interactive record resuming its id is its own row (FR15)",
}
