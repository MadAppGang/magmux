package sockdir

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestReapStaleTokens is the token half of the startup sweep, added with
// --listen.
//
// A stale token file is worse litter than a stale socket. It is a live
// credential, written 0600 by a magmux that was SIGKILLed before it could
// remove it, and nothing else in the system will ever clean it up. Its rules
// are the socket's rules with one substitution — a regular file instead of a
// socket inode — and this is deliberately written as the same shape of table,
// so a rule that drifts on one side and not the other shows up as an asymmetry
// rather than as a gap.
func TestReapStaleTokens(t *testing.T) {
	dir := sockTestDir(t)
	dead := deadPid(t)
	alive := os.Getpid()

	writeToken := func(name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("sometoken\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Removed: a pid-named token, and a temp file whose own pid is dead. The
	// temp file is the other half of the write path — WriteFile creates
	// `<path>.<pid>.tmp` and renames it, so a process killed between those two
	// steps leaves one behind with its writer's pid in the name.
	goneToken := writeToken(fmt.Sprintf("magmux-%d.token", dead))
	goneTmp := writeToken(fmt.Sprintf("magmux-web.token.%d.tmp", dead))
	goneTmpPid := writeToken(fmt.Sprintf("magmux-%d.token.%d.tmp", dead, dead))

	// Kept, each for its own reason.
	keptAlive := writeToken(fmt.Sprintf("magmux-%d.token", alive)) // its owner is running
	keptAliveTmp := writeToken(fmt.Sprintf("magmux-ci.token.%d.tmp", alive))
	keptNamed := writeToken("magmux-web.token")                        // --id: no liveness oracle
	keptPadded := writeToken(fmt.Sprintf("magmux-0%d.token", dead))    // not a name magmux mints
	keptOther := writeToken("something-else.token")                    // not ours at all
	keptSuffix := writeToken(fmt.Sprintf("magmux-%d.token.bak", dead)) // not a name magmux mints
	keptSock := filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", alive))
	mkSocket(t, keptSock)

	// A DIRECTORY named exactly like a stale token. Rule 4 asks for a regular
	// file, and a sweep that unlinked by name alone would reach for this.
	keptDir := ""
	if pidIsGone(dead + 1) {
		keptDir = filepath.Join(dir, fmt.Sprintf("magmux-%d.token", dead+1))
		if err := os.Mkdir(keptDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	n := ReapStale(dir, "", ReapDeadline)
	if n < 3 {
		t.Errorf("ReapStale removed %d files, want at least the 3 whose owning pid is dead", n)
	}

	for _, p := range []string{goneToken, goneTmp, goneTmpPid} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived; its owning pid is dead and it holds a live credential", filepath.Base(p))
		}
	}
	kept := []string{keptAlive, keptAliveTmp, keptNamed, keptPadded, keptOther, keptSuffix, keptSock}
	if keptDir != "" {
		kept = append(kept, keptDir)
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed and must not have been: %v", filepath.Base(p), err)
		}
	}
}

// TestReapStaleSweepsBothKindsInOnePass: the token sweep shares the socket
// sweep's directory listing, because os.ReadDir is the expensive half (4.4ms
// against 78µs per removal) and a second pass would double the expensive half
// to halve the cheap one.
func TestReapStaleSweepsBothKindsInOnePass(t *testing.T) {
	dir := sockTestDir(t)
	dead := deadPid(t)

	mkSocket(t, filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", dead)))
	tok := filepath.Join(dir, fmt.Sprintf("magmux-%d.token", dead))
	if err := os.WriteFile(tok, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if n := ReapStale(dir, "", ReapDeadline); n != 2 {
		t.Fatalf("ReapStale removed %d, want 2 (the socket and the token, from one listing)", n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the directory should be empty, it holds %v", entries)
	}
}
