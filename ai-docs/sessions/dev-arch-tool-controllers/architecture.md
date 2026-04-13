# Interactive Tool Controllers

## Problem

magmux currently detects "agent is done / waiting for input" by scraping the pane's screen buffer and watching escape sequences (window title changes, bracketed paste cycling, text idle). This is fragile:

- **Wrong content in DONE popup**: scraping the visible pane finds whatever happens to be on screen — sometimes the actual response, sometimes a spinner phrase like `Waddling… running stop hooks`, sometimes an MCP warning. We have no semantic distinction between "this line is the response" and "this line is UI chrome."
- **False positives on transition**: Claude Code briefly flashes the idle title (`✳`) between the model finishing and stop hooks running. Title-based detection fires on the flash. We've patched it with a 2-second debounce, but that's a guess.
- **No way to interact**: the screen-scraping model is read-only. There's no path to "ask the agent a question" or "send a structured user input request and wait for a structured reply."

Different agents will keep being added (codex, copilot CLI, opencode, custom tools). Each has a different idea of "done", a different shape of output, and different signals. Putting one if/else chain inside magmux's VT parser would not scale.

## Goal

Define a small, stable interface for **interactive tool controllers** — pluggable adapters that know how to read a specific agent's state of the world and report back to magmux in a uniform way. magmux core never has to know "this is Claude Code" or "this is codex." It just polls the controller.

First implementation: Claude Code, reading from its session JSONL files (the ground truth) instead of scraping the pane.

## Non-goals

- **Replacing screen rendering.** Panes still draw to the screen normally. Controllers are a side-channel that observes state, not a replacement for the PTY.
- **Cross-agent abstraction over agent capabilities.** Controllers don't try to make codex look like Claude Code. They just expose a small uniform status surface.
- **Synchronous request/response in v1.** v1 is read-only (status polling). The interface is shaped so a future v2 can add input requests without breaking changes.

## Design

### The interface (Go)

```go
// ToolController is a side-channel observer + interactor for a specific
// interactive tool running inside a pane. magmux holds one controller per
// pane that has one attached.
type ToolController interface {
    // Name identifies the controller implementation.
    // E.g. "claude-code", "codex", "opencode".
    Name() string

    // Poll returns the current snapshot of the controlled tool's state.
    // Called from magmux's render loop (once per ~250ms, not every frame).
    // Implementations should be cheap and non-blocking — read a file,
    // parse a small chunk, return. Heavy work goes in Start.
    Poll() (Snapshot, error)

    // Start begins any long-running work (file watchers, background
    // goroutines). Called once when the controller is attached. Must be
    // idempotent — magmux may call it multiple times.
    Start(ctx context.Context) error

    // Stop tears down resources. Called when the pane closes or the
    // controller is detached.
    Stop() error
}

// Snapshot is the uniform status surface every controller produces.
// All fields are optional except State. Future fields are additive.
type Snapshot struct {
    // State is the high-level lifecycle stage. Required.
    State State

    // Project is a short label for the work being done, if known.
    // E.g. "magmux", "user-service", "<no project>".
    Project string

    // Model is the model name in use, if applicable. E.g. "claude-opus-4-6".
    Model string

    // LastUserPrompt is the most recent user input the tool received,
    // truncated to a reasonable length. Empty if unknown.
    LastUserPrompt string

    // LastResponse is the most recent assistant response text the tool
    // produced. For tools that emit structured content (tool calls vs
    // text), this is the last text block. Empty if unknown.
    LastResponse string

    // LastTool is the name of the most recent tool the agent used,
    // if any. E.g. "Bash", "Read", "Edit". Empty if not applicable
    // or no tool was used in the latest turn.
    LastTool string

    // StartedAt is when the controller observed work begin (first
    // user prompt, first stream chunk, etc). Zero if unknown.
    StartedAt time.Time

    // CompletedAt is when the most recent turn finished. Zero while
    // the tool is still working.
    CompletedAt time.Time

    // Error captures a tool-side error if the agent reported one
    // (auth failure, rate limit, etc). Nil for normal operation.
    Error error
}

// State is the lifecycle stage of an interactive tool.
type State int

const (
    StateUnknown          State = iota // controller hasn't observed enough to decide
    StateStarting                      // tool is initializing
    StateWorking                       // tool is actively processing
    StateAwaitingInput                 // tool finished a turn, waiting for user
    StateAwaitingPermission            // tool is blocked on a permission prompt
    StateError                         // tool reported an error
    StateGone                          // tool process has exited
)
```

