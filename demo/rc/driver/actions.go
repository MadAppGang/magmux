package main

// The thirteen actions. This file is the demo.
//
// Three rules it is built on, each easy to break by accident:
//
//  1. NOTHING IS SIMULATED. Every action is an HTTP request to magmux, and
//     every response reported is the response that came back.
//  2. EVERY ACTION SAYS WHERE TO LOOK. An effect nobody notices is the same as
//     no effect, and the whole claim is that two independent clients follow.
//  3. IT USES THE SAME PUBLIC API AS EVERYTHING ELSE. No private side channel
//     into the clients, no unix socket shortcut, no flag magmux does not
//     document. The one thing the public API cannot do — retarget somebody
//     else's stream — is action 6, and it is handled by the clients' own
//     pickers rather than by inventing a back door.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// Ctx is what an action is handed. It is re-made for every run, except for the
// three pieces of memory that must survive one (`Opened`, `Stalled`, `Over`).
type Ctx struct {
	cfg  Config
	api  *API
	sink Sink

	// Target is the pane both mirrors are watching, re-read before every run.
	Target int
	// Opened is the pane action 5 opened, if it is still open.
	Opened *int
	// Stalled is the subscriber action 11 is holding, if any.
	Stalled **StalledWS
	// Confirmed is true when the operator has said the confirm word. Only
	// action 12 reads it.
	Confirmed bool
	// Over is set by action 12 once magmux is gone.
	Over *bool
	// Telemetry is the driver's own subscriber, so an action that moves the
	// target can move the header with it.
	Telemetry *Telemetry
}

func (c *Ctx) emit(l Line) { c.sink.Emit(l) }

// Action is one menu entry.
type Action struct {
	Key   string
	Group string
	What  string
	How   string
	// Cred is the credential this action SPENDS, and it is on the menu entry
	// rather than only inside Run because the list renders it as a colour: the
	// actions that reach for the read-only token are the demo's whole point and
	// must be findable before anything has been pressed. The zero value is the
	// session token, which is what almost every action uses.
	Cred Cred
	// Confirm, when set, is the word an operator must type before Run is
	// called at all. Exactly one action has one.
	Confirm string
	Run     func(*Ctx) error
}

