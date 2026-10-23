package mux

// The pane ports, answered over the SOCKET instead of out of magmux's own
// pane table. This is what lets `magmux agents focus` — a process that is not a
// magmux — actually focus a pane.
//
// # Phase 6 left this refusing, and the reason it gave was the wrong one
//
// It said the socket protocol had no verb mapping a session to a pane. That is
// false, and it was the SECOND time this project made that claim (phase 3 made
// it about the pane column). `sockList` is buildPaneResults verbatim, and
// inside that builder `entry["pid"] = c.Process.Pid` — so every pane with a
// live child publishes its pid, and it is the SHELL's pid, which is precisely
// what the ancestry walk in obspane.go walks TOWARDS. The `focus` verb already
// takes a pane id. Nothing on the wire had to change, and nothing did.
//
// What was missing was a CLIENT, and this is it. The rule that produced both
// wrong claims is now written down in the implementation log: when asking what
// a wire surface carries, read the builder, not the verb.
//
// # Where the pane table comes from, and why it costs one round of discovery
//
// discoverSessions already dials every magmux-*.sock and reads exactly ONE
// line — the connect-time aggregate, which is buildPaneResults for every pane.
// So the pane ids and the pane pids arrive as a side effect of finding the
// sockets at all, and this file adds no probe of its own. The table is built
// ONCE, at construction, and is read-only afterwards: no mutex, no refresh,
// and no question about who may change it while an actor is walking it.
//
// A table that is one moment old is the right trade for a one-shot command. It
// is emphatically NOT the right trade for the streaming read path, which is
// why `magmux agents` still answers the pane questions "undetermined": re-dialling
// every magmux on the machine on every cycle is the shape requirements.md R3
// exists to forbid, and the honest "-" costs nobody anything.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/client"
	"github.com/MadAppGang/magmux/obsview"
)

// sockPaneDiscovery is how long the client will spend finding magmuxes.
//
// It is a bound on a fan-out of dials that each read one line, not a budget
// anybody is expected to use: on this machine discovery of three sockets
// completes in single-digit milliseconds. A magmux that is wedged and not
// reading its listener is what this protects against, and the consequence of
// the bound expiring is a pane nobody could resolve — reported as
// undetermined, which is the honest answer.
const sockPaneDiscovery = 2 * time.Second

// sockFocusTimeout bounds one `focus` round trip.
//
// navigate.PaneFocuser's doc warns that an implementation waiting on a socket
// would wedge the actor calling it. The rule underneath that warning is "must not
// block indefinitely", and this is how this implementation obeys it: every
// wait here has a deadline, and expiring is reported as a failure to focus
// rather than as a focus.
const sockFocusTimeout = 5 * time.Second

// sockPaneRef is one pane in one magmux.
//
// The socket is carried beside the id because a PANE ID IS NOT UNIQUE ACROSS
// MAGMUXES: pane 0 exists in every one of them. Everything below that resolves
// an id therefore resolves a (socket, id) pair, and the one place that cannot
// — navigate.PaneFocuser.Focus, which takes a bare id — is reached only through a
// table narrowed to ONE magmux (see forSession), where the id is unique.
type sockPaneRef struct {
	sock string
	id   int
}

// sockPaneLocator answers ports.PaneLocator and navigate.PaneFocuser against
// every magmux reachable on this machine.
//
// Built once and read-only afterwards, so it is safe for every session's actor
// to share — the same contract muxPaneLocator has, reached a different way.
type sockPaneLocator struct {
	// byPanePID maps a pane's OWN process pid to the pane AND THE MAGMUX it is
	// in. Pane pids are real pids and therefore globally unique, so this map
	// is unambiguous across magmuxes even though pane ids are not — and it
	// keeps the socket, because the walk that reaches a pane pid is the only
	// thing that knows which magmux the session is in.
	//
	// It was map[int]int until fix round 1. The walk found the right pane of
	// the right magmux and handed back a bare id, and Focus then refused that
	// id as ambiguous whenever a second magmux had a pane with the same number
	// — which, since every magmux numbers its panes from 0, was most sessions
	// on any machine running two.
	byPanePID map[int]sockPaneRef

	// byPaneID maps a pane id to every magmux that has one. More than one
	// entry is an ambiguity, and it is refused where it matters rather than
	// resolved by guessing.
	byPaneID map[int][]sockPaneRef

	// ppid is the parent lookup, indirected so a test can stage a process tree
	// without a live process anywhere. nil takes proc.PPIDOf.
	ppid func(int) (int, error)
	// hops overrides maxAncestryHops. Non-positive takes it.
	hops int

	// focus performs the socket round trip. nil takes sockFocusOverSocket, and
	// a test replaces it so that no test ever speaks to a real magmux.
	focus func(ctx context.Context, sock string, paneID int) error
}

