package mux

// A gated typed send writes the next chunk only once the input box holds
// exactly what was written so far. A reader that takes its input late still
// shows the earlier box, in raw mode, with nothing changing — readiness says
// ready throughout — so readiness alone cannot authorise the next chunk: only
// the box showing the written prefix can.

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// lateReader models a child that draws what it read only `lag` after the bytes
// reached it. A sampler records the child's bytes every millisecond; the box
// is the newest sample at least `lag` old. Each time the child's bytes grow,
// it also records what the box showed at that moment: the box a write landed
// on.
type lateReader struct {
	lag  time.Duration
	log  *byteLog
	mu   sync.Mutex
	hist []lateSample
	// landed[i] is the box at the moment the child had received the i-th
	// distinct byte prefix after the first (i.e. a later write arrived).
	landed []lateLanding
	stop   chan struct{}
}

type lateSample struct {
	at  time.Time
	got string
}

type lateLanding struct {
	before string // the child's bytes before this write
	box    string // what the box showed when this write reached the child
}

func newLateReader(t *testing.T, log *byteLog, lag time.Duration) *lateReader {
	r := &lateReader{lag: lag, log: log, stop: make(chan struct{})}
	r.hist = []lateSample{{at: time.Now(), got: ""}}
	go func() {
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-tk.C:
			}
			got := log.String()
			r.mu.Lock()
			last := r.hist[len(r.hist)-1].got
			if got != last {
				if last != "" {
					r.landed = append(r.landed, lateLanding{before: last, box: r.boxLocked(time.Now())})
				}
				r.hist = append(r.hist, lateSample{at: time.Now(), got: got})
			}
			r.mu.Unlock()
		}
	}()
	t.Cleanup(func() { close(r.stop) })
	return r
}

func (r *lateReader) boxLocked(now time.Time) string {
	box := ""
	for _, s := range r.hist {
		if now.Sub(s.at) >= r.lag {
			box = s.got
		}
	}
	return box
}

func (r *lateReader) check(want BoxExpect) BoxCheck {
	r.mu.Lock()
	box := r.boxLocked(time.Now())
	r.mu.Unlock()
	switch {
	case box == want.Exact:
		return verifiedBox(verifiedExact)
	case box == "":
		return BoxCheck{Readiness: readyNow(), Verdict: BoxEmpty, Detail: "the input box is empty"}
	}
	return boxMismatchCheck(box, "the box holds a prefix of what was typed", false)
}

func (r *lateReader) landings() []lateLanding {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]lateLanding(nil), r.landed...)
}

func TestTypedChunkWaitsForTheBoxToShowThePreviousChunk(t *testing.T) {
	// REGRESSION: a gated typed send wrote its next chunk on a readiness "ready" while a late reader's box still showed the earlier text.
	text := strings.Repeat("abcdefghij", 10) // four typed writes: 32 + 32 + 32 + 4
	if n := len(typedWrites(text)); n != 4 {
		t.Fatalf("typedWrites(100 runes) = %d writes, the test assumes 4", n)
	}
	var rd *lateReader
	g := &fakeGate{checkFn: func(want BoxExpect, _ int) BoxCheck { return rd.check(want) }}
	m, _, log := gatedMux(t, g)
	rd = newLateReader(t, log, 200*time.Millisecond)

	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})

	if got := log.settle(); got != text+"\r" {
		t.Errorf("the child received %q; want the text once and one Enter", got)
	}
	if o.err != nil {
		t.Fatalf("send: %v; a late reader must be waited for, chunk by chunk", o.err)
	}
	ahead := 0
	for _, l := range rd.landings() {
		if l.box != l.before {
			ahead++
			t.Errorf("a write reached the child while the box showed %d of the %d bytes already written",
				len(l.box), len(l.before))
		}
	}
	if ahead > 0 {
		t.Errorf("%d writes ran ahead of the box; each must wait until the box shows exactly what was written", ahead)
	}
}

func TestASlowReaderIsNotCutOffByTheSharedDeadline(t *testing.T) {
	// Every chunk is drawn 700 ms after it reached the child. Five writes
	// take ~2.8 s of waiting, past the 1 s readiness deadline; each chunk's
	// wait has its own floor, so the whole text is typed and verified.
	text := strings.Repeat("abcdefghij", 16) // five typed writes
	if n := len(typedWrites(text)); n != 5 {
		t.Fatalf("typedWrites(160 runes) = %d writes, the test assumes 5", n)
	}
	var rd *lateReader
	g := &fakeGate{checkFn: func(want BoxExpect, _ int) BoxCheck { return rd.check(want) }}
	m, _, log := gatedMux(t, g)
	rd = newLateReader(t, log, 700*time.Millisecond)

	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true, readyTimeoutMs: 1000})

	if o.err != nil {
		t.Fatalf("send: %v; a slow but steady reader must get the whole text", o.err)
	}
	if got := log.settle(); got != text+"\r" {
		t.Errorf("the child received %q; want the text once and one Enter", got)
	}
	for _, l := range rd.landings() {
		if l.box != l.before {
			t.Errorf("a write reached the child while the box showed %d of the %d bytes already written", len(l.box), len(l.before))
		}
	}
}

func TestBoxBehind(t *testing.T) {
	for _, c := range []struct {
		box, written, held string
		want               bool
	}{
		{"", "abc", "", true},
		{"ab", "abc", "", true},
		{"abc", "abc", "", false},
		{"abd", "abc", "", false},
		{"abcd", "abc", "", false},
		{"[Image#1]", "[Image #1]abc", "[Image #1]", true},
		{"[Image#1]ab", "[Image #1]abc", "[Image #1]", true},
		{"", "[Image #1]abc", "[Image #1]", false}, // the token was lost: not behind
		{"ab", "!abc", "", true},                   // bash mode shows the text without its `!`
	} {
		if got := boxBehind(c.box, c.written, c.held); got != c.want {
			t.Errorf("boxBehind(%q, %q, %q) = %v, want %v", c.box, c.written, c.held, got, c.want)
		}
	}
}
