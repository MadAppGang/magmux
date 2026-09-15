package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/MadAppGang/magmux/client"
)

// describeTurn turns a client.TurnResult into the text the driving model reads. A
// port of pilot/pilot.ts:445-485, including the awaiting-input-with-no-response
// paragraph, which is load-bearing: saying "(no response)" reads as "nothing
// happened", and a driver then burns its budget on sanity checks — observed
// costing half a run.
//
// screen is the last few rendered lines, appended when a turn settles with no
// response text. The pilot could only *explain* the emptiness; with capture we
// can show what actually happened.
func describeTurn(r client.TurnResult, screen string) string {
	secs := fmt.Sprintf("%.0f", r.Duration.Seconds())
	var b strings.Builder
	switch r.State {
	case "awaiting_input":
		if r.Response == "" {
			fmt.Fprintf(&b, "The session finished the turn in %ss and is waiting for the next "+
				"instruction.\n\nIt produced no text summary — normal when a turn is just tool "+
				"calls", secs)
			if r.Tool != "" {
				fmt.Fprintf(&b, " (last tool: %s)", r.Tool)
			}
			b.WriteString(". This does NOT mean the instruction failed, and the session is " +
				"working normally. If you need to know the outcome, make it part of the next " +
				"instruction — ask the session to state the result in its reply, in words.")
			appendScreen(&b, screen)
			return b.String()
		}
		fmt.Fprintf(&b, "The session finished the turn in %ss and is waiting for the next "+
			"instruction.\n\nIt reported:\n%s", secs, r.Response)
		if r.Tool != "" {
			fmt.Fprintf(&b, "\n\nLast tool used: %s", r.Tool)
		}
		return b.String()
	case "awaiting_permission":
		fmt.Fprintf(&b, "After %ss the session is BLOCKED on a permission prompt and cannot "+
			"continue on its own. Last output:\n%s", secs, orNone(r.Response))
		appendScreen(&b, screen)
		b.WriteString("\n\nAnswer it with send_keys (for example keys:[\"1\"] or keys:[\"enter\"]) " +
			"after reading the prompt.")
		return b.String()
	case "error":
		fmt.Fprintf(&b, "The session reported an error after %ss:\n%s", secs, orDetail(r.Response))
		appendScreen(&b, screen)
		return b.String()
	case "gone":
		fmt.Fprintf(&b, "The session process exited after %ss. No further instructions can be "+
			"sent to this pane.", secs)
		appendScreen(&b, screen)
		return b.String()
	default:
		fmt.Fprintf(&b, "The instruction did not produce a turn within %ss — the session never "+
			"started working. It may not have received the instruction. Do not assume the step "+
			"was done.", secs)
		appendScreen(&b, screen)
		b.WriteString("\n\nRead the pane before retrying: the session may be at a prompt, " +
			"mid-render, or waiting on something else.")
		return b.String()
	}
}

func appendScreen(b *strings.Builder, screen string) {
	if strings.TrimSpace(screen) == "" {
		return
	}
	b.WriteString("\n\nWhat the pane shows right now:\n```\n")
	b.WriteString(strings.TrimRight(screen, "\n"))
	b.WriteString("\n```")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func orDetail(s string) string {
	if s == "" {
		return "(no detail)"
	}
	return s
}

// ── JSON helpers ────────────────────────────────────────────────────────────
//
// magmux's events are decoded into map[string]any because `pane` is `int or
// "*"` on the wire and the optional fields are omitted rather than zeroed;
// these keep the type assertions in one place.

func evStr(m map[string]any, k string) (string, bool) {
	v, ok := m[k]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func evInt(m map[string]any, k string) (int, bool) {
	v, ok := m[k]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	}
	return 0, false
}

func evBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}
