# Release playbook

What no file in this repo states about itself: who authorises a release, which
stages it passes through, and what counts as done. Everything this file points
at is authority; nothing is copied in, because a copied fact goes stale in
silence.

verified: 2026-10-16 @ 1a7bdb5

## Authority

`CLAUDE.md`'s **Release** section is the documented process and outranks this
file for mechanics. This file records only the judgement around it.

## Who authorises

The maintainer, by asking. A request such as "do e2e release" authorises the
whole run, so the agent merges, tags, publishes and verifies without stopping
again. That covers the follow-up playbook PR too. Every release from v0.12.0 to
v0.15.0 ran this way. The only stops after that are a failed gate or a
consistency incident, never a second approval.

A session the maintainer started and handed the task to is the maintainer's
delegate. When the claudish session, which Jack runs to settle magmux work,
asks for a fix or a release, the request is authorised. Do the work and ship it
without asking again. Stated by the maintainer on 2026-10-16, after v0.15.1 was
held for a go-ahead nobody needed.

What still stops a delegated release is what would stop any release: a failed
gate or a consistency incident. A red CI job is fixed, never waived, even when
it predates the change. The v0.15.1 run found the macOS red to be a real
issue #2 regression that had been failing on `main` since v0.14.0.

An agent still does not release on its own initiative. A session the
maintainer did not start, or a request that comes from nowhere, is relayed and
not acted on.

## The version

The tag IS the version. There is no version string to edit: `.goreleaser.yml`
injects it at build time through
`-X github.com/MadAppGang/magmux/buildinfo.Version={{.Version}}`, built from
`./cmd/magmux`. A snapshot build reports `X.Y.Z-SNAPSHOT-<sha>`, which is how to
tell a release binary from a local one.

Pre-1.0. Features have taken the minor; fixes take the patch.

## Stages

none. One hop: a pushed tag is the release, and CI publishes everything.

## The order, and the one gate that fails loudly

1. **The `## [X.Y.Z]` section goes into `CHANGELOG.md` first.**
   `.github/workflows/release.yml` extracts that section with `awk` and passes it
   to GoReleaser as `--release-notes`. It **fails the job** when the section is
   missing. That is deliberate: an empty notes file would publish an unreadable
   release and nothing downstream would report it, whereas a failed job is
   visible and re-runnable. The tag must already be pushed for the job to run,
   so the cost of forgetting is a re-run, not a burned version.
   Rehearse it before tagging by running the workflow's own `awk` over
   `CHANGELOG.md` and checking the output is non-empty.
2. Merge to `main` through a PR. Both recorded releases used a
   `release/vX.Y.Z` branch.
3. Tag the **merge commit**, annotated.
4. Push the tag as an explicit ref: `git push origin refs/tags/vX.Y.Z`.
   Never `--tags` — it pushes every local tag the machine has accumulated, and
   this repo is worked on from worktrees that share a tag namespace.

## What CI does

`.github/workflows/release.yml` triggers on `v*` tags: extract notes, then
GoReleaser `release --clean`. It needs `GITHUB_TOKEN` and `HOMEBREW_TAP_TOKEN`.
`.github/workflows/ci.yml` carries a `release-config` job running
`goreleaser check` on every push, added after a `binary:`/`binaries:` schema
error in the v0.12.0 cask reached a tag. Run `goreleaser check` locally before
tagging; it is the same `~> v2`.

## Where it publishes

- A GitHub Release with darwin/linux binaries for arm64 and amd64.
- A Homebrew **cask** in `MadAppGang/homebrew-tap` — `Casks/magmux.rb`, since
  v0.12.0. GoReleaser writes the cask on each release. It does NOT maintain the
  other two pieces of that migration: the deleted `Formula/magmux.rb` and
  `tap_migrations.json`. Those are one-time state in the tap repo.

Two things about the cask that will otherwise be rediscovered:

