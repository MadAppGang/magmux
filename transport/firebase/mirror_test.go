package firebase

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// testMirror builds a mirror over the fake, with a small budget by default so a
// test can exercise shedding without generating a megabyte of frames.
func testMirror(t *testing.T, f *fakeRTDB, tweak func(*Config)) (*mirror, *client, string) {
	t.Helper()
	cfg := &Config{
		Root: "magmux", Host: "test-host",
		Emulator: &EmulatorConfig{Host: f.Host(), NS: "demo-magmux"},
	}
	if tweak != nil {
		tweak(cfg)
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	// The real assembly, not a test-only one: emulator mode is exactly what the
	// fake serves, so the client under test is the one magmux ships.
	c, err := newClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess := sessionPath(cfg.Root, cfg.Host, "magmux-1780000000")
	return newMirror(c, sess, cfg, func(string) {}), c, sess
}

func line(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

func frameLine(pane int, seq uint64, key bool, rows int, lines ...protocol.Line) []byte {
	return line(protocol.Frame{
		FrameHeader: protocol.FrameHeader{
			Type: protocol.EventFrame, Pane: pane, Seq: seq, Rows: rows, Cols: 80,
			Cur: protocol.Cursor{Y: 0, X: 1, Vis: true},
		},
		Key:   key,
		Lines: lines,
	})
}

// TestMirrorWritesOneMultiPathPatch is the shape of the whole mirror: one
// request per tick, at the SESSION node, print=silent, and every value hanging
// off a relative path — never an ancestor and a descendant together.
func TestMirrorWritesOneMultiPathPatch(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, sess := testMirror(t, f, nil)

	m.SetSessionMeta(map[string]any{"pid": 42, "alive": true})
	m.Write(line(map[string]any{
		"type": protocol.EventSnapshot,
		"panes": []map[string]any{
			{"pane": 0, "state": "running", "label": "build", "cmd": "make", "dead": false, "exitCode": 0},
			{"pane": 3, "state": "panel", "control": true, "hidden": true},
		},
	}))
	m.Write(frameLine(0, 1, true, 2,
		protocol.Line{Y: 0, T: "hello", R: []protocol.Run{protocol.NewRun(0, 5, 2, -1, 1)}},
		protocol.Line{Y: 1, T: "world"},
	))

	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	patches := f.Patches()
	if len(patches) != 1 {
		t.Fatalf("%d PATCHes for one tick; the mirror must send at most one", len(patches))
	}
	p := patches[0]
	if p.Path != sess {
		t.Errorf("PATCH at %q, want the session node %q", p.Path, sess)
	}
	if p.Query.Get("print") != "silent" {
		t.Errorf("print = %q, want silent", p.Query.Get("print"))
	}
	if p.Query.Get("ns") != "demo-magmux" {
		t.Errorf("ns = %q", p.Query.Get("ns"))
	}
	if f.ancestorViolations != 0 {
		t.Errorf("the batch held an ancestor and a descendant; RTDB refuses that")
	}

	want := []string{"meta", "panes/p0/meta", "panes/p0/state", "panes/p3/meta", "panes/p3/state", "panes/p0/frame"}
	for _, k := range want {
		if _, ok := p.Payload[k]; !ok {
			t.Errorf("no %q in the batch; got %v", k, keysOf(p.Payload))
		}
	}

	// A keyframe is ONE path holding the whole node, so a later shrink can
	// drop rows by replacing it.
	var frame struct {
		Seq   int                        `json:"seq"`
		Rows  int                        `json:"rows"`
		Cur   map[string]any             `json:"cur"`
		Lines map[string]json.RawMessage `json:"lines"`
	}
	if err := json.Unmarshal(p.Payload["panes/p0/frame"], &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Seq != 1 || frame.Rows != 2 {
		t.Errorf("frame scalars = %+v", frame)
	}
	if len(frame.Lines) != 2 || frame.Lines["r0"] == nil || frame.Lines["r1"] == nil {
		t.Errorf("frame lines = %v; want r0 and r1", keysOfRaw(frame.Lines))
	}

	// A second tick with nothing new sends nothing at all. An idle session must
	// cost nothing but the heartbeat.
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Patches()); n != 1 {
		t.Fatalf("%d PATCHes after an idle tick, want 1", n)
	}
}

// TestDeltaWritesRowsAndKeyframeReplacesTheSubtree: the two frame shapes, and
// the reason they are different. A delta names the rows that changed; a
// keyframe replaces the node, so rows past a shrink vanish rather than standing
// forever beside a smaller screen.
func TestDeltaWritesRowsAndKeyframeReplacesTheSubtree(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, sess := testMirror(t, f, nil)
	ctx := context.Background()

	m.Write(frameLine(0, 1, true, 3,
		protocol.Line{Y: 0, T: "one"}, protocol.Line{Y: 1, T: "two"}, protocol.Line{Y: 2, T: "three"}))
	if err := m.flush(ctx, nil); err != nil {
		t.Fatal(err)
	}

	m.Write(frameLine(0, 2, false, 3, protocol.Line{Y: 1, T: "TWO"}))
	if err := m.flush(ctx, nil); err != nil {
		t.Fatal(err)
	}
	p := f.Patches()[1]
	if _, ok := p.Payload["panes/p0/frame"]; ok {
		t.Error("a delta replaced the whole frame node; it must name rows")
	}
	if _, ok := p.Payload["panes/p0/frame/lines/r1"]; !ok {
		t.Errorf("a delta did not write the row that changed: %v", keysOf(p.Payload))
	}
	if _, ok := p.Payload["panes/p0/frame/lines/r0"]; ok {
		t.Error("a delta wrote a row that did not change")
	}
	if _, ok := p.Payload["panes/p0/frame/seq"]; !ok {
		t.Error("a delta did not update seq")
	}
	if f.ancestorViolations != 0 {
		t.Error("a delta put an ancestor and a descendant in one batch")
	}

	// The shrink. A keyframe for a 2-row screen must leave r2 gone.
	m.Write(frameLine(0, 3, true, 2, protocol.Line{Y: 0, T: "a"}, protocol.Line{Y: 1, T: "b"}))
	if err := m.flush(ctx, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := f.Value(sess + "/panes/p0/frame").(map[string]any)
	lines, _ := got["lines"].(map[string]any)
	if len(lines) != 2 {
		t.Fatalf("after a shrink the mirror holds %d rows: %v", len(lines), lines)
	}
	if _, ok := lines["r2"]; ok {
		t.Error("r2 survived a keyframe for a shorter screen")
	}
}

// TestFrameMergeIsLatestWins: two frames between ticks are one write, and a
// keyframe swallows the deltas behind it — the same merge the hub's slot does,
// for the same reason.
func TestFrameMergeIsLatestWins(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, _ := testMirror(t, f, nil)

	m.Write(frameLine(0, 1, false, 2, protocol.Line{Y: 0, T: "first"}))
	m.Write(frameLine(0, 2, false, 2, protocol.Line{Y: 0, T: "second"}, protocol.Line{Y: 1, T: "new"}))
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	p := f.Patches()[0]
	var row map[string]any
	if err := json.Unmarshal(p.Payload["panes/p0/frame/lines/r0"], &row); err != nil {
		t.Fatal(err)
	}
	if row["t"] != "second" {
		t.Errorf("r0 = %v; the newer frame must win", row["t"])
	}
	if _, ok := p.Payload["panes/p0/frame/lines/r1"]; !ok {
		t.Error("the merge lost a row the second frame carried")
	}

	// A keyframe arriving behind a delta makes the whole thing a keyframe.
	m.Write(frameLine(0, 3, false, 2, protocol.Line{Y: 1, T: "delta"}))
	m.Write(frameLine(0, 4, true, 2, protocol.Line{Y: 0, T: "K0"}, protocol.Line{Y: 1, T: "K1"}))
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	p = f.Patches()[1]
	if _, ok := p.Payload["panes/p0/frame"]; !ok {
		t.Errorf("a keyframe merged behind a delta did not replace the node: %v", keysOf(p.Payload))
	}
}

// TestPaneStateMergesRatherThanReplaces: a partial event must not delete the
// fields it did not carry. A PATCH replaces the node a path names, so the
// mirror keeps the node whole in memory and writes the merged value.
func TestPaneStateMergesRatherThanReplaces(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, sess := testMirror(t, f, nil)
	ctx := context.Background()

	m.Write(line(map[string]any{
		"type": protocol.EventSnapshot, "pane": 0, "state": "running",
		"controller": "claude", "response": "working on it", "model": "opus",
	}))
	if err := m.flush(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// An `exit` carries exitCode and dead and nothing else.
	m.Write(line(map[string]any{"type": protocol.EventExit, "pane": 0, "exitCode": 7, "duration": "1s"}))
	if err := m.flush(ctx, nil); err != nil {
		t.Fatal(err)
	}
	state, _ := f.Value(sess + "/panes/p0/state").(map[string]any)
	if state["response"] != "working on it" {
		t.Errorf("the controller's response was lost by a partial event: %v", state)
	}
	if state["exitCode"] != float64(7) {
		t.Errorf("exitCode = %v", state["exitCode"])
	}
	if state["model"] != "opus" {
		t.Errorf("model was lost: %v", state)
	}
}

// TestPaneClosedIsExplicit: `pane_closed` carries no `closed` field — the type
// IS the statement — so the mirror makes it explicit rather than waiting for an
// aggregate that may never come.
func TestPaneClosedIsExplicit(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, sess := testMirror(t, f, nil)
	m.Write(line(map[string]any{"type": protocol.EventPaneOpened, "pane": 2, "cmd": "sh", "cwd": "/tmp"}))
	m.Write(line(map[string]any{"type": protocol.EventPaneClosed, "pane": 2}))
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	meta, _ := f.Value(sess + "/panes/p2/meta").(map[string]any)
	if meta["closed"] != true {
		t.Errorf("meta = %v; want closed:true", meta)
	}
	if meta["cmd"] != "sh" {
		t.Errorf("the close lost the pane's cmd: %v", meta)
	}
	state, _ := f.Value(sess + "/panes/p2/state").(map[string]any)
	if state["state"] != "closed" {
		t.Errorf("state = %v", state)
	}
}

// TestEventRingTrimsInTheSameBatch: the delete of the entry that fell off the
// end rides in the SAME PATCH as the entry that pushed it, so the ring never
// exceeds its bound even for one tick.
func TestEventRingTrimsInTheSameBatch(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, _ := testMirror(t, f, nil)
	for i := 0; i < eventRing+3; i++ {
		m.Write(line(map[string]any{"type": protocol.EventControl, "dir": "note", "n": i}))
	}
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	p := f.Patches()[0]
	for i := 1; i <= 3; i++ {
		k := "events/" + eventKey(uint64(i))
		v, ok := p.Payload[k]
		if !ok {
			t.Fatalf("%s was never scheduled for deletion", k)
		}
		if string(v) != "null" {
			t.Errorf("%s = %s, want null (a delete)", k, v)
		}
	}
	if _, ok := p.Payload["events/"+eventKey(uint64(eventRing+3))]; !ok {
		t.Error("the newest event is missing")
	}
}

// TestEventDataIsAJSONString: the three free-form payloads are stored as
// strings, so a plugin's `$ref` cannot become an RTDB key.
func TestEventDataIsAJSONString(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, _ := testMirror(t, f, nil)
	m.Write(line(map[string]any{
		"type": protocol.EventPlugin, "plugin": "ticket", "event": "progress", "pane": 5,
		"data": map[string]any{"$ref": "#/defs/x", "a.b": 1},
	}))
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	p := f.Patches()[0]
	var rec struct {
		Type   string `json:"type"`
		Plugin string `json:"plugin"`
		Event  string `json:"event"`
		Pane   int    `json:"pane"`
		Data   string `json:"data"`
	}
	if err := json.Unmarshal(p.Payload["events/"+eventKey(1)], &rec); err != nil {
		t.Fatalf("the event record is not the documented shape: %v", err)
	}
	if rec.Plugin != "ticket" || rec.Event != "progress" || rec.Pane != 5 {
		t.Errorf("record = %+v", rec)
	}
	if !strings.Contains(rec.Data, `"$ref"`) {
		t.Errorf("data = %q; the payload must survive verbatim inside the string", rec.Data)
	}
	// And it really is a STRING: the dollar never reaches a key.
	var probe map[string]any
	if err := json.Unmarshal(p.Payload["events/"+eventKey(1)], &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["data"].(string); !ok {
		t.Fatalf("data is %T, not a JSON string", probe["data"])
	}
}

// TestOpsNodeIsRewrittenWholesaleAsStrings: a plugin op's schema can hold a
// `$ref`, so every spec is a JSON string, and the key is the RTDB-safe
// spelling of the op name.
func TestOpsNodeIsRewrittenWholesaleAsStrings(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, _ := testMirror(t, f, nil)
	ops := func() map[string]any {
		return map[string]any{
			opKey("open_pane"):         jsonString(protocol.OpSpec{Name: "open_pane", Class: protocol.ClassControl}),
			opKey("ticket.run_ticket"): jsonString(protocol.OpSpec{Name: "ticket.run_ticket", Class: protocol.ClassControl, Schema: json.RawMessage(`{"$ref":"#/x"}`)}),
			"rev":                      3,
		}
	}
	m.MarkOpsChanged()
	if err := m.flush(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	p := f.Patches()[0]
	var node map[string]any
	if err := json.Unmarshal(p.Payload["ops"], &node); err != nil {
		t.Fatal(err)
	}
	if _, ok := node["ticket__run_ticket"]; !ok {
		t.Errorf("the plugin op is not under its RTDB-safe key: %v", keysOfAny(node))
	}
	if _, ok := node["ticket.run_ticket"]; ok {
		t.Error("a dot reached an RTDB key")
	}
	if _, ok := node["ticket__run_ticket"].(string); !ok {
		t.Fatalf("the spec is %T, not a JSON string", node["ticket__run_ticket"])
	}
}

// TestBadPathIsRetriedAloneThenDropped is the 400 recovery, end to end. One
// value the database will not take must not stall the mirror forever, and it
// must be dropped rather than retried into a wall.
func TestBadPathIsRetriedAloneThenDropped(t *testing.T) {
	f := newFakeRTDB(t)
	logged := make(chan string, 8)
	m, _, sess := testMirror(t, f, nil)
	m.log = func(s string) {
		select {
		case logged <- s:
		default:
		}
	}
	bad := "events/" + eventKey(1)
	f.SetStatus(func(n int, rec patchRecord) int {
		if _, ok := rec.Payload[bad]; ok {
			return http.StatusBadRequest
		}
		return http.StatusNoContent
	})

	m.Write(line(map[string]any{"type": protocol.EventControl, "dir": "note"}))
	m.Write(line(map[string]any{"type": protocol.EventSnapshot, "pane": 0, "state": "running"}))
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatalf("a 400 must not be reported as a flush failure: %v", err)
	}

	// The good path landed during the single-path retry.
	if st, _ := f.Value(sess + "/panes/p0/state").(map[string]any); st["state"] != "running" {
		t.Errorf("the good path did not land: %v", st)
	}
	select {
	case s := <-logged:
		if !strings.Contains(s, bad) {
			t.Errorf("the log line does not name the offending path: %q", s)
		}
	default:
		t.Error("the dropped path was not logged")
	}

	// And it is never offered again.
	before := len(f.Patches())
	m.Write(line(map[string]any{"type": protocol.EventSnapshot, "pane": 1, "state": "running"}))
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.Patches()[before:] {
		if _, ok := p.Payload[bad]; ok {
			t.Fatal("the dropped path was sent again")
		}
	}
}

// TestRateLimitIsRetriedWithBackoffAndLosesNothing: a 429 is a failure of the
// REQUEST and not of the data, so nothing is swept and the next attempt carries
// the same values.
func TestRateLimitIsRetriedWithBackoffAndLosesNothing(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, sess := testMirror(t, f, nil)
	f.SetStatus(func(n int, _ patchRecord) int {
		if n == 0 {
			return http.StatusTooManyRequests
		}
		return http.StatusNoContent
	})
	m.Write(line(map[string]any{"type": protocol.EventSnapshot, "pane": 0, "state": "running", "response": "hi"}))

	err := m.flush(context.Background(), nil)
	if err == nil {
		t.Fatal("a 429 was not reported as a flush failure")
	}
	he, ok := asHTTPError(err)
	if !ok || !he.retryable() {
		t.Fatalf("a 429 must be retryable; got %v", err)
	}
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	st, _ := f.Value(sess + "/panes/p0/state").(map[string]any)
	if st["response"] != "hi" {
		t.Fatalf("the rate-limited values were lost: %v", st)
	}

	// The schedule itself: 1 s doubling to a 60 s ceiling.
	var bo backoff
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for i, w := range want {
		if got := bo.next(); got != w {
			t.Errorf("backoff %d = %s, want %s", i, got, w)
		}
	}
	for i := 0; i < 10; i++ {
		bo.next()
	}
	if got := bo.next(); got != backoffMax {
		t.Errorf("backoff ceiling = %s, want %s", got, backoffMax)
	}
}

// TestRedirectReAddsAuthorizationOnlyForAdmittedHosts is Correction 13.
//
// Go strips Authorization on a cross-host redirect, which is right; RTDB
// answers the project URL with a 307 to the instance, which means the header
// has to go back. Both halves are asserted: it IS re-added for a host the
// predicate admits, and it is NOT for one it does not.
func TestRedirectReAddsAuthorizationOnlyForAdmittedHosts(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RTDB's own answer to a request at the project URL: go to the instance
		// that actually holds the data.
		http.Redirect(w, r, "http://target.test"+r.URL.Path+"?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	// TWO NAMES, not two ports. Go strips Authorization only when the HOSTNAME
	// changes (shouldCopyHeaderOnRedirect compares hostnames, not addresses),
	// so 127.0.0.1:A → 127.0.0.1:B keeps the header by itself and would prove
	// nothing at all. The dialer maps the two names onto the two servers.
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		switch addr {
		case "origin.test:80":
			return d.DialContext(ctx, "tcp", mustHost(t, origin.URL))
		case "target.test:80":
			return d.DialContext(ctx, "tcp", mustHost(t, target.URL))
		}
		return nil, fmt.Errorf("unexpected dial to %s", addr)
	}

	run := func(allow func(*url.URL) bool) string {
		mu.Lock()
		gotAuth = ""
		mu.Unlock()
		hc := newHTTPClient(allow)
		hc.Transport = &http.Transport{DialContext: dial}
		c := testClient(t, "http://origin.test", allow)
		c.hc = hc
		if err := c.patch(context.Background(), "magmux/x", map[string]any{"a": 1}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return gotAuth
	}

	// Admitted: both hosts pass the predicate, exactly as a project URL and its
	// instance URL both pass IsDatabaseURL in production.
	both := func(u *url.URL) bool {
		return u != nil && (u.Hostname() == "origin.test" || u.Hostname() == "target.test")
	}
	if got := run(both); got != "Bearer "+emulatorToken {
		t.Fatalf("Authorization after an admitted redirect = %q; the header was not re-added", got)
	}

	// Refused: the redirect target is not a host the predicate admits, so the
	// credential does not follow it. This is the half that matters — an
	// unconditional re-add would hand an admin token to whoever answered.
	originOnly := func(u *url.URL) bool { return u != nil && u.Hostname() == "origin.test" }
	if got := run(originOnly); got != "" {
		t.Fatalf("Authorization reached a host the predicate refuses: %q", got)
	}
}

// TestFramesAreShedBeforeState: when the byte budget runs out, the frames go
// and the report stays. A cheap mirror that lost the state would be cheap and
// wrong.
func TestFramesAreShedBeforeState(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, _ := testMirror(t, f, func(c *Config) { c.ByteBudgetPerSec = MinByteBudget })
	// Spend the bucket, then offer a big screen and a state change together.
	m.budget.tokens = 0
	m.budget.now = func() time.Time { return time.Unix(0, 0) } // no refill
	m.budget.last = time.Unix(0, 0)

	m.Write(line(map[string]any{"type": protocol.EventSnapshot, "pane": 0, "state": "awaiting_input"}))
	rows := make([]protocol.Line, 0, 40)
	for y := 0; y < 40; y++ {
		rows = append(rows, protocol.Line{Y: y, T: strings.Repeat("x", 200)})
	}
	m.Write(frameLine(0, 1, true, 40, rows...))

	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	p := f.Patches()[0]
	if _, ok := p.Payload["panes/p0/state"]; !ok {
		t.Error("state was shed; only frames may be")
	}
	if _, ok := p.Payload["panes/p0/frame"]; ok {
		t.Error("a frame was sent with an empty budget")
	}

	// The frame is not LOST — it is still pending, and the next tick with
	// tokens carries it.
	m.budget.tokens = MinByteBudget
	if err := m.flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Patches()[1].Payload["panes/p0/frame"]; !ok {
		t.Error("the shed frame was dropped instead of deferred")
	}
}

// TestFinalFlushWritesAliveFalse: the clean-shutdown marker, and the frames
// with it — there is no next tick to carry them.
func TestFinalFlushWritesAliveFalse(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, sess := testMirror(t, f, nil)
	m.SetSessionMeta(map[string]any{"alive": true, "pid": 9})
	m.Write(line(map[string]any{"type": protocol.EventResults, "panes": []map[string]any{
		{"pane": 0, "state": "completed", "exitCode": 0, "dead": true},
	}}))
	m.FinalFlush(context.Background(), nil)

	meta, _ := f.Value(sess + "/meta").(map[string]any)
	if meta["alive"] != false {
		t.Fatalf("meta = %v; want alive:false", meta)
	}
	if meta["pid"] != float64(9) {
		t.Errorf("the final flush lost a meta field: %v", meta)
	}
	st, _ := f.Value(sess + "/panes/p0/state").(map[string]any)
	if st["state"] != "completed" {
		t.Errorf("results did not reach the mirror: %v", st)
	}
}

// TestWriteNeverFailsAndNeverBlocks pins the Sink contract this adapter relies
// on: the hub's torn-write rule is about bytes half-written to a connection,
// and there is no connection here.
func TestWriteNeverFailsAndNeverBlocks(t *testing.T) {
	f := newFakeRTDB(t)
	m, _, _ := testMirror(t, f, nil)
	msg := line(map[string]any{"type": protocol.EventControl})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			n, err := m.Write(msg)
			if err != nil || n != len(msg) {
				t.Errorf("Write = (%d, %v)", n, err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked")
	}
	if err := m.SetWriteDeadline(time.Now()); err != nil {
		t.Errorf("SetWriteDeadline = %v", err)
	}
	m.Close("done")
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfRaw(m map[string]json.RawMessage) []string { return keysOf(m) }

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

var _ = fmt.Sprintf
