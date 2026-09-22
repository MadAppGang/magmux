# Feature request: the status line as a surface a controller can drive

**From:** Jack Rudenko — claudish
**Magmux version in use:** 0.11.0
**Platform:** macOS

---

## What we want

The status line, driven entirely from the socket by the program that launched magmux:

1. **Show and hide it on demand.** Hidden by default. It appears when something is worth
   reporting and goes away when that is over. The row should not be spent while nothing is
   wrong.

2. **Draw whatever we want in it.** Arbitrary content, not only the segment vocabulary magmux
   currently offers. We should be able to decide the text, the colours and the layout of the
   line ourselves.

3. **Set its height.** One row is often enough, but not always. A controller should be able to
   ask for more rows and give them back, so a short notice and a fuller report can use the same
   surface.

4. **Read back what happened.** Whether the line is currently shown, and what height it actually
   got, so a controller can render correctly instead of tracking state it cannot confirm — and so
   a refusal is legible rather than silent.

## Why

claudish keeps a Claude Code session alive through a network outage: it holds the request open and
retries on a backoff ladder instead of letting the turn die. While that is happening the user
should see, on the session they are already looking at, what is wrong, which host is unreachable,
how long until the next attempt and how many attempts have been made. When the connection comes
back, all of that should disappear.

That is a status line's job, and magmux already has one. What is missing is the ability for the
program driving magmux to control it: today the line can only be shown or hidden by a person
pressing a key, so a controller cannot use it as an on-demand surface at all.

## What we do instead today, and why it is worse

Lacking that, claudish opens a real pane for the outage and closes it afterwards. Two costs:

- **It is far heavier than the job.** A whole pane, with its own process and its own channel to
  talk to, to display a few lines of text that belong on a status line.

- **It damages the session.** Closing that pane leaves the surviving Claude Code pane mispainted:
  its bottom chrome collapses onto a single line and the input box is destroyed, and nothing we
  have tried from outside repairs it. The user is handed back a session they have to restart.
  magmux resizes correctly here — we measured the surviving pane's terminal as exactly the full
  size — so this is not a magmux defect. It is the consequence of reaching for the heaviest
  mechanism available because the light one is out of reach.

A status line we can show, draw and size from the socket removes both costs: nothing is created,
nothing is destroyed, and the session's layout is never disturbed.

## What we would build with it

- An outage opens: show the line, draw the provider, the host, the reason.
- Every second: redraw the countdown and the attempt count.
- Recovery: draw the outcome briefly, then hide the line.
- Nothing wrong: no line, no rows spent, nothing on screen.

## Also worth having

The same control over the **control panel** would be useful for the same reason, though the status
line is what we need first.
