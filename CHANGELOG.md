# Changelog

All notable changes to magmux are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Releases before v0.11.0 predate this file; their notes were generated from
commit subjects and remain on the
[GitHub releases page](https://github.com/MadAppGang/magmux/releases).

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

[0.11.0]: https://github.com/MadAppGang/magmux/releases/tag/v0.11.0
