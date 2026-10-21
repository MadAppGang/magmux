package pipeline

import (
	"github.com/MadAppGang/magmux/internal/obs/navigate"
)

// navigateStage is the factory for the one stage in a chain that can WRITE:
// the navigator, which focuses the pane a session is running in.
//
// # Appended by this package rather than remembered by a caller
//
// The same rule as the pane gate and the verdict, for the same reason. A
// caller that forgot it would produce a pipeline that silently cannot act,
// and "nothing happened" is the failure mode this whole layer is written
// against. It sits AFTER the tool middleware and BEFORE the verdict: it needs
// [obs.Session.PaneID], which the gate at the front resolves, and the verdict
// is documented as last and always.
//
// # It is added whether or not there is a focuser
//
// A nil [Config.Focuser] does not remove the stage; it removes the stage's
// ability to act, which it reports by declaring no capability. That is the
// honest shape for a process with no pane table — `magmux agents` is exactly one
// — and it is the difference between "nothing here can do that" and "you may
// not", which act.ErrNotCapable and act.ErrForbidden keep apart all the way
// out to the audit trail.
//
// [navigate.Stage] satisfies [Middleware] and [Actuating] STRUCTURALLY,
// naming neither, so the dependency runs one way: this package imports that
// one to append the stage, exactly as it imports verdict.
func navigateStage(cfg Config) Factory {
	return func(sessionID string) Middleware {
		return navigate.New(navigate.Config{Focuser: cfg.Focuser})
	}
}
