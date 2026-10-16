package mcp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestMCPSendErrorCarriesReason: the send tools forward the gate's arguments
// and, when magmux refuses, say the code, the reason and whether text was
// typed — `busy (dialog:permission)` and `not_accepted (still_in_box)` call
// for different next steps.
func TestMCPSendErrorCarriesReason(t *testing.T) {
	f := startFakeMagmux(t, true)
	var sent []map[string]any
	f.onVerb = func(f *fakeMagmux, fc *fakeConn, verb string, msg map[string]any) (map[string]any, bool) {
		if verb != "send" {
			return nil, false
		}
		f.mu.Lock()
		sent = append(sent, msg)
		f.mu.Unlock()
		out, _ := json.Marshal(map[string]any{"type": "reply", "id": msg["id"], "ok": false,
			"code": "not_accepted", "reason": "still_in_box", "typed": true,
			"error": "pane 0: Enter was pressed twice and the text is still in the input box"})
		fc.send(string(out))
		return nil, true
	}
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()
	sess, err := s.attach(ctx, "fake", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer sess.Close()

	res, rerr := toolSendKeys(ctx, s, json.RawMessage(
		`{"pane":0,"text":"run the tests","dismiss_dialogs":true,"ready_timeout_ms":1500,"no_gate":false,"session_id":"fake"}`))
	if rerr != nil {
		t.Fatalf("send_keys: %v", rerr)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	want := "could not send to pane 0: not_accepted (still_in_box): pane 0: Enter was pressed twice and the text is still in the input box (text was typed)"
	if res["isError"] != true || text != want {
		t.Errorf("tool text\n got %q\nwant %q", text, want)
	}
	f.mu.Lock()
	msg := sent[0]
	f.mu.Unlock()
	if msg["dismissDialogs"] != true || msg["readyTimeoutMs"] != float64(1500) {
		t.Errorf("the gate arguments did not reach the wire: %v", msg)
	}
	if _, ok := msg["noGate"]; ok {
		t.Errorf("noGate:false went on the wire: %v", msg)
	}

	res, _ = toolSendAndWait(ctx, s, json.RawMessage(
		`{"pane":0,"instruction":"go","no_gate":true,"start_timeout_ms":200,"turn_timeout_ms":200,"session_id":"fake"}`))
	text = res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, "could not deliver the instruction to pane 0: not_accepted (still_in_box): ") {
		t.Errorf("send_and_wait text %q", text)
	}
	f.mu.Lock()
	msg = sent[len(sent)-1]
	f.mu.Unlock()
	if msg["noGate"] != true {
		t.Errorf("no_gate did not reach the wire: %v", msg)
	}

	if _, rerr := toolSendKeys(ctx, s, json.RawMessage(`{"pane":0,"text":"x","ready_timeout_ms":-1,"session_id":"fake"}`)); rerr == nil {
		t.Errorf("a negative ready_timeout_ms was not refused as invalid params")
	}
}
