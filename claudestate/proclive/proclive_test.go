package proclive

// The ONE test file in the module that reaches the real kill(pid, 0), and it is
// allowlisted in G8 for exactly two subjects: this test process's own pid, and
// a child it spawned and reaped itself. No other pid is ever probed.

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/model"
)

func withKill(t *testing.T, fn func(int) error) *[]int {
	t.Helper()
	var seen []int
	prev := kill
	kill = func(pid int, sig syscall.Signal) error {
		seen = append(seen, pid)
		if sig != 0 {
			t.Errorf("signal %v was sent; this package must never affect a process", sig)
		}
		return fn(pid)
	}
	t.Cleanup(func() { kill = prev })
	return &seen
}

func TestTheErrnosMapOntoTheThreeAnswers(t *testing.T) {
	cases := []struct {
		err  error
		want model.Liveness
	}{
		{nil, model.LivenessYes},
		{syscall.EPERM, model.LivenessYes},
		{syscall.ESRCH, model.LivenessNo},
		{os.NewSyscallError("kill", syscall.ESRCH), model.LivenessNo},
		{syscall.EINVAL, model.LivenessUnknown},
	}
	for _, c := range cases {
		withKill(t, func(int) error { return c.err })
		if got := New().Alive(4242); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}

func TestANonPositivePIDIsUnknownWithoutASyscall(t *testing.T) {
	seen := withKill(t, func(int) error { return nil })
	for _, pid := range []int{0, -1, -4242} {
		if got := New().Alive(pid); got != model.LivenessUnknown {
			t.Errorf("pid %d: %v", pid, got)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("a non-positive pid reached the kernel: %v", *seen)
	}
}

// The real syscall, on this process's own pid.
func TestTheRealOracleSeesThisProcess(t *testing.T) {
	if got := New().Alive(os.Getpid()); got != model.LivenessYes {
		t.Fatalf("this process reads %v", got)
	}
}

// The real syscall, on a child this test spawned and reaped: after Wait the
// kernel has released the pid, so it is ESRCH unless it was recycled in the
// instant between, which the test reports rather than fails on.
func TestTheRealOracleSeesAReapedChildAsGone(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawning a child: %v", err)
	}
	pid := cmd.Process.Pid
	switch got := New().Alive(pid); got {
	case model.LivenessNo:
	case model.LivenessYes:
		t.Skipf("pid %d was recycled between the reap and the probe", pid)
	default:
		t.Fatalf("a reaped child reads %v", got)
	}
}
