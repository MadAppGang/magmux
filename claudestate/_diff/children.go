package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/proclive"
)

// The sandbox pids (§8.2 step 4, decision AQ1 as the owner replaced it).
//
// Every canonical pid becomes the pid of a /bin/cat child this driver spawns,
// with stdin a pipe the driver holds, in ascending canonical order. An ALIVE
// pid's child is kept running until teardown. A DEAD pid's child has already
// been ended and reaped before tt runs: its stdin is closed, cat reads EOF and
// exits, and Wait reaps it. So every pid tt and the library probe is this
// driver's own child, alive or reaped, and the only probe either side makes is
// kill(pid, 0). The driver itself sends NO signal: ending a child is closing
// its stdin, and a child that will not end is reported and left alone.

// child is one /bin/cat.
type child struct {
	canon int
	pid   int
	stdin io.WriteCloser
	done  chan error // receives cmd.Wait's result once
	ended bool       // Wait has returned: the child is reaped
}

// children is one scenario's set.
type children struct {
	list   []*child
	oracle ports.ProcessOracle
}

// errNotAscending: the kernel did not hand out ascending pids.
var errNotAscending = errors.New("the children's pids are not strictly ascending in spawn order")

// spawnChildren starts one child per canonical pid, ascending, and checks the
// pids came back strictly ascending. On failure every child it started is
// ended before it returns.
func spawnChildren(canon []int) (*children, error) {
	cs := &children{oracle: proclive.New()}
	for _, c := range canon {
		cmd := exec.Command("/bin/cat")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			cs.endAll(5 * time.Second)
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			cs.endAll(5 * time.Second)
			return nil, fmt.Errorf("starting /bin/cat: %w", err)
		}
		ch := &child{canon: c, pid: cmd.Process.Pid, stdin: stdin, done: make(chan error, 1)}
		go func() { ch.done <- cmd.Wait() }()
		cs.list = append(cs.list, ch)
	}
	for i := 1; i < len(cs.list); i++ {
		if cs.list[i].pid <= cs.list[i-1].pid {
			cs.endAll(5 * time.Second)
			return nil, errNotAscending
		}
	}
	return cs, nil
}

// pidMap is canonical -> sandbox.
func (cs *children) pidMap() map[int]int {
	m := make(map[int]int, len(cs.list))
	for _, c := range cs.list {
		m[c.canon] = c.pid
	}
	return m
}

// inverse is sandbox -> canonical.
func (cs *children) inverse() map[int]int {
	m := make(map[int]int, len(cs.list))
	for _, c := range cs.list {
		m[c.pid] = c.canon
	}
	return m
}

// end closes one child's stdin and waits, bounded, for Wait to reap it.
func (c *child) end(bound time.Duration) bool {
	if c.ended {
		return true
	}
	_ = c.stdin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	select {
	case <-c.done:
		c.ended = true
		return true
	case <-ctx.Done():
		return false
	}
}

// killDead ends and reaps every child whose canonical pid is dead, then
// requires kill(pid, 0) to say ESRCH for each, and kill(pid, 0) to say alive
// for every other. A dead pid that answers alive was recycled at once: the
// scenario is RACED.
func (cs *children) killDead(dead map[int]bool) (raced []int, err error) {
	for _, c := range cs.list {
		if dead[c.canon] && !c.end(5*time.Second) {
			return nil, fmt.Errorf("the child for dead pid %d (sandbox %d) did not exit after its stdin closed", c.canon, c.pid)
		}
	}
	return cs.check(dead), nil
}

// check is the reuse guard: every pid must answer as its scenario says.
func (cs *children) check(dead map[int]bool) (raced []int) {
	for _, c := range cs.list {
		want := model.LivenessYes
		if dead[c.canon] {
			want = model.LivenessNo
		}
		if got := cs.oracle.Alive(c.pid); got != want {
			raced = append(raced, c.canon)
		}
	}
	return raced
}

// endAll is teardown: every remaining child's stdin is closed and it is
// waited for. The pids of children that did not exit are returned; they are
// reported and NOT signalled.
func (cs *children) endAll(bound time.Duration) (stuck []int) {
	for _, c := range cs.list {
		if !c.end(bound) {
			stuck = append(stuck, c.pid)
		}
	}
	return stuck
}
