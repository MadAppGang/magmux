package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/sse"
	"github.com/MadAppGang/magmux/transport/ws"
)

// ── harness ─────────────────────────────────────────────────────────────────

type fixture struct {
	srv   *Server
	hub   *hub.Hub
	full  string
	view  string
	base  string
	calls chan string
}

// newFixture builds a server over a REAL hub with fake ops registered, and a
// real loopback listener.
//
// A real hub rather than a mock: every rule this package enforces is a rule
// about what reaches the registry, and a fake registry could not tell a refusal
// made here from one the hub would have made anyway.
func newFixture(t *testing.T, opts ...func(*Config)) *fixture {
	t.Helper()
	h := hub.New()
	calls := make(chan string, 64)
	record := func(name string) hub.OpFunc {
		return func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
			select {
			case calls <- name:
			default:
			}
			return map[string]any{"op": name, "args": string(args), "client": c.Client, "readOnly": c.ReadOnly}, nil
		}
	}
	ops := []hub.Op{
		{Spec: protocol.OpSpec{Name: "capabilities", Class: protocol.ClassRead}, Fn: record("capabilities")},
		{Spec: protocol.OpSpec{Name: "list", Class: protocol.ClassRead}, Fn: record("list")},
		{Spec: protocol.OpSpec{Name: "ops", Class: protocol.ClassRead}, Fn: record("ops")},
		{Spec: protocol.OpSpec{Name: "capture", Class: protocol.ClassRead}, Fn: record("capture")},
		{Spec: protocol.OpSpec{Name: "send", Class: protocol.ClassControl}, Fn: record("send")},
		{Spec: protocol.OpSpec{Name: "input", Class: protocol.ClassInput}, Fn: record("input")},
		{Spec: protocol.OpSpec{Name: "boom", Class: protocol.ClassRead}, Fn: func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
			var a struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(args, &a)
			if a.Code == "" {
				a.Code = protocol.CodeInternal
			}
			return nil, protocol.Errf(a.Code, "deliberate %s", a.Code)
		}},
	}
	if err := h.Register(protocol.SourceBuiltin, ops...); err != nil {
		t.Fatal(err)
	}

	full, _ := auth.Generate()
	view, _ := auth.Generate()
	cfg := Config{
		Addr:      "127.0.0.1:0",
		Hub:       h,
		Tokens:    auth.NewStore(full, view, nil),
		Tickets:   auth.NewTickets(0),
		Aggregate: func() []byte { return []byte(`{"type":"snapshot","panes":[]}` + "\n") },
		Ready:     func(time.Duration) bool { return true },
		ErrorLog:  log.New(io.Discard, "", 0),
	}
	for _, o := range opts {
		o(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go srv.Serve()
	t.Cleanup(srv.Close)

	return &fixture{srv: srv, hub: h, full: full, view: view, base: "http://" + srv.Addr().String(), calls: calls}
}

func (f *fixture) do(t *testing.T, method, path, token string, body io.Reader, mut ...func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, m := range mut {
		m(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body %q: %v", b, err)
	}
	return m
}

// ── the auth matrix ─────────────────────────────────────────────────────────

// TestAuthMatrix is the whole credential surface in one table: what is
// accepted, what is refused, and with which status.
func TestAuthMatrix(t *testing.T) {
	f := newFixture(t)
	other, _ := auth.Generate()

	for _, tc := range []struct {
		name   string
		method string
		path   string
		mut    func(*http.Request)
		want   int
	}{
		{"no credential at all", "GET", "/v1/panes", nil, 401},
		{"a wrong bearer token", "GET", "/v1/panes", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+other)
		}, 401},
		{"an empty bearer token", "GET", "/v1/panes", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer ")
		}, 401},
		{"a Basic credential", "GET", "/v1/panes", func(r *http.Request) {
			r.Header.Set("Authorization", "Basic aGk6dGhlcmU=")
		}, 401},
		{"the session token", "GET", "/v1/panes", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+f.full)
		}, 200},
		{"the session token, lower-case scheme", "GET", "/v1/panes", func(r *http.Request) {
			r.Header.Set("Authorization", "bearer "+f.full)
		}, 200},
		{"the view token on a read op", "GET", "/v1/panes", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+f.view)
		}, 200},
		{"a ticket is refused on an endpoint that may not take one", "GET", "/v1/panes?ticket=x", nil, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.do(t, tc.method, tc.path, "", nil, orNop(tc.mut))
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == 401 {
				if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
					t.Errorf("a 401 must say how to authenticate, got %q", got)
				}
				if code := decode(t, resp)["code"]; code != protocol.CodeUnauthorized {
					t.Errorf("code = %v, want %q", code, protocol.CodeUnauthorized)
				}
			}
		})
	}
}

