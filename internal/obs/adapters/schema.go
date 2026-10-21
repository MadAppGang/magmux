// Package adapters translates the on-disk formats of the tools magmux
// observes into the core vocabulary of internal/obs.
//
// It is the only package that knows both sides, and schema.go is the only file
// in the whole tree that DECODES Claude Code's strings. That rule is not
// tidiness: these formats are undocumented, they are not ours, and they change
// without notice. When one of them changes, exactly one file should have to
// move — and the way to keep that true is to make the second copy impossible
// to add by accident, which is what TestOnlySchemaFileDecodesClaudeStrings
// does.
//
// The rule is about decoding rather than spelling because obs deliberately
// mirrors four of Claude's status words in its own wire vocabulary. Those are
// homographs — same spelling, different owners — and if Claude renames one,
// DecodeStatus below absorbs it and our word does not move. That IS the rule
// working.
//
// Everything that leaves this package is expressed in obs's vocabulary. Nothing
// that enters it is trusted: the decoders answer (value, ok), and on !ok the
// caller keeps the raw string and reports Unknown. A guess would be worse than
// silence, because a plausible guess is indistinguishable from an observation.
package adapters

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// ---------------------------------------------------------------------------
// Claude Code's registry keys.
//
// These are the keys in ~/.claude/sessions/<pid>.json. Parse BY NAME, never by
// position: key order is unstable between records, and a positional read gives
// one record's session id out of another record's field.
// ---------------------------------------------------------------------------

const (
	// KeySessionID is the session's own identifier, and the leaf of its
	// transcript path.
	KeySessionID = "sessionId"
	// KeyStatus is the session's status. Decode with DecodeStatus.
	KeyStatus = "status"
	// KeyStatusUpdatedAt is when the session ENTERED its current status, in
	// epoch MILLISECONDS. It is not "when this record was last refreshed", and
	// the difference matters: a three-day-old value on a live session means the
	// session has been idle for three days, which is correct and useful.
	//
	// # The unit was wrong here until 2026-09-18, and it was wrong in one direction
	//
	// This comment used to say epoch SECONDS and contrasted itself with
	// KeyTranscriptSavedAt below. That contrast was false: all three of these
	// timestamps are milliseconds. Re-measured against the live registry on
	// 2026-09-18 — every value of this key and of KeyUpdatedAt in all 34 live
	// records is 13 digits, and no 10-digit value exists anywhere in the
	// directory. The proof that the corpus was converted rather than Claude's
	// format changed is that ~/.claude/sessions/98198.json had not been
	// rewritten since the corpus was captured: same session, same file, same
	// Claude version, live value 1789536692864 against the corpus's
	// 1789536692, and the live value is 36ms before the file's own mtime,
	// which is what a process that computes a timestamp and then writes it
	// produces.
	//
	// Getting this wrong is not a small error and it does not fail loudly: read
	// as seconds, a real value lands in the year 58600; divided again by a
	// thousand it lands in 1970. Decode it with DecodeInstant, which refuses a
	// seconds-shaped value rather than converting it.
	KeyStatusUpdatedAt = "statusUpdatedAt"
	// KeyUpdatedAt is when the record was last written. Epoch MILLISECONDS,
	// same as KeyStatusUpdatedAt and measured the same way; decode it with
	// DecodeInstant.
	KeyUpdatedAt = "updatedAt"
	// KeyWaitingFor is what a waiting session is blocked on. Optional: it is
	// absent on a session that is waiting for nothing nameable, and absence is
	// NOT one of the two values. Decode with DecodeWaitingFor.
	KeyWaitingFor = "waitingFor"
	// KeyPID is the session's process id, and the subject of the first
	// liveness gate.
	KeyPID = "pid"
	// KeyName is the session's display name.
	KeyName = "name"
	// KeyNameSource is where the name came from — chosen by a human, generated
	// by Claude Code, or derived from something else.
	KeyNameSource = "nameSource"
	// KeyFormerNames is the session's previous names, in order. Optional.
	// Preserve the order; do not flatten to the latest.
	//
	// Its ELEMENTS come in two shapes; decode each with DecodeFormerName. A
	// bare string, which is what the 2026-09-16 test data holds (45682.json —
	// labelled REAL SHAPE, and unconfirmed since: no string-shaped record was
	// left to check it against). And an object keyed by KeyFormerNameName,
	// KeyFormerNameUntil and KeyFormerNameSessionID, measured live on
	// 2026-09-19 on Claude Code 2.1.276 (76136.json) — the only shape any live
	// record carried that day, and the one Claude Code's own session listing
	// reads back ("says it was X until 5m ago"). Until fix round 2 this
	// package read strings only and dropped every object.
	KeyFormerNames = "formerNames"
	// KeyFormerNameName, KeyFormerNameUntil and KeyFormerNameSessionID are the
	// keys INSIDE one object-shaped former name. The name is the former
	// name; until is when it stopped being current, in epoch MILLISECONDS
	// (measured equal to the record's own nameSince); sessionId is the
	// session the name was recorded under. The first and third are the same
	// spellings as KeyName and KeySessionID at the top level, and are named
	// again here because they are keys of a different object and could move
	// independently.
	KeyFormerNameName      = "name"
	KeyFormerNameUntil     = "until"
	KeyFormerNameSessionID = "sessionId"
	// KeyCWD is the session's LIVE working directory. It is not the directory
	// the session was launched in, and it is therefore not a safe input to
	// EncodeProjectDir — see that function.
	KeyCWD = "cwd"
	// KeyTmux is the session's terminal coordinates, formatted
	// "<session>:<window>.<pane>". The sigils in the window and pane ids are
	// part of the ids: strip them and the lookup silently matches nothing.
	KeyTmux = "tmux"
	// KeyPeerProtocol and KeyPeerFeatures describe what the session's own peer
	// interface supports. magmux does not use that interface; the keys are
	// named here so a reader of this file sees the whole record.
	KeyPeerProtocol = "peerProtocol"
	KeyPeerFeatures = "peerFeatures"
)

