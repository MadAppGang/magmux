package mux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFirebaseConfigPathPrefersTheFlag: --firebase wins over MAGMUX_FIREBASE,
// as every other flag does over its variable. The variable exists for the case
// where a flag cannot be passed — an MCP client's env block, a launchd plist.
func TestFirebaseConfigPathPrefersTheFlag(t *testing.T) {
	t.Setenv("MAGMUX_FIREBASE", "/from/env.json")
	if got := firebaseConfigPath("/from/flag.json"); got != "/from/flag.json" {
		t.Errorf("with both set, got %q", got)
	}
	if got := firebaseConfigPath(""); got != "/from/env.json" {
		t.Errorf("with only the variable set, got %q", got)
	}
	t.Setenv("MAGMUX_FIREBASE", "")
	if got := firebaseConfigPath(""); got != "" {
		t.Errorf("with neither set, got %q; --firebase must be entirely opt-in", got)
	}
}

// TestBadFirebaseConfigIsRefusedBeforeInit: every one of these is a message on
// a NORMAL terminal and an exit 1, because prepareFirebase runs before init().
// After init() there is an alternate screen in the way and raw mode on top of
// it, and a line printed there is either invisible or corrupts the frame.
func TestBadFirebaseConfigIsRefusedBeforeInit(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	m := &Magmux{}
	cases := []struct {
		name string
		path string
		want string
	}{
		{"a path that does not exist", filepath.Join(dir, "nope.json"), "no such file"},
		{"not JSON", write("bad.json", "not json at all"), "invalid character"},
		{
			"a look-alike databaseURL",
			write("evil.json", `{"databaseURL":"https://x.firebaseio.com.evil.com","root":"magmux","host":"h","credentials":"sa.json"}`),
			"not a Firebase Realtime Database URL",
		},
		{
			"credentials and emulator together",
			write("both.json", `{"root":"magmux","host":"h","credentials":"sa.json","emulator":{"host":"127.0.0.1:9000","ns":"demo"}}`),
			`exactly one of "credentials" and "emulator"`,
		},
		{
			"commands enabled with no owners",
			write("noowners.json", `{"root":"magmux","host":"h","emulator":{"host":"127.0.0.1:9000","ns":"demo"},"commands":{"enabled":true,"keyFile":"k","allowOps":["list"]}}`),
			"commands.owners",
		},
	}
	for _, c := range cases {
		_, err := m.prepareFirebase(c.path)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
}

// TestFirebaseLifecycleIsSafeWithNoAdapter: every hook is reachable from a
// magmux started without --firebase, from a defer and from the failure paths,
// so all of them must be nil-safe. This is the test that would have caught a
// panic in teardown on the overwhelmingly common configuration.
func TestFirebaseLifecycleIsSafeWithNoAdapter(t *testing.T) {
	m := &Magmux{}
	m.startFirebase(nil)
	m.finalizeFirebase(finalizeDeadline)
	m.cleanupFirebase()
	firebaseNote(nil)
}
