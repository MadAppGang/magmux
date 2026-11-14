# Test data provenance

Where every file under `internal/obs/testdata/` came from, and how far it can
be trusted. Read this before writing a test against any of it.

## SANITISED — 2026-09-26

Every identifier naming a real user, machine or project was replaced before
this corpus was published. **No file here is byte-identical to what was
captured**, and the earlier sections of this document describe the corpus as it
was captured, not as it now reads.

The replacement was mechanical and LENGTH-PRESERVING wherever a length is
load-bearing, so the 200-character truncation boundary in the project-directory
encoder falls in the same place it did:

The originals are deliberately NOT listed here. A mapping table naming them
would republish exactly what the replacement removed, so what follows says what
each class of identifier became, and nothing about what it was.

| Class of identifier | Became | Note |
|---|---|---|
| the account name in every absolute path | `user` | same length, so path lengths are unchanged |
| the directory holding the repositories | `dev` | in both the path form and the encoder's dashed form |
| the terminal multiplexer's session name | `devmx` | same length |
| three project directory names | `sampleapp`, `userconf`, `agent-fleet-sessions` | same lengths |
| three agent session names | `fleet-migrate`, `agent-src-15`, `multilayer-support-40` | same lengths, except the last |

Session names that describe a test case rather than a real session —
`torn-read-session`, `dead-pid-stale-record` and the rest — were already
synthetic and are unchanged.

The four directories under `projects/` were renamed to match, because their
names are the encoder's output over those paths.

**What this does not change.** Key order, the millisecond timestamps, the
object-shaped `formerNames`, the UTF-16 encoding cases, the torn-read record,
the dead-pid records and every REAL/SYNTHETIC label above. The decoders are
pinned against structure, and the structure is untouched.

**What it did change, and where it is recorded.** Two expected hash values in
`adapters` are derived from the string being hashed, so they moved:
`javaHash("/Users/user/dev/" + 250 x's)` and the `-c9gw6w` suffix in
`EncodeProjectDir`. Neither was copied from this package's own output. An
independent implementation of the algorithm — 31x over UTF-16 code units,
truncated to int32, `Math.abs` in base 36 — was written in awk and produced the
same values. The five externally-derived truncation vectors in
`spec_decode_test.go` contain no identifiers, were not touched, and still pin
the hash against the extracted bundle. Both test comments say all of this.


These files were copied from the test-data set built by the test architect for
session `dev-feature-claude-session-sv-20260916-172122-b78b0781`. The full
manifest — one section per file, with the reasoning behind each — lives under
that session's `tests/` directory in `MANIFEST.md`, and is the authority if
this summary and that document ever disagree. Nothing here was regenerated,
reformatted or topped up.

Three non-record files differ from the source in one respect only: the
borrowed term the source used for this kind of data was replaced with Go's
own, `testdata`, since the toolchain defines it. That is `LIVE-PIDS.txt`'s
comments, and the canary string inside the two `.key` files, which now begins
`TESTDATA-CANARY-` where the source used the other prefix. No value any parser
reads was touched by that substitution.

**The records are NOT byte-identical to the live registry, and this document
used to say they were.** That sentence was here from the start, it was false in
two ways, and being false is what made the first of them hard to find. Both are
recorded below, under "Corrections".

## REAL vs SYNTHETIC

The labels are load-bearing and are preserved verbatim from the manifest.

- **REAL** — the value or shape was measured on this machine on 2026-09-16 and
  is recorded in `research/synthesis.md` or `research/watcher-mechanism.md` of
  the same session.
- **SYNTHETIC** — the case was never observed on this machine and had to be
  constructed. Every synthetic file says what was observed instead.

The distinction is not bookkeeping. 32 of 32 live records were live, and
`"permission prompt"` did not occur once in 1920 observations over 304 seconds.
Two acceptance criteria therefore cannot be validated against live data at all,
and the synthetic records below are a declared SUBSTITUTION rather than a
proof. A report that counts them as live verification is wrong.

## Rules these files enforce on the tests

