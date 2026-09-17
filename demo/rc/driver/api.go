package main

// The wire, and the evidence it produces.
//
// NOTHING IS SIMULATED. Every action is an HTTP request to magmux and every
// response shown is the response that came back: no sleep standing in for
// work, no message invented to look like an error, no code path that reports a
// result it did not receive. The TUI and the plain `--run N` printer are two
// renderings of the SAME Line stream, which is what stops the demo and its
// regression net drifting into two different stories.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Cred is which of the two credentials a request is made with. It is an
// argument on every single call and never has a default: the difference
// between the two tokens is what this whole driver demonstrates, so no request
// may be made without saying which one it used.
type Cred int

const (
	CredSession Cred = iota // the FULL token — only this process holds it
	CredView                // the read-only token — every mirror holds it
)

func (c Cred) String() string {
	if c == CredView {
		return "VIEW token"
	}
	return "session token"
}

// Role is what a line of evidence IS, so that two renderers can agree on how
// to draw it without either of them parsing the other's text.
type Role int

const (
	RoleRequest Role = iota
	RoleResponse
	RoleNote
	RoleLook
	RoleVerdict
	RoleHeading
)

// Line is one piece of evidence.
type Line struct {
	Role Role
	Text string

	Cred   Cred // RoleRequest
	Status int  // RoleResponse: the HTTP status, or 0 for a request that never completed
	// Dur is the ROUND TRIP, kept as a Duration rather than as whole
	// milliseconds: a loopback op answers in a few hundred microseconds, and an
	// integer ms turns every honest measurement on this demo into "0ms" —
	// which reads as "not measured" and makes the latency meter a flat bar for
	// ever.
	Dur   time.Duration
	OK    bool // RoleVerdict: was this the outcome the action set out to prove?
	Badge string
}

// Sink receives evidence. The TUI collects it; `--run N` prints it.
type Sink interface{ Emit(Line) }

// Convenience constructors, so an action reads as prose.
func req(text string, c Cred) Line { return Line{Role: RoleRequest, Text: text, Cred: c} }
func note(text string) Line        { return Line{Role: RoleNote, Text: text} }
func look(text string) Line        { return Line{Role: RoleLook, Text: text} }
func heading(text string) Line     { return Line{Role: RoleHeading, Text: text} }
func verdict(ok bool, badge, text string) Line {
	return Line{Role: RoleVerdict, OK: ok, Badge: badge, Text: text}
}

// Result is one HTTP exchange.
type Result struct {
	Status int
	Body   map[string]any
	Text   string
	Dur    time.Duration
	OK     bool
	Err    error
}

// ResultPane digs the pane id out of {"ok":true,"result":{"pane":N}}.
func (r Result) ResultPane() (int, bool) {
	res, _ := r.Body["result"].(map[string]any)
	if res == nil {
		return 0, false
	}
	f, ok := res["pane"].(float64)
	return int(f), ok
}

// Code is magmux's own error code, which is carried in the body of every
// refusal beside the message. The status alone is not enough — 409 covers five
// codes — so a caller that branches on failure branches on this.
func (r Result) Code() string {
	s, _ := r.Body["code"].(string)
	return s
}

func (r Result) ErrorMessage() string {
	s, _ := r.Body["error"].(string)
	return s
}

// API is the driver's one HTTP client.
type API struct {
	cfg  Config
	http *http.Client
	sink Sink
}

func NewAPI(cfg Config, sink Sink) *API {
	return &API{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}, sink: sink}
}

func (a *API) token(c Cred) string {
	if c == CredView {
		return a.cfg.ViewToken
	}
	return a.cfg.Token
}

// Brief is one line of JSON, short enough for a panel a quarter of a window
// tall. Newlines OUT: magmux's REST bodies end with one, and a body printed
// with it breaks the request/response pairing that makes this readable.
func Brief(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + fmt.Sprintf(" …(%d chars)", len(r))
}

// Call posts one op and emits both halves of the exchange.
func (a *API) Call(op string, args map[string]any, c Cred) Result {
	body, _ := json.Marshal(args)
	a.sink.Emit(req(fmt.Sprintf("POST /v1/ops/%s  %s", op, string(body)), c))
	return a.do("POST", "/v1/ops/"+op, body, c)
}

// Get is a read endpoint that is not an op call.
func (a *API) Get(path string, c Cred) Result {
	a.sink.Emit(req("GET "+path, c))
	return a.do("GET", path, nil, c)
}