// Actions is the menu, in order. The keys are strings because they are what an
// operator types and what `--run N` is given.
var Actions = []Action{
	// ── typing into the session ──────────────────────────────────────────────
	{
		Key: "1", Group: "TYPE INTO THE SESSION",
		What: "type a command",
		How:  "`ls --color=auto`, one POST /v1/ops/input",
		Run: func(c *Ctx) error {
			c.api.Call("input", map[string]any{
				"pane": c.Target, "text": "ls --color=auto", "keys": []string{"enter"},
			}, CredSession)
			c.emit(look(c.cfg.Mirrors() + " — the same colours, decoded from the same frame"))
			return nil
		},
	},
	{
		Key:  "2",
		What: "a burst of scrolling output",
		How:  "200 lines as fast as the shell can print them",
		Run: func(c *Ctx) error {
			c.api.Call("input", map[string]any{
				"pane": c.Target,
				"text": `i=1; while [ $i -le 200 ]; do printf '%3d  row deltas, not a screenshot\n' $i; i=$((i+1)); done`,
				"keys": []string{"enter"},
			}, CredSession)
			c.emit(look("the mirrors keep up because magmux sends CHANGED ROWS, not screens"))
			c.emit(note("the framer coalesces — at 15fps you see fewer frames than there were screens,"))
			c.emit(note("and never an older one. The seq counter jumps; that is the latest-wins slot"))
			c.emit(note("doing its job, and the frame sparkline above is it happening."))
			return nil
		},
	},
	{
		Key:  "3",
		What: "run `top`",
		How:  "the ALT SCREEN — magmux forces a keyframe across the switch",
		Run: func(c *Ctx) error {
			c.api.Call("input", map[string]any{
				"pane": c.Target, "text": "top", "keys": []string{"enter"},
			}, CredSession)
			c.emit(look(c.cfg.Mirrors() + " — the status bar now says alt"))
			c.emit(note("A client cannot apply row deltas across an alt-screen switch, so magmux"))
			c.emit(note("forces a keyframe: clear, then every row. Action 4 comes back the same way."))
			return nil
		},
	},
	{
		Key:  "4",
		What: "quit top",
		How:  "the letter q, straight at the PTY — another forced keyframe",
		Run: func(c *Ctx) error {
			c.api.Call("input", map[string]any{"pane": c.Target, "keys": []string{"q"}}, CredSession)
			c.emit(look("the primary screen returns with its scrollback intact — the alt screen never recorded any"))
			return nil
		},
	},

	// ── panes ────────────────────────────────────────────────────────────────
	{
		Key: "5", Group: "PANES",
		What: "open a second pane",
		How:  "POST /v1/ops/open_pane — magmux splits its own layout",
		Run: func(c *Ctx) error {
			r := c.api.Call("open_pane", map[string]any{
				"cmd":   `date; echo 'this pane was opened by the driver, over POST /v1/ops/open_pane'; exec ${SHELL:-/bin/sh} -i`,
				"label": "driver",
				"split": "auto",
			}, CredSession)
			id, ok := r.ResultPane()
			if !ok {
				c.emit(verdict(false, "NO PANE", "no pane id came back — nothing was opened"))
				return nil
			}
			*c.Opened = id
			c.emit(verdict(true, "OPENED", fmt.Sprintf("pane %d — remember it for actions 6 and 7", id)))
			c.emit(look("the magmux pane splits; the mirrors keep watching their own pane"))
			c.emit(note("action 6 is how you move them, and why there is no op that does it for them"))
			return nil
		},
	},
	{
		Key:  "6",
		What: "point the mirrors at it",
		How:  "each client retargets ITSELF — there is no op that does it to them",
		Run: func(c *Ctx) error {
			list, err := c.api.Panes()
			if err != nil {
				return err
			}
			c.emit(heading("panes right now"))
			for _, p := range list {
				mark := ""
				if p.ID == c.Target {
					mark = "   ← the mirrors are here"
				}
				label := p.Label
				if label == "" {
					label = p.Cmd
				}
				if len(label) > 46 {
					label = label[:46]
				}
				c.emit(note(fmt.Sprintf("%3d  %-14s %s%s", p.ID, p.State, label, mark)))
			}
			c.emit(heading("why there is no action that just does this"))
			c.emit(note("A watch is per-SUBSCRIBER state: the hub keeps one latest-wins slot per"))
			c.emit(note("(connection, pane), and magmux offers no op that reaches into somebody"))
			c.emit(note("else's connection to point it somewhere new. That is not a gap — a demo"))
			c.emit(note("that invented a private side channel here would be contradicting the one"))
			c.emit(note("public API it exists to demonstrate. So retargeting is the CLIENT's own"))
			c.emit(note("act, `unwatch` then `watch`, and every client exposes it:"))
			c.emit(note(fmt.Sprintf("terminal  focus the mirror %s and press ] or [, or the pane's digit", c.cfg.MirrorKeys())))
			c.emit(note("browser   the pane picker in the status bar, top right"))
			c.emit(note("this driver  ] and [ in the header — the same two ops, from here"))
			c.emit(look("the mirror's own status bar shows the pane list and which one it is on"))
			return nil
		},
	},
	{
		Key:  "7",
		What: "close it (id is a tombstone)",
		How:  "close_pane, then the same id refused, then a NEW id for a new pane",
		Run: func(c *Ctx) error {
			// This process's memory, and a driver started fresh against a
			// running demo has none. So fall back to asking magmux: the
			// highest-numbered pane action 5 labelled. Highest, because ids only
			// ever go up, so the newest one is the one action 5 just made.
			id := *c.Opened
			if id < 0 {
				list, _ := c.api.Panes()
				best := -1
				for _, p := range list {
					if p.Label == "driver" && p.ID > best {
						best = p.ID
					}
				}
				id = best
				if id >= 0 {
					c.emit(note(fmt.Sprintf("this driver did not open it, but magmux says pane %d is labelled \"driver\"", id)))
				}
			}
			if id < 0 {
				c.emit(verdict(false, "NOTHING", "nothing to close — run action 5 first"))
				return nil
			}
			c.api.Call("close_pane", map[string]any{"pane": id}, CredSession)
			c.emit(heading("and now the same id again — an id slot is never renumbered and never reused"))
			c.api.Call("close_pane", map[string]any{"pane": id}, CredSession)
			c.emit(heading("a fresh pane takes the NEXT id, not the free one"))
			again := c.api.Call("open_pane", map[string]any{"cmd": "sleep 5", "label": "proof"}, CredSession)
			if next, ok := again.ResultPane(); ok {
				if next > id {
					c.emit(verdict(true, "APPEND-ONLY", fmt.Sprintf("pane %d was closed and the new pane is %d, not %d", id, next, id)))
				} else {
					c.emit(verdict(false, "REUSED", fmt.Sprintf("the new pane is %d — that should never happen", next)))
				}
				c.emit(note("m.allPanes is an append-only slot table and Pane.id IS the index, so a"))
				c.emit(note(fmt.Sprintf("`send` to pane %d can never quietly reach a different session later.", id)))
				c.api.Call("close_pane", map[string]any{"pane": next, "force": true}, CredSession)
			}
			*c.Opened = -1
			c.emit(look("the magmux pane reflows back — the layout collapses the closed pane's parent"))
			return nil
		},
	},

	// ── what a viewer cannot do ─────────────────────────────────────────────
	{
		Key: "8", Group: "WHAT A VIEWER CANNOT DO",
		What: "try `input`, VIEW token",
		How:  "the credential both mirrors hold. magmux answers, not this driver",
		Cred: CredView,
		Run: func(c *Ctx) error {
			r := c.api.Call("input", map[string]any{"pane": c.Target, "text": "rm -rf /"}, CredView)
			if r.Status == 403 && r.Code() == "forbidden" {
				c.emit(verdict(true, "BOUNDARY PROVEN", "403 forbidden, off the wire, in magmux's own words:"))
				c.emit(note(r.ErrorMessage()))
				c.emit(note("`input` is ClassInput (mux/input.go), and a view token is refused every"))
				c.emit(note("class above read. The refusal is an HTTP STATUS, decided before anything"))
				c.emit(note("is upgraded or streamed — nothing was typed into any pane."))
			} else {
				c.emit(verdict(false, "NOT REFUSED", fmt.Sprintf("expected 403 forbidden and got HTTP %d — that is a real failure", r.Status)))
			}
			c.emit(look("the mirrors show nothing new, because nothing happened"))
			return nil
		},
	},
	{
		Key:  "9",
		What: "try `open_pane`, VIEW token",
		How:  "same credential, a control-class op this time",
		Cred: CredView,
		Run: func(c *Ctx) error {
			r := c.api.Call("open_pane", map[string]any{"cmd": "sleep 60"}, CredView)
			if r.Status == 403 && r.Code() == "forbidden" {
				c.emit(verdict(true, "BOUNDARY PROVEN", "403 forbidden: "+r.ErrorMessage()))
				c.emit(note("class control this time, class input last time — one rule, not a list of blocked ops"))
			} else {
				c.emit(verdict(false, "NOT REFUSED", fmt.Sprintf("expected 403 forbidden and got HTTP %d — that is a real failure", r.Status)))
			}
			c.emit(heading("and the same token on a READ op, to show it is a capability and not a ban"))
			g := c.api.Get("/v1/panes", CredView)
			if g.OK {
				c.emit(verdict(true, "READ ALLOWED", "the same credential, a read-class op, HTTP 200"))
			}
			c.emit(look("that 200 is why the mirrors can watch at all: `watch` and `list` are read-class"))
			return nil
		},
	},

	// ── the plugin ───────────────────────────────────────────────────────────
	{
		Key: "10", Group: "THE PLUGIN",
		What: "run a ticket",
		How:  "ticket.run_ticket — an op no line of magmux knows about",
		Run: func(c *Ctx) error {
			rev, names, err := c.api.Ops()
			if err != nil {
				return err
			}
			if !contains(names, "ticket.run_ticket") {
				c.emit(verdict(false, "NO PLUGIN", "the ticket-runner plugin is not registered — skipping"))
				c.emit(note(fmt.Sprintf("%d ops are registered and none of them is ticket.run_ticket.", len(names))))
				c.emit(note("The launcher passes --plugin 'bun examples/plugins/ticket-runner/main.ts';"))
				c.emit(note("a plugin that fails to start is reported and skipped rather than fatal, so"))
				c.emit(note(fmt.Sprintf("look in %s/magmux-%s.plugin-ticket.log.", c.cfg.State, c.cfg.ID)))
				return nil
			}
			c.emit(note(fmt.Sprintf("op rev %d · the plugin registered these: %s", rev, strings.Join(qualified(names), ", "))))
			r := c.api.Call("ticket.run_ticket", map[string]any{"title": "mirror this ticket end to end"}, CredSession)
			pane, ok := r.ResultPane()
			if !ok {
				return nil
			}
			c.emit(look(fmt.Sprintf("pane %d opens, the plugin waits for its prompt, sends the ticket, and watches for the answer", pane)))
			c.emit(heading("polling ticket.status until the plugin says the turn is over"))
			for range 12 {
				time.Sleep(600 * time.Millisecond)
				s := c.api.Call("ticket.status", map[string]any{}, CredSession)
				t := lastTicket(s)
				if t == nil {
					continue
				}
				state, _ := t["state"].(string)
				if state == "sent" || state == "starting" {
					continue
				}
				elapsed, _ := t["elapsedMs"].(float64)
				resp, _ := t["response"].(string)
				c.emit(verdict(true, strings.ToUpper(state), fmt.Sprintf("in %dms — %s", int(elapsed), resp)))
				c.emit(note("That answer is the PLUGIN's observation of its own pane, pushed as a"))
				c.emit(note("controller snapshot. magmux reconciles it with the terminal's own idle"))
				c.emit(note("signals, which is why the live snapshot and the shutdown results agree."))
				return nil
			}
			c.emit(verdict(false, "STILL RUNNING", "the ticket has not finished yet — poll ticket.status again with action 10"))
			return nil
		},
	},

	// ── failure, on purpose ─────────────────────────────────────────────────
	{
		Key: "11", Group: "FAILURE, ON PURPOSE",
		What: "stall a WebSocket client",
		How:  "a real subscriber that stops reading, held while the session is driven",
		Run: func(c *Ctx) error {
			if *c.Stalled != nil {
				c.emit(note("a stalled client is already connected — releasing it first"))
				(*c.Stalled).Close()
				*c.Stalled = nil
			}
			s := &StalledWS{}
			c.emit(req(fmt.Sprintf("raw TCP → /v1/ws, watch pane %d at 30fps, then STOP READING", c.Target), CredSession))
			if err := s.ConnectAndWatch(c.cfg, c.Target); err != nil {
				c.emit(verdict(false, "NOT STAGED", "could not stage the stall: "+err.Error()))
				return nil
			}
			*c.Stalled = s
			c.emit(verdict(true, "STALLED", fmt.Sprintf("connected and watching; it read %d bytes including a frame, and stops here", s.BytesRead)))
			c.emit(look(c.cfg.Mirrors() + " — they must keep painting. That is the whole property"))
			c.emit(heading("driving the session while the stall is in place"))
			for i := 1; i <= 5; i++ {
				c.api.Call("input", map[string]any{
					"pane": c.Target,
					"text": fmt.Sprintf(`printf 'stalled-client test %d of 5 — the live mirrors are still painting\n' %d`, i, i),
					"keys": []string{"enter"},
				}, CredSession)
				time.Sleep(1500 * time.Millisecond)
			}
			c.emit(heading(fmt.Sprintf("releasing the stalled client — it read %d bytes in total", s.BytesRead)))
			s.Close()
			*c.Stalled = nil
			c.emit(note("Each subscriber owns a bounded FIFO and one writer goroutine, so a client"))
			c.emit(note("that stops reading fills its OWN queue and is closed with slow_consumer,"))
			c.emit(note("alone. test/rc/case6-slow-client.ts is where that is measured; this is"))
			c.emit(note("where you watch it not matter to anybody else."))
			return nil
		},
	},
	{
		Key:     "12",
		What:    "kill magmux",
		How:     "SIGKILL — ENDS THE DEMO. Both mirrors go red and exit non-zero",
		Confirm: "kill",
		Run: func(c *Ctx) error {
			if !c.Confirmed {
				c.emit(verdict(false, "CANCELLED", "nothing was killed"))
				return nil
			}
			// By the run's unique id, and never `pkill -f magmux`: that is how
			// somebody loses the magmux they were working in. The pattern is
			// the two flags server.sh writes ADJACENTLY, so it names this run's
			// magmux and nothing else.
			//
			// The `--` is load-bearing. `-f` takes no argument, so without it
			// pgrep's getopt reads the pattern — which begins `--id` — as an
			// option and matches nothing, and the action reports "it may
			// already be gone" against a magmux that is plainly running.
			pattern := fmt.Sprintf("--id %s --view-token-file", c.cfg.ID)
			c.emit(req("pgrep -f -- "+strconv.Quote(pattern), CredSession))
			out, _ := exec.Command("pgrep", "-f", "--", pattern).Output()
			var pids []string
			for _, l := range strings.Fields(string(out)) {
				if n, err := strconv.Atoi(l); err == nil && n > 0 {
					pids = append(pids, l)
				}
			}
			if len(pids) == 0 {
				c.emit(verdict(false, "NOT FOUND", "found no magmux carrying --id "+c.cfg.ID+" — it may already be gone"))
				return nil
			}
			c.emit(req("kill -KILL "+strings.Join(pids, " "), CredSession))
			for _, p := range pids {
				if err := exec.Command("kill", "-KILL", p).Run(); err != nil {
					c.emit(note(fmt.Sprintf("pid %s: %v", p, err)))
				}
			}
			// Proof it is gone, from the outside: the port stops answering.
			answering := true
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && answering; {
				time.Sleep(200 * time.Millisecond)
				if _, err := c.api.Panes(); err != nil {
					answering = false
				}
			}
			if answering {
				c.emit(verdict(false, "STILL UP", c.cfg.URL+" is STILL answering — the kill did not take"))
			} else {
				c.emit(verdict(true, "GONE", c.cfg.URL+" no longer answers"))
			}
			c.emit(heading("what happens now, and why"))
			c.emit(note("SIGKILL, so there was no orderly shutdown: no `results`, no `shutdown`,"))
			c.emit(note("no close frame. Each mirror's socket simply ends, which is a VERDICT —"))
			c.emit(note("they print the close code in red and exit 1. Silence would not have been:"))
			c.emit(note("an idle pane legitimately produces no frames, which is why every client"))
			c.emit(note("also runs a liveness probe rather than trusting a quiet stream."))
			c.emit(note("SIGTERM would have produced the other path — results → shutdown → EOF,"))
			c.emit(note("green, exit 0. remain-on-exit keeps both corpses on screen."))
			*c.Over = true
			return nil
		},
	},

	// ── the wire ─────────────────────────────────────────────────────────────
	{
		Key: "13", Group: "THE WIRE",
		What: "print the next raw frame",
		How:  "a viewer's own connection; the JSON both mirrors decode",
		Cred: CredView,
		Run: func(c *Ctx) error {
			c.emit(req(fmt.Sprintf("a third viewer on /v1/ws, watching pane %d", c.Target), CredView))
			got, err := RawFrames(c.cfg, c.Target, 2, 4*time.Second)
			if err != nil {
				c.emit(verdict(false, "NO STREAM", err.Error()))
				return nil
			}
			if len(got) == 0 {
				c.emit(verdict(false, "IDLE", "no frame arrived within 4s — the pane is idle, which is legitimate"))
				return nil
			}
			for i, raw := range got {
				var f protocol.Frame
				if err := json.Unmarshal([]byte(raw), &f); err != nil {
					continue
				}
				kind := "delta"
				if f.Key {
					kind = "KEYFRAME"
				}
				c.emit(heading(fmt.Sprintf(
					"frame %d  %s · pane %d · seq %d · %dx%d · %d rows · cursor %d,%d · %d bytes",
					i+1, kind, f.Pane, f.Seq, f.Cols, f.Rows, len(f.Lines), f.Cur.Y, f.Cur.X, len(raw))))
				c.emit(note(Brief(raw, 520)))
			}
			c.emit(note("A keyframe is every row and means `clear, then draw`. A delta replaces"))
			c.emit(note("whole rows: each `lines` entry is {y, t, r} — the row index, its text,"))
			c.emit(note("and its style runs — and each run is [col, len, fg, bg, attr]."))
			c.emit(note("The first frame a watcher gets is ALWAYS a keyframe, and so is the first"))
			c.emit(note("after a resize or an alt-screen switch, because no client can apply a row"))
			c.emit(note("delta across a geometry change."))
			c.emit(look("that is byte for byte what the mirror pane and the browser tab are decoding"))
			return nil
		},
	},
}

// ActionByKey finds one, or nil.
func ActionByKey(k string) *Action {
	for i := range Actions {
		if Actions[i].Key == k {
			return &Actions[i]
		}
	}
	return nil
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// qualified is the plugin's own ops: the ones carrying a dot.
func qualified(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.Contains(n, ".") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func lastTicket(r Result) map[string]any {
	res, _ := r.Body["result"].(map[string]any)
	if res == nil {
		return nil
	}
	list, _ := res["tickets"].([]any)
	if len(list) == 0 {
		return nil
	}
	t, _ := list[len(list)-1].(map[string]any)
	return t
}
