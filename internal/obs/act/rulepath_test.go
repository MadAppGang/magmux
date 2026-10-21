package act

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/internal/obs"
)

// A rule's `match.cwd` as a person writes it. Fix round 2, item 5.
//
// requirements.md R6 writes rules as `"cwd": "~/dev/**"`. The loader stored
// the `~` literally, a session's launch directory never contains one, and so
// the rule R6 itself sketches could never match anything. It failed safe — the
// refusal said no rule matched — but a rule the user wrote that silently never
// applies is exactly what DisallowUnknownFields exists to prevent for a key,
// one level down.
//
// Every test here pins HOME, so no real home directory is consulted and the
// expansion is the same on every machine.

// tildeRule is R6's own example, with the tool term left out so that the
// PATTERN is what is being tested.
const tildeRule = `{"actions":{"navigate":"rule","rules":[{"do":"navigate","match":{"cwd":"~/dev/**"}}]}}`

// TestARuleWrittenAsTheSpecWritesItMatches is the defect: the spec's syntax
// against an absolute launch directory, which is the only kind production has.
func TestARuleWrittenAsTheSpecWritesItMatches(t *testing.T) {
	t.Setenv("HOME", "/home/someone")

	p, err := ParseConfig([]byte(tildeRule), "inline.json")
	if err != nil {
		t.Fatalf("R6's own rule syntax was refused: %v", err)
	}
	if got := p.Rules[0].LaunchDir; got != "/home/someone/dev/**" {
		t.Errorf("the pattern was stored as %q, want /home/someone/dev/** — a `~` kept literally "+
			"can never match a launch directory", got)
	}

	inside := obs.Session{ID: "id-a", LaunchDir: "/home/someone/dev/magmux"}
	if d := p.Decide(TierNavigate, inside); !d.Allowed {
		t.Errorf("a rule written as R6 writes it did not match %s: %s", inside.LaunchDir, d.Rule)
	}
	// And the expansion is to THIS home, not a match on anything ending in
	// mag/: another user's tree is outside the rule.
	outside := obs.Session{ID: "id-b", LaunchDir: "/home/else/dev/magmux"}
	if d := p.Decide(TierNavigate, outside); d.Allowed {
		t.Errorf("the rule matched %s, which is not under the home it names", outside.LaunchDir)
	}

	for _, tt := range []struct{ written, stored string }{
		{"~", "/home/someone"},
		{"~/", "/home/someone"},
		{"~/**", "/home/someone/**"},
		{" ~/dev/** ", "/home/someone/dev/**"},
		{"/already/absolute/**", "/already/absolute/**"},
	} {
		body := `{"actions":{"rules":[{"do":"navigate","match":{"cwd":"` + tt.written + `"}}]}}`
		p, err := ParseConfig([]byte(body), "inline.json")
		if err != nil {
			t.Errorf("%q was refused: %v", tt.written, err)
			continue
		}
		if got := p.Rules[0].LaunchDir; got != tt.stored {
			t.Errorf("%q was stored as %q, want %q", tt.written, got, tt.stored)
		}
	}
}

// TestATildeWithNoHomeIsALoadErrorNotASilentMiss is R6.3 for this case: if
// the home directory cannot be found, the rule cannot mean what was written,
// and a rule that quietly matches nothing is the failure being fixed. So it is
// a LOAD error — the whole file is discarded and the default posture is in
// force, loudly.
func TestATildeWithNoHomeIsALoadErrorNotASilentMiss(t *testing.T) {
	for _, home := range []string{"", "relative/home"} {
		t.Run("HOME="+home, func(t *testing.T) {
			t.Setenv("HOME", home)

			_, err := ParseConfig([]byte(tildeRule), "inline.json")
			if err == nil {
				t.Fatal("a `~` rule was accepted with no usable home directory; it would match nothing")
			}
			if !strings.Contains(err.Error(), "~/dev/**") {
				t.Errorf("the error does not name the pattern it could not expand: %v", err)
			}

			path := filepath.Join(t.TempDir(), "sessions.json")
			if err := os.WriteFile(path, []byte(tildeRule), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Load(FileLoader(path))
			if err == nil {
				t.Error("Load reported nothing; the user wrote a rule that is not in force")
			}
			if got.Actions != Default().Actions || len(got.Rules) != 0 {
				t.Errorf("Load returned %+v with %d rules, want the default posture and none", got.Actions, len(got.Rules))
			}

			// Only a `~` needs a home. A file without one loads as before, so a
			// missing HOME is not a new way for every config to fail.
			if _, err := ParseConfig([]byte(`{"actions":{"rules":[{"do":"navigate","match":{"cwd":"/a/**"}}]}}`),
				"inline.json"); err != nil {
				t.Errorf("an absolute rule needed a home directory: %v", err)
			}
		})
	}
}

// TestAnotherUsersTildeIsRefused — `~name/…` is a shell's syntax for SOMEONE
// ELSE's home. Expanding it would mean reading the user database, which this
// loader does not do, and keeping it literally is the silent never-match
// again. So it is refused, naming the pattern.
func TestAnotherUsersTildeIsRefused(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	body := `{"actions":{"rules":[{"do":"navigate","match":{"cwd":"~user/dev/**"}}]}}`
	_, err := ParseConfig([]byte(body), "inline.json")
	if err == nil {
		t.Fatal("~user/dev/** was accepted; it can never match, because it is never expanded")
	}
	if !strings.Contains(err.Error(), "~user/dev/**") {
		t.Errorf("the error does not name the pattern: %v", err)
	}
}
