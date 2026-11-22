package mux

// boxEmpty is stated on every not_accepted / text_mismatch a gated send
// returns: true when the final verify read the box EMPTY after the send
// typed, false when the box held text. Nothing else carries it.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

func wantBoxEmpty(t *testing.T, o sendOutcome, want bool) {
	t.Helper()
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	empty, stated := protocol.BoxEmptyOf(o.err)
	if !stated || empty != want {
		t.Errorf("BoxEmptyOf(%v) = %v, stated %v; want %v, stated", o.err, empty, stated, want)
	}
}

func TestBoxEmptyOnEveryTextMismatch(t *testing.T) {
	const partial = "the box holds 3 of 5 non-space runes"
	for _, tc := range []struct {
		name  string
		req   sendRequest
		check func(el time.Duration, n int) BoxCheck
		want  bool
	}{
		{"never drawn", sendRequest{text: "hello", enter: true},
			func(time.Duration, int) BoxCheck { return boxEmptyCheck() }, true},
		{"reset seen", sendRequest{text: "hello", enter: true},
			func(time.Duration, int) BoxCheck { return lostBoxCheck() }, true},
		{"window cut", sendRequest{text: "hello", enter: true, readyTimeoutMs: 1500},
			func(time.Duration, int) BoxCheck { return boxEmptyCheck() }, true},
		{"no loss signal", sendRequest{text: "hello", enter: true},
			func(el time.Duration, _ int) BoxCheck {
				if el >= 600*time.Millisecond && el < 700*time.Millisecond {
					return checkNotReady(notReady(protocol.ReasonNoInputBox))
				}
				return boxEmptyCheck()
			}, true},
		{"paste lost", sendRequest{mode: sendPaste, text: "hello", enter: true},
			func(time.Duration, int) BoxCheck { return boxEmptyCheck() }, true},
		{"conversation moved", sendRequest{text: "hello", enter: true},
			func(time.Duration, int) BoxCheck {
				return emptyConvChanged("the conversation above the input box changed")
			}, true},
		{"left the box", sendRequest{text: "hello", enter: true},
			func(el time.Duration, _ int) BoxCheck {
				if el < 200*time.Millisecond {
					return boxMismatchCheck("hel", partial, false)
				}
				return boxEmptyCheck()
			}, true},
		{"partial", sendRequest{text: "hello", enter: true},
			func(time.Duration, int) BoxCheck { return boxMismatchCheck("hel", partial, false) }, false},
		{"more than typed", sendRequest{text: "hello", enter: true},
			func(time.Duration, int) BoxCheck {
				return boxMismatchCheck("hellohello", "the box holds more than was typed: 10 non-space runes where 5 were typed", true)
			}, false},
		{"still changing", sendRequest{text: "hello", enter: true},
			func(_ time.Duration, n int) BoxCheck {
				return boxMismatchCheck(fmt.Sprintf("frame%d", n), partial, false)
			}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clk lagClock
			g := &fakeGate{checkFn: func(_ BoxExpect, n int) BoxCheck { return tc.check(clk.since(), n) }}
			m, _, _ := gatedMux(t, g)
			o := runSend(t, m, tc.req)
			wantBoxEmpty(t, o, tc.want)
		})
	}
}

// TestBoxEmptyIsNotStatedOnEmptyBox: an Enter-only submit into an empty box
// typed nothing; its empty_box reply states no boxEmpty.
func TestBoxEmptyIsNotStatedOnEmptyBox(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return boxEmptyCheck() }}
	m, _, _ := gatedMux(t, g)
	o := runSend(t, m, sendRequest{enter: true})
	if protocol.ReasonOf(o.err) != protocol.ReasonEmptyBox {
		t.Fatalf("err %v, want empty_box", o.err)
	}
	if _, stated := protocol.BoxEmptyOf(o.err); stated {
		t.Errorf("empty_box states boxEmpty: %+v", o.err)
	}
}

// TestSocketReplyCarriesBoxEmpty: on the socket, boxEmpty rides beside the
// code on a verb reply and on a `call`, and is absent when not stated.
func TestSocketReplyCarriesBoxEmpty(t *testing.T) {
	empty := true
	t.Run("a verb reply", func(t *testing.T) {
		m := &Magmux{}
		var got map[string]any
		err := &sockErr{Code: protocol.CodeNotAccepted, Msg: "lost", Reason: protocol.ReasonTextMismatch, Typed: true, BoxEmpty: &empty}
		if e := json.Unmarshal(m.replyBytes(json.RawMessage(`1`), nil, err), &got); e != nil {
			t.Fatal(e)
		}
		if got["boxEmpty"] != true {
			t.Errorf("reply = %v, want boxEmpty:true", got)
		}
		got = nil
		if e := json.Unmarshal(m.replyBytes(json.RawMessage(`2`), nil, sockErrf(sockCodePaneDead, "gone")), &got); e != nil {
			t.Fatal(e)
		}
		if _, ok := got["boxEmpty"]; ok {
			t.Errorf("a reply that states no boxEmpty carries the key: %v", got)
		}
	})

	t.Run("a call over the real socket", func(t *testing.T) {
		m, _, dial := sockMux(t)
		if err := m.bus().Register("boxemptytest", hub.Op{
			Spec: protocol.OpSpec{Name: "emptied", Class: protocol.ClassRead},
			Fn: func(context.Context, hub.Caller, json.RawMessage) (map[string]any, error) {
				return nil, &protocol.Error{Code: protocol.CodeNotAccepted, Msg: "stayed empty",
					Reason: protocol.ReasonTextMismatch, Typed: true, BoxEmpty: &empty}
			},
		}); err != nil {
			t.Fatal(err)
		}
		conn := dial()
		writeLines(t, conn, map[string]any{"type": "call", "op": "emptied", "id": "b1"})
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) != nil || ev["type"] != "reply" || ev["id"] != "b1" {
				continue
			}
			if ev["boxEmpty"] != true || ev["reason"] != protocol.ReasonTextMismatch {
				t.Errorf("call reply = %v, want boxEmpty:true beside reason text_mismatch", ev)
			}
			return
		}
		t.Fatalf("no reply to the call: %v", sc.Err())
	})
}
