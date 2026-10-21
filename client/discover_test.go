package client

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Moved from mcp/mcp_test.go with the discovery code itself.

// TestProbeSocketReadsOneLine pins the probe's contract: exactly one line is
// consumed (the connect-time aggregate is guaranteed first), and the
// connection is closed rather than left as a subscriber.
func TestProbeSocketReadsOneLine(t *testing.T) {
	path := tempSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	closed := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		snapshot := `{"type":"snapshot","panes":[{"pane":0,"state":"awaiting_input"},` +
			`{"pane":1,"state":"panel","control":true}]}`
		fmt.Fprintln(conn, snapshot)
		// Then hold the connection open, exactly as magmux does — the probe
		// must not wait for more.
		buf := make([]byte, 1)
		_, _ = conn.Read(buf) // returns when the probe closes its end
		close(closed)
	}()

	panes, err := probeSocket(context.Background(), path, 2*time.Second)
	if err != nil {
		t.Fatalf("probeSocket: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("got %d panes, want 2: %v", len(panes), panes)
	}
	if idx, _ := EventInt(panes[0], "pane"); idx != 0 {
		t.Errorf("first pane index = %v, want 0", panes[0]["pane"])
	}
	if state, _ := EventString(panes[1], "state"); state != "panel" {
		t.Errorf("second pane state = %v, want panel", panes[1]["state"])
	}

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("probeSocket did not close the connection")
	}
	wg.Wait()
}

// tempSockPath returns a short-lived socket path. t.TempDir() is not usable
// here: on darwin it nests the test's own name under /var/folders/…, which
// overruns sun_path's 104-byte limit, which this test's plain net.Listen
// cannot bind, and turns the test into a silent skip.
func tempSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mmx")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestProbeSocketRejectsNonMagmuxSockets(t *testing.T) {
	path := tempSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		fmt.Fprintln(conn, `{"type":"hello"}`)
		conn.Close()
	}()
	if _, err := probeSocket(context.Background(), path, time.Second); err == nil {
		t.Fatal("probeSocket accepted a socket whose first line was not a pane snapshot")
	}
}

func TestDiscoverSessionsIgnoresForeignNames(t *testing.T) {
	// Names are matched by pattern, not by content, so this is a pure unit
	// check on the regexp — /tmp is shared and must not be assumed empty.
	cases := map[string]string{
		"magmux-1234.sock":  "1234",
		"magmux-work.sock":  "work",
		"magmux-a.b.sock":   "a.b",
		"tmux-1000":         "",
		"magmux.sock":       "",
		"magmux-1234.socks": "",
	}
	for name, want := range cases {
		m := sockNamePattern.FindStringSubmatch(name)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", name, got, want)
		}
	}
}
