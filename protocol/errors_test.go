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
