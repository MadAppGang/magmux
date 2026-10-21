// Package stage is the contract every per-session stage implements, library
// and host alike. It is a leaf (it imports only model) so a host stage need
// not import the pipeline that runs it.
package stage

import (
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// Stage is one step of an actor's chain: value in, value out. The result must
// keep the input's Key; a result with any other Key, the zero Key included,
// is discarded and reported (C21).
type Stage interface {
	Name() string
	Observe(model.Session) model.Session
}

// Factory builds a fresh Stage per actor. A stage instance is never shared
// between sessions, which is what lets it remember things without a lock.
type Factory func(key model.Key) Stage

// Waker is a Stage that wants to be observed again later. ok == false or
// d <= 0 arms nothing. The actor arms ONE one-shot timer, from the earliest
// answer of all its Wakers.
type Waker interface {
	Stage
	WakeIn() (time.Duration, bool)
}

// Targeting is a Stage that reads files and must say which, BEFORE the arm
// wave, so they are armed and identity-verified before any byte is read.
//
// Targets is called with the Location the coordinator computed this cycle. It
// must never read file content, and it returns files only, never a directory.
// Granted is called after the arm wave and the identity pass, before Observe,
// on the same actor goroutine.
type Targeting interface {
	Stage
	Targets(s model.Session, loc model.Location) []model.WatchTarget
	Granted(g Grant)
}

// Grant is what a Targeting stage may read this cycle.
type Grant struct {
	// Armed: held and identity-verified this cycle (shed targets included).
	Armed []model.WatchTarget
	// Pull: refused by the budget; readable under this cycle's degraded report.
	Pull []model.WatchTarget
	// Unwatched: Watch is off or on-demand, so every target is readable as a pull.
	Unwatched bool
}

// Effect is any host value. The pipeline hands back EXACTLY the value
// Effects() returned, never a copy-wrapper, so a host may type-assert its own
// effect type; that assertion is the contract of the hand-off.
type Effect interface {
	EffectName() string
}

// Effecting is a Stage that produces effects. They are drained after Observe
// and handed over exactly once.
type Effecting interface {
	Stage
	Effects() []Effect
}
