package main

// The two shapes that are not a TUI: `--run N`, and the line-driven menu a pipe
// gets. Both render the SAME Line stream the TUI collects, so the evidence an
// automated check reads is the evidence a human sees.

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Raw ANSI rather than Lip Gloss, deliberately: this renderer's job is to be
// byte-predictable for a test that greps it, and a styling library that decides
// for itself whether the destination has colour is one more thing between the
// two. Empty strings when there is no terminal, so a captured transcript is
// plain text.
type ansi struct{ reset, bold, grey, red, green, yellow, cyan, violet string }

func newANSI(colour bool) ansi {
	if !colour {
		return ansi{}
	}
	return ansi{
		reset: "\x1b[0m", bold: "\x1b[1m", grey: "\x1b[90m", red: "\x1b[31m",
		green: "\x1b[32m", yellow: "\x1b[33m", cyan: "\x1b[36m", violet: "\x1b[35m",
	}
}

// PlainSink prints evidence as it happens.
type PlainSink struct {
	c ansi
	w *bufio.Writer
	// Failed records whether any verdict said no. It is what `--run N` exits
	// with, so a check can assert on the process rather than on its output.
	Failed bool
}

func NewPlainSink() *PlainSink {
	return &PlainSink{
		c: newANSI(os.Getenv("NO_COLOR") == "" && isStdoutTTY()),
		w: bufio.NewWriter(os.Stdout),
	}
}

func isStdoutTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (p *PlainSink) Flush() { _ = p.w.Flush() }

func (p *PlainSink) out(format string, a ...any) {
	fmt.Fprintf(p.w, format+"\n", a...)
	_ = p.w.Flush()
}

func (p *PlainSink) Emit(l Line) {
	c := p.c
	switch l.Role {
	case RoleRequest:
		tag := c.violet + "session token" + c.reset
		if l.Cred == CredView {
			tag = c.yellow + "VIEW token" + c.reset
		}
		p.out("  %s▶%s %s  %s· as the %s", c.bold, c.reset, l.Text, c.grey, tag)
	case RoleResponse:
		colour := c.green
		if l.Status/100 != 2 {
			colour = c.red
		}
		if l.Status == 0 {
			p.out("  %s◀%s %s %s(%s)%s", c.red, c.reset, l.Text, c.grey, FmtDur(l.Dur), c.reset)
			return
		}
		p.out("  %s◀%s HTTP %d  %s  %s%s%s", colour, c.reset, l.Status, l.Text, c.grey, FmtDur(l.Dur), c.reset)
	case RoleLook:
		p.out("  %s↳%s %s", c.cyan, c.reset, l.Text)
	case RoleHeading:
		p.out("  %s%s%s", c.bold, l.Text, c.reset)
	case RoleVerdict:
		if l.OK {
			p.out("  %s✓ %s%s %s", c.green, l.Badge, c.reset, l.Text)
		} else {
			p.Failed = true
			p.out("  %s✗ %s%s %s", c.red, l.Badge, c.reset, l.Text)
		}
	default:
		p.out("    %s%s%s", c.grey, l.Text, c.reset)
	}
}

// ── --run N ─────────────────────────────────────────────────────────────────

// runOne performs one action and exits with a meaningful code:
//
//	0  the action ran and every verdict it made was the one it set out to prove
//	1  a verdict failed, or the action could not run at all
//	2  there is no such action
//
// This is what demo/rc/selftest.ts drives, because a TUI cannot be driven by
// piping stdin.
func runOne(cfg Config, args Args) int {
	sink := NewPlainSink()
	defer sink.Flush()
	c := sink.c

	act := ActionByKey(strings.TrimSpace(args.Run))
	if act == nil {
		fmt.Fprintf(os.Stderr, "drive: no action %q — the actions are 1 to %d\n", args.Run, len(Actions))
		return 2
	}
	api := NewAPI(cfg, sink)

	target := args.Pane
	if target < 0 {
		list, err := api.Panes()
		if err != nil {
			sink.out("  %s✗ magmux did not answer: %v%s", c.red, err, c.reset)
			return 1
		}
		target = Watched(list)
		if target < 0 && act.Key != "12" {
			sink.out("  %s✗ magmux reports no session pane%s", c.red, c.reset)
			return 1
		}
	}

	if act.Confirm != "" && !args.Yes {
		sink.out("  %s%s needs --yes: it types %q and %s%s", c.yellow, act.Key, act.Confirm, act.How, c.reset)
		return 1
	}

	opened, over := -1, false
	var stalled *StalledWS
	ctx := &Ctx{
		cfg: cfg, api: api, sink: sink, Target: target,
		Opened: &opened, Stalled: &stalled, Over: &over, Confirmed: args.Yes,
	}
	sink.out("")
	sink.out("  %s%s · %s%s  %s%s%s", c.bold, act.Key, act.What, c.reset, c.grey, act.How, c.reset)
	sink.out("")
	if err := act.Run(ctx); err != nil {
		sink.out("  %s✗ action %s failed: %v%s", c.red, act.Key, err, c.reset)
		return 1
	}
	if stalled != nil {
		stalled.Close()
	}
	if sink.Failed {
		return 1
	}
	return 0
}

// ── the line menu, for a pipe ───────────────────────────────────────────────