1. **Nothing here touches `~/.claude`.** Every path is under `testdata/`.
2. **No test probes a live pid.** `sessions/LIVE-PIDS.txt` is the liveness
   oracle. A pid in this data could collide with a real process on the machine
   running the suite, and the answer would then depend on what else is running.

   **Enforced since phase 5, and it was being broken until then.** Four tests
   reached magmux's real `obsOracle` — `kill(pid, 0)` against whatever pid their
   temporary registry named — because `startObservation` defaulted to it. The
   oracle is now a parameter, `obsOracle` is named only by `main()` and by
   `magmux obs`, and `TestNoTestCanReachTheSignalOracle` reads the source of
   every `.go` file in the repository and fails if a test names it, names
   `runObs`, or calls `syscall.Kill`.

   The rule means "a test must not depend on what else is running", which is
   why it does NOT forbid a test forking its own `sleep` and asking about that
   pid: `sockdir.go`'s `pidIsGone` tests do exactly that and are correct. The
   guard says so in its own scope note rather than leaving the distinction to
   be rediscovered.
3. **No test reads a `.key` file.** They are named like key material and hold a
   canary string. If the canary ever appears in output, something read them.
4. **Time is injected, never read from the clock.** The reference instant for
   every time-derived expectation is `NOW = 1789545200` (epoch seconds).

## Corrections — 2026-09-18, phase 2

Two, both found by re-measuring against the live registry while phase 2 was
being built, and both against this document rather than against Claude Code.

### 1. `statusUpdatedAt` and `updatedAt` are epoch MILLISECONDS

The corpus carried 10-digit values. The live registry carries 13-digit values
in all 34 records, and no 10-digit value exists anywhere in the directory.

The proof it is the corpus that was converted, and not Claude's format that
changed, is that `~/.claude/sessions/98198.json` has not been rewritten since
the day the corpus was captured — same session id, same file, same Claude
version 2.1.273:

    live mtime           : 2026-09-16T15:31:32.900044
    live statusUpdatedAt : 1789536692864   -> as ms: 2026-09-16T15:31:32.864000
    corpus (as captured) : 1789536692
    live // 1000 == corpus ?  True

The live value is 36 ms before the file's own mtime, which is what a process
that computes a timestamp and then writes it produces. No schema change can
explain a difference in a file nobody has touched.

**What was done:** the 18 records carrying those two keys were multiplied by
1000, as a text edit appending `000` to each value, so key order — which
`60008.json` exists to test — and every other byte is unchanged. The unit is
now right; the PRECISION is not recoverable. The corpus was truncated to whole
seconds when it was built, so `98198.json` now reads `1789536692000` where the
live record reads `...864`. Nothing tests sub-second precision, and inventing
the missing milliseconds would be manufacturing data.

**The trap is guarded rather than just fixed.** `adapters.DecodeInstant`
REFUSES a seconds-shaped value instead of converting it, and
`TestASecondsShapedInstantIsRefusedNotConverted` fails on a 10-digit value with
this explanation attached. A converting decoder would have made this corpus
green forever, which is how it survived phase 0 and phase 1 unnoticed.

### 2. The records are a stripped subset, and eight keys are named now

The "Known incompleteness" section below says eight universal keys are missing
and that their names are unknown to the documents the corpus was built from.
Their names are no longer unknown. Every live record carries all of these, and
no record here carries any of them:

    entrypoint 'cli' | kind 'interactive' | messagingSocketPath | nameSince
    pidDomain 'darwin' | procStart 'Wed Sep 16 05:31:13 2026' | startedAt
    version '2.1.273'

They are NOT added here. Phase 2 reads none of them, and data with no reader is
data nobody notices going stale — the same rule that kept the transcript tree
out. They are named so that the phase which first needs one knows it exists and
knows this corpus does not have it.

Three Claude versions are live on this machine as of 2026-09-18 — 2.1.273 (26
records), 2.1.274 (5), 2.1.275 (3) — so the format does drift between versions.
It did not drift here.

## Corrections — 2026-09-19, fix round 2

### 3. Former names are written as OBJECTS by Claude Code 2.1.276

Found by re-running the live validation, three days after the corpus was
captured. The registry then held 39 records, several on 2.1.276, and the only
one carrying former names wrote them like this:

    ~/.claude/sessions/76136.json   version 2.1.276
      "formerNames": [
        { "name": "agent-fleet-sessions",
          "until": 1789745531896,
          "sessionId": "d1f05570-4e47-4517-b4c5-9bda85d3bd6d" } ]