// ---------------------------------------------------------------------------
// Claude Code's status values.
// ---------------------------------------------------------------------------

const (
	statusIdle    = "idle"
	statusBusy    = "busy"
	statusShell   = "shell"
	statusWaiting = "waiting"
)

// ---------------------------------------------------------------------------
// Claude Code's waitingFor values.
//
// Two values, and they must never be merged into a boolean. Only one of them
// has ever been observed live — 55 occurrences in 1920 observations, all of
// them "input needed" — so the other one's handling can only ever be proved
// from test data, and an implementation that collapses them looks correct on
// every live machine.
// ---------------------------------------------------------------------------

const (
	waitingForInput      = "input needed"
	waitingForPermission = "permission prompt"
)

// ---------------------------------------------------------------------------
// Claude Code's transcript and subagent layout.
// ---------------------------------------------------------------------------

const (
	// KeyAgentColor is the transcript record key carrying the session's
	// colour. It is NOT "color": a plain color key exists in the same tree
	// with a different meaning, and reading it instead is a bug that hides
	// itself whenever the two agree.
	KeyAgentColor = "agentColor"
	// KeyIsAPIErrorMessage marks a transcript record as an API-level error.
	// This is the error signal. The other one — a tool result's own error
	// flag, KeyIsError — marks errors the session RECOVERED from; every one of
	// 497 observed occurrences was recovered, and counting them reports 29 of
	// 32 healthy sessions as broken.
	KeyIsAPIErrorMessage = "isApiErrorMessage"
	// KeyIsError is named so that it is recognisable and avoidable. See
	// KeyIsAPIErrorMessage.
	KeyIsError = "is_error"
	// KeyTranscriptSavedAt is a bookkeeping timestamp inside the transcript,
	// in epoch MILLISECONDS. Records carrying it are written with no
	// conversational turn behind them, which is why transcript activity is not
	// a valid proxy for session activity.
	KeyTranscriptSavedAt = "savedAt"
	// ToolNameAgent is the tool that spawns a subagent. It is not called Task.
	ToolNameAgent = "Agent"

	// SubagentsDirName is the directory holding one metadata file per
	// subagent, at <projects>/<slug>/<sessionId>/subagents/. Note the depth:
	// it is under the session id, not directly under the project directory,
	// and looking one level too high yields a silent zero.
	SubagentsDirName = "subagents"
	// SubagentMetaSuffix is the suffix a file must have to be counted. The
	// directory holds other files, and counting them inflates the answer.
	SubagentMetaSuffix = ".meta.json"
	// KeyTaskKind is a subagent's kind, and KeySubagentColor its colour, which
	// is present only on an in-process teammate. KeySpawnDepth is how deeply
	// nested the subagent is; the nested ones leave no record in the
	// transcript at all, which is why subagents are counted from these files
	// and never from the transcript.
	KeyTaskKind       = "taskKind"
	KeySubagentColor  = "color"
	KeySpawnDepth     = "spawnDepth"
	TaskKindInProcess = "in_process_teammate"

	// TranscriptSuffix is the extension of a transcript file. The file's stem
	// is the SESSION ID, which is the one thing that makes a wrongly derived
	// project directory a miss rather than a wrong answer: a reader that opens
	// <sessionId>.jsonl cannot open another session's transcript by accident,
	// and a reader that takes "the newest .jsonl in this directory" can.
	TranscriptSuffix = ".jsonl"
)

