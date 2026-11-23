package protocol

import (
	"errors"
	"fmt"
	"testing"
)

// TestErrrCarriesCodeReasonAndMessage: Errr is Errf with a reason, and the
// reason is a machine-readable sub-code that rides BESIDE the code rather than
// inside the message, where a caller would have to parse prose to find it.
func TestErrrCarriesCodeReasonAndMessage(t *testing.T) {
	err := Errr(CodeBusy, ReasonVimMode, "pane %d is in vim mode", 2)
	if err.Code != CodeBusy || err.Reason != ReasonVimMode || err.Msg != "pane 2 is in vim mode" {
		t.Fatalf("Errr = %+v", err)
	}
	if err.Typed {
		t.Error("Errr set Typed; only the caller that wrote bytes may say so")
	}
	if err.Error() != "pane 2 is in vim mode" {
		t.Errorf("Error() = %q; the message is unchanged by a reason", err.Error())
	}
}

// TestReasonOfAndTypedOfWorkThroughWrapping: both read through errors.As, the
// same way CodeOf does, so wrapping an error with context cannot strip the
// fields a caller branches on.
func TestReasonOfAndTypedOfWorkThroughWrapping(t *testing.T) {
	typed := &Error{Code: CodeNotAccepted, Msg: "lost", Reason: ReasonUnconfirmed, Typed: true}
	wrapped := fmt.Errorf("send to pane 3: %w", typed)

	cases := []struct {
		name   string
		err    error
		code   string
		reason string
		typed  bool
	}{
		{"nil", nil, "", "", false},
		{"a plain error", errors.New("boom"), CodeInternal, "", false},
		{"an Errf has no reason", Errf(CodeBusy, "x"), CodeBusy, "", false},
		{"an Errr", Errr(CodeBusy, ReasonQueueFull, "x"), CodeBusy, ReasonQueueFull, false},
		{"direct", typed, CodeNotAccepted, ReasonUnconfirmed, true},
		{"wrapped", wrapped, CodeNotAccepted, ReasonUnconfirmed, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CodeOf(c.err); got != c.code {
				t.Errorf("CodeOf = %q, want %q", got, c.code)
			}
			if got := ReasonOf(c.err); got != c.reason {
				t.Errorf("ReasonOf = %q, want %q", got, c.reason)
			}
			if got := TypedOf(c.err); got != c.typed {
				t.Errorf("TypedOf = %v, want %v", got, c.typed)
			}
		})
	}
}

// TestNewCodesAndReasonsAreTheWireSpelling pins the strings, because they are
// PUBLIC: a client written against them branches on these exact bytes.
func TestNewCodesAndReasonsAreTheWireSpelling(t *testing.T) {
	want := map[string]string{
		CodeNotAccepted:        "not_accepted",
		CodeObsOff:             "obs_off",
		CodeObsPending:         "obs_pending",
		CodePanelPrivate:       "panel_private",
		ReasonStarting:         "starting",
		ReasonTerminalNotReady: "terminal_not_ready",
		ReasonNoInputBox:       "no_input_box",
		ReasonBoxNotEmpty:      "box_not_empty",
		ReasonVimMode:          "vim_mode",
		ReasonDialogPrefix:     "dialog:",
		ReasonTypingInProgress: "typing_in_progress",
		ReasonQueueFull:        "queue_full",
		ReasonInFlight:         "in_flight",
		ReasonTextMismatch:     "text_mismatch",
		ReasonEmptyBox:         "empty_box",
		ReasonStillInBox:       "still_in_box",
		ReasonUnconfirmed:      "unconfirmed",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("constant = %q, want %q", got, w)
		}
	}
}

// TestTypingOfWorksThroughWrappingAndAddsItsKeys: TypingOf reads a gated
// send's typing through wrapping, states nothing for an error without it, and
// AddTo writes typingStopped only when typing stopped short.
func TestTypingOfWorksThroughWrappingAndAddsItsKeys(t *testing.T) {
	e := Errr(CodeNotAccepted, ReasonTextMismatch, "stopped")
	e.Typing = &Typing{Writes: 2, WritesTotal: 4, Stopped: TypingStoppedDeadline}
	got, ok := TypingOf(fmt.Errorf("wrapped: %w", e))
	if !ok || got != *e.Typing {
		t.Fatalf("TypingOf(wrapped) = %+v, %v; want %+v", got, ok, *e.Typing)
	}
	if _, ok := TypingOf(Errf(CodePaneDead, "x")); ok {
		t.Error("an error with no typing states some")
	}
	if _, ok := TypingOf(errors.New("plain")); ok {
		t.Error("a plain error states typing")
	}
	m := map[string]any{}
	got.AddTo(m)
	if m["writes"] != 2 || m["writesTotal"] != 4 || m["typingStopped"] != TypingStoppedDeadline {
		t.Errorf("AddTo = %v", m)
	}
	m = map[string]any{}
	Typing{Writes: 3, WritesTotal: 3}.AddTo(m)
	if _, ok := m["typingStopped"]; ok || m["writes"] != 3 {
		t.Errorf("AddTo of a complete typing = %v, want writes and no typingStopped", m)
	}
}
