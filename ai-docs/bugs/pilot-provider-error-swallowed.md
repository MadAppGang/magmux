# Bug: a refused model request is reported as the pilot's own misbehaviour

**Severity:** major — a dead API key is diagnosed as "the model would not call
its tools", which sends the reader looking at prompts and model choice instead
of at billing.

**Fixed in:** v0.11.0 (`pilot/pilot.ts`).

---

## Summary

`@earendil-works/pi-coding-agent` records a provider refusal as a **completed**
assistant message carrying `stopReason: "error"` and an `errorMessage` field.
`session.prompt()` resolves normally and throws nothing; the turn simply
produces no tool calls.

The pilot subscribed only to `text_delta` events, so it never saw the error. It
read "no tool call" as the model narrating instead of acting, nudged twice
(`NUDGE_LIMIT = 2`), and finished with `the pilot stopped without calling
finish` — its own name on a failure it never had a turn to cause.

## What it looked like

The control panel read `0 sent │ 0 done`, "no instructions yet", `FAILED`. The
pilot pane read:

```
15:53:13 ◆ pilot connected to pane 0
15:53:13 · pilot model anthropic/claude-sonnet-5
15:53:35 ◀ session ready
15:53:35 · tools: send_to_session, finish
         ! pilot replied without calling a tool — nudging
         ! pilot replied without calling a tool — nudging
15:53:29 ✗ failed
  the pilot stopped without calling finish
```

The whole run took 17s, which is the tell: three model turns that fast are
three turns that never reached a model.

## The actual cause

Subscribing to every event exposed it immediately:

```
"stopReason":"error"
"errorMessage":"400 {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",
  \"message\":\"Your credit balance is too low to access the Anthropic API.\"}}"
```

## Why it survived

The failure has the shape of a success. `prompt()` resolved, the turn ended
cleanly, and the failure rode as a *field* on a well-formed message rather than
as an exception. Every downstream check was written against "did a tool get
called?", which returns a truthful "no" whether the model declined or never ran.

The retry loop made it worse: it turned one clear symptom into three identical
ones and put the pilot's name on the last line.

## Two traps around it

1. `pi`'s `getModel()` resolves from the catalog and `getAvailable()` lists a
   model as available when a key **exists**, not when it has credit. Every
   preflight passes; only the live request fails. `task pilot:check` is
   therefore not proof that a run will work.
2. Claude Code itself is unaffected by a dead `ANTHROPIC_API_KEY` because it
   runs on the Max subscription — a different credential path. So the session
   pane boots perfectly while the pilot cannot make a single call, which makes
   the problem look like it is in the pilot.

## The fix

Watch `event.message.stopReason === "error"` alongside `text_delta`, report the
provider's own message (dug out of its JSON body by `describeProviderError`),
and stop nudging once a provider error has been seen. The closing summary
becomes `the pilot could not reach its model: <provider message>`.
