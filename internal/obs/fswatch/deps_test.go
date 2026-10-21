package fswatch

import (
	"os"
	"strings"
	"testing"
)

// magmux's dependency rule used to be "none": two golang.org/x modules for the
// PTY ioctls and raw mode, and nothing else. That rule was lifted for exactly
// one module, for a reason that is itself measured — see
// TestFsnotifySeesInPlaceWrites, which is the behaviour being bought.
//
// "Lifted for one module" is a claim that decays silently. This test is what
// keeps it true: adding a dependency now requires deciding to, in a diff that
// says so.
func TestDependencySetIsExactlyWhatWasDecided(t *testing.T) {
	const goMod = "../../../go.mod"
	allowed := map[string]string{
		"golang.org/x/sys":             "PTY ioctls",
		"golang.org/x/term":            "raw mode",
		"github.com/fsnotify/fsnotify": "the directory watch this package is built on",
	}

	b, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatalf("reading %s: %v", goMod, err)
	}

	seen := map[string]bool{}
	var inBlock bool
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "//") || line == "":
			continue
		}

		var spec string
		switch {
		case inBlock:
			spec = line
		case strings.HasPrefix(line, "require "):
			spec = strings.TrimPrefix(line, "require ")
		default:
			continue
		}
		spec, _, _ = strings.Cut(spec, "//")
		fields := strings.Fields(spec)
		if len(fields) < 2 {
			continue
		}
		path, version := fields[0], fields[1]

		if _, ok := allowed[path]; !ok {
			t.Errorf("go.mod requires %s, which is not one of the three modules magmux "+
				"decided to depend on. Adding a dependency is a decision; make it "+
				"explicitly here, or drop the import.", path)
			continue
		}
		seen[path] = true

		if path == "github.com/fsnotify/fsnotify" && version != fsnotifyVersion {
			t.Errorf("go.mod requires fsnotify %s but the regression test names %s. "+
				"Update the constant together with the module, so the failure message "+
				"in TestFsnotifySeesInPlaceWrites names the version that actually broke.",
				version, fsnotifyVersion)
		}
	}

	for path, why := range allowed {
		if !seen[path] {
			t.Errorf("go.mod no longer requires %s, which was here for %s. "+
				"If the need has genuinely gone, remove it from this test in the same diff.",
				path, why)
		}
	}
}
