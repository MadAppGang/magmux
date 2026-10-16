# Claude Code screens (testdata for `claude_screen.go`)

Real captures only. No file here is a hand-drawn Claude screen.

## `s*.raw` + `s*.marks.json`: Claude Code 2.1.301, byte captures

Recorded 2026-10-16. Claude ran as `script -q -t 0 <raw> claude --model haiku …` inside a
headless magmux 0.15.1 pane, 100x39, on macOS. `<name>.raw` is every byte Claude wrote to
its pty. `<name>.marks.json` lists `{label, offset, cols, rows, text}`: the raw file's byte
offset at a labelled moment, plus magmux's own text capture of that moment.

A screen at a label is `raw[0:offset]` replayed through the VT parser into a 100x39
`newScrollPane` (`claudePaneAt` in `claude_screen_test.go`).
`TestClaudeCapturesReplayFaithfully` checks every replay against the mark's `text`.

| file | what it holds |
|---|---|
| `s1-box-history` | idle box with a faint placeholder; typed text wrapping mid-word; the history entry; a 9-row typed prompt in the box and in history (not abbreviated); assistant prose that begins with the prompt's words; an assistant reply quoting `❯`, `Do you want to proceed?` and `Esc to cancel`; `/clear` and `/compact` |
| `s2-permission` | the Bash permission dialog; the AskUserQuestion dialog (`❯ 1. Red` at column 0); both after Escape |
| `s3-paste` | a 4-line bracketed paste collapsed to `[Pasted text #1 +3 lines]`; a small paste NOT collapsed; a 955-byte single-line auto send collapsed to `[Pasted text #2]`; history shows both expanded |
| `s4-midturn` | a running turn (4 frames); a queued mid-turn send with its `ctrl+x ctrl+s to send now` hint and the faint `Press up to edit queued messages` placeholder (3 frames); a queued paste-mode send; after both ran |
| `s5-insert-plan` | vim on: `-- INSERT --` footer (the status line's ` I ` is the user's own); the ExitPlanMode dialog |
| `s6-markdown` | research F10: a markdown probe in the box (raw) and in history (rendered: `` ` `` `**` `*` `~~` dropped, `_` `[]()` `<b>` `\*` kept); claudish's backticked launch line in both; after it, Claude's faint (SGR 2) suggested next prompt in the box |
| `s7-bashmode` | bash mode: a `!` typed into the empty box is consumed and becomes the box's glyph (`!` + NBSP, faint `Try "…"` placeholder, footer `! for shell mode`); `! echo BASHMODE-OK` typed; the history entry `! echo BASHMODE-OK` (ordinary space) with its `⎿` output, and the `❯` box back |

Scrubbed before commit, same length so every offset still holds: the session id
(`session_01` + 22 chars → `X`s), `/Users/<name>/` → `/Users/user/` and `-Users-<name>-` →
`-Users-user-`.

Derived frames (offsets with no mark, named in `claude_screen_test.go`): Claude ends every
frame with `ESC[?25h`, so a prefix ending there is a complete frame.
`TestDerivedFramesAreFrameEnds` pins each offset.

- `s4-midturn` 12771 and 12825: the background `Bash(…)` row with its running `⏺` blinked
  off, then on.

## `claudish-2.1.290-dialog-*.json`: boot dialogs, text captures

No trust dialog could be captured for 2.1.301, because every scratch directory was already
trusted. These three files are claudish's own live captures of Claude Code 2.1.290
(`claudish/packages/cli/src/pane/test-fixtures/screens/`), copied unchanged: trust, bypass
and onboarding. They hold text only, so every rune reads as non-faint.

## `research-2.1.301-midturn.txt`: text capture (research F9)

magmux text captures of a mid-turn send, one `=== title` block per screen, each row
prefixed with `|`. Blank rows were dropped when it was recorded. Its first screen has the
blinked-off tool row directly under the `❯` entry, which the r3 H3 rule exists for.
