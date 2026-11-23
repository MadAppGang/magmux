package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/ws"
)

// TestReplyCarriesBoxEmpty: a text_mismatch's boxEmpty reaches the caller on
// HTTP, on the WebSocket and in an SSE error event, true and false alike, and
// is ABSENT when the error does not state it.
func TestReplyCarriesBoxEmpty(t *testing.T) {
	f := newFixture(t)

	t.Run("http", func(t *testing.T) {
		for _, empty := range []bool{true, false} {
			body, _ := json.Marshal(map[string]any{
				"code": protocol.CodeNotAccepted, "reason": protocol.ReasonTextMismatch, "typed": true, "boxEmpty": empty,
				"writes": 2, "writesTotal": 4, "typingStopped": protocol.TypingStoppedBoxChanged})
			got := decode(t, f.do(t, "POST", "/v1/ops/boom", f.full, bytes.NewReader(body)))
			if got["boxEmpty"] != empty || got["reason"] != protocol.ReasonTextMismatch {
				t.Errorf("body = %v, want boxEmpty %v beside reason text_mismatch", got, empty)
			}
			if got["writes"] != 2.0 || got["writesTotal"] != 4.0 || got["typingStopped"] != protocol.TypingStoppedBoxChanged {
				t.Errorf("body = %v, want writes 2 of 4, typingStopped box_changed", got)
			}
		}
		body, _ := json.Marshal(map[string]any{"code": protocol.CodeNotAccepted, "reason": protocol.ReasonUnconfirmed, "typed": true,
			"writes": 3, "writesTotal": 3})
		got := decode(t, f.do(t, "POST", "/v1/ops/boom", f.full, bytes.NewReader(body)))
		if _, ok := got["boxEmpty"]; ok {
			t.Errorf("an error that does not state boxEmpty carries the key: %v", got)
		}
		if _, ok := got["typingStopped"]; ok || got["writes"] != 3.0 || got["writesTotal"] != 3.0 {
			t.Errorf("body = %v, want writes 3 of 3 and no typingStopped", got)
		}
		body, _ = json.Marshal(map[string]any{"code": protocol.CodePaneDead})
		got = decode(t, f.do(t, "POST", "/v1/ops/boom", f.full, bytes.NewReader(body)))
		for _, k := range []string{"writes", "writesTotal", "typingStopped"} {
			if _, ok := got[k]; ok {
				t.Errorf("an error that states no typing carries %s: %v", k, got)
			}
		}
	})

	t.Run("ws", func(t *testing.T) {
		h := http.Header{}
		h.Set("Sec-WebSocket-Protocol", "magmux.v1, magmux.auth."+f.full)
		cc, _ := dialWS(t, f, h, "")
		if cc == nil {
			t.Fatal("handshake failed")
		}
		next := func() map[string]any {
			t.Helper()
			for {
				_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
				op, msg, err := cc.Read()
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				if op != ws.OpText {
					continue
				}
				var m map[string]any
				if err := json.Unmarshal(msg, &m); err != nil {
					t.Fatalf("frame %q: %v", msg, err)
				}
				if m["type"] == "reply" {
					return m
				}
			}
		}
		if err := cc.WriteText(`{"id":1,"op":"boom","args":{"code":"not_accepted","reason":"text_mismatch","typed":true,"boxEmpty":true,"writes":1,"writesTotal":3,"typingStopped":"deadline"}}`); err != nil {
			t.Fatal(err)
		}
		if r := next(); r["boxEmpty"] != true || r["writes"] != 1.0 || r["writesTotal"] != 3.0 || r["typingStopped"] != "deadline" {
			t.Errorf("ws reply = %v, want boxEmpty:true and writes 1 of 3, typingStopped deadline", r)
		}
		if err := cc.WriteText(`{"id":2,"op":"boom","args":{"code":"pane_dead"}}`); err != nil {
			t.Fatal(err)
		}
		if r := next(); r["boxEmpty"] != nil || r["writes"] != nil || r["typingStopped"] != nil {
			t.Errorf("a ws reply with no boxEmpty or typing carries the keys: %v", r)
		}
	})

	t.Run("sse error event", func(t *testing.T) {
		empty := false
		e := protocol.Errr(protocol.CodeNotAccepted, protocol.ReasonTextMismatch, "partial")
		e.BoxEmpty = &empty
		e.Typing = &protocol.Typing{Writes: 2, WritesTotal: 5, Stopped: protocol.TypingStoppedTerminalReset}
		var m map[string]any
		if err := json.Unmarshal(errorEvent(3, e), &m); err != nil {
			t.Fatal(err)
		}
		if v, ok := m["boxEmpty"]; !ok || v != false {
			t.Errorf("error event = %v, want boxEmpty:false", m)
		}
		if m["writes"] != 2.0 || m["writesTotal"] != 5.0 || m["typingStopped"] != protocol.TypingStoppedTerminalReset {
			t.Errorf("error event = %v, want writes 2 of 5, typingStopped terminal_reset", m)
		}
	})
}
