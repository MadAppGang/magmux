package mux

// The macOS notification port: requirements.md R4.4's last inch.
//
// Everything that DECIDES whether to notify is in internal/obs/notify, where
// it can be tested without a notification centre. This file is delivery and
// nothing else — which is why it is small, and why the one thing in it worth
// reading carefully is the quoting.

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/internal/obs/notify"
	"github.com/MadAppGang/magmux/obsview"
)

// notifyTimeout bounds one delivery.
//
// The bound lives HERE and not in the pipeline, deliberately: notify.Port's
// contract says an implementation must bound itself, because the pipeline
// creates no timers at all and a deadline there would be the first. Five
// seconds is far beyond a healthy `osascript`, which is tens of milliseconds,
// and far short of wedging the coordinator — which is what an unbounded one
// would do, since the send happens on the goroutine that services every
// session's next cycle.
const notifyTimeout = 5 * time.Second

// osaNotifier delivers a notification through macOS's own notification centre.
//
// It holds no state, so one serves the whole pipeline. It is reached from the
// coordinator's goroutine only; see notify.Port.
type osaNotifier struct {
	// run is the exec seam. nil takes the real osascript, and a test supplies
	// its own — which is what keeps `go test .` from raising banners on the
	// developer's own machine.
	run func(ctx context.Context, script string) error
}

// newNotifier returns the port for this platform, or nil when there is none.
//
// nil is a real answer and the pipeline knows what to do with it: a decided
// notification is still recorded, with an outcome saying nothing could deliver
// it. That is the honest state of a magmux on Linux, and it is better than a
// port that silently succeeds at doing nothing — which would put "done":true
// in the audit trail for a notification no human ever saw.
//
// requirements.md R4.4 says macOS, and this is the whole of the platform
// dependence. A `notify-send` implementation would be a second type satisfying
// the same interface and one more line here; it is not written because nothing
// in this project's evidence is from a Linux desktop, and a delivery mechanism
// nobody has watched work is not one worth claiming.
func newNotifier() notify.Port {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return &osaNotifier{}
}

// Send displays one notification.
//
// # The quoting is the load-bearing part
//
// The title is a session NAME and the body can carry a waiting reason, and
// both come out of Claude Code's registry — a file magmux does not write and
// cannot constrain. So the text is an UNTRUSTED string being placed inside an
// AppleScript literal, and the two characters that end one (`"` and `\`) are
// escaped rather than stripped, and every control character is neutralised by
// obsview.DisplaySafe — the one rule every renderer of registry text shares — because
// a newline would otherwise end the -e argument's own line. See
// appleScriptString.
//
// This comment used to say appleScriptString "is a pure function with its own
// test". It had no test anywhere until fix round 2, which is when
// TestRegistryTextReachesNoTerminalWithItsControlsIntact started driving Send
// with a hostile name.
//
// The arguments go through exec.Command's argv, never through a shell, so
// there is no second quoting layer to get wrong.
func (n *osaNotifier) Send(ctx context.Context, note notify.Note) error {
	script := fmt.Sprintf("display notification %s with title %s",
		appleScriptString(note.Body()), appleScriptString(note.Title()))

	run := n.run
	if run == nil {
		run = runOsascript
	}
	// The bound is applied here rather than left to the caller: notify.Port's
	// contract is that an implementation does not block indefinitely, and a
	// context with no deadline is exactly that.
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if err := run(ctx, script); err != nil {
		return fmt.Errorf("osascript: %w", err)
	}
	return nil
}

func runOsascript(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if text := strings.TrimSpace(string(out)); text != "" {
			return fmt.Errorf("%w: %s", err, text)
		}
		return err
	}
	return nil
}

// appleScriptString wraps an untrusted string as an AppleScript literal.
//
// Three transformations, in this order:
//
//  1. obsview.DisplaySafe, FIRST — the one rule for registry text reaching a person
//     (obsview/display.go). A newline or tab becomes a space, because a literal
//     newline inside an AppleScript string ends the statement rather than
//     appearing in it; every other control character, C1 included, becomes
//     U+FFFD. Until fix round 2 this function kept its own copy of that rule,
//     and the copy flattened C0 and DEL but passed C1 — the reason there is
//     now exactly one;
//  2. every backslash is doubled — before the quotes, because doing it after
//     would double the backslashes this function itself just added, and turn
//     an escaped quote back into a live one;
//  3. every double quote is escaped.
//
// The result is bounded, because a notification is a banner and a session name
// long enough to be a denial of service is a session name nobody can read
// anyway.
func appleScriptString(s string) string {
	const maxLen = 240

	s = obsview.DisplaySafe(s)
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	written := 0
	for _, r := range s {
		if written >= maxLen {
			b.WriteString("…")
			break
		}
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
		written++
	}
	b.WriteByte('"')
	return b.String()
}
