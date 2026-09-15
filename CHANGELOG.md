# Changelog

All notable changes to magmux are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Releases before v0.11.0 predate this file; their notes were generated from
commit subjects and remain on the
[GitHub releases page](https://github.com/MadAppGang/magmux/releases).

## [0.12.0] - 2026-09-15

### Changed

- **Homebrew installs a cask now, not a formula.** GoReleaser deprecated the
  formula path because the formulas it generated were a hack — they declared a
  source build and then installed a pre-compiled binary, which was the only way
  to reach Linuxbrew when it was written. It no longer is: Homebrew ships a
  Linux Caskroom, and the generated cask carries `on_linux` blocks for both
  arm64 and amd64, so the same four platforms are covered. Existing installs
  are migrated by `tap_migrations.json` in the tap, so `brew upgrade` finds the
  cask instead of silently finding nothing.

  Two consequences worth knowing. Casks have no `test` stanza, so
  `brew test magmux` no longer exists. And casks propagate macOS's download
  quarantine onto the payload where formulas never did, so the cask carries a
  hook that strips it — without it `magmux --version` prints nothing and exits
  while `brew install` still reports success.

### Fixed

- **The pilot broke its own gutter on long tokens.** A body carrying a
  transcript path or a URL emitted that token whole — 85 characters into a
  40-column pane — and the terminal soft-wrapped it with the continuation
  starting at column 0 with no gutter, breaking the exact vertical rule the
  layout exists to draw. It now hard-splits, matching the Go implementation of
  the same job that has always done so.
- **A recovered provider error could still decide the summary.** The flag was
  set on the first refusal and never cleared, so a run that recovered and then
  ended for an unrelated reason closed with "could not reach its model:
  <the old error>" — the same misattribution the mechanism exists to prevent,
  pointed the other way. It is now cleared by any later message that did not
  error. An error carrying no readable message is reported rather than passed
  over, so a rename in the upstream SDK surfaces instead of silently disabling
  detection.
- **The control panel's route table dropped its data on medium-width panes.**
  The state chip widened the row, and the layout drops the entire right-hand
  group rather than trimming it, so duration, sparkline and tool only appeared
  from 61 columns — leaving an 11-column band showing a long state label in
  padding and no moving information at all. A third width band keeps the data
  and shortens the label to pay for it, bringing the threshold to 50 columns:
  a 100-column terminal split in two.

### Internal

- `goreleaser check` runs in CI. Nothing validated the release config until a
  tag was pushed, by which point the tag is immutable and the failure costs a
  renumbered release. It is pinned to the same GoReleaser version the release
  uses, because a gate that validates a different version than the one that
  ships is not a gate.

## [0.11.0] - 2026-09-09

### Added

- **magmux tells its children which background it resolved**, as
  `MAGMUX_THEME=light|dark`. A TUI child never needed it — it queries OSC 11
  and magmux answers from the same resolution — but a child that is not a TUI
  had no way to ask, and so had to hardcode one background's palette. The
  export is appended after the inherited environment, so magmux's own
  resolution beats a `MAGMUX_THEME` set in the shell; `--theme light` under a
  dark shell no longer tells children the opposite of what magmux is drawing.
- **A staged multi-agent demo** (`task demo`, `demo/`). Three agent panes
  driven by one controller, with the control panel open — free, deterministic,
  and needing no API key. The agents are staged, but each files a real Claude
  Code transcript, so magmux attaches a real controller to every pane and the
  panel's `◀ IN` rows remain what magmux itself observed.

### Changed

- **The control panel reads as badges rather than coloured text.** Route states
  are filled chips, the interleaved stream carries a direction-tinted rule that
  a request's acknowledgement inherits, and each exchange is headed by the
  controller's step tag on the left and the state magmux observed on the right.
  The two chips are coloured from different sources on purpose: the panel's
  provenance rule is now legible at a glance instead of on a careful read.
- **The pilot screen splits its messages.** Every entry is a chip plus a body
  block under a direction-tinted gutter, so an instruction sent and a turn
  observed are distinguishable without reading either.

### Fixed

- **The pilot was illegible on a light terminal.** Its palette was truecolor
  chosen for a dark background, and body text was a near-white lavender — so
  the most important content on the screen was the least readable thing on it.
  Body text now follows `MAGMUX_THEME`, and falls back to the terminal's own
  default foreground when it is unset. Badges stay truecolor deliberately: a
  chip paints its own ground, so the only contrast that matters is ink against
  chip.
- **The pilot blamed itself for provider failures.** `pi` records a refused
  request as a *completed* assistant message carrying `stopReason: "error"` —
  `prompt()` returns normally and throws nothing. Watching only text deltas
  missed it entirely, so a dead API key surfaced as three empty turns, two
  nudges, and the summary "the pilot stopped without calling finish". The
  provider's own error is now reported, and nudging stops once one is seen.

[0.12.0]: https://github.com/MadAppGang/magmux/releases/tag/v0.12.0
[0.11.0]: https://github.com/MadAppGang/magmux/releases/tag/v0.11.0
