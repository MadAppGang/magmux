package pipeline

import "github.com/MadAppGang/magmux/internal/obs"

// PaneGate is the factory for the middleware that fills in a session's pane:
// [obs.Session.PaneID] and [obs.Session.PaneAlive], the second of the two
// independent gates on whether a session should be reported at all.
//
// It is added to the front of every chain automatically when [Config.Locator]
// is set, so a caller cannot forget it. A nil locator produces a stage that
// leaves both facts exactly as they arrived, which is what running outside a
// magmux honestly looks like.
func PaneGate(loc obs.PaneLocator) Factory {
	return func(sessionID string) Middleware { return &paneGate{loc: loc} }
}

// paneGate is ONE session's pane resolver. It remembers the pane it matched,
// which is the whole reason a middleware instance may not be shared between
// sessions: a shared one would answer every session with the first session's
// pane.
type paneGate struct {
	loc obs.PaneLocator
	// pane is the pane this session was matched to, and matched says whether
	// it was. The match is remembered and the MISS is not, deliberately:
	//
	//   - a match cannot go stale in the way that matters. Ancestry is fixed
	//     at fork time, so the pane that owns a process is the pane that owns
	//     it for as long as it lives, and a pane id is a permanent slot in an
	//     append-only table. What CAN change is whether that pane is still
	//     there, and that is re-asked on every observation.
	//
	//     That argument holds for the SESSION's side only, and it needs the
	//     pane's side to hold too: a pane's pid must stay in the table only
	//     while the pane's child still owns the number. Until fix round 2 a
	//     REAPED pane kept its pid in both of magmux's tables, so once the OS
	//     reused it a session under the new owner matched the finished pane —
	//     and this cache then kept that wrong match for the actor's life. Both
	//     tables now drop a reaped pane's pid (muxPaneLocator.panePIDs,
	//     sockPaneReaped), so a first match can no longer be made through a
	//     recycled number, and there is nothing wrong for this to remember.
	//   - a miss can. The pane table grows — an agent's open_pane, the panel
	//     being revealed — and a session that was nobody's a moment ago is
	//     cheap enough to ask about again. An UNKNOWN especially: it means the
	//     walk failed, and caching a failure is how a transient syscall error
	//     becomes a permanent wrong answer.
	pane    int
	matched bool
}

func (g *paneGate) Name() string { return "pane" }

// Observe resolves the pane and records both facts, or records neither.
//
// A miss leaves the fact set untouched rather than writing "no pane" into it.
// The model already spells that: a nil PaneID means the session is not one of
// magmux's panes, which is not hypothetical — discovery sees sessions magmux
// did not start — and they are NOT filtered out here. Whether to show them is
// a sink's decision, and throwing them away at the source removes the choice.
//
// PaneAlive stays undetermined for a session with no pane, because it is a
// fact about a pane and there is no pane to have one. Writing LivenessNo there
// would tell a consumer acting on the gate that a pane had vanished, when what
// happened is that magmux never owned one.
func (g *paneGate) Observe(s obs.Session) obs.Session {
	if g.loc == nil {
		return s
	}

	if !g.matched {
		id, found := g.loc.PaneFor(s.PID)
		if found != obs.LivenessYes {
			// LivenessNo — walked the whole chain and reached nothing of ours.
			// LivenessUnknown — could not walk it. Neither is a claim about a
			// pane, so neither writes one.
			return s
		}
		g.pane, g.matched = id, true
	}

	pane := g.pane
	s.PaneID = obs.Ptr(pane)
	// Re-asked on every observation, never cached: this is the half that
	// changes. A pane that has been closed reports LivenessNo while PaneID
	// keeps naming it, which is exactly the information the gate exists to
	// carry — "this session ran in pane 3, and pane 3 is gone" is a different
	// situation from "this session was never one of ours".
	s.PaneAlive = g.loc.PaneAlive(pane)
	return s
}