// newSockPaneLocator discovers every reachable magmux and builds the table.
//
// It returns nil when nothing was reachable, and nil is a REAL answer that the
// pipeline knows what to do with: no locator and no focuser means the pane
// questions stay undetermined and a navigate is refused with
// act.ErrNotCapable — "cannot", not "will not". A machine with no magmux
// running is the ordinary case for `magmux agents focus`, not a failure.
func newSockPaneLocator(ctx context.Context) *sockPaneLocator {
	l := &sockPaneLocator{
		byPanePID: map[int]sockPaneRef{},
		byPaneID:  map[int][]sockPaneRef{},
	}
	for _, s := range client.DiscoverSessions(ctx, sockPaneDiscovery) {
		if !s.Reachable {
			// A stale socket file, or one belonging to another user.
			// discoverSessions surfaces those rather than hiding them; a pane
			// table is not the place to report them.
			continue
		}
		for _, entry := range s.Panes {
			id, okID := client.EventInt(entry, "pane")
			pid, okPID := client.EventInt(entry, "pid")
			if !okID || !okPID || pid <= 0 {
				// A pane with no live child has no pid — the control panel is
				// always one of those. It cannot be an ancestor of anything,
				// so it contributes nothing to the walk.
				continue
			}
			if client.EventBool(entry, "closed") {
				continue
			}
			ref := sockPaneRef{sock: s.SockPath, id: id}
			l.byPaneID[id] = append(l.byPaneID[id], ref)
			if sockPaneReaped(entry) {
				// The pane stays addressable by id, and its pid is not an
				// ancestor of anything any more — see sockPaneReaped.
				continue
			}
			l.byPanePID[pid] = ref
		}
	}
	if len(l.byPanePID) == 0 {
		return nil
	}
	return l
}

// sockPaneReaped reports whether a pane's child has been REAPED, read off the
// connect-time aggregate — and a reaped pane contributes no pid to the table.
//
// This is muxPaneLocator.panePIDs's rule, over the wire; see there for why the
// condition is `reaped` and not `dead`. The aggregate carries no `reaped` key,
// and none is needed: buildPaneResultsLocked reports a pane whose PTY closed
// before cmd.Wait returned as "running" (with dead:true), precisely so that it
// never invents an exit status. So an entry that carries a pid and reads
// "completed" or "failed" is one whose child has been waited on — whose pid the
// kernel is free to hand to someone else. A dead-but-unreaped pane reads
// "running" and keeps its claim, which is right: its pid is still held.
//
// The alternative, a new `reaped` key, is a change to the pane
// results builder and to the socket schema for a fact the schema already implies.
func sockPaneReaped(entry map[string]any) bool {
	state, _ := client.EventString(entry, "state")
	return state == "completed" || state == "failed"
}

// PaneFor walks up the process tree from a session's pid until it reaches a
// pane's own pid. See walkToPane, which both pane tables share.
func (l *sockPaneLocator) PaneFor(s model.Session) (model.PaneRef, model.Liveness) {
	id, live := l.paneForPID(s.PID)
	if live != model.LivenessYes {
		return "", live
	}
	return obsview.PaneRef(id), live
}

