# Config test data — provenance, and the format translation

These four files are **translations** of the four written by the test architect
for session `dev-feature-claude-session-sv-20260916-172122-b78b0781`. The
originals are TOML and are still at
`ai-docs/sessions/dev-feature-claude-session-sv-20260916-172122-b78b0781/tests/fixtures/config/*.toml`,
untouched. Read them for the reasoning; this file records only what the
translation changed.

## Why they are JSON here

Phase 7 chose `encoding/json` for the config file. The decision, its reasoning
and its cost are written out in full at the top of `internal/obs/act/config.go`
— in one sentence: TOML is the better format for a file a human writes and
annotates, and a TOML library would be a FOURTH module against a dependency set
pinned at three by `TestDependencySetIsExactlyWhatWasDecided`. That pin exists
so a fourth is a deliberate decision in a diff that says so, and that decision
is the user's and has not been made. JSON-now-TOML-later is reversible against
an unchanged schema struct; a dependency added without the decision is not.

## What the translation changed, and what survived it

**The case each file tests survived, including the unparseable one.** R6.3 is
about a parser that fails halfway and keeps what it read, and that hazard is
identical in both formats — so `unparseable.json` keeps the original's
ordering, which is the whole of its design: the fully permissive `actions`
stanza comes FIRST and parses cleanly, the syntax error is in the LAST stanza.
A parser that returned partial results would therefore hand back
`approve/send/lifecycle = "always"`, the exact more-permissive outcome R6.3
forbids. **If this file is ever "fixed" so that it parses, the test built on it
stops testing anything. Do not tidy it.**

**What did NOT survive is the comments**, and that is the translation's one
real loss. Every original carries its PROVENANCE header inside the file, where
somebody editing it would meet it. JSON has no comments, and this loader sets
`DisallowUnknownFields`, so a `"_comment"` key would not even be a workaround —
it would be a parse error. That is why the headers are here instead of there,
and it is a concrete instance of the argument that TOML is the better format
for this job.

`permissive-but-deferred.json` keeps its `match.tool = "Read"`, verbatim from
the R6 schema sketch. It is accepted by the loader and lands in `act.Rule.Tool`,
where it makes the rule match **nothing** — R5.2 is deferred and produces no
tool to match against. That is the narrower reading of what the user wrote and
is deliberate; ignoring the key would silently widen their rule to cwd alone.

| File | Original | Label | What it is for |
|---|---|---|---|
| `default-posture.json` | `default-posture.toml` | REAL SCHEMA, R6.1 values | The baseline every other file is a deviation from. It is R6.1's posture written out, so a test can compare against a document rather than a hand-copied expectation. |
| `permissive-but-deferred.json` | `permissive-but-deferred.toml` | SYNTHETIC | Valid, and asks in writing for every deferred tier to act unattended. The service must still refuse, because the refusal lives in code (`act.Tier.Deferred`) and not in config. A user who copies a config off the internet must not be able to turn on tiers that were never built. |
| `quiet-crossing-midnight.json` | `quiet-crossing-midnight.toml` | REAL SCHEMA, synthetic values | `"22:00-08:00"` CROSSES MIDNIGHT. The naive `start <= t && t <= end` is false for every instant of it, so quiet hours silently never apply and the user is woken at 03:00. The second window is the contrasting same-day case. |
| `unparseable.json` | `unparseable.toml` | SYNTHETIC | R6.3. See above. |

## Rules

The four rules in `internal/obs/testdata/PROVENANCE.md` apply here unchanged.
Rule 4 is the one with teeth for these files: the quiet-window tests assert the
INTERIOR of each window and a point well outside it, never a boundary, because
requirements.md does not say whether the endpoints are inclusive and a test
that assumed one reading would silently bless it. `act.QuietWindow.Contains`
does have an answer — start inclusive, end exclusive — and it is stated at the
method, where it is a decision rather than an accident.