The adapter read strings only, so this session was published with
`previousNames: null` and a problem on stderr —
`previous name 0 is map[string]interface {}, not a string`.

The object shape is Claude Code's own current format, not a malformed record:
its session listing renders that peer as *"says it was agent-fleet-sessions
until 5m ago"*, which is `name` and `until` read back. In this one record
`until` equals the record's own `nameSince`, and `sessionId` equals the
record's own session id. A former name recorded under a DIFFERENT id has not
been observed.

**What it does to `45682.json`'s label.** That row said REAL SHAPE for the
string form. It cannot now be confirmed: no string-shaped record remained on the
machine to check it against, and this document has twice before claimed more
fidelity than it had (Corrections 1 and 2). So either the format changed between
2.1.273 and 2.1.276, or the label was wrong. The row now says UNCONFIRMED, and
the adapter accepts both shapes, because nothing proves the string form was
never written.

**What was done:** `76136.json` added, copied byte for byte; `76136 alive`
added to `LIVE-PIDS.txt`; `registry_test.go`'s count of readable records moved
from 18 to 19. No existing record was touched.

## Known incompleteness

The registry schema is 19 universal keys plus 2 optional. The research
documents name only 11 of them:

    sessionId  pid  name  nameSource  cwd  tmux  status
    statusUpdatedAt  updatedAt  peerProtocol  peerFeatures

These records are therefore a faithful SUBSET, not a faithful whole. A parser
written against them is correct on what it sees and untested on the rest. Eight
universal keys are missing from every record here.

Their names were unknown to the documents this data was built from. **They are
known now** — see Correction 2 above, which names all eight and measured them on
the live registry on 2026-09-18. This paragraph is kept because the subsetting
is still true; only the "unknown" half was superseded.

## `sessions/` — the registry directory

Stands in for `~/.claude/sessions/`. Copied whole, because the corpus only
works as a whole: several records exist to be the one case that distinguishes
two gates, and removing their neighbours removes the distinction.

| File | Label | What it is for |
|---|---|---|
| `98198.json` | REAL | `idle`. The one session id and the one terminal-coordinate string recorded verbatim. Its status instant is 8508 s before `NOW`. |
| `98693.json` | REAL | `busy`, 63 s before `NOW`. Its live working directory is a worktree, which is what makes it the subject of the wrong-directory trap. |
| `1724.json` | REAL | `waiting` + `"input needed"` — the measured case; all 55 observed `waiting` records carried this value. |
| `15775.json` | REAL | `shell`, the rare fourth status (117 of 1920 observations). |
| `1828.json` | REAL | `shell`, third `nameSource` value, and deliberately has no transcript anywhere. |
| `45682.json` | REAL pid, string shape UNCONFIRMED | `idle`, with two previous names written as bare STRINGS — the minimum that can detect flattening. This row said REAL SHAPE until 2026-09-19, and that claim could not be re-checked: see Correction 3. The shape is still accepted, and still tested, because nothing proves Claude Code never wrote it. |
| `76136.json` | REAL, byte for byte, 2026-09-19, Claude Code 2.1.276 | `busy`, with one previous name written as an OBJECT — `{name, until, sessionId}` — which is the only shape any live record carried on that date. Copied whole from the live `~/.claude/sessions/76136.json` (read only; `cmp` identical at copy time, sha1 `fcdd8e9b…`), so unlike the records above it carries every key, including the eight named in Correction 2. Nothing reads `messagingSocketPath`; it names a socket under `/tmp/cc-socks` and no test opens it. Added in fix round 2. See Correction 3. |
| `96632.json` | REAL pid, SYNTHETIC omission | `idle` with **no terminal-coordinate key at all**. No observed record lacked one; the case exists because the second liveness gate is conditional. |
| `98299.json` | REAL | `busy`. The measured worst case for subagent counting: its transcript parses as 0 against 23 actual. |
| `60001.json` | SYNTHETIC | **Dead pid.** Cannot occur on this machine — 32/32 records were live — so the dead-pid exclusion is a declared substitution. |
| `60002.json` | SYNTHETIC | `"permission prompt"`. **Never observed**, in 1920 observations. Without it, "the two waiting values must never be merged into a boolean" has no test. |
| `60003.json` | SYNTHETIC | `status: "compacting"` — a fifth, unrecognised value, chosen because it is plausible enough that an implementation tempted to guess will guess `busy`. |
| `60004.json` | SYNTHETIC content, REAL mechanism | A torn read: the file stops mid-string. In-place rewriting is measured three independent ways; a parse failure means torn, not dead. |
| `60005.json` | SYNTHETIC | **Zero bytes** — the degenerate torn read, separate because an empty file and a truncated one fail differently in most decoders. |
| `60006.json` | SYNTHETIC | `waiting` with **no waiting-reason key**. The sharpest case in the set: the wrong answer is right 55 times out of 55 on live data. |
| `60007.json` | SYNTHETIC | A record from a hypothetical future Claude — a newer peer protocol and five keys no source has ever named. |
| `60008.json` | SYNTHETIC ordering, REAL content | Every key in a different order. Key-order instability is measured; this permutation is not. |
| `60009.json` | SYNTHETIC | A terminal-coordinate value with no separators, so no window or pane id can be extracted. |
| `60010.json` | SYNTHETIC instance, REAL class | Live pid, status instant three days old. The class is measured: 23 of 32 sessions had one from second 1-17 of their lifetime. |
| `60011.json` | SYNTHETIC | The filename says 60011, the record says 60099, and the oracle gives the two pids **opposite** answers. |
| `60012.json` | SYNTHETIC | Live pid, dead pane — the only record that isolates the second liveness gate from the first. |
| `*.key` (2) | REAL SHAPE | The directory really does hold `<pid>.<sha256>.key` siblings, and one really does exist with no matching record. Contents are a canary. |
| `LIVE-PIDS.txt` | SYNTHETIC | The liveness oracle, with its own inline header. Doubles as a non-`.json` negative control. |

