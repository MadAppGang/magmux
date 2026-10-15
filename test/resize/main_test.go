package resize_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// builtBin is the magmux TestMain built from ../../cmd/magmux, or "" when
// MAGMUX_BIN was set (it wins) or the build failed (buildErr says why).
var (
	builtBin string
	buildErr error
)

// TestMain builds the binary once so `go test ./...` runs these tests instead
// of skipping them; MAGMUX_BIN still overrides. Same idiom as mux's TestMain.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("MAGMUX_BIN") == "" {
		dir, err := os.MkdirTemp("", "magmux-resize-bin")
		if err != nil {
			buildErr = err
		} else {
			defer os.RemoveAll(dir)
			bin := filepath.Join(dir, "magmux")
			build := exec.Command("go", "build", "-o", bin, "../../cmd/magmux")
			if out, berr := build.CombinedOutput(); berr != nil {
				buildErr = fmt.Errorf("go build ../../cmd/magmux: %v\n%s", berr, out)
			} else {
				builtBin = bin
			}
		}
	}
	return m.Run()
}
