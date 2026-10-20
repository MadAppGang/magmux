package client

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/unixsock"
	"github.com/MadAppGang/magmux/unixsock/unixsocktest"
)

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
//
// TestDialDeepSocket: client.Dial reaches a magmux whose socket path is past
// sun_path. client is a public package, so this is the dial every importer
// gets.
func TestDialDeepSocket(t *testing.T) {
	path := filepath.Join(unixsocktest.DeepDir(t, "m.sock", unixsocktest.DeepPathLen), "m.sock")
	if len(path) < unixsocktest.DeepPathLen {
		t.Fatalf("the socket path is %d bytes, want >= %d: %s", len(path), unixsocktest.DeepPathLen, path)
	}

	ln, err := unixsock.Listen(path)
	if err != nil {
		t.Fatalf("fake magmux: listen on a %d-byte path: %v", len(path), err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(path)
	})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// The connect-time aggregate, then silence: the capability probe goes
		// unanswered, which the short probe budgets below turn into a fast
		// verdict rather than a slow test.
		fmt.Fprintln(conn, `{"type":"snapshot","panes":[{"pane":0,"state":"running"}]}`)
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Dial(ctx, "deep", path, 0,
		WithProbeTimeout(50*time.Millisecond), WithProbeConfirmTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("client.Dial(%d-byte path): %v", len(path), err)
	}
	defer s.Close()
}