// paneForPID is PaneFor on a bare pid: the pane id the walk reaches.
func (l *sockPaneLocator) paneForPID(pid int) (int, model.Liveness) {
	if pid <= 0 {
		return 0, model.LivenessUnknown
	}
	if len(l.byPanePID) == 0 {
		return 0, model.LivenessNo
	}
	ref, live := walkToPane(pid, l.byPanePID, l.ppid, l.hops)
	return ref.id, live
}

// forSession narrows the table to the ONE magmux whose pane is an ancestor of
// pid, and reports whether there was one.
//
// This is the fix for the ambiguity Focus refuses. navigate.PaneFocuser.Focus takes
// a bare pane id, and a bare id means nothing across magmuxes; within one it
// is a permanent slot in an append-only table. So the command resolves the
// session first, walks ONCE from its pid, keeps the socket that walk reached,
// and hands the pipeline a focuser that knows only that magmux. Every id it
// can be asked about is then unique, and the focus goes to the magmux the
// session is actually running in — which the pid proved, and the id alone
// never could.
//
// The result is a NEW table, built from this one and sharing nothing mutable
// with it: both are read-only after construction, which is what lets every
// session's actor hold them without a lock.
//
// found is LivenessYes with a usable table; otherwise the table is nil and
// found says why — a determined "this session is in no magmux" or a walk that
// could not be completed. Neither is an error: the caller gives the pipeline
// no focuser, the navigate stage declares nothing, and the gate records the
// refusal as act.ErrNotCapable, which is the truth.
func (l *sockPaneLocator) forSession(pid int) (*sockPaneLocator, model.Liveness) {
	if pid <= 0 {
		return nil, model.LivenessUnknown
	}
	if len(l.byPanePID) == 0 {
		return nil, model.LivenessNo
	}
	hit, live := walkToPane(pid, l.byPanePID, l.ppid, l.hops)
	if live != model.LivenessYes {
		return nil, live
	}
	one := &sockPaneLocator{
		byPanePID: map[int]sockPaneRef{},
		byPaneID:  map[int][]sockPaneRef{},
		ppid:      l.ppid,
		hops:      l.hops,
		focus:     l.focus,
	}
	for panePID, ref := range l.byPanePID {
		if ref.sock != hit.sock {
			continue
		}
		one.byPanePID[panePID] = ref
		one.byPaneID[ref.id] = append(one.byPaneID[ref.id], ref)
	}
	return one, model.LivenessYes
}

// bindToSession is forSession as obsFocus consumes it: an obsPaneTable, and a
// NIL INTERFACE rather than a typed nil when there is nothing to bind to — see
// obsPaneTableOrNil for what a typed nil does to the pipeline.
func (l *sockPaneLocator) bindToSession(pid int) obsPaneTable {
	one, live := l.forSession(pid)
	if live != model.LivenessYes || one == nil {
		return nil
	}
	return one
}

// PaneAlive reports whether the pane still exists.
//
// The evidence is the `list` reply this table was built from, which is one
// moment old. That is stated rather than hidden: a pane closed since then
// answers "yes" here and the `focus` that follows is refused by magmux itself
// with no_such_pane, which is the same answer arriving from the only place
// that can be certain of it.
//
// It takes a bare id, like Focus, and across magmuxes answers "yes" when ANY
// of them has that pane. The pane gate asks it only about an id PaneFor just
// resolved, and PaneFor resolves only panes present in this table, so the
// answer it is asked for is always the one the walk already established.
func (l *sockPaneLocator) PaneAlive(ref model.PaneRef) model.Liveness {
	paneID, ok := obsview.PaneID(ref)
	if !ok {
		// Not an address. "There is no such pane" would claim we looked.
		return model.LivenessUnknown
	}
	if len(l.byPaneID[paneID]) == 0 {
		return model.LivenessNo
	}
	return model.LivenessYes
}

