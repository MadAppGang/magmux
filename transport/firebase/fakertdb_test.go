package firebase

// A fake Realtime Database over httptest.
//
// It is deliberately NOT a mock with expectations. It is a small server that
// accepts the requests RTDB accepts, refuses the ones RTDB refuses, and records
// what it was sent — so a test asserts on the WIRE rather than on which methods
// were called. The three RTDB behaviours that magmux's code exists to handle are
// all in here and all programmable: the 307 to the real instance, the 400 that
// names no path, and the SSE stream.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// patchRecord is one multi-path PATCH as it arrived.
type patchRecord struct {
	Path    string
	Payload map[string]json.RawMessage
	Auth    string
	Query   url.Values
}

// fakeRTDB is the server.
type fakeRTDB struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	data    map[string]json.RawMessage
	patches []patchRecord
	puts    []patchRecord
	// status maps a request index (0-based, PATCH only) to a status to answer
	// with instead of 204. Used for the 400 and 429 cases.
	statusFor func(n int, rec patchRecord) int
	patchN    int

	// sse is the channel of raw event frames the command stream emits.
	sse chan string
	// ssePaths records the node each SSE subscription was opened on. A
	// restarted magmux must listen on a DIFFERENT path, and this is how a test
	// sees that rather than inferring it.
	ssePaths []string

	// ancestorViolations counts PATCH bodies that held both an ancestor and a
	// descendant path. RTDB refuses those with a 400; this test server counts
	// them so the assertion can name the bug rather than the symptom.
	ancestorViolations int
}

func newFakeRTDB(t *testing.T) *fakeRTDB {
	t.Helper()
	f := &fakeRTDB{
		t:    t,
		data: make(map[string]json.RawMessage),
		sse:  make(chan string, 64),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRTDB) URL() string  { return f.srv.URL }
func (f *fakeRTDB) Host() string { u, _ := url.Parse(f.srv.URL); return u.Host }

func (f *fakeRTDB) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".json")
	switch r.Method {
	case http.MethodGet:
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			f.serveSSE(w, r, path)
			return
		}
		f.mu.Lock()
		v := f.subtree(path)
		f.mu.Unlock()
		b, _ := json.Marshal(v)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	case http.MethodPatch:
		f.servePatch(w, r, path)
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		var v json.RawMessage = body
		f.mu.Lock()
		f.puts = append(f.puts, patchRecord{Path: path, Auth: r.Header.Get("Authorization"), Query: r.URL.Query()})
		f.data[path] = v
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.data, path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeRTDB) servePatch(w http.ResponseWriter, r *http.Request, path string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	rec := patchRecord{Path: path, Payload: payload, Auth: r.Header.Get("Authorization"), Query: r.URL.Query()}

	f.mu.Lock()
	n := f.patchN
	f.patchN++
	f.patches = append(f.patches, rec)
	if hasAncestorPair(payload) {
		f.ancestorViolations++
		f.mu.Unlock()
		// RTDB's own answer to this, and it names nothing useful either.
		http.Error(w, `{"error":"Invalid path: an ancestor and a descendant cannot both be written"}`, http.StatusBadRequest)
		return
	}
	statusFor := f.statusFor
	f.mu.Unlock()

	if statusFor != nil {
		if st := statusFor(n, rec); st != 0 && st != http.StatusNoContent {
			http.Error(w, fmt.Sprintf(`{"error":"programmed %d"}`, st), st)
			return
		}
	}

	f.mu.Lock()
	for k, v := range payload {
		full := path + "/" + strings.TrimPrefix(k, "/")
		if string(v) == "null" {
			f.deleteSubtreeLocked(full)
			continue
		}
		// A write to a node REPLACES its subtree, which is the behaviour a
		// keyframe depends on: rows past a shrink must vanish.
		f.deleteSubtreeLocked(full)
		f.data[full] = v
	}
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeRTDB) deleteSubtreeLocked(prefix string) {
	for k := range f.data {
		if k == prefix || strings.HasPrefix(k, prefix+"/") {
			delete(f.data, k)
		}
	}
}

// subtree renders everything stored at or under path as one value, so a test
// can read a node back the way a client would.
func (f *fakeRTDB) subtree(path string) any {
	if v, ok := f.data[path]; ok {
		var out any
		_ = json.Unmarshal(v, &out)
		return out
	}
	out := map[string]any{}
	found := false
	for k, v := range f.data {
		if !strings.HasPrefix(k, path+"/") {
			continue
		}
		found = true
		var decoded any
		_ = json.Unmarshal(v, &decoded)
		cur := out
		parts := strings.Split(strings.TrimPrefix(k, path+"/"), "/")
		for i, p := range parts {
			if i == len(parts)-1 {
				cur[p] = decoded
				break
			}
			next, ok := cur[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[p] = next
			}
			cur = next
		}
	}
	if !found {
		return nil
	}
	return out
}

// Value reads one stored path.
func (f *fakeRTDB) Value(path string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subtree(path)
}

// Patches returns a copy of every PATCH received.
func (f *fakeRTDB) Patches() []patchRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]patchRecord(nil), f.patches...)
}

// SetStatus installs the programmed-status hook.
func (f *fakeRTDB) SetStatus(fn func(n int, rec patchRecord) int) {
	f.mu.Lock()
	f.statusFor = fn
	f.mu.Unlock()
}

// serveSSE holds one command stream open and writes whatever Emit sends.
func (f *fakeRTDB) serveSSE(w http.ResponseWriter, r *http.Request, path string) {
	f.mu.Lock()
	f.ssePaths = append(f.ssePaths, path)
	f.mu.Unlock()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-f.sse:
			if !ok {
				return
			}
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// Emit queues one SSE event onto the command stream.
func (f *fakeRTDB) Emit(name string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		f.t.Fatalf("emit %s: %v", name, err)
	}
	f.sse <- "event: " + name + "\ndata: " + string(b) + "\n\n"
}

// EmitRaw queues an event whose data is already encoded.
func (f *fakeRTDB) EmitRaw(name, data string) {
	f.sse <- "event: " + name + "\ndata: " + data + "\n\n"
}

// StreamsOpened is how many SSE subscriptions the server has served.
func (f *fakeRTDB) StreamsOpened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ssePaths)
}

// StreamPaths is the node each SSE subscription was opened on.
func (f *fakeRTDB) StreamPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ssePaths...)
}

// hasAncestorPair reports whether a PATCH body holds both a path and a path
// below it. RTDB refuses the pair, so the mirror must never build one.
func hasAncestorPair(payload map[string]json.RawMessage) bool {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, strings.Trim(k, "/"))
	}
	for i, a := range keys {
		for j, b := range keys {
			if i == j {
				continue
			}
			if strings.HasPrefix(b, a+"/") {
				return true
			}
		}
	}
	return false
}

// testClient builds a client pointed at the fake, with the auth predicate a
// test wants. It is the production assembly minus the credential: the fake is
// an emulator as far as this package is concerned.
func testClient(t *testing.T, base string, allow func(*url.URL) bool) *client {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %q: %v", base, err)
	}
	if allow == nil {
		host := u.Host
		allow = func(target *url.URL) bool { return target != nil && target.Host == host }
	}
	return &client{
		base:    u,
		tokens:  staticToken(emulatorToken),
		hc:      newHTTPClient(allow),
		authFor: allow,
	}
}

// waitFor polls until cond is true or the deadline passes. Polling rather than
// sleeping a fixed time: the flusher's tick is 500 ms and a fixed sleep would
// either be flaky or slow, and this is neither.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}
