package protocol

import (
	"strings"
	"testing"
	"time"
)

func TestReadyWait(t *testing.T) {
	for _, c := range []struct {
		ms   int
		want time.Duration
	}{
		{0, SendReadyWaitDefault},
		{-5, SendReadyWaitDefault}, // refused by magmux before this is ever read
		{2500, 2500 * time.Millisecond},
		{60000, SendReadyWaitMax},
		{600000, SendReadyWaitMax},
	} {
		if got := ReadyWait(c.ms); got != c.want {
			t.Errorf("ReadyWait(%d) = %v, want %v", c.ms, got, c.want)
		}
	}
}

// TestSendBudgetWorkedValues pins the worked values the design states (§3.4),
// so a constant that drifts shows up as a number, not as a cut-short reply.
func TestSendBudgetWorkedValues(t *testing.T) {
	ms := func(d time.Duration) int64 { return d.Milliseconds() }
	for _, c := range []struct {
		name string
		s    SendShape
		want int64
	}{
		// one attempt = typing + SendSettle (150) + SendVerifyMin (1200) + SendVerifyStep (40); two attempts.
		{"short submit", SendShape{Submit: true}, 10000 + 2*1390 + 6000 + 2000},
		{"500 keys and a submit", SendShape{Keys: 500, Submit: true}, 10000 + 2*(500*22+1390) + 6000 + 2000},
		{"readyTimeoutMs 60000", SendShape{ReadyTimeoutMs: 60000, Submit: true}, 60000 + 2*1390 + 6000 + 2000},
		{"a TYPE", SendShape{Typed: true, TypedWrites: 3}, 10000 + 2*(3*10+1390) + 2000},
	} {
		if got := ms(SendBudget(c.s)); got != c.want {
			t.Errorf("%s: %dms, want %dms", c.name, got, c.want)
		}
	}
}

func TestTypedWriteCount(t *testing.T) {
	for _, c := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"hi", 1},
		{strings.Repeat("a", 32), 1},
		{strings.Repeat("a", 33), 2},
		{"a\nb", 3},
		{"a\r\nb", 3},
		{"\n", 1},
	} {
		if got := TypedWriteCount(c.text); got != c.want {
			t.Errorf("TypedWriteCount(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}
