package pipeline

import (
	"github.com/MadAppGang/magmux/internal/obs/verdict"
)

// verdictStage is the factory for the stage appended to the END of every
// chain: the inferred tier, the two-tier resolution, and the attention verdict.
//
// # Why this package depends on that one and not the other way round
//
// [verdict.Stage] satisfies [Middleware] and [Waker] STRUCTURALLY, without
// naming either — it imports internal/obs and internal/obs/termev and nothing
// else from this layer. That is what lets the dependency run one way and lets
// the stage be appended here rather than remembered by every caller. A verdict
// package that imported this one for the interface would make the append
// impossible and put the policy back in the caller's hands, which is where a
// document silently shipped with no attention verdict comes from.
//
// # It is added whether or not there is an inferred tier
//
// A nil [Config.Activity] does not remove the stage; it removes the stage's
// terminal evidence. The resolution still runs, every session still carries a
// [obs.Session.Confidence] and an attention verdict, and no conflict can arise
// because only one tier spoke. That is what `magmux agents` looks like from
// outside a magmux, and it is honest rather than degraded.
func verdictStage(cfg Config) Factory {
	return func(sessionID string) Middleware {
		return verdict.New(verdict.Config{
			Activity: cfg.Activity,
			Shell:    cfg.Shell,
			Now:      cfg.Now,
		})
	}
}