func (a *API) do(method, path string, body []byte, c Cred) Result {
	started := time.Now()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	r, err := http.NewRequestWithContext(context.Background(), method, a.cfg.URL+path, rdr)
	if err != nil {
		return a.fail(err, started)
	}
	r.Header.Set("Authorization", "Bearer "+a.token(c))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	res, err := a.http.Do(r)
	if err != nil {
		return a.fail(err, started)
	}
	defer res.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(res.Body)
	d := time.Since(started)
	out := Result{Status: res.StatusCode, Text: string(raw), Dur: d, OK: res.StatusCode/100 == 2}
	_ = json.Unmarshal(raw, &out.Body)
	a.sink.Emit(Line{Role: RoleResponse, Status: out.Status, Dur: d, Text: Brief(out.Text, 260)})
	return out
}

func (a *API) fail(err error, started time.Time) Result {
	d := time.Since(started)
	a.sink.Emit(Line{Role: RoleResponse, Status: 0, Dur: d, Text: "the request never completed: " + err.Error()})
	return Result{Dur: d, Err: err}
}

// ── quiet reads, for the driver's own bookkeeping ───────────────────────────

// Pane is one row of the pane aggregate.
type Pane struct {
	ID    int    `json:"pane"`
	Label string `json:"label"`
	State string `json:"state"`
	Cmd   string `json:"cmd"`
}

// Panes reads the pane list WITHOUT emitting evidence: it is the driver's own
// bookkeeping before almost every action, and printing it each time would bury
// the exchange the operator asked for.
func (a *API) Panes() ([]Pane, error) {
	var out struct {
		Panes []Pane `json:"panes"`
	}
	if err := a.quiet("GET", "/v1/panes", CredSession, &out); err != nil {
		return nil, err
	}
	return out.Panes, nil
}

// Op is one row of the op table. Class is the field that decides every
// credential refusal in this demo — a view token reaches class read and nothing
// above it — and Source is "magmux" or the name of the plugin that registered
// it, which is how a plugin's ops are told from the built-ins.
type Op struct {
	Name   string `json:"name"`
	Class  string `json:"class"`
	Source string `json:"source"`
}

// OpTable is the op table and the revision that identifies it. Rev changes when
// a plugin registers or dies, which is what lets a client tell that the list it
// cached is stale.
type OpTable struct {
	Rev int  `json:"rev"`
	Ops []Op `json:"ops"`
}

func (t OpTable) Names() []string {
	out := make([]string, 0, len(t.Ops))
	for _, o := range t.Ops {
		out = append(out, o.Name)
	}
	return out
}

// OpsTable reads the op table, which is how the plugin action learns whether
// the plugin registered and how the ops panel draws its classes.
func (a *API) OpsTable() (OpTable, error) {
	var out OpTable
	err := a.quiet("GET", "/v1/ops", CredSession, &out)
	return out, err
}

// Ops is the names-only form the actions use.
func (a *API) Ops() (rev int, names []string, err error) {
	t, err := a.OpsTable()
	if err != nil {
		return 0, nil, err
	}
	return t.Rev, t.Names(), nil
}

// Capabilities proves the credential before anything else is attempted. A menu
// whose every action fails identically tells nobody which of the two tokens,
// the URL or the process is the problem.
type Capabilities struct {
	Protocol   int            `json:"protocol"`
	ReadOnly   bool           `json:"readOnly"`
	Verbs      []string       `json:"verbs"`
	Transports map[string]any `json:"transports"`
}

func (a *API) Capabilities() (Capabilities, error) {
	var c Capabilities
	err := a.quiet("GET", "/v1/capabilities", CredSession, &c)
	return c, err
}

func (a *API) quiet(method, path string, c Cred, into any) error {
	r, err := http.NewRequestWithContext(context.Background(), method, a.cfg.URL+path, nil)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+a.token(c))
	res, err := a.http.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d %s", res.StatusCode, Brief(string(raw), 160))
	}
	return json.Unmarshal(raw, into)
}

// Watched is the pane the mirrors are watching: the first non-panel pane in the
// aggregate, which is the SAME rule both clients use. Every session now carries
// a control panel and pane ids are sparse, so "the first pane" is not "pane 0"
// and assuming it is would type into a pane nobody is looking at.
func Watched(list []Pane) int {
	for _, p := range list {
		if p.State != "panel" {
			return p.ID
		}
	}
	return -1
}

// TransportNames is the transports object's keys. It is an OBJECT and not a
// list because each transport carries its own facts (its allow-origin set,
// whether TLS is on), which are facts about an adapter and not about the hub.
func (c Capabilities) TransportNames() []string {
	var out []string
	for k, v := range c.Transports {
		if m, ok := v.(map[string]any); ok && m != nil {
			out = append(out, k)
		}
	}
	return out
}
