package firebase

// The emulator end-to-end case, gated on MAGMUX_FIREBASE_EMULATOR=1.
//
// Everything above this file is magmux talking to a server this repository
// wrote. This one talks to Google's own Realtime Database emulator, with the
// rules magmux ships loaded from examples/firebase, and it is the only place
// three assumptions are actually TESTED rather than asserted:
//
//   - `Authorization: Bearer owner` really is the emulator's admin bypass. It
//     is not in the public documentation; the source is firebase-tools 15.19.1,
//     lib/emulator/hubExport.js:152-157.
//   - the shipped rules really do refuse a non-owner, on the REAL rules engine
//     rather than on a reading of the JSON.
//   - a plugin op whose schema contains `$ref` really does mirror, because it
//     travels as a JSON string and never as a key. That is the negative
//     control: it is the case that would silently break the whole mirror if the
//     string rule were ever relaxed.
//
// It is skipped by default because it needs the firebase CLI and a JDK, and
// downloads an emulator jar on first run. It is never weakened to make it run.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

const (
	emuProject = "demo-magmux"
	emuNS      = "demo-magmux-default-rtdb"
	emuAddr    = "127.0.0.1:9000"
)

// TestEmulatorEndToEnd is the whole Firebase surface against the real thing.
func TestEmulatorEndToEnd(t *testing.T) {
	if os.Getenv("MAGMUX_FIREBASE_EMULATOR") != "1" {
		t.Skip("set MAGMUX_FIREBASE_EMULATOR=1 to run the emulator end-to-end case")
	}
	t0 := time.Now()
	startEmulator(t)
	t.Logf("emulator ready in %s", time.Since(t0).Round(time.Millisecond))

	host := "emu-host"
	sid := MakeSID("magmux", time.Now())
	root := "magmux"
	sess := sessionPath(root, host, sid)

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "cmd.key")
	key := []byte("magmux-emulator-key-not-a-real-secret")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Root: root, Host: host,
		Emulator: &EmulatorConfig{Host: emuAddr, NS: emuNS},
		FrameFPS: 2,
		Commands: CommandsConfig{
			Enabled: true, Owners: []string{"owner-uid-1"}, KeyFile: keyPath,
			AllowOps: []string{"list", "send", "ticket.*"},
		},
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}

	fx := &sideEffects{}
	h := hub.New()
	st := &fakeStreamer{subs: map[*hub.Sub]bool{}}
	h.SetWatcher(st)
	if err := h.Register(protocol.SourceBuiltin,
		hub.Op{
			Spec: protocol.OpSpec{Name: "list", Class: protocol.ClassRead,
				Description: "List panes.", Schema: json.RawMessage(`{"type":"object"}`)},
			Fn: func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
				fx.note("run list by " + c.Client)
				return map[string]any{"panes": []any{map[string]any{"pane": 0, "state": "running"}}}, nil
			},
		},
		hub.Op{
			Spec: protocol.OpSpec{Name: "send", Class: protocol.ClassControl},
			Fn: func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
				fx.note("pty write " + string(args))
				return map[string]any{"sent": true}, nil
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	// The NEGATIVE CONTROL: a plugin op whose JSON Schema holds `$ref`, a legal
	// JSON key and an illegal RTDB one. It must mirror, because a spec travels
	// as a JSON string.
	if err := h.Register("ticket", hub.Op{
		Spec: protocol.OpSpec{
			Name: "ticket.run_ticket", Class: protocol.ClassControl,
			Description: "Run a ticket.",
			Schema: json.RawMessage(
				`{"type":"object","properties":{"ticket":{"$ref":"#/$defs/id"}},` +
					`"$defs":{"id":{"type":"string","pattern":"^t[0-9]+$"}}}`),
		},
		Fn: func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
			fx.note("run_ticket")
			return map[string]any{"ticket": "t1"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a, err := New(cfg, Options{
		Hub: h, SID: sid, PID: os.Getpid(), Version: "emulator-test",
		Geometry: func() (int, int) { return 24, 80 },
		Aggregate: func() []byte {
			return line(map[string]any{"type": protocol.EventSnapshot, "panes": []map[string]any{
				{"pane": 0, "state": "running", "label": "build", "cmd": "make -j", "cwd": "/tmp", "dead": false, "exitCode": 0},
			}})
		},
		Log: func(s string) { t.Log(s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Start()
	defer a.Close()

	// A live event and a frame, through the real Sub.
	h.Publish(line(map[string]any{
		"type": protocol.EventSnapshot, "pane": 0, "controller": "claude",
		"state": "awaiting_input", "response": "all green", "model": "opus",
	}))
	st.attach(a.sub, 0)
	st.offerKeyframe(a.sub, 0, []string{"magmux mirrors", "to firebase"})

	admin := emuREST(t, "")

	// ── 1. the mirror ───────────────────────────────────────────────────────
	waitFor(t, 15*time.Second, "the session meta to appear", func() bool {
		v, _ := admin.get(sess + "/meta").(map[string]any)
		return v != nil && v["version"] == "emulator-test"
	})
	waitFor(t, 15*time.Second, "pane meta and state to appear", func() bool {
		meta, _ := admin.get(sess + "/panes/p0/meta").(map[string]any)
		state, _ := admin.get(sess + "/panes/p0/state").(map[string]any)
		return meta != nil && meta["label"] == "build" && state != nil && state["state"] == "awaiting_input"
	})
	waitFor(t, 15*time.Second, "the frame to appear", func() bool {
		fr, _ := admin.get(sess + "/panes/p0/frame").(map[string]any)
		if fr == nil {
			return false
		}
		lines, _ := fr["lines"].(map[string]any)
		r0, _ := lines["r0"].(map[string]any)
		return r0 != nil && r0["t"] == "magmux mirrors"
	})
	state, _ := admin.get(sess + "/panes/p0/state").(map[string]any)
	t.Logf("mirrored pane state: %v", state)
	if state["response"] != "all green" {
		t.Errorf("the controller's response did not mirror: %v", state)
	}

	// ── 2. the negative control: a `$ref` schema mirrors ─────────────────────
	waitFor(t, 15*time.Second, "the ops node", func() bool {
		ops, _ := admin.get(sess + "/ops").(map[string]any)
		return ops != nil && ops["ticket__run_ticket"] != nil
	})
	ops, _ := admin.get(sess + "/ops").(map[string]any)
	spec, ok := ops["ticket__run_ticket"].(string)
	if !ok {
		t.Fatalf("the plugin op mirrored as %T, not as a JSON string", ops["ticket__run_ticket"])
	}
	if !strings.Contains(spec, `"$ref"`) {
		t.Errorf("the `$ref` did not survive: %s", spec)
	}
	var decoded protocol.OpSpec
	if err := json.Unmarshal([]byte(spec), &decoded); err != nil {
		t.Fatalf("the mirrored spec does not parse: %v", err)
	}
	if decoded.Name != "ticket.run_ticket" || decoded.Source != "ticket" {
		t.Errorf("spec = %+v", decoded)
	}
	t.Logf("negative control: a $ref schema mirrored as a %d-byte string", len(spec))

	// ── 3. a signed command from owners[0] executes ──────────────────────────
	ownerTok := emuIDToken(t, "owner-uid-1")
	owner := emuREST(t, ownerTok)
	ts := time.Now().UnixMilli()
	nonce := "emulator-nonce-00000001"
	args := `{"all":true}`
	good := map[string]any{
		"uid": "owner-uid-1", "ts": ts, "nonce": nonce, "op": "list", "args": args,
		"sig": Sign(key, host, sid, "owner-uid-1", ts, nonce, "list", args),
	}
	cmdStart := time.Now()
	if code := owner.put(sess+"/commands/-Nemu0001", good); code != http.StatusOK {
		t.Fatalf("the rules refused a signed command from an owner: HTTP %d", code)
	}
	waitFor(t, 20*time.Second, "the command's result", func() bool {
		r, _ := admin.get(sess + "/results/-Nemu0001").(map[string]any)
		return r != nil && r["state"] == "done"
	})
	res, _ := admin.get(sess + "/results/-Nemu0001").(map[string]any)
	if res["ok"] != true {
		t.Fatalf("the command failed: %v", res)
	}
	if s, _ := res["result"].(string); !strings.Contains(s, "panes") {
		t.Errorf("the op's result did not mirror as a JSON string: %v", res["result"])
	}
	t.Logf("signed command executed and its result appeared in %s", time.Since(cmdStart).Round(time.Millisecond))
	waitFor(t, 10*time.Second, "the executed command to be deleted", func() bool {
		return admin.get(sess+"/commands/-Nemu0001") == nil
	})

	// ── 4. a non-owner uid is refused BY THE RULES ───────────────────────────
	otherTok := emuIDToken(t, "not-an-owner")
	other := emuREST(t, otherTok)
	tsN := time.Now().UnixMilli()
	forged := map[string]any{
		"uid": "not-an-owner", "ts": tsN, "nonce": "emulator-nonce-00000002", "op": "list", "args": args,
		// Correctly signed, even. The rules never get as far as caring.
		"sig": Sign(key, host, sid, "not-an-owner", tsN, "emulator-nonce-00000002", "list", args),
	}
	if code := other.put(sess+"/commands/-Nemu0002", forged); code != http.StatusUnauthorized {
		t.Fatalf("a non-owner's write returned HTTP %d; the rules must refuse it with 401", code)
	}
	if admin.get(sess+"/commands/-Nemu0002") != nil {
		t.Fatal("a non-owner's command reached the database")
	}
	t.Log("a non-owner uid was refused by the rules (401), before magmux ever saw it")

	// ── 5. an owner with a bad HMAC is refused BY MAGMUX ─────────────────────
	tsB := time.Now().UnixMilli()
	bad := map[string]any{
		"uid": "owner-uid-1", "ts": tsB, "nonce": "emulator-nonce-00000003",
		"op": "send", "args": `{"pane":0,"text":"should never be typed"}`,
		"sig": strings.Repeat("0", 64),
	}
	if code := owner.put(sess+"/commands/-Nemu0003", bad); code != http.StatusOK {
		t.Fatalf("the rules refused a well-formed command from an owner: HTTP %d", code)
	}
	waitFor(t, 20*time.Second, "magmux to reject the bad signature", func() bool {
		r, _ := admin.get(sess + "/results/-Nemu0003").(map[string]any)
		return r != nil && r["state"] == "done"
	})
	res, _ = admin.get(sess + "/results/-Nemu0003").(map[string]any)
	if res["ok"] != false || res["code"] != protocol.CodeUnauthorized {
		t.Fatalf("a bad HMAC produced %v; want ok:false unauthorized", res)
	}
	waitFor(t, 10*time.Second, "the rejected command to be deleted", func() bool {
		return admin.get(sess+"/commands/-Nemu0003") == nil
	})
	for _, e := range fx.all() {
		if strings.Contains(e, "should never be typed") {
			t.Fatalf("a command with a bad signature reached a PTY: %v", fx.all())
		}
	}
	t.Log("an owner uid with a bad HMAC was rejected by magmux and the command deleted")

	// ── 6. teardown, in magmux's own order ───────────────────────────────────
	//
	// Hub.Finalize hands the mirror `results` through the ordinary Write path
	// and waits for it; the adapter's Finalize is what flushes it, with
	// alive:false. Doing them in this order here is the point — it is the order
	// shutdownSocket uses, and a test that published `results` and finalized the
	// adapter without it would be asserting on a race.
	h.Finalize(
		line(map[string]any{"type": protocol.EventResults, "panes": []map[string]any{
			{"pane": 0, "state": "completed", "exitCode": 0, "dead": true},
		}}),
		line(map[string]any{"type": protocol.EventShutdown}),
	)
	a.Finalize(2 * time.Second)
	meta, _ := admin.get(sess + "/meta").(map[string]any)
	if meta["alive"] != false {
		t.Errorf("the final flush did not write alive:false: %v", meta)
	}
	final, _ := admin.get(sess + "/panes/p0/state").(map[string]any)
	if final["state"] != "completed" {
		t.Errorf("results did not reach the mirror: %v", final)
	}
	t.Logf("total %s", time.Since(t0).Round(time.Millisecond))
}

// ── the emulator ────────────────────────────────────────────────────────────

// startEmulator runs `firebase emulators:start --only database` from
// examples/firebase, so the SHIPPED rules are the ones loaded, and waits for it
// to answer.
func startEmulator(t *testing.T) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "examples", "firebase"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("firebase", "emulators:start", "--only", "database", "--project", emuProject)
	cmd.Dir = dir
	// Its own process group, so the whole tree goes at the end: the CLI spawns
	// a Java child and killing only the CLI leaves the jar holding port 9000.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), emulatorJavaEnv()...)
	log, err := os.CreateTemp("", "magmux-emulator-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Skipf("the firebase CLI could not be started (%v); install firebase-tools to run this case", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = cmd.Wait()
		if t.Failed() {
			if b, err := os.ReadFile(log.Name()); err == nil {
				t.Logf("emulator log:\n%s", b)
			}
		}
		log.Close()
		os.Remove(log.Name())
	})

	rest := emuREST(t, "")
	deadline := time.Now().Add(5 * time.Minute) // the jar downloads on first run
	for time.Now().Before(deadline) {
		if rest.ping() {
			return
		}
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	b, _ := os.ReadFile(log.Name())
	t.Fatalf("the database emulator never answered on %s.\n%s", emuAddr, b)
}

// emulatorJavaEnv points the CLI at a JDK it will accept.
//
// firebase-tools 15.19.1 refuses a JDK older than 21 and exits before binding
// the port, and the `java` on PATH here is 17. This overrides the environment
// for the CHILD only; changing the machine's default JDK is not a test's
// business.
func emulatorJavaEnv() []string {
	for _, home := range []string{
		"/opt/homebrew/opt/openjdk@21",
		"/opt/homebrew/opt/openjdk@25",
		"/opt/homebrew/opt/openjdk",
		"/usr/local/opt/openjdk@21",
	} {
		if _, err := os.Stat(filepath.Join(home, "bin", "java")); err != nil {
			continue
		}
		return []string{
			"JAVA_HOME=" + home,
			"PATH=" + filepath.Join(home, "bin") + ":" + os.Getenv("PATH"),
		}
	}
	return nil
}

// emuClient is a tiny REST client for the emulator, used by the test to read
// and write as somebody OTHER than magmux.
type emuClient struct {
	t *testing.T
	// token is an unsigned emulator ID token passed as `?auth=`, or "" for the
	// admin bypass.
	//
	// It has to be `?auth=` and NOT `Authorization: Bearer <token>`: the
	// emulator treats any bearer JWT as a service-account token and grants
	// OWNERSHIP ("assuming ownership" in database-debug.log), which would
	// bypass the very rules this test exists to check.
	token string
	hc    *http.Client
}

func emuREST(t *testing.T, token string) *emuClient {
	return &emuClient{t: t, token: token, hc: &http.Client{Timeout: 10 * time.Second}}
}

func (e *emuClient) url(path string) string {
	q := url.Values{"ns": {emuNS}}
	if e.token != "" {
		q.Set("auth", e.token)
	}
	return "http://" + emuAddr + "/" + strings.TrimPrefix(path, "/") + ".json?" + q.Encode()
}

func (e *emuClient) do(method, path string, body io.Reader) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.url(path), body)
	if err != nil {
		e.t.Fatal(err)
	}
	if e.token == "" {
		req.Header.Set("Authorization", "Bearer "+emulatorToken)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b
}

func (e *emuClient) ping() bool {
	code, _ := e.do(http.MethodGet, "/", nil)
	return code == http.StatusOK
}

func (e *emuClient) get(path string) any {
	e.t.Helper()
	code, b := e.do(http.MethodGet, path, nil)
	if code != http.StatusOK {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	return v
}

func (e *emuClient) put(path string, value any) int {
	e.t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		e.t.Fatal(err)
	}
	code, _ := e.do(http.MethodPut, path, strings.NewReader(string(b)))
	return code
}

// emuIDToken mints an UNSIGNED Firebase ID token in the shape
// @firebase/rules-unit-testing produces: header {"alg":"none"}, the usual
// securetoken claims, and an EMPTY signature segment.
//
// Only an emulator accepts one. Nothing here is a credential and nothing here
// works against a real project.
func emuIDToken(t *testing.T, uid string) string {
	t.Helper()
	seg := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	now := time.Now().Unix()
	header := map[string]any{"alg": "none", "typ": "JWT"}
	claims := map[string]any{
		"iss": "https://securetoken.google.com/" + emuProject,
		"aud": emuProject, "sub": uid, "user_id": uid,
		"iat": now, "exp": now + 3600, "auth_time": now,
		"firebase": map[string]any{"identities": map[string]any{}, "sign_in_provider": "custom"},
	}
	return seg(header) + "." + seg(claims) + "."
}

// ── a Watcher, so frames can travel the real path ───────────────────────────

// fakeStreamer stands in for mux's Streamer. It does not diff a screen; it lets
// a test put a frame into the Sub's slot so the mirror receives it exactly as
// it would from a real pane.
type fakeStreamer struct {
	mu   sync.Mutex
	subs map[*hub.Sub]bool
}

func (f *fakeStreamer) Watch(s *hub.Sub, pane int, mode protocol.WatchMode, fps int) (protocol.WatchInfo, error) {
	return protocol.WatchInfo{Pane: pane, Rows: 24, Cols: 80, Mode: mode, FPS: protocol.ClampFPS(fps)}, nil
}
func (f *fakeStreamer) Unwatch(*hub.Sub, int)      {}
func (f *fakeStreamer) Resync(*hub.Sub, int) error { return nil }
func (f *fakeStreamer) WatchAll(s *hub.Sub, fps int) {
	f.mu.Lock()
	f.subs[s] = true
	f.mu.Unlock()
}
func (f *fakeStreamer) Drop(s *hub.Sub) {
	f.mu.Lock()
	delete(f.subs, s)
	f.mu.Unlock()
}

// attach opens a pane's slot on a Sub, as the streamer does when a pane is
// opened under a WatchAll.
func (f *fakeStreamer) attach(s *hub.Sub, pane int) {
	if _, err := s.Watch(pane, protocol.WatchFrames, 2); err != nil {
		panic(err)
	}
}

// offerKeyframe hands the Sub a whole screen, in the wire shape the hub's slot
// assembles: a header object left OPEN, plus one encoded row per line.
func (f *fakeStreamer) offerKeyframe(s *hub.Sub, pane int, text []string) {
	hdr, err := json.Marshal(protocol.FrameHeader{
		Type: protocol.EventFrame, Pane: pane, Seq: 1,
		Rows: len(text), Cols: 80, Cur: protocol.Cursor{Y: 0, X: 0, Vis: true},
	})
	if err != nil {
		panic(err)
	}
	rows := make(map[int][]byte, len(text))
	for y, t := range text {
		rows[y] = []byte(fmt.Sprintf(`{"y":%d,"t":%q}`, y, t))
	}
	// The hub appends `,"key":…,"lines":[…]}`, so the header must arrive as an
	// object that has been opened and not closed.
	s.Offer(pane, true, rows, hdr[:len(hdr)-1])
}
