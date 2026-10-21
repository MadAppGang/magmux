package obs

import "time"

// PaneLocator answers which magmux pane owns a process, and whether that pane
// is still there.
//
// # Why this is a port and not a function
//
// This package cannot import package main — Go forbids importing main — and
// that is the point rather than the obstacle. magmux's pane model and this
// session model are different vocabularies: a pane is a rectangle with a PTY
// and a slot in an append-only table, a session is something a tool wrote a
// record about. Only the two ends know both, so the pane answer arrives
// through an interface this package declares and package main implements
// (obspane.go).
//
// # Both answers are tri-state, and that is load-bearing
//
// [Liveness] and not a bool, and a found-flag that is a Liveness rather than
// an ok-bool, because "we could not tell" is a real outcome here and a common
// one: the walk described below is made of syscalls, and a syscall that fails
// says nothing about whether the pane exists. A session reported pane-less
// because nobody could look would be filtered out of a report by a consumer
// acting on [Session.PaneAlive], which is the exact failure the tri-state
// exists to prevent.
//
// # The matching mechanism is ANCESTRY, and an implementer who compares pids
// will match nothing
//
// magmux spawns a SHELL and the shell spawns the tool. So the pane's
// cmd.Process.Pid is the shell's and the session record's pid is the tool's;
// they are never equal. An implementation walks UP the process tree from the
// record's pid until it reaches a pid that is a pane's, and it bounds that
// walk — a cycle, a reparented orphan or an unusually deep tree must not hang
// the caller. Exceeding the bound is LivenessUnknown, never LivenessNo.
//
// # It may do I/O, and it is called from an actor goroutine for that reason
//
// PaneFor is a sysctl on Darwin and a /proc read on Linux. magmux's own rule —
// never hold treeMu across blocking I/O — therefore applies to any
// implementation that consults the pane table: resolve under the lock, release
// it, then walk. The rule has no exception; controller.Poll was documented as
// one and was the bug.
//
// # One locator serves every session, so it must be safe for concurrent use
//
// There is one actor goroutine per session and they all hold the same locator
// — unlike the MIDDLEWARE in front of it, which is per-session precisely
// because it remembers things. An implementation that counts, caches or
// memoises anything therefore needs its own synchronisation; magmux's own
// takes the pane table's read lock and keeps nothing. The distinction is
// stated here because it is the opposite of the rule one line downstream, and
// a fake locator that counted its calls was the first thing to trip over it.
//
// A nil PaneLocator means "no pane table in this process", which is honest for
// anything running outside a magmux — `magmux agents` is exactly that — and is
// answered by leaving both pane facts undetermined rather than by asserting
// their absence.
type PaneLocator interface {
	// PaneFor reports the pane whose process is an ancestor of pid.
	//
	// found is LivenessYes with a usable paneID; LivenessNo when the walk
	// completed and reached nothing of ours, which is a determined "this is
	// not one of magmux's sessions"; and LivenessUnknown when the walk could
	// not be completed. paneID is meaningless unless found is LivenessYes.
	PaneFor(pid int) (paneID int, found Liveness)

	// PaneAlive reports whether the pane is still part of magmux.
	//
	// It is NOT "the process in it is still running" — that is the other
	// gate, [Session.ProcessAlive], and the two are separate fields precisely
	// because they fail separately and have different remedies.
	PaneAlive(paneID int) Liveness
}

