package mux

// The ticket-runner demo, end to end, driven from Go.
//
// This is the test that proves the whole of R6/R7 in one run: a plugin written
// in another language, started by magmux, adding ops that BOTH transports can
// call, opening a session of its own, driving it, and reporting what it saw —
// with no model, no network and no API key.
//
// Its assertions are deliberately the ones a user would make:
//
//	the op is in `ops` (and therefore in /v1/ops, and therefore an MCP tool)
//	POST /v1/ops/ticket.run_ticket does something real
//	the same op over the socket does the same thing
//	the `progress` events arrive
//	the pane reaches awaiting_input carrying the agent's own DONE: line
//	`status` agrees with all of it afterwards

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// demoPluginCmd is the --plugin command line for the bundled demo, absolute so
// it does not depend on where the test binary runs from.
func demoPluginCmd(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun is not installed; the ticket-runner demo is a TypeScript plugin")
	}
	// go test runs in the package directory, so the repo root is one up.
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "examples", "plugins", "ticket-runner", "main.ts")
	if _, err := exec.Command("test", "-f", main).Output(); err != nil {
		t.Fatalf("the demo plugin is missing: %s", main)
	}
	return "bun " + main
}

// TestTicketRunnerDemoDrivesASessionFromBothTransports is the P5 end-to-end
// gate. It logs its timings, because "the plugin answered" and "the plugin
// answered in under a second" are different claims and only one of them is
// worth making.
func TestTicketRunnerDemoDrivesASessionFromBothTransports(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("subprocess test requires darwin or linux")
	}
	started := time.Now()
	m := startRemoteMagmux(t, "--plugin", demoPluginCmd(t), "-e", "sleep 120")
	t.Logf("magmux listening after %v", time.Since(started))

	// 1. The plugin's ops reach the op table. This is the only poll in the
	//    test: a runtime takes a moment to start, and everything after it is
	//    event-driven.
	appeared := time.Now()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, body := m.req(t, "GET", "/v1/ops", m.token, "")
		if opNames(body)["ticket.run_ticket"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ticket.run_ticket never appeared in /v1/ops\nstderr: %s\n%s",
				m.stderr.String(), pluginLogs(t, m.dir))
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("the plugin registered %v after magmux was listening", time.Since(appeared))

	// A subscriber for the events both halves of the test assert on. Opened
	// before either call: `plugin` events and per-pane snapshots are live
	// broadcasts and are never replayed.
	sub := dialPlugin(t, m.sock)

	t.Run("over HTTP", func(t *testing.T) {
		call := time.Now()
		resp, body := m.req(t, "POST", "/v1/ops/ticket.run_ticket", m.token,
			`{"title":"over http"}`)
		if resp.StatusCode != http.StatusOK || body["ok"] != true {
			t.Fatalf("status %d: %v\n%s", resp.StatusCode, body, pluginLogs(t, m.dir))
		}
		result, _ := body["result"].(map[string]any)
		pane := int(result["pane"].(float64))
		t.Logf("run_ticket over HTTP returned %v in %v (ticket %v, pane %d)",
			result["state"], time.Since(call), result["ticket"], pane)

		awaitTicket(t, sub, pane, "over http", call)
	})

	t.Run("over the socket", func(t *testing.T) {
		client := dialPlugin(t, m.sock)
		call := time.Now()
		res := replyOK(t, client.request(map[string]any{
			"type": "call", "op": "ticket.run_ticket",
			"args": map[string]any{"title": "over the socket"},
		}))
		pane := int(res["pane"].(float64))
		t.Logf("run_ticket over the socket returned %v in %v (ticket %v, pane %d)",
			res["state"], time.Since(call), res["ticket"], pane)

		awaitTicket(t, sub, pane, "over the socket", call)
	})

	// 4. `status` is the plugin's own view, and it must agree with what magmux
	//    reported: two tickets, both done, each with its agent's answer.
	_, body := m.req(t, "POST", "/v1/ops/ticket.status", m.token, `{}`)
	if body["ok"] != true {
		t.Fatalf("status failed: %v", body)
	}
	result, _ := body["result"].(map[string]any)
	tickets, _ := result["tickets"].([]any)
	if len(tickets) != 2 {
		t.Fatalf("status reports %d tickets, want the two this test ran: %v", len(tickets), result)
	}
	for _, raw := range tickets {
		ticket := raw.(map[string]any)
		if ticket["state"] != "done" {
			t.Errorf("ticket %v is %v, want done: %v", ticket["ticket"], ticket["state"], ticket)
		}
		if !strings.HasPrefix(fmt.Sprint(ticket["response"]), "DONE:") {
			t.Errorf("ticket %v has no answer from its agent: %v", ticket["ticket"], ticket)
		}
		t.Logf("status: ticket %v on pane %v — %v in %vms",
			ticket["ticket"], ticket["pane"], ticket["state"], ticket["elapsedMs"])
	}
	t.Logf("whole demo: %v", time.Since(started))
}

// awaitTicket follows one ticket through the event stream: the plugin's own
// progress events, and then the pane state magmux publishes from the snapshot
// the plugin pushed.
//
// The second is the one that matters. `awaiting_input` carrying the agent's
// DONE: line is a claim about a session magmux is not itself following, made by
// a plugin, reconciled with what the terminal saw — which is the whole feature.
func awaitTicket(t *testing.T, sub *fakePlugin, pane int, title string, since time.Time) {
	t.Helper()

	sent := sub.await("the plugin's `sent` progress event", func(ev map[string]any) bool {
		if ev["type"] != protocol.EventPlugin || ev["plugin"] != "ticket" {
			return false
		}
		data, _ := ev["data"].(map[string]any)
		return data["step"] == "sent" && data["title"] == title
	})
	t.Logf("progress %v after %v", sent["data"], time.Since(since))

	done := sub.await("the plugin's `done` progress event", func(ev map[string]any) bool {
		if ev["type"] != protocol.EventPlugin {
			return false
		}
		data, _ := ev["data"].(map[string]any)
		return data["step"] == "done" && int(orZero(ev["pane"])) == pane
	})
	t.Logf("progress %v after %v", done["data"], time.Since(since))

	snap := sub.await("the pane settling at awaiting_input", func(ev map[string]any) bool {
		if ev["type"] != protocol.EventSnapshot || ev["panes"] != nil {
			return false
		}
		return int(orZero(ev["pane"])) == pane && ev["state"] == "awaiting_input"
	})
	if !strings.HasPrefix(fmt.Sprint(snap["response"]), "DONE: "+title) {
		t.Errorf("the settled pane does not carry the agent's answer: %v", snap)
	}
	if snap["controller"] != "plugin:ticket" {
		t.Errorf("controller = %v; the pane must be attributed to the plugin that claimed it",
			snap["controller"])
	}
	t.Logf("pane %d settled at awaiting_input with %q after %v",
		pane, snap["response"], time.Since(since))
}

func orZero(v any) float64 {
	f, _ := v.(float64)
	return f
}

func opNames(body map[string]any) map[string]bool {
	out := map[string]bool{}
	ops, _ := body["ops"].([]any)
	for _, raw := range ops {
		spec, _ := raw.(map[string]any)
		out[fmt.Sprint(spec["name"])] = true
	}
	return out
}

// pluginLogs is what a failure needs to be diagnosable: the plugin's own
// output, which by design never reaches magmux's stderr.
func pluginLogs(t *testing.T, dir string) string {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(dir, "*.plugin-*.log"))
	var b strings.Builder
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n--- %s\n%s", p, raw)
	}
	return b.String()
}