// runMenu is the shape a pipe gets: the same actions, chosen by typing a
// number, with the banner and the menu as lines rather than as panels. It
// exists because `demo.sh` in adopt mode may be spawned with pipes for stdio
// and because a TUI on a pipe writes escape sequences into somebody's
// transcript.
func runMenu(cfg Config, args Args) int {
	sink := NewPlainSink()
	defer sink.Flush()
	c := sink.c
	api := NewAPI(cfg, sink)

	fp := Fingerprint
	sink.out("")
	sink.out("  %smagmux remote control · THE DRIVER%s   %s%s%s", c.bold, c.reset, c.grey, cfg.URL, c.reset)
	sink.out("  %s▸ this driver: FULL SESSION TOKEN%s %s%s%s   %s▸ every mirror: READ-ONLY VIEW TOKEN%s %s%s%s",
		c.violet, c.reset, c.grey, fp(cfg.Token), c.reset,
		c.yellow, c.reset, c.grey, fp(cfg.ViewToken), c.reset)
	sink.out("    %sread from %s/ — actions 8 and 9 make magmux refuse the read-only one, in its own words%s",
		c.grey, cfg.State, c.reset)

	// Prove the credential before printing a menu of things that need it. A
	// menu whose every action fails identically tells nobody which of the two
	// tokens, the URL or the process is the problem.
	caps, err := api.Capabilities()
	if err != nil {
		fmt.Fprintf(os.Stderr, "drive: magmux refused the session token: %v\n", err)
		return 1
	}
	if caps.ReadOnly {
		fmt.Fprintln(os.Stderr, "drive: the token at magmux-*.token reports readOnly — this driver needs the session token")
		return 1
	}
	sink.out("  %scapabilities ok · protocol %d · readOnly=false · %d verbs · transports %s%s",
		c.grey, caps.Protocol, len(caps.Verbs), strings.Join(caps.TransportNames(), ", "), c.reset)

	opened, over := -1, false
	var stalled *StalledWS

	printMenu := func(target int) {
		sink.out("")
		sink.out("  %s%s%s", c.bold, strings.Repeat("─", 72), c.reset)
		group := ""
		for _, a := range Actions {
			if a.Group != "" && a.Group != group {
				group = a.Group
				sink.out("  %s%s%s", c.grey, group, c.reset)
			}
			sink.out("   %s%2s%s  %-33s %s%s%s", c.bold, a.Key, c.reset, a.What, c.grey, a.How, c.reset)
		}
		sink.out("    %sm%s  %-33s", c.bold, c.reset, "this menu")
		sink.out("    %sq%s  %-33s %sthe demo keeps running; %s to end it%s",
			c.bold, c.reset, "quit the driver", c.grey, cfg.QuitHint, c.reset)
		t := "?"
		if target >= 0 {
			t = fmt.Sprint(target)
		}
		sink.out("  %swatching pane %s · %s%s", c.grey, t, cfg.Mirrors(), c.reset)
	}

	list, _ := api.Panes()
	printMenu(Watched(list))

	in := bufio.NewScanner(os.Stdin)
	for !over {
		fmt.Fprintf(os.Stdout, "\n  %schoose%s %s1-13, m, q%s ▸ ", c.bold, c.reset, c.grey, c.reset)
		if !in.Scan() {
			sink.out("")
			sink.out("  %sstdin closed; the driver is done. The demo is still running.%s", c.grey, c.reset)
			break
		}
		choice := strings.ToLower(strings.TrimSpace(in.Text()))
		switch choice {
		case "":
			continue
		case "q", "quit", "exit":
			over = true
			continue
		case "m", "menu", "h", "help", "?":
			list, _ := api.Panes()
			printMenu(Watched(list))
			continue
		}
		act := ActionByKey(choice)
		if act == nil {
			sink.out("  %sno action %q — 1 to 13, m for the menu, q to quit%s", c.yellow, choice, c.reset)
			continue
		}
		// Re-read the pane list before every action: a pane may have exited,
		// and typing into a tombstone is a 404 that reads like a broken demo.
		list, _ := api.Panes()
		target := Watched(list)
		if args.Pane >= 0 {
			target = args.Pane
		}
		if target < 0 && act.Key != "12" {
			sink.out("  %smagmux reports no session pane%s — is it still running?", c.red, c.reset)
			continue
		}
		if opened >= 0 {
			still := false
			for _, p := range list {
				if p.ID == opened {
					still = true
				}
			}
			if !still {
				opened = -1
			}
		}
		confirmed := args.Yes
		if act.Confirm != "" && !confirmed {
			fmt.Fprintf(os.Stdout, "  type %s%s%s to confirm, anything else to cancel ▸ ", c.bold, act.Confirm, c.reset)
			if in.Scan() {
				confirmed = strings.TrimSpace(in.Text()) == act.Confirm
			}
		}
		ctx := &Ctx{
			cfg: cfg, api: api, sink: sink, Target: target,
			Opened: &opened, Stalled: &stalled, Over: &over, Confirmed: confirmed,
		}
		sink.out("")
		sink.out("  %s%s · %s%s  %s%s%s", c.bold, act.Key, act.What, c.reset, c.grey, act.How, c.reset)
		sink.out("")
		if err := act.Run(ctx); err != nil {
			sink.out("  %saction %s failed: %v%s", c.red, act.Key, err, c.reset)
		}
	}
	if stalled != nil {
		stalled.Close()
	}
	if over {
		sink.out("")
		sink.out("  %sdriver out. The demo is still running — %s to end it.%s", c.grey, cfg.QuitHint, c.reset)
	}
	return 0
}
