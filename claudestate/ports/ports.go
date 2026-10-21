// Package ports is the boundary a host implements.
//
// The rule every port here shares: a NIL port means UNDETERMINED, never a
// negative (FR21). A process with no pane table leaves both pane facts
// Unknown rather than asserting the session has no pane, and a host with no
// process oracle leaves every liveness Unknown rather than reporting the
// session dead.
package ports

import (
	"context"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// ProcessOracle answers whether a process exists. A pid <= 0 must answer
// LivenessUnknown without a syscall.
type ProcessOracle interface {
	Alive(pid int) model.Liveness
}

// PaneLocator answers which host pane a session runs in, and whether that
// pane is still part of the host. One instance serves every actor, so an
// implementation must be safe for concurrent use.
type PaneLocator interface {
	// PaneFor: Yes with a usable ref; No when the walk completed and the
	// session is not one of ours; Unknown when it could not tell.
	PaneFor(s model.Session) (model.PaneRef, model.Liveness)
	// PaneAlive is "part of the host", NOT "a process is running in it".
	PaneAlive(ref model.PaneRef) model.Liveness
}

// CycleLocator is an optional extension of a PaneLocator: snapshot the pane
// table once per cycle. On error the cycle uses [UnknownLocator] — never the
// configured locator and never the previous cycle's table.
type CycleLocator interface {
	ForCycle(ctx context.Context) (PaneLocator, error)
}

// UnknownLocator answers ("", LivenessUnknown) and LivenessUnknown to every
// query. It is what a cycle uses when a CycleLocator fails.
var UnknownLocator PaneLocator = unknownLocator{}

type unknownLocator struct{}

func (unknownLocator) PaneFor(model.Session) (model.PaneRef, model.Liveness) {
	return "", model.LivenessUnknown
}

func (unknownLocator) PaneAlive(model.PaneRef) model.Liveness { return model.LivenessUnknown }

// PaneActivity reports when a pane last produced output and when something
// was last pushed into it. It must not block. ok is false when this process
// has no such pane.
type PaneActivity interface {
	Activity(ref model.PaneRef) (lastOutput, lastInput time.Time, ok bool)
}

// PaneShell reports what the terminal says about a pane's shell. It must not
// block. ok is false when there is no such pane or it has no terminal.
type PaneShell interface {
	Shell(ref model.PaneRef) (model.ShellState, bool)
}

// Listing is one `claude agents --json` result as a host obtained it.
type Listing struct {
	Raw    []byte
	At     time.Time
	Cached bool
}

// AgentsListing obtains a listing. Spawning, caching and locking live in the
// host; the library only schedules the call.
type AgentsListing interface {
	List(ctx context.Context) (Listing, error)
}