## What phase 0 uses, and what it does not

Phase 0 reads these records for one purpose: to prove the decoders in
`adapters/schema.go` handle the values Claude actually writes, including the
one it does not recognise. It does not enumerate the directory, does not gate
on liveness, and does not read the `.key` files or the oracle. Those are later
phases, and the data is here whole so that they do not have to go looking for
it again.

The transcript tree and the terminal-pane oracle from the same source were NOT
copied. Phase 0 reads no transcript, so copying one would be data with no
reader — and data with no reader is data nobody notices going stale.

**The transcript tree arrived in phase 4**, on exactly those terms: phase 4 is
the first phase that reads one. The terminal-pane oracle still has no reader
and is still not here.

## `projects/` — the transcript tree

Added 2026-09-18 by phase 4. Stands in for `~/.claude/projects/`. Copied from
the same source set as `sessions/` above, byte for byte, and verified file by
file with `cmp` before anything was changed.

Two files differ from the source, and in one respect only — the same
substitution recorded at the top of this document, for the same reason. The
borrowed term for this kind of data was replaced with Go's own:
`subagents/notes.txt` now points at `testdata/PROVENANCE.md` where the source
pointed at the manifest under its own name, and `subagents/.gitkeep` says "the
test data for" where the source said the other word. **Neither file is read by
any parser**: `notes.txt` exists to be EXCLUDED by the `*.meta.json` suffix
filter, and `.gitkeep` exists because git does not track empty directories and
doubles as the same negative control. No value any parser reads was touched.

The layout is the measured one, and the DEPTH is load-bearing:

    projects/<project-slug>/<sessionId>.jsonl
    projects/<project-slug>/<sessionId>/subagents/agent-*.meta.json

The subagents directory is under the SESSION ID, not directly under the project
slug. A tree with the flatter layout would train an implementation to look one
level too high, and the failure mode there is a silent zero.

### Record envelope — REAL KEYS, INFERRED SHAPE

This bounds what the tree proves, and it is preserved from the source manifest
because it is the most important sentence about this data.

- **REAL**: the key `agentColor`, and that it is *not* `color`; the key
  `isApiErrorMessage`; the key `is_error` and that all 497 observed occurrences
  were recovered from; the record type `artifact-autoreact-ledger` with its
  `savedAt`, which one source document prints verbatim; the tool name `Agent`,
  which is never `Task`; the subagent keys `taskKind`, `color` and `spawnDepth`.