// ---------------------------------------------------------------------------
// Claude Code's transcript record envelope.
//
// REAL KEYS, INFERRED SHAPE — and the distinction bounds what may be built on
// them. The source research prints the keys and their semantics; it does not
// print whole records except one. What is measured: agentColor (and that it is
// not color), isApiErrorMessage and its three classes, is_error and that all
// 497 observed occurrences were recovered, the artifact-autoreact-ledger
// record type, and the tool name Agent. What is CONVENTIONAL rather than
// measured: the envelope below — type, message, content blocks, timestamp.
// A reader that keys off the measured names is reading Claude Code; one that
// keys off the envelope is reading a plausible reconstruction of it, and must
// degrade to "no opinion" rather than to a guess when the shape is not there.
// ---------------------------------------------------------------------------

const (
	// KeyRecordType is a transcript record's own type, and also a content
	// block's. Both are read with it; they are the same key at two depths.
	KeyRecordType = "type"
	// KeyMessage is the record's message object, and KeyContent that message's
	// content — which is a STRING for a prompt and an ARRAY of blocks for
	// everything else. That difference is the load-bearing one: a `user`
	// record with array content is a tool RESULT and not a prompt, and
	// treating it as a turn boundary resets a turn mid-flight.
	KeyMessage = "message"
	KeyContent = "content"
	// KeyTimestamp is an ISO-8601 instant on a record. It is not one of the
	// epoch-millisecond keys above and does not go through DecodeInstant.
	KeyTimestamp = "timestamp"

	// RecordTypeUser and RecordTypeAssistant are Claude's two conversational
	// record types. They are spelled the same as obs.TurnUser and
	// obs.TurnAssistant, and that is a HOMOGRAPH rather than a copy: those are
	// our wire vocabulary and marshal into our documents, these are Claude's
	// and are read here. If Claude renames one, this constant moves and ours
	// does not.
	RecordTypeUser      = "user"
	RecordTypeAssistant = "assistant"

	// The content-block types, and their fields. BlockTypeToolUse carries
	// KeyBlockID, KeyToolName and KeyToolInput; BlockTypeToolResult carries
	// KeyToolUseID naming the call it answers.
	BlockTypeText       = "text"
	BlockTypeToolUse    = "tool_use"
	BlockTypeToolResult = "tool_result"
	KeyBlockText        = "text"
	KeyBlockID          = "id"
	KeyToolName         = "name"
	KeyToolInput        = "input"
	KeyToolUseID        = "tool_use_id"
)

// ---------------------------------------------------------------------------
// Decoders.
//
// Each answers (value, ok). ok is false for anything Claude has added since
// this file was written, and the contract on the caller is the same every
// time: keep the raw string, report the core's Unknown, and never substitute a
// plausible value. The point is that a format change shows up as a blank
// column and a logged raw value, rather than as a wrong answer nobody can
// distinguish from a right one.
// ---------------------------------------------------------------------------

// DecodeStatus maps Claude Code's status onto the core vocabulary.
//
// An unrecognised value — a future Claude's "compacting", say — returns
// (obs.StatusUnknown, false). Guessing that it means busy would be the exact
// failure this signature exists to prevent: the guess is plausible, it is
// wrong, and nothing downstream can tell.
func DecodeStatus(v string) (obs.Status, bool) {
	switch v {
	case statusIdle:
		return obs.StatusIdle, true
	case statusBusy:
		return obs.StatusBusy, true
	case statusShell:
		return obs.StatusShell, true
	case statusWaiting:
		return obs.StatusWaiting, true
	default:
		return obs.StatusUnknown, false
	}
}

