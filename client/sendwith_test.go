package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"

	"github.com/MadAppGang/magmux/protocol"
)

// TestSendWithCarriesEveryFieldAndTheReason: SendWith puts the gate's fields
// on the wire (and leaves the defaults off it), returns the gated result keys,
// and its errors keep reason and typed.
func TestSendWithCarriesEveryFieldAndTheReason(t *testing.T) {
	no := false
	for _, c := range []struct {
		name  string
		o     SendOptions
		reply string
		want  map[string]any
	}{
		{"defaults", SendOptions{Text: "hi"},
			`{"type":"reply","id":%q,"ok":true,"result":{"pane":2,"verified":"exact","accept":"confirmed","evidence":"history"}}`,
			map[string]any{"type": "send", "pane": float64(2), "text": "hi"}},
		{"gate fields", SendOptions{Keys: []string{"2"}, Enter: &no, DismissDialogs: true, ReadyTimeoutMs: 1500, NoGate: true, Typed: true, Label: "x"},
			`{"type":"reply","id":%q,"ok":false,"code":"busy","reason":"dialog:permission","typed":true,"error":"a dialog"}`,
			map[string]any{"type": "send", "pane": float64(2), "keys": []any{"2"}, "enter": false,
				"dismissDialogs": true, "readyTimeoutMs": float64(1500), "noGate": true, "typed": true, "label": "x"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ours, theirs := net.Pipe()
			defer ours.Close()
			defer theirs.Close()
			s := &Session{ID: "u", state: NewSessionState(), pending: map[string]chan mcpReply{},
				closed: make(chan struct{}), inFlight: map[int]bool{}, conn: ours}
			s.proveReplies()
			got := make(chan map[string]any, 1)
			go func() {
				line, err := bufio.NewReader(theirs).ReadBytes('\n')
				if err != nil {
					return
				}
				var msg map[string]any
				_ = json.Unmarshal(line, &msg)
				id, _ := msg["id"].(string)
				delete(msg, "id")
				got <- msg
				s.ingest([]byte(fmt.Sprintf(c.reply, id)))
			}()
			res, err := s.SendWith(context.Background(), 2, c.o)
			msg := <-got
			if fmt.Sprint(msg) != fmt.Sprint(c.want) {
				t.Errorf("wire message %v, want %v", msg, c.want)
			}
			if c.want["noGate"] == true {
				if protocol.ReasonOf(err) != "dialog:permission" || !protocol.TypedOf(err) || protocol.CodeOf(err) != protocol.CodeBusy {
					t.Errorf("err %v lost its code/reason/typed", err)
				}
				return
			}
			if err != nil || res["accept"] != "confirmed" {
				t.Errorf("SendWith = %v, %v", res, err)
			}
		})
	}
}

// TestSendWithWaitsTheBudget: the client's own timer is max(SendTimeout,
// the gated budget), so a long gated send is not cut short by a default.
func TestSendWithWaitsTheBudget(t *testing.T) {
	if b := (SendOptions{Text: "hi"}).SendBudget(); b != protocol.SendBudget(protocol.SendShape{Submit: true}) {
		t.Errorf("a short submit's budget %v", b)
	}
	keys := make([]string, 500)
	for i := range keys {
		keys[i] = "a"
	}
	long := SendOptions{Keys: keys}
	if b := long.SendBudget(); b <= SendTimeout {
		t.Errorf("a 500-key submit's budget %v must exceed SendTimeout %v", b, SendTimeout)
	}
	no := false
	if (SendOptions{Keys: []string{"h", "enter"}, Enter: &no}).submits() != true {
		t.Errorf("a trailing enter key is a submit")
	}
	if (SendOptions{Keys: []string{"2"}, Enter: &no}).submits() {
		t.Errorf("keys with enter:false and no Enter key do not submit")
	}
	if b := (SendOptions{Text: "x", NoGate: true}).SendBudget(); b != 0 {
		t.Errorf("a noGate send has no gated budget, got %v", b)
	}
	typed := SendOptions{Text: "abc\ndef", Typed: true}
	want := protocol.SendBudget(protocol.SendShape{Typed: true, TypedWrites: 3, Submit: true})
	if b := typed.SendBudget(); b != want {
		t.Errorf("typed budget %v, want %v", b, want)
	}
}
