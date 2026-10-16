package protocol

import "time"

// ── how long a gated `send` can take ─────────────────────────────────────────
//
// A send to a pane whose input is gated waits for the tool to be ready,
// types at a keystroke's pace, verifies the input box, presses Enter and then
// watches for evidence that the prompt was taken. Every one of those is paced
// by a constant below, and SendBudget adds them up. It is THE upper bound, and
// it lives here, beside the wire types, because two sides need the same
// number: magmux uses it as the default wait of every transport that has one,
// and a client (client.SendWith) uses it as its own reply timeout. Two copies
// of the formula would be two answers to "how long is too long".
//
// The pacing constants are exported so magmux's delivery uses these values
// and not a second copy of them.
const (
	// SendReadyWaitDefault / SendReadyWaitMax bound the readiness wait
	// (`readyTimeoutMs`): 0 is the default, anything above the max is clamped.
	SendReadyWaitDefault = 10 * time.Second
	SendReadyWaitMax     = 60 * time.Second

	// SendTypedChunkRunes / SendTypedChunkGap: a typed send's writes.
	SendTypedChunkRunes = 32
	SendTypedChunkGap   = 8 * time.Millisecond
	// SendKeyGap is the pause before each named key.
	SendKeyGap = 20 * time.Millisecond
	// SendSettle is the pause before the box is verified.
	SendSettle = 150 * time.Millisecond
	// SendGateCheckCost is the allowance for the readiness check made before
	// every write after the first.
	SendGateCheckCost = 2 * time.Millisecond
	// SendAcceptWindow is one acceptance window; at most SendAcceptWindows run.
	SendAcceptWindow  = 2 * time.Second
	SendAcceptWindows = 3
	// SendBudgetSlack covers scheduling and the final reply.
	SendBudgetSlack = 2 * time.Second
)

// SendShape is what bounds one gated send.
type SendShape struct {
	ReadyTimeoutMs int  // the request's readyTimeoutMs (0 = default)
	Typed          bool // typed mode: TypedWrites are paced
	TypedWrites    int  // the writes a typed send makes (TypedWriteCount)
	Keys           int  // named keys, each paced by SendKeyGap
	Submit         bool // a final Enter follows, so acceptance runs
}

// ReadyWait resolves a request's readyTimeoutMs to the readiness deadline.
// A negative value is refused by magmux before it is read; here it is the
// default, so a client that computes a wait for one never gets less than it.
func ReadyWait(ms int) time.Duration {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case ms <= 0:
		return SendReadyWaitDefault
	case d > SendReadyWaitMax:
		return SendReadyWaitMax
	}
	return d
}

// SendBudget is the upper bound on how long a gated send can take from the
// moment it starts running. The gate, the typing token and the pre-Enter
// re-wait share ONE deadline, so the readiness wait is counted once.
func SendBudget(s SendShape) time.Duration {
	b := ReadyWait(s.ReadyTimeoutMs)
	if s.Typed {
		b += time.Duration(s.TypedWrites) * (SendTypedChunkGap + SendGateCheckCost)
	}
	b += time.Duration(s.Keys) * (SendKeyGap + SendGateCheckCost)
	b += SendSettle // every gated send verifies the box after the settle
	if s.Submit {
		b += SendAcceptWindows * SendAcceptWindow
	}
	return b + SendBudgetSlack
}

// TypedWriteCount is how many writes a typed send of text makes: runs of at
// most SendTypedChunkRunes runes, and one write per newline (\n, \r\n or \r).
// magmux's own splitter is pinned to agree with it.
func TypedWriteCount(text string) int {
	n, run := 0, 0
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\r', '\n':
			if rs[i] == '\r' && i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			if run > 0 {
				n++
				run = 0
			}
			n++
		default:
			run++
			if run == SendTypedChunkRunes {
				n++
				run = 0
			}
		}
	}
	if run > 0 {
		n++
	}
	return n
}