// Focus moves keyboard focus, over the socket.
//
// # previous is always -1, and that is honest rather than lazy
//
// The `focus` verb replies `{"pane": N, "focused": true}` and does not say
// which pane held focus before. navigate.PaneFocuser documents -1 as "nothing held
// focus", which is not quite what this is — it is "we cannot see what held
// focus" — and the two are close enough that inventing a value would be worse
// than the small imprecision. What matters for an undo is in the audit trail
// either way: the pane that was focused TO is recorded, and the human's own
// screen is the record of where they were.
//
// # An ambiguous pane id is REFUSED, and obsFocus never asks one
//
// A pane id is unique within one magmux and not across several. If this table
// spans two magmuxes that both have pane 3, this refuses and names both
// sockets, because focusing the wrong one is exactly the class of silent error
// this layer exists to avoid.
//
// This comment used to say the refusal was "reachable mainly by a caller that
// invented an id". It was reachable by the ORDINARY case: every magmux numbers
// its panes from 0, so with two running, most ids exist in both, and the
// unbound table refused a pane the ancestry walk had found unambiguously
// through a globally unique pid. That was the review's HIGH finding in fix
// round 1. obsFocus now binds the table to the session's own magmux before
// the pipeline sees a focuser (forSession), and in a table narrowed to one
// magmux this branch is unreachable. It stays as the answer an unbound table
// gives, rather than a pick.
func (l *sockPaneLocator) Focus(paneID int) (int, error) {
	if paneID < 0 {
		return -1, fmt.Errorf("%d is not a pane id", paneID)
	}
	refs := l.byPaneID[paneID]
	switch len(refs) {
	case 0:
		return -1, fmt.Errorf("no magmux on this machine has a pane %d", paneID)
	case 1:
	default:
		var socks []string
		for _, r := range refs {
			socks = append(socks, r.sock)
		}
		sort.Strings(socks)
		return -1, fmt.Errorf("pane %d exists in %d magmuxes (%s), so focusing it is ambiguous; "+
			"attach to one of them and focus there", paneID, len(refs), strings.Join(socks, ", "))
	}

	do := l.focus
	if do == nil {
		do = sockFocusOverSocket
	}
	// The bound navigate.PaneFocuser's contract asks for. It is here and not in the
	// caller because the caller is an actor goroutine with no deadline of its
	// own, and "must not block indefinitely" is this implementation's promise
	// to keep.
	ctx, cancel := context.WithTimeout(context.Background(), sockFocusTimeout)
	defer cancel()
	if err := do(ctx, refs[0].sock, paneID); err != nil {
		return -1, err
	}
	return -1, nil
}

// sockFocusOverSocket dials one magmux and asks it to focus a pane.
//
// It uses the SAME client `magmux mcp` uses — dialSession plus a request with
// an id — rather than a second hand-rolled one. That client already handles
// the connect-time aggregate, the reply routing and the legacy-magmux verdict,
// and a second implementation of the same three things is how two clients
// disagree about what a reply means.
func sockFocusOverSocket(ctx context.Context, sock string, paneID int) error {
	sess, err := client.Dial(ctx, sockIDFromPath(sock), sock, 0)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", sock, err)
	}
	defer sess.Close()

	if _, err := sess.Request(ctx, map[string]any{"type": "focus", "pane": paneID},
		sockFocusTimeout); err != nil {
		// client.ErrLegacyMagmux and a code-carrying refusal both arrive here. Both
		// are reported as they came: "this magmux is too old to answer" and
		// "that pane is not on screen" are different situations, and the gate
		// records whichever one it was.
		return fmt.Errorf("magmux %s refused to focus pane %d: %w", sock, paneID, err)
	}
	return nil
}

// sockIDFromPath is the name inside magmux-<id>.sock, for diagnostics only.
//
// It reads the same shape client's discovery matches (`^magmux-(.+)\.sock$`)
// without that package's pattern, which is unexported there: a diagnostic name
// is not worth a public API, and a path that is not that shape is returned as
// its base name, as before.
func sockIDFromPath(sock string) string {
	base := sock
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	const prefix, suffix = "magmux-", ".sock"
	if len(base) > len(prefix)+len(suffix) && strings.HasPrefix(base, prefix) && strings.HasSuffix(base, suffix) {
		return base[len(prefix) : len(base)-len(suffix)]
	}
	return base
}