- **INFERRED**: the surrounding envelope — `type`, `uuid`, `parentUuid`, `cwd`,
  `version`, `timestamp`, `message.role`, `message.content[]`. These are written
  in the conventional Claude Code transcript shape. They are plausible and they
  are not measured.

A test must therefore key off the REAL names. A test that asserts on
`parentUuid` threading is testing somebody's guess. `adapters/schema.go` says
the same thing at the constants themselves.

### The transcripts

| File | Label | What it is for |
|---|---|---|
| `-Users-user-dev-magmux/a1478d55-….jsonl` | REAL KEYS | Session `98693`. Three `isApiErrorMessage` records, one per measured class — transport, auth, quota — and **no** `is_error`, so a count taken from it is unambiguous. Expected: **3 errors**, colour `orange`. It is also half of the wrong-directory trap below. |
| `-Users-user-dev-magmux/c874de20-….jsonl` | REAL (third record verbatim) | Session `98198`. A user turn, an assistant turn carrying `agentColor: "cyan"`, and the `artifact-autoreact-ledger` record reproduced from the research verbatim. Its `savedAt` is **1.37 h later** than the session's own `statusUpdatedAt`, which is the bait for the refuted "correct the status instant against transcript activity" cross-check. |
| `-Users-user-dev-magmux/d0c31e88-….jsonl` | REAL KEYS | Session `45682`. Three `agentColor` records — `blue`, `green`, `yellow` — with a record carrying **`color: "red"`** between the last two. Expected: **`yellow`**. Without the decoy, an implementation reading the wrong key still gets the right answer whenever the two agree, and the bug ships. |
| `-Users-user-dev-magmux/2f7a8e14-….jsonl` | SYNTHETIC SIZE, REAL BOUND | Session `96632`. 19 records, **48,183 bytes**, and its ONLY `agentColor` (`purple`) is in the first ~380 bytes. With a test bound of 8 KB the colour is outside the tail window and only the bounded head fallback finds it; under the production 200 KB bound the whole file is inside one window. **It is not a proof of the production bound** — that was measured as 0 misses across 35 transcripts, worst case 44.1 KB, against a largest transcript of 630.9 MB, and a 48 KB file cannot stand in for that. |
| `-Users-user-dev-sampleapp/5f1c9a02-….jsonl` | REAL KEYS | Session `98299`. Contains **no `Agent` tool records at all**, which is what makes it the 0-against-23 case beside its 23 metadata files. Colour `teal`. |
| `-Users-user-dev-sampleapp/8e2d4b17-….jsonl` | REAL KEYS | Session `1724`. **Both signals in one file**: three recovered `is_error` tool results AND one `isApiErrorMessage`. Expected: **1**. This is the discriminating case — counting both reports 4, counting only the tool flag reports 3, and only a correct implementation reports 1. |
| `-Users-user-dev-agent-fleet-sessions/3b90f6cc-….jsonl` | REAL KEYS | Session `15775`. The inversion guard: recovered `is_error` tool results, **no** `isApiErrorMessage`, and the session finishes normally. Expected: **0 errors**. It also carries **no `agentColor`**, making it the "colour is unknown" case — distinct from "colour is the default", where the empty string is a value. |
| `-Users-user-dev-magmux--claude-worktrees-claude-session-control/b0000000-….jsonl` | SYNTHETIC, REAL FAILURE MODE | **The decoy.** See below. |

Session `1828` (`6c5b2f09-…`) has **no transcript anywhere**, deliberately.
Every transcript-derived field for it must come back as no opinion — which is
a different answer from zero. It is the ONLY session here whose subagent count
is undetermined; see the correction below.

### The wrong-directory trap

Session `98693`'s record says its working directory is
`/Users/user/dev/magmux/.claude/worktrees/claude-session-control`. Its
transcript is filed under **`-Users-user-dev-magmux`**, the parent repository's
name, because the session was launched there and moved afterwards. That is not
exotic: 115 transcripts on the machine this was measured on sit under a parent
repository's name for the same reason.

And the tree contains a **decoy**: the directory you get by encoding that live
working directory EXISTS, and holds `b0000000-….jsonl`, which is a different
session entirely. Its only record carries
`agentColor: "WRONG-SESSION-DO-NOT-REPORT-THIS"`, so reading it is loud rather
than a subtly wrong colour.

