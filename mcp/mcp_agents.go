package mcp

// list_agents: the MCP surface of the session-observation document.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/client"
	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/obsview"
	"github.com/MadAppGang/magmux/protocol"
)

// toolListAgents is requirements.md R4.1's MCP surface, and it is a
// TRANSLATION of the socket verb rather than a second implementation.
//
// # The ordering constraint, which is not negotiable
//
// `magmux mcp` is a SEPARATE PROCESS that reaches magmux only over the unix
// socket. So there is no route from here into the pane table, the observation
// pipeline or the document — the verb has to exist first, and this asks for
// it. A tool written as though it could reach inside would compile, pass a
// unit test, and fail the first time it ran.
//
// # It renders with the CLI's own renderer
//
// R4.1 says the three surfaces return the identical document and R4.3 says the
// panel's content must match it exactly. obsview.WriteFleetTable is what
// `magmux agents` prints, and calling it here makes the agreement a property
// of the code rather than of two tables kept in step by hand — "-" for
// undetermined, "?" for an undetermined attention verdict, the why-block under
// the table, all of it, once. TestTheThreeSurfacesAgree walks one document
// through all three.
func toolListAgents(ctx context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}
	sess, err := s.resolveSession(ctx, args.SessionID)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	fleet, err := observeFleet(ctx, sess)
	if err != nil {
		// The two refusals magmux can give have different remedies and are
		// reported as such. A model told "not available" for a switch the
		// HUMAN has to flip will otherwise keep trying.
		switch protocol.CodeOf(err) {
		case protocol.CodeObsOff:
			return toolResultError("Session observation is not running in magmux %s. It is "+
				"opt-in: ask the human to restart magmux with MAGMUX_AGENTS=1. Until then, "+
				"list_panes shows that magmux's own panes.", sess.ID), nil
		case protocol.CodeObsPending:
			return toolResultError("magmux %s has session observation running but has not "+
				"published a document yet. Try again in a moment.", sess.ID), nil
		case protocol.CodeUnknownVerb, protocol.CodeUnsupported:
			return toolResultError("magmux %s is too old to report Claude Code sessions. Ask "+
				"the human to restart magmux with the current binary.", sess.ID), nil
		}
		return toolResultError("could not read the session document from magmux %s: %v",
			sess.ID, err), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Claude Code sessions on this machine, as of %s\n",
		fleet.Epoch.Format(time.RFC3339))
	fmt.Fprintf(&b, "(read through magmux %s)\n\n", sess.ID)
	// The same renderer `magmux agents` prints, into a buffer. A second table
	// written for a model would be a second vocabulary for one document.
	obsview.WriteFleetTable(&b, fleet)
	b.WriteString("\nATTENTION has three values and \"?\" is not a no: it means the evidence " +
		"supported neither verdict. A \"-\" anywhere else means magmux did not determine that " +
		"value, which is also not a no — a \"-\" in SUB or ERR means no transcript was found " +
		"to count from, where a \"0\" means it looked.")
	return toolText(b.String()), nil
}

// observeFleet asks one magmux for the session-observation document over the
// socket `agents` verb.
//
// # It decodes into obs.Fleet, and that is not an exception to client's
// no-shared-types rule
//
// The client package keeps wire shapes of its own, because the MCP server is a
// separate process from the multiplexer and a type shared with mux's socket
// layer would couple two programs that only speak JSON. obs.Fleet is not one
// of those types: it is the DEFINITION of the document, and both ends of this
// hop are rendering it rather than passing magmux's internals through. A
// second, hand-written decoder here would be a second copy of that key table —
// and R4.1's whole requirement is that the three surfaces return the identical
// document, which is a property one definition has and two spellings do not.
//
// It lives in mcp rather than on client.Session because client is a PUBLIC
// package and the document's types are internal to this module.
func observeFleet(ctx context.Context, sess *client.Session) (obs.Fleet, error) {
	res, err := sess.Request(ctx, map[string]any{"type": "agents"}, client.ReadTimeout)
	if err != nil {
		return obs.Fleet{}, err
	}
	// Back through JSON rather than by walking the map: the reply is the
	// marshalled document (see mux's fleetPayload), so re-marshalling the map
	// and decoding it into the type that defines it is the shortest path that
	// cannot spell a key differently.
	data, err := json.Marshal(res)
	if err != nil {
		return obs.Fleet{}, fmt.Errorf("re-encoding the session document: %w", err)
	}
	var fleet obs.Fleet
	if err := json.Unmarshal(data, &fleet); err != nil {
		return obs.Fleet{}, fmt.Errorf("reading the session document: %w", err)
	}
	return fleet, nil
}
