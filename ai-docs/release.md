# Release playbook

What no file in this repo states about itself: who authorises a release, which
stages it passes through, and what counts as done. Everything this file points
at is authority; nothing is copied in, because a copied fact goes stale in
silence.

verified: 2026-09-17 @ <merge-sha>

## Authority

`CLAUDE.md`'s **Release** section is the documented process and outranks this
file for mechanics. This file records only the judgement around it.

## Who authorises

unknown — TODO: the v0.12.0 and v0.13.0 releases were both run by the maintainer
on request, with the agent executing end to end once asked. Whether an agent may
start a release unprompted has never been stated.

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

## Deploy monitoring

none. There is nothing deployed — the artifacts are binaries and a cask.

## Gates to run before tagging

The project's own, named here rather than copied: `gofmt -l .`, `go vet ./...`,
`go test -race -count=1 ./...` **run alone** (CLAUDE.md records two
load-sensitive socket tests), and `goreleaser check`. The bun suites
(`task test:rc`, `task demo:rc:test`) cost nothing and need no credentials.

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
