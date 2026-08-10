package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSendVerbReachesPane is the end-to-end proof that a controlled session
// actually works: an instruction pushed over the IPC socket must arrive on
// the pane's PTY as if it had been typed.
//
// The pane runs `head -n 1 | sed s/^/GOT:/`, so it blocks until a line of
// input arrives and then puts the received text somewhere observable — the
// exit event's lastLine. Nothing short of the bytes reaching the PTY produces
// that, which is what makes this a real assertion rather than a check that
// magmux logged its own intent.
//
// The command deliberately contains no `$`: magmux wraps -e commands in
// `zsh -l -c`, so a `read line; echo $line` fixture has its variable expanded
// by the outer shell and silently reports an empty echo — a fixture that
// fails while the feature works.
//
// It also covers two things the control pane could easily break:
//   - the panel must appear in `results` as a panel, not as a stuck session
//   - -w must still auto-exit, which it cannot if the panel (never dead,
//     never idle) is counted as an unfinished pane
func TestSendVerbReachesPane(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY/socket test requires darwin or linux")
	}

	binPath := magmuxBinForTest(t)

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	// A freshly opened PTY reports 0x0, and magmux sizes its panes from the
	// terminal — so without this every pane's screen is zero rows and the
	// exit event's lastLine is empty no matter what the pane printed.
	setWinSize(master, 24, 100)

	// The trailing sleep matters: the pane's exit event reports the last line
	// on its *screen*, and the child's final write has to be read and parsed
	// before the process exit is observed. Exiting the instant sed writes
	// races that and reports an empty line.
	cmd := exec.Command(binPath, "-c", "-w", "-e", `head -n 1 | sed s/^/GOT:/; sleep 1`)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()

	sockPath := fmt.Sprintf("/tmp/magmux-%d.sock", cmd.Process.Pid)
	var conn net.Conn
	for i := 0; i < 40; i++ {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("could not connect to magmux socket %s: %v", sockPath, err)
	}
	defer conn.Close()

	// Give the pane a moment to reach its `read`, then drive it.
	time.Sleep(400 * time.Millisecond)
	const payload = "ping-42"
	write := func(v any) {
		b, _ := json.Marshal(v)
		if _, err := conn.Write(append(b, '\n')); err != nil {
			t.Fatalf("socket write: %v", err)
		}
	}
	write(map[string]any{
		"type": "pilot", "event": "start", "pane": 0,
		"goal": "prove send reaches the pane", "steps": 1, "model": "test/none",
	})
	write(map[string]any{
		"type": "send", "pane": 0, "text": payload, "label": "step 1/1",
	})

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	var (
		sawControlOut bool
		echoed        bool
		panelState    string
		gotResults    bool
		lastLines     []string // kept for the failure message
	)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "control":
			if ev["dir"] == "out" && ev["text"] == payload {
				sawControlOut = true
			}
		case "exit":
			// The pane's own output is the only witness that the bytes
			// actually landed on the PTY.
			s, _ := ev["lastLine"].(string)
			lastLines = append(lastLines, s)
			if strings.Contains(s, "GOT:"+payload) {
				echoed = true
			}
		case "results":
			gotResults = true
			panes, _ := ev["panes"].([]any)
			for _, p := range panes {
				m, _ := p.(map[string]any)
				if b, _ := m["control"].(bool); b {
					panelState, _ = m["state"].(string)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("socket read error before EOF: %v", err)
	}

	if !sawControlOut {
		t.Error("no control event with dir=out for the instruction we sent")
	}
	if !echoed {
		t.Errorf("the instruction never reached the pane's PTY: want an exit lastLine containing %q, got %q",
			"GOT:"+payload, lastLines)
	}
	if !gotResults {
		t.Fatal("no results event — magmux did not shut down cleanly")
	}
	if panelState != "panel" {
		t.Errorf("control pane reported state %q in results, want %q", panelState, "panel")
	}
}

// TestControlPaneDoesNotBlockAutoExit isolates the deadlock the control pane
// would otherwise cause. The panel has no process, so it is never dead and
// never goes idle; if allPanesDone counted it, -w could never fire and magmux
// would hang forever after its real work finished.
func TestControlPaneDoesNotBlockAutoExit(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY test requires darwin or linux")
	}

	binPath := magmuxBinForTest(t)
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	cmd := exec.Command(binPath, "-c", "-w", "-e", `sh -c "echo hi; sleep 0.5"`)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()

	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()

	select {
	case <-time.After(8 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-exitCh
		t.Fatal("magmux with -c never auto-exited: the control pane is being counted as an unfinished session")
	case err := <-exitCh:
		if err != nil {
			t.Fatalf("magmux exited with error: %v", err)
		}
	}
}

// TestKeyBytes covers the named-key table a pilot uses to answer prompts and
// interrupt a session, including the single-character passthrough that lets
// {"keys":["2"]} answer a numbered menu.
func TestKeyBytes(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"enter", "\r", true},
		{"Escape", "\x1b", true},
		{"ctrl-c", "\x03", true},
		{"up", "\x1b[A", true},
		{"2", "2", true},
		{"nonsense-key", "", false},
	}
	for _, c := range cases {
		got, ok := keyBytes(c.in)
		if ok != c.ok {
			t.Errorf("keyBytes(%q) ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && string(got) != c.want {
			t.Errorf("keyBytes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
