package mux

// expectBox lets a gated send type into a box that already holds something —
// an image token, a declared paste — by naming it. The gate types only into a
// box holding exactly that, and every check after it expects that content plus
// what was written.

import (
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/protocol"
)

const imageToken = "[Image #1]"

func TestExpectBoxTypesAfterTheExpectedContent(t *testing.T) {
	text := strings.Repeat("describe the picture ", 4) // several typed writes
	var log *byteLog
	g := &fakeGate{
		readyFn: func(q ReadinessQuery, _ int) InputReadiness {
			r := readyNow()
			r.BoxHasText = true // the image token is in the box
			if q.Types && q.ExpectBox != imageToken {
				return notReady(protocol.ReasonBoxNotEmpty)
			}
			return r
		},
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			got := imageToken + log.String()
			if inputKey(got) == inputKey(want.Exact) {
				return verifiedBox(verifiedExact)
			}
			return boxMismatchCheck(inputKey(got), "the box holds a prefix of what was typed", false)
		},
	}
	m, _, l := gatedMux(t, g)
	log = l

	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: false, expectBox: imageToken})

	if o.err != nil {
		t.Fatalf("send: %v; a box holding exactly expectBox must be typed into", o.err)
	}
	if o.rep.accept != acceptInBox || o.rep.verified != verifiedExact {
		t.Errorf("accept=%q verified=%q, want in_box / exact", o.rep.accept, o.rep.verified)
	}
	if got := log.settle(); got != text {
		t.Errorf("the child received %q, want the text once and no Enter", got)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.expects) < 2 {
		t.Fatalf("%d box checks, want one per write after the first and the final verify", len(g.expects))
	}
	for _, e := range g.expects {
		if !strings.HasPrefix(e.Exact, imageToken) {
			t.Errorf("a box check expected %q, want the expectBox content first", e.Exact)
		}
	}
	if last := g.expects[len(g.expects)-1].Exact; last != imageToken+text {
		t.Errorf("the final verify expected %q, want expectBox + the whole text", last)
	}
	for _, q := range g.queries {
		if q.Types && q.ExpectBox != imageToken {
			t.Errorf("a typing readiness query carried ExpectBox %q", q.ExpectBox)
		}
	}
}

func TestExpectBoxNotHeldWritesNothing(t *testing.T) {
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { return notReady(protocol.ReasonBoxNotEmpty) }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{mode: sendTyped, text: "describe it", enter: false, expectBox: imageToken, readyTimeoutMs: 300})
	wantErr(t, o, protocol.CodeBusy, protocol.ReasonBoxNotEmpty, false)
	if got := log.settle(); got != "" {
		t.Errorf("the child received %q, want nothing", got)
	}
}

func TestExpectBoxOnAnUngatedSendIsBadRequest(t *testing.T) {
	m, _, log := gatedMux(t, &fakeGate{})
	o := runSend(t, m, sendRequest{text: "hello", enter: false, expectBox: imageToken, noGate: true})
	if got := verbErrCode(o.err); got != protocol.CodeBadRequest {
		t.Fatalf("code = %q (%v), want bad_request", got, o.err)
	}
	if got := log.settle(); got != "" {
		t.Errorf("the child received %q, want nothing", got)
	}
}
