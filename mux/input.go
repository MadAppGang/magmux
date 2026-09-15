package mux

// `input`: the human-equivalent path into a pane.
//
// magmux already had two ways to put bytes in a PTY and they answer different
// questions. `writePTY` is the local keyboard: it has nobody to report to, so
// it reports nothing. `send` is a CONTROLLER's instruction: it is paced like
// typing, it writes an OUT row in the control panel, and it tells the pane's
// controller that a new turn is starting. Both are right for what they are.
//
// Neither is right for a person at a remote keyboard. A browser sending one
// keystroke must not write a panel row per key — the panel is the ledger of a
// controller driving a session, and filling it with somebody's arrow keys
// destroys the provenance it exists for. But it also cannot be silent about
// failure, because the person typing needs to know the pane is gone.
//
// So `input` is exactly one thing: WRITE THESE BYTES, AND TELL ME WHAT
// HAPPENED. It refuses what writePTY refuses, clears the same completion state
// writePTY clears, and returns the real byte count and the real error. There is
// no OUT row, and NotifyInput fires only on a submit — a keystroke that does not
// press Enter has not started a turn, and claiming one would make the panel's
// `done` counter answer for something no controller asked for.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// inputArgs is `input` on the wire.
//
// Pane is `any` for the same reason every other verb's is: the wire has always
// accepted an index or a string, and one shape for one field across every verb
// is worth more than a stricter type here.
type inputArgs struct {
	Pane  any      `json:"pane"`
	Text  string   `json:"text"`
	Keys  []string `json:"keys"`
	Paste bool     `json:"paste"`
}

// writeInput writes bytes to the pane's PTY on behalf of a remote keyboard and
// reports exactly what happened.
//
// It is injectPTY's twin with an honest return: the byte count and the error,
// because the caller is a request that has to be answered. The refusal is the
// same one — a pane with no PTY or a dead child — and so is clearCompletionLocked,
// which is what stops the ✓ DONE chrome outliving the keystroke that un-stuck
// the pane.
//
// The write happens with p.mu RELEASED, like writePTY's and injectPTY's: a child
// that has stopped reading can block a PTY write for as long as it likes, and
// treeMu -> p.mu means a stalled keystroke would stall the next frame behind it.
//
// Caller must NOT hold p.mu.
func (p *Pane) writeInput(data []byte) (int, error) {
	p.mu.Lock()
	if p.ptmx == nil || p.dead {
		p.mu.Unlock()
		return 0, sockErrf(sockCodePaneDead,
			"pane %d cannot take input: its child has exited or its PTY is closed", p.id)
	}
	p.clearCompletionLocked()
	p.dirty = true
	ptmx := p.ptmx
	p.mu.Unlock()

	n, err := ptmx.Write(data)
	if err != nil {
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[input] pane %d write error after %d bytes: %v\n", p.id, n, err)
		}
		// The pane died between the check above and the write. EIO is what a
		// closed slave side gives on Linux and darwin alike; os.ErrClosed is
		// magmux closing the master from reapPane. Both mean the same thing to
		// the person typing, and it is not `internal`.
		if isPaneGone(err) {
			return n, sockErrf(sockCodePaneDead,
				"pane %d stopped accepting input mid-write (%d bytes written): %v", p.id, n, err)
		}
		return n, sockErrf(sockCodeInternal,
			"pane %d: writing to the PTY failed after %d bytes: %v", p.id, n, err)
	}
	return n, nil
}

// isPaneGone reports whether a write error means the pane went away rather than
// something magmux got wrong. Kept as its own predicate because the two deserve
// different codes and only one of them is worth a bug report.
//
// Three errors, because there are three ways the far end disappears: EIO is a
// PTY whose slave side has closed (darwin and Linux alike), EPIPE is the same
// event on a plain pipe, and os.ErrClosed is magmux closing the master itself
// from reapPane. To the person typing they are one fact.
func isPaneGone(err error) bool {
	return errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EPIPE)
}