// DecodeWaitingFor maps Claude Code's waitingFor onto the core vocabulary.
//
// The empty string is the key being ABSENT, and it returns
// (obs.WaitingReasonUnknown, false) — not obs.WaitingReasonNone and certainly
// not obs.WaitingReasonInput. Defaulting an absent key to "input needed" is
// right on every live observation made so far and wrong in exactly the case a
// user depends on it.
func DecodeWaitingFor(v string) (obs.WaitingReason, bool) {
	switch v {
	case waitingForInput:
		return obs.WaitingReasonInput, true
	case waitingForPermission:
		return obs.WaitingReasonPermission, true
	default:
		return obs.WaitingReasonUnknown, false
	}
}

// The bounds DecodeInstant refuses outside of. They are expressed as the two
// mistakes they catch rather than as dates, because that is what a reader of a
// failure needs to know.
//
// minPlausibleMillis is 2001-09-09, the instant epoch milliseconds reached
// thirteen digits. Every value below it is ten to twelve digits — which is what
// a SECONDS value looks like, and seconds is the unit this schema was
// documented with until it was re-measured. maxPlausibleMillis is the year
// 5138, above which a value is microseconds or nanoseconds.
//
// Nothing in between is checked. A registry record could carry a date in 2019
// and that would be a fact about the session, not a unit error.
const (
	minPlausibleMillis = int64(1_000_000_000_000)
	maxPlausibleMillis = int64(100_000_000_000_000)
)

// DecodeInstant maps one of Claude Code's epoch timestamps — KeyStatusUpdatedAt,
// KeyUpdatedAt, KeyTranscriptSavedAt — onto an instant. All three are epoch
// MILLISECONDS; see KeyStatusUpdatedAt for how that was established and for
// what the documentation said before.
//
// It returns (zero, false) for a value that is not plausibly milliseconds
// instead of converting it, and that refusal is the whole point of the
// function. A seconds value read as milliseconds is the year 1970; a
// milliseconds value read as seconds is the year 58600. Neither fails: both
// produce an instant, both marshal cleanly, and a reader who is not already
// suspicious sees a date and believes it. The one case that is actually
// UNSAFE — silently multiplying a ten-digit value by a thousand to "fix" it —
// would also make a corpus with the wrong unit test green forever, which is
// exactly how this was missed the first time.
//
// The result is UTC so that two machines building the same document produce
// the same bytes.
func DecodeInstant(v int64) (time.Time, bool) {
	if v < minPlausibleMillis || v > maxPlausibleMillis {
		return time.Time{}, false
	}
	return time.UnixMilli(v).UTC(), true
}

// DecodeFormerName maps ONE element of KeyFormerNames onto the core
// vocabulary, in either of the two shapes that key's comment describes.
//
// It answers per ELEMENT, never per field, and in two grades — because the
// lenient-adapter rule is about what a degradation COSTS:
//
//   - ok is false when the element names nothing: it is neither a string nor
//     an object with a string name. The caller skips it, and why says what it
//     was. The other elements are unaffected.
//   - ok is true with a non-empty why when the NAME survived but something
//     recorded beside it did not — an until that is not a plausible
//     epoch-millisecond instant, a session id that is not text. The name is
//     kept and only the bad part is dropped, and why says which. The name is
//     what requirements.md R2 is about; losing it over a timestamp would be
//     the all-or-nothing failure one level down.
//
// Every value quoted in why goes through %q, because the caller's notes reach
// a terminal on stderr and an element is text Claude Code may have generated
// from conversation content.
//
// v is a value out of encoding/json: a number arrives as json.Number when the
// decoder used UseNumber, as ParseRecord's does, and as float64 otherwise.
// Both are read.
func DecodeFormerName(v any) (p obs.PreviousName, ok bool, why []string) {
	switch e := v.(type) {
	case string:
		// The string shape carries a name and nothing else, so Until is
		// undetermined rather than zero.
		return obs.PreviousName{Name: e}, true, nil
	case map[string]any:
		rawName, present := e[KeyFormerNameName]
		name, isText := rawName.(string)
		switch {
		case !present:
			return obs.PreviousName{}, false, []string{fmt.Sprintf(
				"is an object with no %q, so it names nothing", KeyFormerNameName)}
		case !isText:
			return obs.PreviousName{}, false, []string{fmt.Sprintf(
				"is an object whose %q is %s, not text, so it names nothing", KeyFormerNameName,
				jsonKind(rawName))}
		}
		p = obs.PreviousName{Name: name}
		if raw, present := e[KeyFormerNameUntil]; present && raw != nil {
			ms, isInt := jsonInt64(raw)
			switch {
			case !isInt:
				why = append(why, fmt.Sprintf("keeps its name, but its %q is %s, not a whole "+
					"number of epoch milliseconds", KeyFormerNameUntil, jsonKind(raw)))
			default:
				if at, plausible := DecodeInstant(ms); plausible {
					p.Until = &at
				} else {
					why = append(why, fmt.Sprintf("keeps its name, but its %q is %d, which is "+
						"not a plausible epoch-millisecond value", KeyFormerNameUntil, ms))
				}
			}
		}
		if raw, present := e[KeyFormerNameSessionID]; present && raw != nil {
			if id, isText := raw.(string); isText {
				p.Session = id
			} else {
				why = append(why, fmt.Sprintf("keeps its name, but its %q is %s, not text",
					KeyFormerNameSessionID, jsonKind(raw)))
			}
		}
		return p, true, why
	default:
		return obs.PreviousName{}, false, []string{fmt.Sprintf(
			"is %s, which is neither a name nor an object carrying one", jsonKind(v))}
	}
}

