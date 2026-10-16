package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/ws"
)

// TestReplyCarriesReasonAndTyped: an error's reason and typed flag reach the
// caller on HTTP and on the WebSocket, beside the code, and are ABSENT when the
// error has neither — so every reply that predates them is byte-identical.
func TestReplyCarriesReasonAndTyped(t *testing.T) {
	f := newFixture(t)

	t.Run("http", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"code": protocol.CodeNotAccepted, "reason": protocol.ReasonStillInBox, "typed": true})
		resp := f.do(t, "POST", "/v1/ops/boom", f.full, bytes.NewReader(body))
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("not_accepted answered %d, want 409", resp.StatusCode)
		}
		got := decode(t, resp)
		if got["code"] != protocol.CodeNotAccepted || got["reason"] != protocol.ReasonStillInBox || got["typed"] != true {
			t.Errorf("body = %v, want code, reason and typed", got)
		}

		body, _ = json.Marshal(map[string]any{"code": protocol.CodeBusy, "reason": protocol.ReasonVimMode})
		got = decode(t, f.do(t, "POST", "/v1/ops/boom", f.full, bytes.NewReader(body)))
		if got["reason"] != protocol.ReasonVimMode {
			t.Errorf("busy body = %v, want reason vim_mode", got)
		}
		if _, ok := got["typed"]; ok {
			t.Errorf("typed is present when false: %v", got)
		}

		body, _ = json.Marshal(map[string]any{"code": protocol.CodePaneDead})
		got = decode(t, f.do(t, "POST", "/v1/ops/boom", f.full, bytes.NewReader(body)))
		for _, k := range []string{"reason", "typed"} {
			if _, ok := got[k]; ok {
				t.Errorf("an error with no %s carries the key: %v", k, got)
			}
		}
	})

	t.Run("http writeErrStatus", func(t *testing.T) {
		rec := &recorder{header: http.Header{}}
		writeErrStatus(rec, http.StatusServiceUnavailable,
			&protocol.Error{Code: protocol.CodeBusy, Msg: "x", Reason: protocol.ReasonInFlight, Typed: true})
		var got map[string]any
		if err := json.Unmarshal(rec.body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["reason"] != protocol.ReasonInFlight || got["typed"] != true {
			t.Errorf("writeErrStatus body = %v", got)
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
		if err := cc.WriteText(`{"id":1,"op":"boom","args":{"code":"not_accepted","reason":"text_mismatch","typed":true}}`); err != nil {
			t.Fatal(err)
		}
		r := next()
		if r["code"] != protocol.CodeNotAccepted || r["reason"] != protocol.ReasonTextMismatch || r["typed"] != true {
			t.Errorf("ws reply = %v, want code, reason and typed", r)
		}
		if err := cc.WriteText(`{"id":2,"op":"boom","args":{"code":"pane_dead"}}`); err != nil {
			t.Fatal(err)
		}
		r = next()
		for _, k := range []string{"reason", "typed"} {
			if _, ok := r[k]; ok {
				t.Errorf("a ws reply with no %s carries the key: %v", k, r)
			}
		}
	})

	t.Run("sse error event", func(t *testing.T) {
		var m map[string]any
		if err := json.Unmarshal(errorEvent(3, protocol.Errr(protocol.CodeBusy, protocol.ReasonQueueFull, "full")), &m); err != nil {
			t.Fatal(err)
		}
		if m["code"] != protocol.CodeBusy || m["reason"] != protocol.ReasonQueueFull {
			t.Errorf("error event = %v", m)
		}
		m = nil
		if err := json.Unmarshal(errorEvent(3, errors.New("plain")), &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["reason"]; ok {
			t.Errorf("an error event with no reason carries the key: %v", m)
		}
	})
}

// recorder is the smallest http.ResponseWriter: enough to read a body back.
type recorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(s int)           { r.status = s }