// TestViewTokenIsRefusedEverythingButReads is D1 on the wire: 403, not 401 —
// the credential is genuine and the permission is not.
func TestViewTokenIsRefusedEverythingButReads(t *testing.T) {
	f := newFixture(t)

	for _, op := range []string{"send", "input"} {
		resp := f.do(t, "POST", "/v1/ops/"+op, f.view, strings.NewReader(`{"pane":0}`))
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s with the view token: status = %d, want 403", op, resp.StatusCode)
		}
		body := decode(t, resp)
		if body["code"] != protocol.CodeForbidden {
			t.Errorf("%s: code = %v, want forbidden", op, body["code"])
		}
		if body["ok"] != false {
			t.Errorf("%s: ok = %v, want false", op, body["ok"])
		}
	}

	// And the refusal happens BEFORE the hub: the op never ran.
	select {
	case name := <-f.calls:
		t.Fatalf("a refused op still reached the registry: %q", name)
	default:
	}

	// The same ops with the session token do run.
	for _, op := range []string{"send", "input"} {
		resp := f.do(t, "POST", "/v1/ops/"+op, f.full, strings.NewReader(`{"pane":0}`))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s with the session token: status = %d", op, resp.StatusCode)
		}
	}

	// A read op with the view token runs, and the hub is told the caller is
	// read-only, so the registry's own class check agrees with ours.
	resp := f.do(t, "POST", "/v1/ops/capture", f.view, strings.NewReader(`{"pane":0}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capture with the view token: status = %d", resp.StatusCode)
	}
	result := decode(t, resp)["result"].(map[string]any)
	if result["readOnly"] != true {
		t.Errorf("the hub Caller must carry ReadOnly for a viewer, got %v", result["readOnly"])
	}
}

// TestTicketsAreSingleUseOnTheWire covers mint, redeem, reuse and expiry as
// they are actually experienced by an EventSource.
func TestTicketsAreSingleUseOnTheWire(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.Tickets = auth.NewTickets(120 * time.Millisecond) })

	// Minting needs a real credential.
	if resp := f.do(t, "POST", "/v1/tickets", "", nil); resp.StatusCode != 401 {
		t.Fatalf("minting without a token: %d", resp.StatusCode)
	}

	resp := f.do(t, "POST", "/v1/tickets", f.full, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("mint: %d", resp.StatusCode)
	}
	body := decode(t, resp)
	ticket, _ := body["ticket"].(string)
	if ticket == "" {
		t.Fatal("no ticket in the reply")
	}
	if body["expiresIn"] == nil {
		t.Error("a client needs expiresIn to schedule a re-mint instead of discovering a 401")
	}

	// The raw token must never be what travels in the URL; the ticket is a
	// different value entirely.
	if ticket == f.full || ticket == f.view {
		t.Fatal("the ticket must not be the token itself")
	}

	// First use: accepted.
	r1 := f.do(t, "GET", "/v1/events?ticket="+ticket, "", nil, func(r *http.Request) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Cleanup(cancel)
		*r = *r.WithContext(ctx)
	})
	if r1.StatusCode != 200 {
		t.Fatalf("first use of a ticket: %d", r1.StatusCode)
	}
	r1.Body.Close()

	// Second use: refused, and as a 401, which EventSource treats as fatal
	// rather than looping on.
	r2 := f.do(t, "GET", "/v1/events?ticket="+ticket, "", nil)
	if r2.StatusCode != 401 {
		t.Fatalf("reusing a ticket: %d, want 401", r2.StatusCode)
	}

	// An expired ticket is the same refusal.
	resp = f.do(t, "POST", "/v1/tickets", f.full, nil)
	expired, _ := decode(t, resp)["ticket"].(string)
	time.Sleep(200 * time.Millisecond)
	r3 := f.do(t, "GET", "/v1/events?ticket="+expired, "", nil)
	if r3.StatusCode != 401 {
		t.Fatalf("an expired ticket: %d, want 401", r3.StatusCode)
	}

	// A viewer's ticket inherits ReadOnly: it is not a way up.
	resp = f.do(t, "POST", "/v1/tickets", f.view, nil)
	vb := decode(t, resp)
	if vb["readOnly"] != true {
		t.Errorf("a viewer's ticket must be marked read-only, got %v", vb["readOnly"])
	}
}

// ── origin, host, CORS ──────────────────────────────────────────────────────

// TestOriginAndHostChecks covers the two browser-facing guards. The Host check
// is DNS rebinding: an <img> or a form post carries no Origin at all, so Origin
// alone would not close it.
func TestOriginAndHostChecks(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.AllowOrigins = []string{"https://app.example"} })
	self := "http://" + f.srv.Addr().String()

	for _, tc := range []struct {
		name   string
		origin string
		host   string
		want   int
	}{
		{"no Origin at all is allowed (curl, an agent, a script)", "", "", 200},
		{"the request's own origin", self, "", 200},
		{"an --allow-origin origin", "https://app.example", "", 200},
		{"an origin nobody allowed", "https://evil.example", "", 403},
		{"a near-miss of an allowed origin", "https://app.example.evil", "", 403},
		{"the allowed origin on the wrong scheme", "http://app.example", "", 403},
		{"a rebound hostname", "", "attacker.example", 403},
		{"a rebound hostname with our port", "", "attacker.example:1", 403},
		// localhost is a loopback NAME, which is the whole of the check: the
		// port is not part of it, because a loopback bind is only reachable
		// from this machine whatever port the Host claims.
		{"localhost by name", "", "localhost", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.do(t, "GET", "/v1/panes", f.full, nil, func(r *http.Request) {
				if tc.origin != "" {
					r.Header.Set("Origin", tc.origin)
				}
				if tc.host != "" {
					r.Host = tc.host
				}
			})
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == 403 && decode(t, resp)["code"] != protocol.CodeForbidden {
				t.Error("a refusal here must carry the forbidden code")
			}
		})
	}
}

func TestHostIsLoopbackTable(t *testing.T) {
	for _, tc := range []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1:7777", true},
		{"127.0.0.1", true},
		{"127.1.2.3:1", true},
		{"localhost:7777", true},
		{"LOCALHOST", true},
		{"[::1]:7777", true},
		{"[::1]", true},
		{"0.0.0.0:7777", false},
		{"192.168.1.9:7777", false},
		{"attacker.example", false},
		{"localhost.attacker.example", false},
		{"", false},
		{"::1", false}, // a bare IPv6 literal is not a legal Host value
	} {
		if got := hostIsLoopback(tc.host); got != tc.ok {
			t.Errorf("hostIsLoopback(%q) = %v, want %v", tc.host, got, tc.ok)
		}
	}
}

func TestAddrIsLoopbackTable(t *testing.T) {
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:7777", true},
		{"localhost:7777", true},
		{"[::1]:7777", true},
		{"0.0.0.0:7777", false},
		{":7777", false},
		{"192.168.1.9:7777", false},
		{"tailscale-host:7777", false},
	} {
		if got := AddrIsLoopback(tc.addr); got != tc.ok {
			t.Errorf("AddrIsLoopback(%q) = %v, want %v", tc.addr, got, tc.ok)
		}
	}
}

// TestCORSPreflight pins the preflight, including the two headers whose absence
// is the point: no wildcard, and no credentials.
func TestCORSPreflight(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.AllowOrigins = []string{"https://app.example"} })

	// Preflight carries NO credential, by design: the browser is asking
	// permission to send the Authorization header it is not sending yet.
	resp := f.do(t, "OPTIONS", "/v1/ops/list", "", nil, func(r *http.Request) {
		r.Header.Set("Origin", "https://app.example")
		r.Header.Set("Access-Control-Request-Method", "POST")
		r.Header.Set("Access-Control-Request-Headers", "authorization")
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", resp.StatusCode)
	}
	h := resp.Header
	if got := h.Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("ACAO = %q, want the exact origin", got)
	}
	if strings.Contains(h.Get("Access-Control-Allow-Origin"), "*") {
		t.Error("ACAO must never be a wildcard: this API is remote code execution")
	}
	if got := h.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, must include Origin or a cache will serve one origin's answer to another", got)
	}
	if got := h.Get("Access-Control-Allow-Methods"); got != "GET, POST" {
		t.Errorf("Allow-Methods = %q", got)
	}
	for _, want := range []string{"Authorization", "Content-Type", "X-Magmux-Client"} {
		if !strings.Contains(h.Get("Access-Control-Allow-Headers"), want) {
			t.Errorf("Allow-Headers is missing %s: %q", want, h.Get("Access-Control-Allow-Headers"))
		}
	}
	if h.Get("Access-Control-Max-Age") != "600" {
		t.Errorf("Max-Age = %q", h.Get("Access-Control-Max-Age"))
	}
	if h.Get("Access-Control-Allow-Credentials") != "" {
		t.Error("magmux authenticates with a bearer token and has no cookies; " +
			"Allow-Credentials would tell the browser to attach ambient ones")
	}

	// An origin nobody allowed gets a refusal, not a silent 204 with no CORS
	// headers — the browser's error is then about magmux rather than about CORS.
	resp = f.do(t, "OPTIONS", "/v1/ops/list", "", nil, func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("preflight from an unlisted origin = %d, want 403", resp.StatusCode)
	}

	// An allowed, authenticated request carries ACAO + Vary too, or the
	// browser discards a response the preflight already permitted.
	resp = f.do(t, "GET", "/v1/panes", f.full, nil, func(r *http.Request) {
		r.Header.Set("Origin", "https://app.example")
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Error("the actual response needs ACAO as well as the preflight")
	}

	// A same-origin request gets no CORS headers, because CORS applies only to
	// --allow-origin origins.
	resp = f.do(t, "GET", "/v1/panes", f.full, nil, func(r *http.Request) {
		r.Header.Set("Origin", "http://"+f.srv.Addr().String())
	})
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("a same-origin request needs no CORS headers and must not get them")
	}
}

// ── limits ──────────────────────────────────────────────────────────────────

// TestBodyLimit: a body past 1 MB is 413 too_large, and the op never runs.
func TestBodyLimit(t *testing.T) {
	f := newFixture(t)

	ok := `{"text":"` + strings.Repeat("a", 1000) + `"}`
	if resp := f.do(t, "POST", "/v1/ops/send", f.full, strings.NewReader(ok)); resp.StatusCode != 200 {
		t.Fatalf("a small body = %d", resp.StatusCode)
	}
	<-f.calls

	big := `{"text":"` + strings.Repeat("a", MaxBody+1024) + `"}`
	resp := f.do(t, "POST", "/v1/ops/send", f.full, strings.NewReader(big))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body = %d, want 413", resp.StatusCode)
	}
	if code := decode(t, resp)["code"]; code != protocol.CodeTooLarge {
		t.Errorf("code = %v, want too_large", code)
	}
	select {
	case name := <-f.calls:
		t.Fatalf("an oversized body still reached the op: %q", name)
	default:
	}
}

func TestBodyMustBeAnObject(t *testing.T) {
	f := newFixture(t)
	for _, body := range []string{`[1,2,3]`, `"a string"`, `42`, `{`} {
		resp := f.do(t, "POST", "/v1/ops/list", f.full, strings.NewReader(body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, resp.StatusCode)
		}
	}
	// An absent body is an empty args object, because half the ops take none.
	if resp := f.do(t, "POST", "/v1/ops/list", f.full, nil); resp.StatusCode != 200 {
		t.Errorf("an empty body must be accepted, got %d", resp.StatusCode)
	}
}

// TestConnectionCapAnswersRatherThanDropping: the 65th connection gets a 503
// with Retry-After, not a TCP reset. A client that gets a reset cannot tell an
// overloaded magmux from one that is not running.
func TestConnectionCapAnswersRatherThanDropping(t *testing.T) {
	const cap = 4
	f := newFixture(t, func(c *Config) { c.MaxConns = cap })
	addr := f.srv.Addr().String()

	// Hold `cap` connections open without letting net/http finish them.
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < cap; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	// Wait for the accept loop to have counted all of them.
	deadline := time.Now().Add(2 * time.Second)
	for f.srv.Conns() < cap && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if f.srv.Conns() != cap {
		t.Fatalf("server counts %d connections, want %d", f.srv.Conns(), cap)
	}

	// The next one is answered, and answered with the right thing.
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "http://"+addr+"/v1/panes", nil)
	req.Header.Set("Authorization", "Bearer "+f.full)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("the connection past the cap was dropped rather than answered: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", resp.Header.Get("Retry-After"))
	}
	if code := decode(t, resp)["code"]; code != protocol.CodeBusy {
		t.Errorf("code = %v, want busy", code)
	}

	// Releasing one lets the next through: the cap is a live count, not a
	// high-water mark.
	held[0].Close()
	held = held[1:]
	for f.srv.Conns() >= cap && time.Now().Before(deadline.Add(2*time.Second)) {
		time.Sleep(2 * time.Millisecond)
	}
	resp2, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("after a release, status = %d, want 200", resp2.StatusCode)
	}
}

// ── the status map ──────────────────────────────────────────────────────────

// TestCodeToStatusMap is the table, and then the same table over the wire so a
// mapping cannot be right in the function and unreachable in the handler.
func TestCodeToStatusMap(t *testing.T) {
	want := map[string]int{
		protocol.CodeBadRequest:    400,
		protocol.CodeTooSmall:      400,
		protocol.CodeUnauthorized:  401,
		protocol.CodeForbidden:     403,
		protocol.CodeNoSuchPane:    404,
		protocol.CodeUnknownVerb:   404,
		protocol.CodePaneDead:      409,
		protocol.CodePaneIsControl: 409,
		protocol.CodePaneHidden:    409,
		protocol.CodeNoController:  409,
		protocol.CodeNoTranscript:  409,
		protocol.CodeTooLarge:      413,
		protocol.CodeBusy:          429,
		protocol.CodeUnsupported:   501,
		protocol.CodePluginGone:    502,
		protocol.CodeNotReady:      503,
		protocol.CodeTimeout:       504,
		protocol.CodeInternal:      500,
		"something invented later": 500,
	}
	for code, status := range want {
		if got := StatusFor(code); got != status {
			t.Errorf("StatusFor(%q) = %d, want %d", code, got, status)
		}
	}

	f := newFixture(t)
	for code, status := range want {
		if code == protocol.CodeUnauthorized || code == protocol.CodeForbidden {
			continue // reached through the auth layer, covered above
		}
		body, _ := json.Marshal(map[string]string{"code": code})
		resp := f.do(t, "POST", "/v1/ops/boom", f.full, bytes.NewReader(body))
		if resp.StatusCode != status {
			t.Errorf("an op failing with %q answered %d, want %d", code, resp.StatusCode, status)
		}
		got := decode(t, resp)
		if got["ok"] != false || got["code"] != code {
			t.Errorf("%q: body = %v; the code must survive beside the status, "+
				"because five codes share 409", code, got)
		}
	}

	// An op nobody registered is 404 from the registry, not 403 from the auth
	// layer: a viewer that could tell those apart would have an op-name oracle.
	resp := f.do(t, "POST", "/v1/ops/nosuchop", f.view, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown op = %d, want 404", resp.StatusCode)
	}
	if code := decode(t, resp)["code"]; code != protocol.CodeUnknownVerb {
		t.Errorf("code = %v, want unknown_verb", code)
	}
}

// ── REST ────────────────────────────────────────────────────────────────────

func TestRESTEndpoints(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Transports = map[string]any{"http": map[string]any{"tls": false}}
	})

	t.Run("capabilities carries the transports block", func(t *testing.T) {
		resp := f.do(t, "GET", "/v1/capabilities", f.full, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		body := decode(t, resp)
		if body["transports"] == nil {
			t.Error("a client must be able to tell a TLS bind from a plaintext one without probing")
		}
		if body["op"] != "capabilities" {
			t.Errorf("the op's own result must be there too: %v", body)
		}
	})

	t.Run("panes is list", func(t *testing.T) {
		resp := f.do(t, "GET", "/v1/panes", f.full, nil)
		if decode(t, resp)["op"] != "list" {
			t.Error("GET /v1/panes must be the list op")
		}
	})

	t.Run("screen is capture with the pane in the path", func(t *testing.T) {
		resp := f.do(t, "GET", "/v1/panes/3/screen?lines=10&offset=2", f.full, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(decode(t, resp)["args"].(string)), &args); err != nil {
			t.Fatal(err)
		}
		if args["pane"] != float64(3) || args["lines"] != float64(10) || args["offset"] != float64(2) {
			t.Errorf("args = %v", args)
		}
	})

	t.Run("a non-numeric pane is a 400", func(t *testing.T) {
		if resp := f.do(t, "GET", "/v1/panes/abc/screen", f.full, nil); resp.StatusCode != 400 {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		if resp := f.do(t, "GET", "/v1/panes/3/screen?lines=abc", f.full, nil); resp.StatusCode != 400 {
			t.Errorf("a non-numeric lines = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("POST /v1/ops/{name} uses the ok/result envelope", func(t *testing.T) {
		resp := f.do(t, "POST", "/v1/ops/list", f.full, strings.NewReader(`{"a":1}`))
		body := decode(t, resp)
		if body["ok"] != true {
			t.Fatalf("ok = %v", body["ok"])
		}
		if body["result"] == nil {
			t.Fatal("the generic endpoint wraps the op's result")
		}
	})

	t.Run("X-Magmux-Client is a panel label", func(t *testing.T) {
		resp := f.do(t, "POST", "/v1/ops/list", f.full, nil, func(r *http.Request) {
			r.Header.Set("X-Magmux-Client", "web/0.1")
		})
		result := decode(t, resp)["result"].(map[string]any)
		if result["client"] != "web/0.1" {
			t.Errorf("client = %v", result["client"])
		}
	})

	t.Run("an unknown endpoint is a 404 in magmux's own shape", func(t *testing.T) {
		resp := f.do(t, "GET", "/nope", f.full, nil)
		if resp.StatusCode != 404 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if decode(t, resp)["code"] != protocol.CodeUnknownVerb {
			t.Error("even a routing miss answers in the protocol's vocabulary")
		}
	})
}

// TestNotReadyBeforeTheLayout: a streaming endpoint refuses with 503 rather
// than opening a stream that would report an empty session.
func TestNotReadyBeforeTheLayout(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.Ready = func(time.Duration) bool { return false } })
	for _, path := range []string{"/v1/events", "/v1/ws"} {
		resp := f.do(t, "GET", path, f.full, nil, func(r *http.Request) {
			if path == "/v1/ws" {
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Upgrade", "websocket")
				r.Header.Set("Sec-WebSocket-Version", "13")
				r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			}
		})
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s with no layout = %d, want 503", path, resp.StatusCode)
		}
		if decode(t, resp)["code"] != protocol.CodeNotReady {
			t.Errorf("%s: want not_ready", path)
		}
	}
}

// ── SSE ─────────────────────────────────────────────────────────────────────

// TestSSEDeliversTheAggregateFirst is the subscribe cut as a client sees it.
func TestSSEDeliversTheAggregateFirst(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", f.base+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.full)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Error("an SSE stream must not be cached")
	}

	r := sse.NewReader(resp.Body)
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if ev.Name != "snapshot" {
		t.Errorf("the first event is the aggregate, got %q", ev.Name)
	}
	if !strings.Contains(string(ev.Data), `"panes"`) {
		t.Errorf("aggregate data = %q", ev.Data)
	}
	if ev.ID != "1" {
		t.Errorf("id = %q; it is a per-connection counter, so the first is 1", ev.ID)
	}

	// A published event follows it, framed as its own type.
	f.hub.Publish([]byte(`{"type":"pane_opened","pane":4}` + "\n"))
	ev, err = r.Next()
	if err != nil {
		t.Fatalf("second event: %v", err)
	}
	if ev.Name != "pane_opened" || !strings.Contains(string(ev.Data), `"pane":4`) {
		t.Errorf("second event = %+v", ev)
	}
	if ev.ID != "2" {
		t.Errorf("id = %q, want 2", ev.ID)
	}
}

// TestSSEGetsResultsThenShutdownThenEOF is the teardown contract on this
// transport: the two finals, then the end of the body.
func TestSSEGetsResultsThenShutdownThenEOF(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", f.base+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.full)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	r := sse.NewReader(resp.Body)
	if ev, err := r.Next(); err != nil || ev.Name != "snapshot" {
		t.Fatalf("aggregate: %+v %v", ev, err)
	}

	f.hub.Quiesce(50 * time.Millisecond)
	f.hub.Finalize(
		[]byte(`{"type":"results","panes":[]}`+"\n"),
		[]byte(`{"type":"shutdown"}`+"\n"))

	ev, err := r.Next()
	if err != nil || ev.Name != "results" {
		t.Fatalf("want results, got %+v %v", ev, err)
	}
	ev, err = r.Next()
	if err != nil || ev.Name != "shutdown" {
		t.Fatalf("want shutdown, got %+v %v", ev, err)
	}
	if _, err := r.Next(); err == nil {
		t.Fatal("the body must end after shutdown")
	}
}

// TestSSESinkReportsAFailedFlushAsTorn is the unit half of
// TestFinalizeTornWriteSSE: a buffered Write that succeeded tells you nothing,
// and the real failure surfaces in Flush — by which time part of a data line
// may already be on the wire.
func TestSSESinkReportsAFailedFlushAsTorn(t *testing.T) {
	fw := &flushFailWriter{}
	sink := newSSESink(fw)
	n, err := sink.Write([]byte(`{"type":"frame"}` + "\n"))
	if err == nil {
		t.Fatal("a failing Flush must be reported as a failed write")
	}
	if n < 1 {
		t.Fatalf("n = %d; an SSE sink must report every failure as torn (n >= 1), "+
			"or a final could be spliced onto a half-written data line", n)
	}
}

// flushFailWriter accepts every Write and fails every Flush, which is exactly
// what a buffered HTTP response does when the peer has stopped reading.
type flushFailWriter struct{ hdr http.Header }

func (f *flushFailWriter) Header() http.Header {
	if f.hdr == nil {
		f.hdr = http.Header{}
	}
	return f.hdr
}
func (f *flushFailWriter) Write(p []byte) (int, error) { return len(p), nil }
func (f *flushFailWriter) WriteHeader(int)             {}
func (f *flushFailWriter) FlushError() error           { return fmt.Errorf("peer is not reading") }

// ── WebSocket ───────────────────────────────────────────────────────────────

func dialWS(t *testing.T, f *fixture, hdr http.Header, query string) (*ws.ClientConn, *http.Response) {
	t.Helper()
	addr := f.srv.Addr().String()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cc, resp, err := ws.ClientHandshake(c, addr, "/v1/ws"+query, hdr)
	if err != nil {
		c.Close()
		if resp == nil {
			t.Fatalf("handshake: %v", err)
		}
		return nil, resp
	}
	t.Cleanup(func() { cc.Close() })
	return cc, resp
}

func TestWebSocketAuthHappensBeforeTheUpgrade(t *testing.T) {
	f := newFixture(t)

	// No credential: an ordinary 401, never a close code. A browser cannot read
	// a close code reliably, and a client that had to upgrade to learn it was
	// unauthorised has already been given a connection.
	if cc, resp := dialWS(t, f, nil, ""); cc != nil || resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// A wrong token in the subprotocol: also a 401.
	other, _ := auth.Generate()
	h := http.Header{}
	h.Set("Sec-WebSocket-Protocol", "magmux.v1, magmux.auth."+other)
	if cc, resp := dialWS(t, f, h, ""); cc != nil || resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// The right token in the subprotocol: upgraded, and magmux.v1 echoed on
	// its own.
	h = http.Header{}
	h.Set("Sec-WebSocket-Protocol", "magmux.v1, magmux.auth."+f.full)
	cc, resp := dialWS(t, f, h, "")
	if cc == nil {
		t.Fatalf("handshake refused: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "magmux.v1" {
		t.Errorf("echoed subprotocol = %q, want exactly magmux.v1", got)
	}
	if strings.Contains(resp.Header.Get("Sec-WebSocket-Protocol"), f.full) {
		t.Fatal("the token must NEVER be echoed into a response header")
	}

	// A Bearer header works too, for a non-browser client.
	h2 := http.Header{}
	h2.Set("Authorization", "Bearer "+f.full)
	if cc, resp := dialWS(t, f, h2, ""); cc == nil {
		t.Fatalf("Bearer over WS refused: %d", resp.StatusCode)
	}

	// And a ticket, which is what a browser uses when it cannot set either.
	resp2 := f.do(t, "POST", "/v1/tickets", f.full, nil)
	ticket, _ := decode(t, resp2)["ticket"].(string)
	if cc, r := dialWS(t, f, nil, "?ticket="+ticket); cc == nil {
		t.Fatalf("a ticket over WS refused: %d", r.StatusCode)
	}
	// Single use here as well.
	if cc, r := dialWS(t, f, nil, "?ticket="+ticket); cc != nil || r.StatusCode != 401 {
		t.Fatal("a WebSocket ticket must be single use")
	}
}

func TestWebSocketSession(t *testing.T) {
	f := newFixture(t)
	h := http.Header{}
	h.Set("Sec-WebSocket-Protocol", "magmux.v1, magmux.auth."+f.full)
	cc, _ := dialWS(t, f, h, "")
	if cc == nil {
		t.Fatal("handshake failed")
	}
	_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))

	// The aggregate is the first frame, exactly as it is the first line on the
	// socket.
	op, msg, err := cc.Read()
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if op != ws.OpText {
		t.Fatalf("first frame op = %v", op)
	}
	var first map[string]any
	if err := json.Unmarshal(msg, &first); err != nil {
		t.Fatalf("first frame %q: %v", msg, err)
	}
	if first["type"] != "snapshot" {
		t.Errorf("the first frame must be the aggregate, got %v", first["type"])
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
			return m
		}
	}

	// hello names the client for the panel.
	if err := cc.WriteText(`{"id":1,"op":"hello","args":{"client":"web/0.1"}}`); err != nil {
		t.Fatal(err)
	}
	reply := next()
	if reply["type"] != "reply" || reply["id"] != float64(1) || reply["ok"] != true {
		t.Fatalf("hello reply = %v", reply)
	}
	if res := reply["result"].(map[string]any); res["client"] != "web/0.1" {
		t.Errorf("hello did not record the client: %v", res)
	}

	// An ordinary op, and the client label carried into the hub Caller.
	if err := cc.WriteText(`{"id":2,"op":"list"}`); err != nil {
		t.Fatal(err)
	}
	reply = next()
	if reply["ok"] != true || reply["id"] != float64(2) {
		t.Fatalf("list reply = %v", reply)
	}
	if res := reply["result"].(map[string]any); res["client"] != "web/0.1" {
		t.Errorf("the hub Caller must carry the name hello gave: %v", res)
	}

	// A failing op answers with the protocol code, not an HTTP status: there is
	// no status on this transport.
	if err := cc.WriteText(`{"id":3,"op":"boom","args":{"code":"pane_dead"}}`); err != nil {
		t.Fatal(err)
	}
	reply = next()
	if reply["ok"] != false || reply["code"] != protocol.CodePaneDead {
		t.Fatalf("boom reply = %v", reply)
	}

	// An unknown op.
	if err := cc.WriteText(`{"id":4,"op":"nosuch"}`); err != nil {
		t.Fatal(err)
	}
	if reply = next(); reply["code"] != protocol.CodeUnknownVerb {
		t.Fatalf("unknown op reply = %v", reply)
	}

	// A message with no op.
	if err := cc.WriteText(`{"id":5}`); err != nil {
		t.Fatal(err)
	}
	if reply = next(); reply["code"] != protocol.CodeBadRequest {
		t.Fatalf("no-op reply = %v", reply)
	}

	// An event published on the bus reaches this connection.
	f.hub.Publish([]byte(`{"type":"pane_opened","pane":9}` + "\n"))
	ev := next()
	if ev["type"] != "pane_opened" {
		t.Fatalf("event = %v", ev)
	}
}

// TestWebSocketViewTokenIsRefusedAWriteOp: the same 403 as REST, in this
// transport's own vocabulary, and the op never runs.
func TestWebSocketViewTokenIsRefusedAWriteOp(t *testing.T) {
	f := newFixture(t)
	h := http.Header{}
	h.Set("Sec-WebSocket-Protocol", "magmux.v1, magmux.auth."+f.view)
	cc, _ := dialWS(t, f, h, "")
	if cc == nil {
		t.Fatal("a view token must still be able to OPEN a WebSocket; it is a genuine credential")
	}
	_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cc.Read(); err != nil { // the aggregate
		t.Fatal(err)
	}

	if err := cc.WriteText(`{"id":1,"op":"input","args":{"pane":0,"text":"x"}}`); err != nil {
		t.Fatal(err)
	}
	_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := cc.Read()
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal(msg, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["ok"] != false || reply["code"] != protocol.CodeForbidden {
		t.Fatalf("input from a viewer = %v, want forbidden", reply)
	}
	select {
	case name := <-f.calls:
		t.Fatalf("a refused op still reached the registry: %q", name)
	default:
	}

	// A read op from the same connection works, which is the point of the
	// credential existing at all.
	if err := cc.WriteText(`{"id":2,"op":"capture","args":{"pane":0}}`); err != nil {
		t.Fatal(err)
	}
	_, msg, err = cc.Read()
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(msg, &reply)
	if reply["ok"] != true {
		t.Fatalf("capture from a viewer = %v", reply)
	}
}

// TestWebSocketProtocolViolationsCloseWithTheRightCode.
func TestWebSocketProtocolViolationsCloseWithTheRightCode(t *testing.T) {
	f := newFixture(t)
	h := http.Header{}
	h.Set("Sec-WebSocket-Protocol", "magmux.v1, magmux.auth."+f.full)

	t.Run("binary is 1003", func(t *testing.T) {
		cc, _ := dialWS(t, f, h, "")
		if cc == nil {
			t.Fatal("handshake failed")
		}
		_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
		cc.Read() // aggregate
		if err := cc.Write(ws.OpBinary, []byte{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
		if got := awaitClose(t, cc); got != ws.CloseUnsupportedData {
			t.Errorf("close code = %d, want 1003", got)
		}
	})

	t.Run("invalid UTF-8 is 1007", func(t *testing.T) {
		cc, _ := dialWS(t, f, h, "")
		if cc == nil {
			t.Fatal("handshake failed")
		}
		_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
		cc.Read()
		if err := cc.Write(ws.OpText, []byte{0xff, 0xfe}); err != nil {
			t.Fatal(err)
		}
		if got := awaitClose(t, cc); got != ws.CloseInvalidPayload {
			t.Errorf("close code = %d, want 1007", got)
		}
	})

	t.Run("a ping is answered with a pong", func(t *testing.T) {
		cc, _ := dialWS(t, f, h, "")
		if cc == nil {
			t.Fatal("handshake failed")
		}
		_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
		cc.Read()
		if err := cc.Write(ws.OpPing, []byte("hi")); err != nil {
			t.Fatal(err)
		}
		for {
			op, msg, err := cc.Read()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if op == ws.OpPong {
				if string(msg) != "hi" {
					t.Errorf("a pong must echo the ping's payload, got %q", msg)
				}
				return
			}
		}
	})

	t.Run("a close is echoed", func(t *testing.T) {
		cc, _ := dialWS(t, f, h, "")
		if cc == nil {
			t.Fatal("handshake failed")
		}
		_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
		cc.Read()
		if err := cc.Write(ws.OpClose, ws.ClosePayload(ws.CloseNormal, "bye")); err != nil {
			t.Fatal(err)
		}
		if got := awaitClose(t, cc); got != ws.CloseNormal {
			t.Errorf("close code = %d, want 1000", got)
		}
	})
}

func awaitClose(t *testing.T, cc *ws.ClientConn) int {
	t.Helper()
	_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		op, msg, err := cc.Read()
		if err != nil {
			t.Fatalf("expected a close frame, got %v", err)
		}
		if op != ws.OpClose {
			continue
		}
		code, _, err := ws.ParseClose(msg)
		if err != nil {
			t.Fatalf("close body: %v", err)
		}
		return code
	}
}

// TestWebSocketGetsTheFinalsThenClose1001.
func TestWebSocketGetsTheFinalsThenClose1001(t *testing.T) {
	f := newFixture(t)
	h := http.Header{}
	h.Set("Sec-WebSocket-Protocol", "magmux.v1, magmux.auth."+f.full)
	cc, _ := dialWS(t, f, h, "")
	if cc == nil {
		t.Fatal("handshake failed")
	}
	_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cc.Read(); err != nil {
		t.Fatal(err)
	}

	f.hub.Quiesce(50 * time.Millisecond)
	f.hub.Finalize(
		[]byte(`{"type":"results","panes":[]}`+"\n"),
		[]byte(`{"type":"shutdown"}`+"\n"))

	var seen []string
	for {
		_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
		op, msg, err := cc.Read()
		if err != nil {
			t.Fatalf("after %v: %v", seen, err)
		}
		if op == ws.OpClose {
			code, _, _ := ws.ParseClose(msg)
			if code != ws.CloseGoingAway {
				t.Errorf("close code = %d, want 1001 (going away)", code)
			}
			break
		}
		var m map[string]any
		_ = json.Unmarshal(msg, &m)
		seen = append(seen, fmt.Sprint(m["type"]))
	}
	if len(seen) != 2 || seen[0] != "results" || seen[1] != "shutdown" {
		t.Fatalf("got %v, want results then shutdown then close", seen)
	}
}

// ── TLS ─────────────────────────────────────────────────────────────────────

// TestBadTLSHandshakeWritesNothingToStderr is the raw-mode rule: magmux may be
// holding an alternate screen, and net/http printing a handshake error into it
// corrupts the frame with no way to repaint.
func TestBadTLSHandshakeWritesNothingToStderr(t *testing.T) {
	certFile, keyFile := writeTestCert(t)

	var logged bytes.Buffer
	var mu sync.Mutex
	f := newFixture(t, func(c *Config) {
		c.TLSCert, c.TLSKey = certFile, keyFile
		c.ErrorLog = log.New(lockedWriter{&mu, &logged}, "http: ", 0)
	})

	// Plain HTTP against a TLS port: the handshake fails inside net/http, which
	// is precisely the noise that used to reach stderr.
	conn, err := net.DialTimeout("tcp", f.srv.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(conn, "GET /v1/panes HTTP/1.1\r\nHost: x\r\n\r\n")
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.Read(buf)
	conn.Close()
	// Garbage that is not even a TLS record.
	conn2, err := net.DialTimeout("tcp", f.srv.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn2.Write([]byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01})
	conn2.Close()
	time.Sleep(200 * time.Millisecond)

	// The point is not that nothing was logged — net/http is entitled to
	// complain. It is that the complaint went where magmux sent it, which is
	// never stderr. A server whose ErrorLog is nil writes to log.Default(),
	// i.e. stderr, so this also pins that New never leaves it nil.
	mu.Lock()
	got := logged.String()
	mu.Unlock()
	if !strings.Contains(got, "TLS handshake error") {
		t.Logf("ErrorLog captured: %q", got)
	}
	if f.srv.srv.ErrorLog == nil {
		t.Fatal("ErrorLog must never be nil: net/http then writes to stderr")
	}

	// And the server still works over TLS afterwards.
	if f.srv.URL()[:5] != "https" {
		t.Errorf("URL = %q, want https", f.srv.URL())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestTLSPairMustBeBothOrNeither(t *testing.T) {
	h := hub.New()
	tok, _ := auth.Generate()
	base := Config{Addr: "127.0.0.1:0", Hub: h, Tokens: auth.NewStore(tok, "", nil)}

	base.TLSCert = "/nonexistent.pem"
	if _, err := New(base); err == nil {
		t.Error("--tls-cert without --tls-key must be refused")
	}
	base.TLSCert, base.TLSKey = "", "/nonexistent.key"
	if _, err := New(base); err == nil {
		t.Error("--tls-key without --tls-cert must be refused")
	}
	base.TLSCert, base.TLSKey = "/nonexistent.pem", "/nonexistent.key"
	if _, err := New(base); err == nil {
		t.Error("a TLS pair that cannot be loaded must be refused at bind time")
	}
}

func TestNewRefusesAConfigurationThatCannotBeSecure(t *testing.T) {
	h := hub.New()
	if _, err := New(Config{Addr: "127.0.0.1:0", Hub: h}); err == nil {
		t.Error("magmux must never listen without a token store")
	}
	tok, _ := auth.Generate()
	if _, err := New(Config{Addr: "127.0.0.1:0", Tokens: auth.NewStore(tok, "", nil)}); err == nil {
		t.Error("a server with no hub has nothing to serve")
	}
	if _, err := New(Config{Addr: "256.256.256.256:99999", Hub: h, Tokens: auth.NewStore(tok, "", nil)}); err == nil {
		t.Error("a bind failure must be reported, not swallowed")
	}
}

func orNop(f func(*http.Request)) func(*http.Request) {
	if f == nil {
		return func(*http.Request) {}
	}
	return f
}
