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
				"code": protocol.CodeNotAccepted, "reason": protocol.ReasonTextMismatch, "typed": true, "boxEmpty": empty})
			got := decode(t, f.do(t, "POST", "/v1/ops/boom", f.full, bytes.NewReader(body)))
			if got["boxEmpty"] != empty || got["reason"] != protocol.ReasonTextMismatch {
				t.Errorf("body = %v, want boxEmpty %v beside reason text_mismatch", got, empty)
			}
		}
		body, _ := json.Marshal(map[string]any{"code": protocol.CodeNotAccepted, "reason": protocol.ReasonUnconfirmed, "typed": true})
		got := decode(t, f.do(t, "POST", "/v1/ops/boom", f.full, bytes.NewReader(body)))
		if _, ok := got["boxEmpty"]; ok {
			t.Errorf("an error that does not state boxEmpty carries the key: %v", got)
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
		if err := cc.WriteText(`{"id":1,"op":"boom","args":{"code":"not_accepted","reason":"text_mismatch","typed":true,"boxEmpty":true}}`); err != nil {
			t.Fatal(err)
		}
		if r := next(); r["boxEmpty"] != true {
			t.Errorf("ws reply = %v, want boxEmpty:true", r)
		}
		if err := cc.WriteText(`{"id":2,"op":"boom","args":{"code":"pane_dead"}}`); err != nil {
			t.Fatal(err)
		}
		if r := next(); r["boxEmpty"] != nil {
			t.Errorf("a ws reply with no boxEmpty carries the key: %v", r)
		}
	})

	t.Run("sse error event", func(t *testing.T) {
		empty := false
		e := protocol.Errr(protocol.CodeNotAccepted, protocol.ReasonTextMismatch, "partial")
		e.BoxEmpty = &empty
		var m map[string]any
		if err := json.Unmarshal(errorEvent(3, e), &m); err != nil {
			t.Fatal(err)
		}
		if v, ok := m["boxEmpty"]; !ok || v != false {
			t.Errorf("error event = %v, want boxEmpty:false", m)
		}
	})
}
