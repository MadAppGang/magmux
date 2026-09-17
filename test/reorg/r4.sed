# r4.sed: the R4 rename map. R4 lifts the socket's wire vocabulary out of
# mux/sockrpc.go into package protocol, and the socket client out of
# mcp/mcp_client.go into package client; this file is every identifier that
# changed name on the way, and doubles as the list of new exports (E5(c)).
#
# Dialect: the same as r3.sed. Statements are perl, applied with `perl -p`, on
# the PRE-R4 sources only (prove-r4.sh). Each rule maps an OLD spelling to the
# NEW one as seen from OUTSIDE the destination package, i.e. qualified
# (`protocol.CodeOf`, `client.Dial`); the proof then strips `protocol.` and
# `client.` before a capital from both sides, so a name reads the same inside
# its own package and out.
#
# Every rule is scoped to the file or directory it applies to. mux keeps its old
# names through aliases (`type sockErr = protocol.Error`, `sockCodeX =
# protocol.CodeX`), so only the DEFINITIONS that moved are renamed in
# mux/sockrpc.go, never a call site.

# ── protocol: the definitions lifted from mux/sockrpc.go ──────────────────────
# `var se *sockErr` occurs twice in sockrpc.go, and only verbErrCode's copy
# moved (replyTo's stays, and still reads sockErr through the alias), so the
# rule is confined to verbErrCode's body.
$in_vec = 1 if $ARGV =~ m{(^|/)mux/sockrpc\.go$} && /^func verbErrCode\(/;
s/\*sockErr\b/*protocol.Error/ if $in_vec;
$in_vec = 0 if /^}$/;
if ($ARGV =~ m{(^|/)mux/sockrpc\.go$}) {
	s/^\/\/ sockProtocol is /\/\/ protocol.Version is /;
	s/^const sockProtocol = 1$/const protocol.Version = 1/;
	# the code constants and the doc lines that name them
	s/^(\t(?:\/\/ )?)sockCode([A-Z]\w*)\b/$1protocol.Code$2/;
	s/^\/\/ sockErr is a verb failure/\/\/ protocol.Error is a verb failure/;
	s/^type sockErr struct/type protocol.Error struct/;
	s/^func \(e \*sockErr\) Error\(\)/func (e *protocol.Error) Error()/;
	s/^func sockErrf\((.*)\) \*sockErr \{$/func protocol.Errf($1) *protocol.Error {/;
	s/^\treturn &sockErr\{/\treturn &protocol.Error{/;
	s/^\/\/ verbErrCode extracts/\/\/ protocol.CodeOf extracts/;
	s/^func verbErrCode\(/func protocol.CodeOf(/;
	s/^\treturn sockCodeInternal$/\treturn protocol.CodeInternal/;
	# sockEvents advertises the event names; it now spells them as constants
	if (/^\tsockEvents = / || /^\t\t"results", "shutdown", "reply"\}$/) {
		s/"(snapshot|exit|control|results|shutdown|reply)"/protocol.Event\u$1/g;
		s/"pane_(opened|closed)"/protocol.EventPane\u$1/g;
	}
}

# ── client: the socket client lifted from mcp/mcp_client.go ───────────────────
# Applied to every file in mcp/, because the rest of mcp (and its tests) now
# reaches these names through package client.
if ($ARGV =~ m{(^|/)mcp/[^/]+\.go$}) {
	s/\bdialSession\b/client.Dial/g;
	s/\bnewSessionState\b/client.NewSessionState/g;
	s/\bsessionState\b/client.SessionState/g;
	s/\bpaneInfo\b/client.PaneInfo/g;
	s/\bturnResult\b/client.TurnResult/g;
	s/\btranscriptTurn\b/client.TranscriptTurn/g;
	s/\btranscriptTool\b/client.TranscriptTool/g;
	s/\baggregateState\b/client.AggregateState/g;
	s/\berrLegacyMagmux\b/client.ErrLegacyMagmux/g;
	s/\brunInstruction\b/client.RunInstruction/g;
	s/\bsockReadTimeout\b/client.ReadTimeout/g;
	s/\bsockSendTimeout\b/client.SendTimeout/g;
	s/\bdefaultStartTimeout\b/client.DefaultStartTimeout/g;
	s/\bdefaultTurnTimeout\b/client.DefaultTurnTimeout/g;
	# test-only knobs: mcp's attach tests shrink the probe budgets
	s/\bsockProbeTimeout\b/client.ProbeTimeout/g;
	s/\bsockProbeConfirmTimeout\b/client.ProbeConfirmTimeout/g;
	s/\blegacyRecheckAfter\b/client.LegacyRecheckAfter/g;
	# the Session type, in type position only: "Session" is also a word
	s/\*Session\b/*client.Session/g;
	s/&Session\{/&client.Session{/g;
	# the pane view mcp reads, through an accessor rather than a field
	s/\bsess\.state\b/sess.State()/g;
	# methods whose names are not ordinary words
	s/\b(isLegacy|listPanes|sendKeys|openPane|closePane|beginTurn|endTurn|capsNote|seedAggregate|applyPane|paneState)\b/\u$1/g;
	# methods whose names ARE words ("capture" is also a verb on the wire):
	# calls and definitions only
	s/\.(capture|transcript|fire|all)\(/.\u$1(/g;
	s/^(func \(\w+ \*[\w.]+\)) (capture|transcript|fire|all)\(/$1 \u$2(/;
	# the code of a failed request is read through protocol; sockErrCode's own
	# definition is deleted, not renamed
	s/(?<!func )\bsockErrCode\(/protocol.CodeOf(/g;
}
if ($ARGV =~ m{(^|/)mcp/mcp_client\.go$}) {
	# the doc comments of the word-named methods
	s/^\/\/ (capture|transcript|fire|all) /\/\/ \u$1 /;
	# ingest switches on protocol's event names
	s/^\tcase "(snapshot|exit|results|shutdown|reply)":$/\tcase protocol.Event\u$1:/;
	s/code == "unknown_verb" \|\| code == "unsupported"/code == protocol.CodeUnknownVerb || code == protocol.CodeUnsupported/;
}