That's the whole interface. **Three methods**, one struct, one enum.

### Why this shape

- **Polling, not push.** magmux already has a render loop running at 60fps. Controllers fit in by being polled at a slower cadence (250ms is plenty — humans don't notice slower than 4Hz). No callbacks, no goroutine ownership confusion.
- **Snapshot is a value type.** No mutation, no locking on the consumer side. Each `Poll()` returns a fresh snapshot. Implementations decide how to compute it (cached file watch, recomputed every call, whatever).
- **Optional fields.** Different tools expose different things. A barebones controller can return just `State`. A rich one can populate everything. magmux uses what's available.
- **`State` enum, not strings.** Type safety; the renderer can switch on it. New states can be added at the end (additive).
- **`Start`/`Stop` for resource lifecycle.** A controller that watches files needs a goroutine; this gives it a clean place to spin up and tear down. Controllers that don't need any state can leave both as no-ops.

### How magmux integrates it

A new field on `Pane`:

```go
type Pane struct {
    // ...existing fields...
    controller     ToolController // nil if no controller attached
    controllerSnap Snapshot       // last snapshot from controller
}
```

A new field on `Magmux`:

```go
type Magmux struct {
    // ...existing fields...
    controllerRegistry []ControllerFactory // registered factories
}

// ControllerFactory inspects a pane's command/env and returns a controller
// if it can handle that tool, or nil if not.
type ControllerFactory func(p *Pane) ToolController
```

When magmux spawns a pane:

```go
func (m *Magmux) attachController(p *Pane) {
    for _, factory := range m.controllerRegistry {
        if c := factory(p); c != nil {
            p.controller = c
            c.Start(m.ctx)
            return
        }
    }
}
```

In the render loop (replacing current heuristic detection):

```go
// Poll controllers at ~4Hz, not every frame
if now.Sub(m.lastControllerPoll) > 250*time.Millisecond {
    m.lastControllerPoll = now
    for _, p := range m.allPanes {
        if p.controller == nil {
            continue
        }
        snap, err := p.controller.Poll()
        if err != nil {
            continue // keep last good snapshot
        }
        p.mu.Lock()
        p.controllerSnap = snap
        applyControllerSnapshot(p, snap)
        p.mu.Unlock()
    }
}
```

Where `applyControllerSnapshot` translates the snapshot to existing pane state (`inputReady`, `tint`, `overlayText`):

```go
func applyControllerSnapshot(p *Pane, s Snapshot) {
    switch s.State {
    case StateAwaitingInput:
        if !p.inputReady {
            p.inputReady = true
            p.inputSignal = "ctrl"
            p.tint = "green"
            p.overlayStyle = "success"
            // Build multi-line popup from snapshot — NOT screen scraping
            lines := []string{"\u2713 DONE"}
            if !s.StartedAt.IsZero() && !s.CompletedAt.IsZero() {
                lines = append(lines, "took "+formatDuration(s.CompletedAt.Sub(s.StartedAt)))
            }
            if s.LastResponse != "" {
                msg := s.LastResponse
                if utf8.RuneCountInString(msg) > 40 {
                    runes := []rune(msg)
                    msg = string(runes[:39]) + "\u2026"
                }
                lines = append(lines, msg)
            }
            p.overlayText = strings.Join(lines, "\n")
            p.dirty = true
        }
    case StateAwaitingPermission:
        p.inputReady = true
        p.inputSignal = "perm"
        p.tint = "yellow"
        p.overlayText = "\u26a0 NEEDS PERMISSION"
        p.overlayStyle = "info"
        p.dirty = true
    case StateError:
        p.tint = "red"
        if s.Error != nil {
            p.overlayText = "\u2717 " + s.Error.Error()
        } else {
            p.overlayText = "\u2717 ERROR"
        }
        p.overlayStyle = "error"
        p.dirty = true
    case StateWorking, StateStarting:
        // Clear any "done" indicators if we previously fired
        if p.inputReady && p.inputSignal == "ctrl" {
            p.inputReady = false
            p.tint = ""
            p.overlayText = ""
            p.overlayStyle = ""
            p.dirty = true
        }
    }
}
```

The screen-scraping detection (title/2004/text-idle) becomes a **fallback** — only used for panes without an attached controller. That keeps backward compatibility for arbitrary shells while giving rich data when a known agent is detected.

### First implementation: ClaudeCodeController

#### Source of truth

Claude Code writes a JSONL transcript for every session at:
```
~/.claude/projects/<cwd-with-slashes-replaced-by-dashes>/<session-uuid>.jsonl
```

Discovery: given a pane's cwd, the project dir is deterministic. The active session is the file with the most recent mtime in that dir.

Each line is a JSON object with a `type` field. The relevant types:

- `{"type":"user","message":{"role":"user","content":"..."},"timestamp":"...","uuid":"..."}` — user prompt
- `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"..."}],"stop_reason":"end_turn"},"timestamp":"..."}` — assistant response
- `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{...}}],"stop_reason":"tool_use"}}` — tool call
- `{"type":"system","subtype":"stop_hook_summary","timestamp":"..."}` — fired AFTER all stop hooks finish (true end of turn)
- `{"type":"permission-mode","permissionMode":"..."}` — permission mode change

#### Detection logic

```go
type ClaudeCodeController struct {
    pane        *Pane
    sessionPath string         // resolved on first poll
    fileSize    int64          // last seen size (mtime alone is unreliable)
    snap        Snapshot       // cached snapshot
    parser      jsonlParser    // streaming line reader
}

func (c *ClaudeCodeController) Name() string { return "claude-code" }

func (c *ClaudeCodeController) Start(ctx context.Context) error {
    // Resolve cwd → project dir
    cwd := c.pane.cwd()
    projectDir := filepath.Join(homeDir(), ".claude", "projects",
        strings.ReplaceAll(cwd, "/", "-"))
    c.parser.dir = projectDir
    return nil
}

func (c *ClaudeCodeController) Poll() (Snapshot, error) {
    // 1. Find the most recent .jsonl in the project dir.
    //    If we don't have a session yet, or the active one changed, switch.
    sessionPath, err := c.parser.findActiveSession()
    if err != nil || sessionPath == "" {
        c.snap.State = StateStarting
        return c.snap, nil
    }
    if sessionPath != c.sessionPath {
        c.sessionPath = sessionPath
        c.fileSize = 0
        c.parser.reset()
    }

    // 2. Read any new lines since the last poll.
    info, err := os.Stat(sessionPath)
    if err != nil {
        return c.snap, err
    }
    if info.Size() > c.fileSize {
        newLines, err := c.parser.readNew(sessionPath, c.fileSize, info.Size())
        if err != nil {
            return c.snap, err
        }
        c.fileSize = info.Size()
        c.applyLines(newLines)
    }

    return c.snap, nil
}

// applyLines walks the parsed lines and updates the cached snapshot.
// State machine:
//   user prompt → StateWorking, StartedAt=now, clear LastResponse
//   assistant tool_use → StateWorking, set LastTool
//   assistant text → StateWorking, set LastResponse to text content
//   stop_hook_summary → StateAwaitingInput, CompletedAt=ts
//   permission required → StateAwaitingPermission
func (c *ClaudeCodeController) applyLines(lines []map[string]any) {
    for _, line := range lines {
        switch line["type"] {
        case "user":
            if msg, ok := line["message"].(map[string]any); ok {
                if content, ok := msg["content"].(string); ok {
                    c.snap.LastUserPrompt = content
                }
            }
            c.snap.State = StateWorking
            c.snap.StartedAt = parseTimestamp(line["timestamp"])
            c.snap.CompletedAt = time.Time{}
            c.snap.LastResponse = ""
            c.snap.LastTool = ""

        case "assistant":
            msg, _ := line["message"].(map[string]any)
            content, _ := msg["content"].([]any)
            for _, block := range content {
                b, _ := block.(map[string]any)
                switch b["type"] {
                case "text":
                    if t, ok := b["text"].(string); ok && t != "" {
                        c.snap.LastResponse = t
                    }
                case "tool_use":
                    if name, ok := b["name"].(string); ok {
                        c.snap.LastTool = name
                    }
                }
            }
            if model, ok := msg["model"].(string); ok {
                c.snap.Model = model
            }
            // Don't transition state on assistant — wait for stop_hook_summary

        case "system":
            if line["subtype"] == "stop_hook_summary" {
                c.snap.State = StateAwaitingInput
                c.snap.CompletedAt = parseTimestamp(line["timestamp"])
            }
        }

        // Project name from cwd
        if c.snap.Project == "" {
            if cwd, ok := line["cwd"].(string); ok {
                c.snap.Project = filepath.Base(cwd)
            }
        }
    }
}

func (c *ClaudeCodeController) Stop() error { return nil }
```

#### Why `stop_hook_summary` is the right "done" signal

It's emitted by Claude Code AFTER:
1. The model has finished generating its response
2. All registered stop hooks have run
3. Any post-processing is complete

This is what's actually meant by "Claude Code is now waiting for the user." Title detection (`✳`) fires before stop hooks finish; we patched it with a debounce. The session JSONL fires it AT THE RIGHT TIME, with no debounce needed.

#### Factory registration

```go
func newClaudeCodeFactory() ControllerFactory {
    return func(p *Pane) ToolController {
        cmd := p.cmd.Path
        if filepath.Base(cmd) != "claude" {
            return nil
        }
        return &ClaudeCodeController{pane: p}
    }
}
```

In `init()` or `main()`:
```go
mux.controllerRegistry = []ControllerFactory{
    newClaudeCodeFactory(),
    // future: newCodexFactory(), newOpencodeFactory(), ...
}
```

Detection is based on the pane's executed command. If the user runs `claude "say hi"`, magmux sees the command starts with `claude` and attaches the controller. If they run `bash`, no controller attaches and the existing screen-scraping fallback kicks in.

### File layout

| File | Purpose |
|---|---|
| `controller.go` | `ToolController` interface, `Snapshot`, `State` enum, `ControllerFactory` |
| `controller_claude.go` | `ClaudeCodeController` + factory |
| `main.go` | Pane fields, registry, integration in render loop |

Keeping each controller in its own file means adding codex later is just `controller_codex.go` + factory registration. No edits to `controller.go` needed.

## Future v2: structured input requests

When v2 needs to ask the agent a question (e.g. "the user wants to grant permission, send the answer"), we extend the interface:

```go
type Interactor interface {
    ToolController
    // SendInput delivers a structured input request and returns the agent's
    // structured response. May block.
    SendInput(ctx context.Context, req InputRequest) (InputResponse, error)
}
```

Controllers that support interaction implement `Interactor`. The base `ToolController` interface is unchanged. Adding interaction is opt-in per controller — no breaking changes.

For Claude Code specifically, this could write to a hook input file or use a future Claude Code MCP. The controller knows the details; magmux core just calls `Interactor.SendInput`.

## Trade-offs

| Decision | Pro | Con |
|---|---|---|
| Polling every 250ms | Simple, no threading drama | 4Hz max responsiveness |
| File-based source of truth | Reliable, no protocol changes needed | Couples to Claude Code's file format |
| Per-pane controller instance | Clean ownership, easy lifecycle | Small memory overhead per pane |
| Optional fields in Snapshot | Forwards-compatible additions | Consumers must handle empty values |
| Factory registry | Order-based dispatch, no ambiguity | Order matters; first matching factory wins |
| Snapshot value, not pointer | No locking, copy on poll | Allocation per poll (small) |

## Implementation order

1. Add `controller.go` with the interface and types — pure data, no logic
2. Add `controller_claude.go` with the file watcher and JSONL parser
3. Add registry + factory + `attachController` to `Magmux`
4. Add controller poll to render loop, gated on controller != nil
5. Add `applyControllerSnapshot` in render path
6. Test with `task test:claude` — verify the DONE popup shows the actual response from the session file
7. Make screen-scraping (title/2004/idle) a fallback ONLY when no controller is attached
8. Document the interface in CLAUDE.md so adding new controllers is obvious

Steps 1–6 are independent of removing the old detection. After verifying step 6 works, step 7 cleans up the legacy path.
