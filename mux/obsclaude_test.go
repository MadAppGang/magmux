package mux

// Phase 4's magmux-side surface: the one encoder, the moved turn types, and
// the launch-directory port.
//
// NOTHING here reads ~/.claude, probes a live pid or speaks to tmux. The panes
// are PTY-less control panes carrying a pid nobody ever signals.

import (
	"os"
	"os/exec"
	"testing"
)

// ── the launch-directory port ───────────────────────────────────────────────

// The port answers with what magmux passed to the child, found by the same
// ancestry walk the pane gate uses: the session's pid is the tool's and the
// pane's is the shell's, so they are never equal.
func TestLaunchDirIsTheDirectoryMagmuxSpawnedWith(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	const spawned = "/Users/user/dev/sampleapp"

	p := m.paneByID(0)
	p.mu.Lock()
	p.cmd = &exec.Cmd{Process: &os.Process{Pid: 400}, Dir: spawned}
	p.mu.Unlock()
	givePID(t, m.paneByID(1), 500)

	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{
		900: 850, // claude
		850: 400, // the shell magmux spawned
		400: 300,
		300: 1,
	})}

	dir, ok := loc.LaunchDir(900)
	if !ok {
		t.Fatal("LaunchDir(900) found nothing; the tool's pid is never the pane's")
	}
	if dir != spawned {
		t.Errorf("LaunchDir(900) = %q, want %q", dir, spawned)
	}
}

// A pane started with no directory of its own inherited magmux's, so that is
// the answer. magmux never chdirs, which is what makes it stable.
func TestLaunchDirFallsBackToMagmuxsOwnDirectory(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), 400)
	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{700: 400, 400: 1})}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	dir, ok := loc.LaunchDir(700)
	if !ok {
		t.Fatal("LaunchDir(700) found nothing")
	}
	if dir != wd {
		t.Errorf("LaunchDir(700) = %q, want magmux's own directory %q", dir, wd)
	}
}

// A session that is not in any of this magmux's panes gets NO answer, rather
// than magmux's own directory.
//
// The difference matters because a wrong directory is not harmless here even
// though it cannot produce a wrong session: it produces a stat in a directory
// that has nothing to do with the session, and then the scan runs anyway. An
// answer nobody can stand behind is worse than no answer.
func TestLaunchDirRefusesASessionThatIsNotOurs(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), 400)
	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{999: 998, 998: 1})}

	if dir, ok := loc.LaunchDir(999); ok {
		t.Errorf("LaunchDir(999) = %q, ok; that session belongs to no pane of ours", dir)
	}
}