// jsonKind names a decoded JSON value's kind in the words a person reading a
// problem would use. It never includes the value itself: a value is quoted,
// if at all, by the caller, through %q.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "text"
	case bool:
		return "a boolean"
	case json.Number, float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("a %T", v)
	}
}

// jsonInt64 reads an integral JSON number in either of encoding/json's
// representations.
func jsonInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		i := int64(n)
		return i, float64(i) == n
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// The NFR6 inventory.
// ---------------------------------------------------------------------------

// claudeOwnedLiterals is every string in this file that belongs to Claude
// Code. TestOnlySchemaFileDecodesClaudeStrings asserts that no file under
// internal/obs except this one DECODES one of them — compares against it,
// switches on it, or names it in a struct tag — and that the ones only Claude
// could have written do not appear at all.
//
// The test splits this list in two by asking obs what its own wire words are,
// so a literal that magmux also emits (the four status values) is policed only
// in decode position while the rest are banned outright. Nothing here has to
// record which is which.
//
// Deliberately NOT in this list: "status", "pid", "name", "cwd", "tmux",
// "color", "until". Those words are Claude's keys AND ordinary English that
// magmux's own vocabulary uses for the same concepts, so banning them would
// forbid the core from naming its own fields — obs.PreviousName spells its
// instant "until" on the wire, exactly as Claude does, for the same reason the
// four status words are homographs. KeyFormerNameSessionID is not listed
// either, and needs no entry: it is the same literal as KeySessionID, which is. The rule protects the strings that could
// only have come from Claude — the camelCase keys, the snake_case keys, the
// specific values and the path components — because those are the ones that
// signal a second decoder has grown somewhere it should not have.
//
// The transcript envelope is out for the same reason and one more: "type",
// "content", "timestamp", "text", "id", "input", "message" are ordinary
// English, and "user" and "assistant" are obs's OWN wire words — obs.TurnUser
// and obs.TurnAssistant marshal into magmux's documents. Banning either would
// forbid the core from spelling its own roles. Only the envelope strings that
// could have come from nowhere else are listed: the two block types, the
// correlation key and the transcript's own extension.
//
// The homograph split does not rescue those: it is derived from the VALUES
// obs marshals as STATUS, WAITING and LIVENESS, and a key like "status" or a
// role like "user" is not one of them. Keys that are also our own field names
// stay out of the inventory by hand.
var claudeOwnedLiterals = []string{
	KeySessionID,
	KeyStatusUpdatedAt,
	KeyUpdatedAt,
	KeyWaitingFor,
	KeyNameSource,
	KeyFormerNames,
	KeyPeerProtocol,
	KeyPeerFeatures,
	KeyAgentColor,
	KeyIsAPIErrorMessage,
	KeyIsError,
	KeyTranscriptSavedAt,
	KeyTaskKind,
	KeySpawnDepth,
	SubagentsDirName,
	SubagentMetaSuffix,
	TaskKindInProcess,
	ToolNameAgent,
	TranscriptSuffix,
	BlockTypeToolUse,
	BlockTypeToolResult,
	KeyToolUseID,
	statusIdle,
	statusBusy,
	statusShell,
	statusWaiting,
	waitingForInput,
	waitingForPermission,
}
