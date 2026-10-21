# Test data provenance

Where every file under `claudestate/testdata/` came from, and how far it can be
trusted. It is the ONE provenance document for the corpus: tt's per-directory
READMEs were folded into it and not copied. `internal/audit`'s G9 guard parses
the table at the end of this file. Every file under `testdata/` must have a row
there, with a class and an origin, and every row must name a file that exists.

## Classes

- **REAL**: a capture from a live machine, byte for byte.
- **DERIVED**: a live capture or a real record's shape, with values rewritten.
  Every identifier was replaced, length-preserving, and the rest was kept.
- **SYNTHETIC**: constructed to exercise a case. Nobody observed it live, so
  a test over it is a declared substitution, not a proof.

The two origins are `magmux` and `tt`. A magmux-origin file was written by this
repository: the S-origin corpus, or the scenarios added for the library. A
tt-origin file is a scrubbed copy of `tools/tt/internal/observer/testdata` at
commit `51ec374` of the tt repository, read with `git show` and never edited
there.

## Rules these files enforce on the tests

1. **Nothing here touches the real Claude root.** Every path a record names is
   under `/Users/user/…` or under the canonical projects prefix
   `/testdata/projects`, and both name nothing on a real machine.
2. **No test probes a pid named here.** A pid in this data could collide with a
   real process. A test answers liveness from its own fake oracle, and
   `roots/magmux-corpus/sessions/LIVE-PIDS.txt` is the S-origin oracle.
3. **No test reads a `.key` file.** They are named like key material and hold a
   canary string.
4. **The privacy denylist (G9).** `intent`, `output`, `suggestedReply` and
   `lastPrompt` appear in no file. `message`, `content` and `text` appear in no
   tt-origin file, which is tt's own rule. They are allowed in magmux-origin
   SYNTHETIC transcripts, which need them for turn parsing, latest text, API
   error text and the padding of the bounded-read cases. A hashed identifier
   denylist covers the whole corpus, so this repository never spells the names
   it bans.

## `roots/magmux-corpus/`: the S-origin corpus

A copy of `internal/obs/testdata/{sessions,projects}` in the root module,
byte-identical to it (`diff -r` clean at copy time). That directory's own
`PROVENANCE.md` is the authority on each file. The rows below give the class it
records. Two re-labellings are deliberate, and neither changes a byte:

- Every record that document calls REAL is **DERIVED** here, because that
  corpus was sanitised and "no file is byte-identical to what was captured".
- All eight transcripts are **SYNTHETIC**. Their keys are real and their
  content was constructed, and G9 allows `message`, `content` and `text` in
  SYNTHETIC magmux-origin files only.

Under D2 none of these transcripts has a colour. They carry `agentColor` on
records whose type is not `agent-color`, and the library reads colour only from
the dedicated record. The files are **not edited**, so the root module's wire
goldens keep their input bytes. The colour cases they used to carry are
re-expressed in `roots/agent-color/`.

## `roots/tt-corpus/` and `agents/`: tt's corpus, scrubbed