// inputBytes assembles the bytes one `input` request means.
//
// Everything is resolved BEFORE anything is written: an unknown key name is the
// caller's mistake, and typing the first half of a request and then refusing it
// would leave a session holding a fragment of a command nobody sent.
//
// It also reports whether the result SUBMITS — a CR or LF outside a paste
// wrapper — which is the one thing that makes this a new turn rather than a
// keystroke.
func (p *Pane) inputBytes(a inputArgs) (data []byte, submits bool, err error) {
	var buf []byte
	if a.Text != "" {
		if a.Paste {
			// A paste is one unit: the wrapper is what stops the receiving TUI
			// treating the first newline as "run this". So a multi-line paste
			// submits nothing, which is the whole reason a client asks for one.
			buf = append(buf, p.pasteWrap(a.Text)...)
		} else {
			buf = append(buf, a.Text...)
			submits = submits || strings.ContainsAny(a.Text, "\r\n")
		}
	}
	for _, k := range a.Keys {
		b, ok := keyBytes(k)
		if !ok {
			return nil, false, sockErrf(sockCodeBadRequest,
				"unknown key %q; use a named key (enter, tab, up, ctrl-c, …) or a single character", k)
		}
		buf = append(buf, b...)
		submits = submits || strings.ContainsAny(string(b), "\r\n")
	}
	if len(buf) == 0 {
		return nil, false, sockErrf(sockCodeBadRequest, "input needs text or keys")
	}
	return buf, submits, nil
}

// sockInput runs one `input`.
//
// It deliberately does NOT go through the sockMsg verb table. `input` is not a
// socket verb — the wire reaches it with `call`, on every transport that can
// carry a human UI — and routing it through dispatchSocketVerb would put it on
// the legacy fire-and-forget path as well, where a write that failed is silent.
// The whole point of this op is that it answers.
func (m *Magmux) sockInput(c hub.Caller, args json.RawMessage) (map[string]any, error) {
	var a inputArgs
	if trimmed := strings.TrimSpace(string(args)); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, sockErrf(sockCodeBadRequest, "input: args must be a JSON object (%v)", err)
		}
	}
	idx := m.parsePaneIndex(a.Pane)
	switch {
	case idx == paneAll:
		return nil, sockErrf(sockCodeBadRequest, `input has no fan-out: "*" is not a target`)
	case idx == paneUnspecified:
		return nil, sockErrf(sockCodeBadRequest, "input needs a pane index")
	case idx < 0:
		return nil, sockErrf(sockCodeBadRequest, "pane is not an index")
	}
	// Resolved through the identity table: after a close_pane the ids are
	// sparse, and a bounds check alone would type into a tombstone.
	p := m.paneByID(idx)
	if p == nil {
		return nil, sockErrf(sockCodeNoSuchPane, "no pane %d (it may have been closed)", idx)
	}
	if p.isControl {
		return nil, sockErrf(sockCodePaneIsControl,
			"pane %d is the control panel; it is magmux's own display and has no session to type into", idx)
	}

	data, submits, err := p.inputBytes(a)
	if err != nil {
		return nil, err
	}
	n, err := p.writeInput(data)
	if err != nil {
		// The count goes out with the failure: a caller that wrote 4 of 9 bytes
		// into a dying pane needs to know the session saw a fragment.
		return map[string]any{"pane": idx, "bytes": n}, err
	}
	if submits {
		// Same reason `send` does it on Enter: a controller whose transcript is
		// missing or lagging stays settled on the previous turn, and a client
		// waiting for this one to begin would wait forever. It is an OBSERVATION
		// input, not provenance — which is why there is no panel row.
		if notifier, ok := p.controller.(InputNotifier); ok && p.controller != nil {
			notifier.NotifyInput()
		}
	}
	return map[string]any{"pane": idx, "bytes": n}, nil
}

// inputOp is `input`'s registration. It is its own op rather than one built by
// m.op() because it is the one built-in that is NOT a wrapper around a socket
// verb: there is no `{"type":"input"}` to wrap.
func (m *Magmux) inputOp() hub.Op {
	return hub.Op{
		Spec: protocol.OpSpec{
			Name:  "input",
			Class: protocol.ClassInput,
			Description: "Type into a pane as a person would: raw bytes to the PTY, no pacing and no " +
				"control-panel row. Answers with the number of bytes actually written. Controllers " +
				"should use `send` instead — it is recorded, paced and tells the pane's tool that a " +
				"new turn has started.",
			Schema: objectSchema(map[string]any{
				"pane": paneProp,
				"text": strProp("Characters to type. Sent verbatim unless paste is set."),
				"keys": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Named keys to press after the text, e.g. [\"enter\"], [\"ctrl-c\"]."},
				"paste": boolProp("Wrap the text as a bracketed paste, so a multi-line block is not submitted line by line."),
			}, "pane"),
			Source: opSource,
		},
		Fn: func(_ context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
			return m.sockInput(c, args)
		},
	}
}