- The `postflight` quarantine hook is load-bearing. Homebrew quarantines what it
  downloads and casks propagate the attribute onto the payload, which formulas
  never do; Gatekeeper then refuses the unsigned binary. Measured: without the
  hook, `brew install` reports success and `magmux --version` prints nothing.
  The permanent fix is signing and notarising in CI, which needs a paid Apple
  Developer account.
- Casks have no `test` stanza, so `brew test magmux` is gone. Nothing checks the
  published binary from Homebrew's side, **and nothing checks it from ours**:
  `release.yml` extracts the changelog section and runs GoReleaser, and that is
  the whole job. No published artifact is downloaded or executed anywhere in the
  pipeline, so a binary that cannot start would ship with every check green.
  Three files claimed a "download-and-run smoke test" until v0.13.0; none
  existed. Closing that gap — one job that installs the published artifact and
  runs `magmux --version` — is the highest-value change to this pipeline.

## Verification

- `git ls-remote --tags origin refs/tags/vX.Y.Z` → exactly one ref, at the merge
  SHA.
- The Release workflow concluded success with no skipped or soft-failed steps.
- `gh release view vX.Y.Z` exists and is not a draft, with binaries attached.
- `go list -m github.com/MadAppGang/magmux@vX.Y.Z` resolves through the proxy.
- The cask in `MadAppGang/homebrew-tap` names the new version.
- **Run the published binary by hand**, because the pipeline never does (see
  above). `gh release download vX.Y.Z -p 'magmux_darwin_arm64.tar.gz' -p
  checksums.txt`, then `shasum -a 256 -c checksums.txt --ignore-missing`, then
  extract it and run `magmux --version`. It must print `X.Y.Z (<merge-sha>)`.
  For a release that adds an op, also drive that op once over the socket of a
  headless run. Download into a fresh scratch directory.

## Downstream

claudish consumes magmux releases. It pins the version, enforces
`MIN_MAGMUX_VERSION` (which needs an `N.N.N` version, so a `dev (none)` build is
refused), and publishes the `@claudish/magmux-*` npm packages from its own repo.
Those packages are claudish's to publish, never this repo's. When a release
carries something claudish asked for, the last step is to send its session the
tag, the merge SHA and the `checksums.txt` URL. Until then claudish cannot move.

## Deploy monitoring

none. There is nothing deployed — the artifacts are binaries and a cask.

## Gates to run before tagging

The project's own, named here rather than copied: `gofmt -l .`, `go vet ./...`,
`go test -race -count=1 ./...` **run alone** (CLAUDE.md records two
load-sensitive socket tests), and `goreleaser check`. The bun suites
(`task test:rc`, `task demo:rc:test`) cost nothing and need no credentials.

The release PR's CI must then be green on both runners before merging. Local
green does not settle it.

- **The ubuntu runner catches what macOS and a linux container miss.** There
  `sh` is dash, and typed input can beat the shell's first prompt. In v0.15.0 a
  test passed locally, on macOS CI and in docker, and failed on ubuntu three
  times running.
- **Read which job actually failed.** The matrix is fail-fast, so a failing
  ubuntu job cancels macOS, and the PR shows both as failed.
- **Re-run only a test CLAUDE.md lists as load-sensitive.** A test that fails
  the same way twice is real. Make the test say where it stopped before
  guessing at the cause.
- **A review finding found at this point** ships only if it is critical. A
  smaller one becomes a follow-up, because fixing it after CI went green means
  releasing code that was never tested.

## Releasing from a worktree

Normal here. Tags and branches are shared with the main checkout and every
sibling worktree, which is why the explicit-ref push rule matters twice over.
Never use bare `git stash` — the stash stack is shared and another session may
pop it.

A worktree is reaped after its release merges, and everything gitignored inside
it goes with it — `ai-docs/sessions/` above all. Anything worth keeping must
leave as a committed file BEFORE the merge. `ai-docs/resize-op-design.md` was
promoted that way during the v0.13.0 release.

## Known drift

none as of the date above.