tt's flat `registry/`, `jobs/` and `projects/` became one Claude root:
`sessions/`, `jobs/` and `projects/`. Registry files keep tt's descriptive names.
They are not `<pid>.json`, so every pid comes from the body. Not copied: the
four READMEs (folded into this document) and `agents/hangs` (a shell script for
tt's spawn timeout; the library spawns nothing).

**The scrub was LENGTH-PRESERVING and mechanical.** Every replacement has the
exact byte length of what it replaced, so file sizes, offsets and directory-name
lengths are unchanged. tt's tests that assert a byte count, such as the 11,594-
and 78,212-byte transcripts, still hold. The originals are deliberately not
listed: a mapping table would republish what the scrub removed. What each class
became:

| Class of identifier | Became |
|---|---|
| the account name in every path and project-directory name | `user` |
| tt's synthetic projects prefix, `/` + a word this repository does not use + `/projects` | `/testdata/projects` (same length) |
| client and product names in paths, tmux session names and record names | neutral words of the same length (`alpha`, `alphamini`, `betaworks`, `gammaapp`, `deltax`, `epsilonapp`, `zetadbx`, `chatserver`, `storeco`, `omega`, `memidx`, `kappa`, `proxyapp`, `benchapp`, `upgrader`) |
| a person's name in a worktree path | `kilo` |
| every session `name` in the three live agents listings (titles generated from conversation content) | `sNN-xxxx…`, the same length, one per distinct name |

The JSON stayed valid in every file that was valid before. The scrub program
checked that. `linkscan-outside-root`'s `../../../etc/passwd` escape survives
unchanged. `/testdata/projects` is the corpus's canonical projects path: it is
the `Tree.Path` a test gives the projects tree, which is what makes the
scrubbed `linkScanPath`s resolve under tt's own lexical containment rule (§8.1).

tt's labels, folded in. For the registry, "REAL shape" became DERIVED. The
SYNTHETIC records are `permission prompt`, no `statusUpdatedAt`, an unknown
status, no tmux, string former names, torn, the duplicate pair and no session
id. For jobs, every record is "derived from a REAL record's key set and shape".
The ones tt calls REAL shape are DERIVED here and the rest are SYNTHETIC.
`pins.json` is REAL (contents `[]`). For projects, the four REAL-shape
transcripts are DERIVED and the rest SYNTHETIC. Two `linkScanPath` targets are
deliberately absent, as tt's are.

## `roots/agent-color/`: D2's colour cases, re-expressed

New SYNTHETIC transcripts, origin magmux, with real `agent-color` records:

- `c0100001…`: `blue`, `green`, then an assistant record carrying
  `agentColor: "red"` and `color: "pink"`, then `yellow`, then an
  `agent-name` record with a bare `color`. The answer is `yellow`. It replaces
  magmux's last-record-wins case (`d0c31e88`).
- `c0100002…`: 48 KB whose only colour record is the first line, `purple`. It
  is inside the 200 KB window, and outside a test's 8 KB one. D2 has no head
  fallback, so the second answer is nil. It replaces `2f7a8e14`.
- `c0100003…` and `…moved/b0100003…`: a session's own transcript (`orange`),
  and a directory holding ANOTHER session's transcript whose colour is
  `WRONG-SESSION-DO-NOT-REPORT-THIS`. Location is by session id, so the decoy
  is never read. It replaces the wrong-directory trap.
- `c0100004…`: `green`, then an explicitly empty colour. `""` is a value, not
  an absence.

## The scenario roots (§8.1), SYNTHETIC, origin magmux

Each is `<scenario>/{sessions,jobs,projects}`, holding only what the case needs.
Records use the 19 universal registry keys and the job keys tt measured, minus
the four the privacy denylist bans. The reference instant is
`1789699211204` (2026-09-18T02:40:11.204Z). Which pids a test treats as dead is
part of the scenario:

| Scenario | Dead pids | The case |
|---|---|---|
| `working-tempo-blocked` | — | a job-only `working` + `tempo: blocked`: blocked, waiting applies, attention needed |
| `twin-dead-blocked` | 70001 | a twin whose process is dead over a blocked job: blocked |
| `twin-waiting-done` | 70002 | registry waiting over a done job, process dead: done, no waiting |
| `bg-two-claimants` | 70011 | two `kind:bg` records on one job with a job record (D12b, D12c) |
| `lone-bg` | — | tt's `background-process.json` alone (D6-lone) |
| `lone-bg-with-transcript` | — | a lone `kind:bg` record whose id has a transcript |
| `lone-bg-two-claimants` | 70031 | two `kind:bg` records on one job, no job record |
| `bg-spare-no-jobid` | — | `kind:bg` with no `jobId` (D6-kind) |
| `job-no-id-shared-short` | — | two id-less job directories sharing one `daemonShort`: two orphans |
| `k3-duplicate-both-mismatched` | — | `100.json` and `150.json`, both body pid 200, one id: `100.json` speaks |
| `twin-dead-overlay` | 70051 | `twin-dead-blocked` with a registry name, cwd and version that differ from the job's |
| `k2-duplicate-different-transcripts` | — | job directories `a` and `b`, one id and short, two transcripts: `a` speaks |
| `k3-duplicate-99-100` | — | `99.json` and `100.json`, body pid 200: `100.json` speaks, byte-wise |

## Every file

| File | Class | Origin | What it is for |
|---|---|---|---|
| `agents/background-only.json` | DERIVED | tt | the three pid-less entries |
| `agents/cross-kind-sessionid.json` | DERIVED | tt | one id as both kinds |
| `agents/empty.json` | SYNTHETIC | tt | `[]`, a valid empty listing |
| `agents/interactive-no-registry.json` | DERIVED | tt | an interactive entry no registry record describes |
| `agents/malformed.json` | SYNTHETIC | tt | not JSON: a FAILED listing, never an empty one |
| `agents/real-37-with-waitingfor.json` | DERIVED | tt | a 35-entry capture with one entry set waiting |
| `agents/real-39.json` | DERIVED | tt | a 39-entry live listing; every name replaced |
| `roots/agent-color/projects/-Users-user-dev-colour--claude-worktrees-moved/b0100003-0000-4000-8000-000000000003.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/agent-color/projects/-Users-user-dev-colour/c0100001-0000-4000-8000-000000000001.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/agent-color/projects/-Users-user-dev-colour/c0100002-0000-4000-8000-000000000002.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/agent-color/projects/-Users-user-dev-colour/c0100003-0000-4000-8000-000000000003.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/agent-color/projects/-Users-user-dev-colour/c0100004-0000-4000-8000-000000000004.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/bg-spare-no-jobid/sessions/70041.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/bg-two-claimants/jobs/btc00001/state.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/bg-two-claimants/sessions/70011.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/bg-two-claimants/sessions/70012.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/job-no-id-shared-short/jobs/nid-a/state.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/job-no-id-shared-short/jobs/nid-b/state.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/k2-duplicate-different-transcripts/jobs/a/state.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/k2-duplicate-different-transcripts/jobs/b/state.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/k2-duplicate-different-transcripts/projects/-Users-user-dev-k2dup-a/50000000-0000-4000-8000-0000000000a1.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/k2-duplicate-different-transcripts/projects/-Users-user-dev-k2dup-b/50000000-0000-4000-8000-0000000000a1.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/k3-duplicate-99-100/sessions/100.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/k3-duplicate-99-100/sessions/99.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/k3-duplicate-both-mismatched/sessions/100.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/k3-duplicate-both-mismatched/sessions/150.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/lone-bg-two-claimants/sessions/70031.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/lone-bg-two-claimants/sessions/70032.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/lone-bg-with-transcript/projects/-Users-user-dev-sample/30000000-0000-4000-8000-000000000021.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/lone-bg-with-transcript/sessions/70021.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/lone-bg/sessions/background-process.json` | DERIVED | tt | tt's background-process.json alone (D6-lone) |
| `roots/magmux-corpus/projects/-Users-user-dev-agent-fleet-sessions/3b90f6cc-1d47-4e20-8a3b-9f5e6c0d2a18.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux--claude-worktrees-claude-session-control/b0000000-1111-4222-8333-444444444444.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux/2f7a8e14-9c63-4051-b7d8-3e1a6f90c4b7.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux/a1478d55-6e02-4bb9-93cf-01d2e8f47a6b.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux/a1478d55-6e02-4bb9-93cf-01d2e8f47a6b/subagents/agent-001.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux/a1478d55-6e02-4bb9-93cf-01d2e8f47a6b/subagents/agent-002.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux/a1478d55-6e02-4bb9-93cf-01d2e8f47a6b/subagents/agent-003.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux/a1478d55-6e02-4bb9-93cf-01d2e8f47a6b/subagents/notes.txt` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux/c874de20-33c1-480a-ba90-8ae3f2f332c4.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux/c874de20-33c1-480a-ba90-8ae3f2f332c4/subagents/.gitkeep` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-magmux/d0c31e88-4f17-4a52-8e6d-b9a70c55f213.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-001.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-002.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-003.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-004.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-005.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-006.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-007.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-008.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-009.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-010.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-011.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-012.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-013.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-014.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-015.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-016.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-017.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-018.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-019.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-020.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-021.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-022.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3/subagents/agent-023.meta.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/projects/-Users-user-dev-sampleapp/8e2d4b17-0c93-4a6f-b512-77c3ad9e4f60.jsonl` | SYNTHETIC | magmux | see the section for this root |
| `roots/magmux-corpus/sessions/15775.json` | DERIVED | magmux | `shell` |
| `roots/magmux-corpus/sessions/1724.json` | DERIVED | magmux | `waiting` + `input needed`, the measured case |
| `roots/magmux-corpus/sessions/1828.json` | DERIVED | magmux | `shell`; no transcript anywhere, deliberately |
| `roots/magmux-corpus/sessions/45682.json` | DERIVED | magmux | former names as bare strings (shape UNCONFIRMED) |
| `roots/magmux-corpus/sessions/56526.e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855.key` | DERIVED | magmux | a `.key` sibling: real name shape, canary content; never opened |
| `roots/magmux-corpus/sessions/60001.json` | SYNTHETIC | magmux | dead pid |
| `roots/magmux-corpus/sessions/60002.json` | SYNTHETIC | magmux | `permission prompt`, never observed live |
| `roots/magmux-corpus/sessions/60003.json` | SYNTHETIC | magmux | `compacting`, an unrecognised status |
| `roots/magmux-corpus/sessions/60004.json` | SYNTHETIC | magmux | torn read, stops mid-string |
| `roots/magmux-corpus/sessions/60005.json` | SYNTHETIC | magmux | zero bytes |
| `roots/magmux-corpus/sessions/60006.json` | SYNTHETIC | magmux | `waiting` with no waitingFor |
| `roots/magmux-corpus/sessions/60007.json` | SYNTHETIC | magmux | a future record with keys no source names |
| `roots/magmux-corpus/sessions/60008.json` | SYNTHETIC | magmux | every key reordered |
| `roots/magmux-corpus/sessions/60009.json` | SYNTHETIC | magmux | a terminal coordinate with no separators |
| `roots/magmux-corpus/sessions/60010.json` | SYNTHETIC | magmux | a three-day-old status instant |
| `roots/magmux-corpus/sessions/60011.json` | SYNTHETIC | magmux | filename 60011, body 60099 |
| `roots/magmux-corpus/sessions/60012.json` | SYNTHETIC | magmux | live pid, dead pane |
| `roots/magmux-corpus/sessions/76136.json` | DERIVED | magmux | former names as objects, Claude Code 2.1.276, every key |
| `roots/magmux-corpus/sessions/96632.json` | DERIVED | magmux | real pid, SYNTHETIC omission of the terminal coordinate |
| `roots/magmux-corpus/sessions/98198.b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9.key` | DERIVED | magmux | a `.key` sibling: real name shape, canary content; never opened |
| `roots/magmux-corpus/sessions/98198.json` | DERIVED | magmux | `idle`; a live capture, sanitised |
| `roots/magmux-corpus/sessions/98299.json` | DERIVED | magmux | `busy`; the 0-against-23 subagent case |
| `roots/magmux-corpus/sessions/98693.json` | DERIVED | magmux | `busy`; its live cwd is a worktree (the wrong-directory trap) |
| `roots/magmux-corpus/sessions/LIVE-PIDS.txt` | SYNTHETIC | magmux | the liveness oracle; doubles as a non-`.json` negative control |
| `roots/tt-corpus/jobs/blocked-beyond-retention/state.json` | DERIVED | tt | linkScanPath target absent |
| `roots/tt-corpus/jobs/blocked-no-needs-tempo-idle/state.json` | DERIVED | tt | blocked with tempo idle; children as artifact objects |
| `roots/tt-corpus/jobs/blocked-with-needs/state.json` | DERIVED | tt | blocked + needs, transcript present |
| `roots/tt-corpus/jobs/blocked-with-needs/timeline.jsonl` | SYNTHETIC | tt | the timeline the record must not be read from |
| `roots/tt-corpus/jobs/cross-kind/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/decoy-nested/tmp/.keep` | SYNTHETIC | tt | a directory whose only content is deeper than depth 1 |
| `roots/tt-corpus/jobs/done/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/dup-a/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/dup-b/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/failed/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/linkscan-outside-root/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/missing-state-json-transient/.keep` | SYNTHETIC | tt | a record briefly absent |
| `roots/tt-corpus/jobs/no-session-id/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/no-state-json/tmp/.keep` | SYNTHETIC | tt | a directory with no record |
| `roots/tt-corpus/jobs/pins.json` | REAL | tt | a root FILE that is not a job; contents `[]` |
| `roots/tt-corpus/jobs/reaped-mid-work/state.json` | DERIVED | tt | reapedMidWorkAt; state changed to working |
| `roots/tt-corpus/jobs/record-later-than-activity/state.json` | DERIVED | tt | record written 20 days after the thread went quiet |
| `roots/tt-corpus/jobs/shortid-mismatch/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/torn/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/unknown-state/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/working-fresh/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/jobs/working-transcript-stale/state.json` | SYNTHETIC | tt | see the section for this root |
| `roots/tt-corpus/projects/-Users-user-dev-alpha-alphamini/093c8e29-d6e3-47d9-9634-35686cc52c13.jsonl` | DERIVED | tt | went quiet 20 days before its record |
| `roots/tt-corpus/projects/-Users-user-dotfiles/97030e53-3ae6-4b0e-9f5a-1c5d4b2a8e01.jsonl` | DERIVED | tt | blocked-with-needs's transcript |
| `roots/tt-corpus/projects/-Users-user-dotfiles/unrelated-neighbour-0000-4000-8000-000000000000.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-mag-gammaapp-gammaapp-cloud/d3ed2cc8-420f-4a35-be7b-5d374345faae.jsonl` | DERIVED | tt | records after reapedMidWorkAt |
| `roots/tt-corpus/projects/-Users-user-proj-a-collide/c01110ed-0000-4000-8000-000000000001.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-a-collide/d0b1e000-0000-4000-8000-000000000002.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-a/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-a/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/subagents/agent-0001aaaaaaaaaaaaa.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-a/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/subagents/agent-0001aaaaaaaaaaaaa.meta.json` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-a/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/subagents/agent-0002aaaaaaaaaaaaa.meta.json` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-a/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/subagents/agent-0003aaaaaaaaaaaaa.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-a/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/subagents/agent-0003aaaaaaaaaaaaa.meta.json` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-a/d0b1e000-0000-4000-8000-000000000002.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-b/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-c/cccccccc-cccc-4ccc-8ccc-cccccccccccc.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-d/dddddddd-dddd-4ddd-8ddd-dddddddddddd.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-e/eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-f/ffffffff-ffff-4fff-8fff-ffffffffffff.jsonl` | DERIVED | tt | the last three records carry no timestamp |
| `roots/tt-corpus/projects/-Users-user-proj-g/99999999-9999-4999-8999-999999999999.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-h/11111111-1111-4111-8111-111111111111.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-h/33333333-3333-4333-8333-333333333333.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/projects/-Users-user-proj-i/22222222-2222-4222-8222-222222222222.jsonl` | SYNTHETIC | tt | real shape, content-bearing keys removed |
| `roots/tt-corpus/sessions/background-process.json` | DERIVED | tt | a background session's own process: kind bg + jobId, no tmux |
| `roots/tt-corpus/sessions/busy-31-minutes.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/dead-pid.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/duplicate-sessionid-a.json` | SYNTHETIC | tt | one id, pid 1000 |
| `roots/tt-corpus/sessions/duplicate-sessionid-b.json` | SYNTHETIC | tt | one id, pid 2000 |
| `roots/tt-corpus/sessions/formernames-strings.json` | SYNTHETIC | tt | former names as strings |
| `roots/tt-corpus/sessions/idle-3-days.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/key-order-shuffled.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/live-busy.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/live-idle.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/live-shell.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/live-waiting-input-needed.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/live-waiting-permission-prompt.json` | SYNTHETIC | tt | `permission prompt`, never seen live |
| `roots/tt-corpus/sessions/missing-optional.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/no-session-id.json` | SYNTHETIC | tt | no sessionId (D11) |
| `roots/tt-corpus/sessions/no-status-updated-at.json` | SYNTHETIC | tt | no statusUpdatedAt |
| `roots/tt-corpus/sessions/no-tmux-field.json` | SYNTHETIC | tt | no tmux key |
| `roots/tt-corpus/sessions/torn.json` | SYNTHETIC | tt | truncated mid-object |
| `roots/tt-corpus/sessions/unknown-status.json` | SYNTHETIC | tt | `reticulating` |
| `roots/tt-corpus/sessions/version-2.1.263.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/waiting-3-days.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/waiting-unknown-reason.json` | DERIVED | tt | REAL shape, invented values |
| `roots/tt-corpus/sessions/with-former-names.json` | DERIVED | tt | REAL shape, invented values |
| `roots/twin-dead-blocked/jobs/tdb00001/state.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/twin-dead-blocked/sessions/70001.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/twin-dead-overlay/jobs/tdo00001/state.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/twin-dead-overlay/sessions/70051.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/twin-waiting-done/jobs/twd00001/state.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/twin-waiting-done/sessions/70002.json` | SYNTHETIC | magmux | see the section for this root |
| `roots/working-tempo-blocked/jobs/wtb00001/state.json` | SYNTHETIC | magmux | see the section for this root |
