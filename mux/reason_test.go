package mux

import (
	"bufio"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// TestReplyCarriesReasonAndTyped: on the socket, an error's reason and typed
// flag ride beside its code — on a verb's own reply and on a `call` — and are
// absent when the error has neither, so every reply that predates them is
// byte-identical.
func TestReplyCarriesReasonAndTyped(t *testing.T) {
	t.Run("a verb reply", func(t *testing.T) {
		m := &Magmux{}
		var got map[string]any
		err := &sockErr{Code: protocol.CodeNotAccepted, Msg: "lost", Reason: protocol.ReasonUnconfirmed, Typed: true}
		if e := json.Unmarshal(m.replyBytes(json.RawMessage(`1`), nil, err), &got); e != nil {
			t.Fatal(e)
		}
		if got["code"] != protocol.CodeNotAccepted || got["reason"] != protocol.ReasonUnconfirmed || got["typed"] != true {
			t.Errorf("reply = %v, want code, reason and typed", got)
		}

		got = nil
		if e := json.Unmarshal(m.replyBytes(json.RawMessage(`2`), nil, sockErrf(sockCodePaneDead, "gone")), &got); e != nil {
			t.Fatal(e)
		}
		for _, k := range []string{"reason", "typed"} {
			if _, ok := got[k]; ok {
				t.Errorf("a reply with no %s carries the key: %v", k, got)
			}
		}
	})

	t.Run("a call over the real socket", func(t *testing.T) {
		m, _, dial := sockMux(t)
		if err := m.bus().Register("reasontest", hub.Op{
			Spec: protocol.OpSpec{Name: "reasoned", Class: protocol.ClassRead},
			Fn: func(context.Context, hub.Caller, json.RawMessage) (map[string]any, error) {
				return nil, &protocol.Error{Code: protocol.CodeBusy, Msg: "held",
					Reason: protocol.ReasonTypingInProgress, Typed: true}
			},
		}); err != nil {
			t.Fatal(err)
		}
		conn := dial()
		writeLines(t, conn, map[string]any{"type": "call", "op": "reasoned", "id": "r1"})
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) != nil || ev["type"] != "reply" || ev["id"] != "r1" {
				continue
			}
			if ev["code"] != protocol.CodeBusy || ev["reason"] != protocol.ReasonTypingInProgress || ev["typed"] != true {
				t.Errorf("call reply = %v, want code, reason and typed", ev)
			}
			return
		}
		t.Fatalf("no reply to the call: %v", sc.Err())
	})
}
