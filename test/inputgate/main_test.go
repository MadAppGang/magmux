package inputgate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Built once by TestMain. Every binary lives under /tmp on purpose: a pane's
// command line is how magmux recognises a DIRECT `claude` launch, and the
// harness's own TMPDIR may contain the word "claude" (it does inside a Claude
// Code session), which would turn an undeclared pane into a direct one.
var (
	builtBin  string // magmux, or MAGMUX_BIN
	agentBin  string // the fake Claude, under a name magmux cannot mistake for claude
	directBin string // the SAME fake, named exactly `claude` (a direct launch)
	buildErr  error
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("/tmp", "igbin")
	if err != nil {
		buildErr = err
		return m.Run()
	}
	defer os.RemoveAll(dir)

	if bin := os.Getenv("MAGMUX_BIN"); bin != "" {
		builtBin = bin
	} else {
		bin := filepath.Join(dir, "magmux")
		if out, berr := exec.Command("go", "build", "-o", bin, "../../cmd/magmux").CombinedOutput(); berr != nil {
			buildErr = fmt.Errorf("go build ../../cmd/magmux: %v\n%s", berr, out)
			return m.Run()
		}
		builtBin = bin
	}

	agent := filepath.Join(dir, "ttyagent")
	if out, berr := exec.Command("go", "build", "-o", agent, "./testdata/fakeclaude").CombinedOutput(); berr != nil {
		buildErr = fmt.Errorf("go build ./testdata/fakeclaude: %v\n%s", berr, out)
		return m.Run()
	}
	agentBin = agent

	if err := os.MkdirAll(filepath.Join(dir, "direct"), 0o755); err != nil {
		buildErr = err
		return m.Run()
	}
	direct := filepath.Join(dir, "direct", "claude")
	if out, berr := exec.Command("go", "build", "-o", direct, "./testdata/fakeclaude").CombinedOutput(); berr != nil {
		buildErr = fmt.Errorf("go build ./testdata/fakeclaude (direct): %v\n%s", berr, out)
		return m.Run()
	}
	directBin = direct
	return m.Run()
}