// PaneActivity answers WHEN a pane last produced output and when something was
// last pushed into it. It is the whole of the inferred tier's input.
//
// # Why it is a second interface and not two more methods on PaneLocator
//
// Segregation, for one reason that is not taste: a method added to
// [PaneLocator] would have to be implemented by every existing fake, including
// the ones in tests written before this port existed. More importantly the two
// ports answer different kinds of question — PaneLocator is about IDENTITY and
// EXISTENCE and is consulted to decide whether a session belongs in the report
// at all, while this one is about TRAFFIC and is consulted to decide what the
// session is doing. A consumer can hold one without the other, and `magmux
// obs` running outside any magmux honestly holds neither.
//
// # The two instants, and what each one means
//
// lastOutput is when BYTES LAST ARRIVED FROM THE CHILD. It is not "when the
// pane last changed on screen": magmux repaints its own chrome — a tint, a
// ✓ DONE overlay, a focus change, a resize, the status bar — about twenty
// times for every one time a child prints anything, and an instant stamped at
// those sites would mean "magmux drew something", so a five-minute silence
// could never be observed because something repainted three seconds ago.
//
// lastInput is when something was last pushed INTO the pane — a human
// keystroke or a controller's injected instruction. It CANCELS the staleness
// measurement rather than restarting it: a pane that has just been typed into
// is waiting for a program to answer a human, which is a different fact
// belonging to a different observer. The ladder measures the CHILD's silence.
//
// Both are the ZERO instant when the thing they describe has not happened yet,
// and the zero instant is a real answer — a pane that has never printed a byte
// is the most interesting staleness there is. It is not an error and it is not
// "now".
//
// # Tri-state, by the ok flag rather than by a Liveness
//
// ok is false when this process has no such pane, which is the honest and
// normal answer for anything running outside a magmux. It is deliberately not
// a [Liveness]: there is no useful distinction here between "no such pane" and
// "could not tell", because both leave the caller with no instants and the
// caller's response to either is the same — say nothing.
//
// # One implementation serves every session
//
// Like [PaneLocator], and for the same reason: there is one actor goroutine
// per session and they all hold the same instance, so an implementation that
// counts, caches or memoises anything needs its own synchronisation. magmux's
// own takes the pane table's read lock, reads two fields under the pane's own
// lock, and keeps nothing.
type PaneActivity interface {
	// Activity reports the two instants for paneID.
	//
	// It must not block: it is called on an actor's goroutine once per
	// observation, and an implementation that reaches for a lock held across
	// I/O would put every session's chain behind that I/O.
	Activity(paneID int) (lastOutput, lastInput time.Time, ok bool)
}

// PaneFocuser moves keyboard focus to a pane. It is the FIRST WRITE port in
// this layer, and for now it is the only one.
//
// # What makes it safe enough to be first
//
// Focus touches the VIEW. No process is signalled, no file is written, no byte
// reaches any child, and the worst outcome of getting it wrong is that a human
// is looking at the wrong pane — which they can undo by looking at another
// one. Every other action requirements.md names acts on a live agent and is
// deferred; see internal/obs/act, where the deferral is enforced rather than
// merely documented.
//
// # A third port rather than a method on [PaneLocator]
//
// The same segregation argument [PaneActivity] makes, with more force: a write
// method on the locator would be implemented by every existing fake, including
// the read-only ones in tests written before actuation existed, and a port
// that can change something should have to be handed over deliberately. A
// process that holds a locator and no focuser can observe and cannot act, and
// that is a distinction worth being able to make — `magmux agents`, running
// outside any magmux, holds neither.
//
// # It is called from an actor goroutine, and must not block indefinitely
//
// Like every other port here. magmux's own implementation takes the pane
// table's write lock, moves one pointer and releases it, doing no I/O under
// the lock; an implementation that waited on a human or on a socket would
// wedge that session's actor and everything queued behind it.
//
// A nil PaneFocuser means "nothing in this process can move focus", which is
// the honest answer for anything running outside a magmux, and the layer
// answers a navigate request for such a session by declaring no capability at
// all rather than by refusing one it has — "cannot" and "will not" are
// different answers and neither is "did nothing".
type PaneFocuser interface {
	// Focus moves keyboard focus to paneID and reports which pane held it
	// before, so that a trail records what was displaced and a human can put
	// it back. previous is -1 when nothing held focus.
	//
	// It is IDEMPOTENT: focusing the pane that already has focus succeeds and
	// reports itself as the previous holder.
	//
	// An error means focus did NOT move, and it must say why in words a
	// person can act on — a pane that was closed and one that is not on
	// screen are different situations with different remedies.
	Focus(paneID int) (previous int, err error)
}