So an implementation that derives the directory from the live working
directory does not fail loudly — it finds a directory that exists, holding
another session's transcript. `TestTheWrongDirectoryTrapIsArmedInTheTestData`
asserts the trap is still armed (the derived directory exists, and does not
hold `98693`'s own transcript) so that
`TestDecoyProjectDirectoryIsNeverRead` cannot pass for the wrong reason.

### The subagent directories

| Directory | Label | What it is for |
|---|---|---|
| `…/5f1c9a02-…/subagents/` | SYNTHETIC CONTENT, REAL COUNT AND KEYS | **23** `agent-0NN.meta.json` files and nothing else, reproducing the measured worst case: pid `98299` parses as **0** subagents from its transcript against **23** actual. REAL keys `taskKind`, `color`, `spawnDepth`; REAL relationship — `color` is present only where `taskKind` is the in-process teammate, an exact 44/44 match across 1913 live files, so 001-008 carry one and 009-023 do not. `spawnDepth: 2` on 016-023 reproduces the nesting that leaves no parent record, which is the reason for the 7.2% undercount. `agentId` is INFERRED; the count must come from the file count, so no test depends on it. |
| `…/c874de20-…/subagents/` | SYNTHETIC | Present and **empty** of `*.meta.json`. Expected: **0, as a determined zero**. Holds only `.gitkeep`, which doubles as a suffix-filter negative control: a counter that counts directory entries reports 1 here. |
| `…/a1478d55-…/subagents/` | SYNTHETIC | Three `agent-*.meta.json` — one teammate, one plain, **one truncated mid-key** — plus `notes.txt`, which must never be counted. |
| `…/3b90f6cc-…` | — | **No `subagents/` directory at all.** Its answer is a determined **0**, on the same terms as `c874de20`'s empty directory — see the correction below. The session that answers "unknown" is `1828`, which has no transcript anywhere. |

### Correction — 2026-09-18, after phase 4's hand-back: a MISSING directory is ZERO

This section said a session with no `subagents/` directory at all was the
"unknown" case, and the implementation agreed with it. Both were wrong, and the
coordinator measured it against the live tree, read-only:

    transcripts examined          : 3189
      subagents/ directory EXISTS : 192   (empty 6, non-empty 186)
      subagents/ directory MISSING: 2997  (94.0%)

**186 of the 192 that exist are non-empty**, so Claude Code creates the
directory LAZILY, on the first spawn. A missing directory is therefore the
ordinary shape of a session that has never spawned a subagent — not a failure
to count. The old reading left the count undetermined for 94% of live sessions,
which meant the layer could essentially never report "zero", and a column that
is blank almost always carries no information.

The rule now is **"were we in a position to look"**, not "was the directory
there":

| Situation | Answer |
|---|---|
| Transcript found, `subagents/` missing | determined **0** |
| Transcript found, directory exists, no `*.meta.json` inside | determined **0** |
| Transcript NOT found | **nil** — there is no session directory to look in |
| The read fails for any reason other than the directory being absent | **nil** — a permission error is not an observation |

That keeps nil meaning what it always meant. The case this data still carries
for it is `1828`, whose transcript does not exist anywhere; `3b90f6cc` is no
longer an "unknown" example and the table above says so.

**Ambiguity A3 is settled here, and the settlement is phase 4's.** The source
manifest raised the truncated metadata file as an open question: counted by
file (3) or no opinion (2). It is counted — a subagent's existence is attested
by its file existing, and a metadata file caught half-written describes a
subagent that is running right now, so the other reading makes a live subagent
vanish from the count at the moment it starts. The number of unparseable files
is reported alongside the count, so a caller that disagrees can see it.
`TestMalformedSubagentMetaIsCountedAndReported` asserts both halves.

### What phase 4 uses, and what it does not

Phase 4 reads this tree for the colour, the API-error count and the subagent
count, and to prove the transcript is located by session id rather than by a
derived directory name. Nothing here is read for TIMING: a transcript's
modification time is used only to decide whether a re-read is needed, never as
a signal that a session is active — which is what `c874de20`'s 1.37-hour
`savedAt` gap exists to punish.
