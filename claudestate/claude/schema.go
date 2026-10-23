package claude

// THIS FILE IS THE ONLY FILE IN THE MODULE THAT SPELLS A STRING CLAUDE CODE
// OWNS (guard G1, TestOnlySchemaFileDecodesClaudeStrings).
//
// The formats here are undocumented, they are not ours, and they change
// without notice. When one changes, exactly one file should move, and the way
// to keep that true is to make the second copy impossible to add by accident.
// Every other file names these constants; claude/doctor, lint and the
// differential use the exported ones and never type a literal.
//
// The rule is about DECODING rather than spelling. model's enum words ("idle",
// "busy", "waiting") are the library's own vocabulary and are HOMOGRAPHS of
// four of Claude's status values: same spelling, different owners. If Claude
// renames one, MapRegistryStatus absorbs it and model's word does not move.

// ---------------------------------------------------------------------------
// The layout under one Claude root.
// ---------------------------------------------------------------------------

const (
	// RecordSuffix is a registry record's extension: sessions/<pid>.json.
	// The directory also holds <pid>.<sha256>.key siblings, which are named
	// like key material and are NEVER opened (FR1).
	RecordSuffix = ".json"
	// KeyFileSuffix is the sibling's extension. Counted by the doctor, never
	// read by anything.
	KeyFileSuffix = ".key"
	// JobStateFile is the one file a job directory holds that is a record:
	// jobs/<dir>/state.json, at depth 1 only. Deeper files of the same name
	// are decoys (measured: depths 5 and 8 under one job's tmp/).
	JobStateFile = "state.json"
	// JobTimelineFile is jobs/<dir>/timeline.jsonl. Nothing at runtime reads
	// it (O10); claude/doctor's two timeline checks do.
	JobTimelineFile = "timeline.jsonl"
	// TranscriptSuffix is a transcript's extension. Its stem is the SESSION
	// ID, which is what makes a wrong directory a miss rather than a wrong
	// answer.
	TranscriptSuffix = ".jsonl"
	// SubagentsDirName is <projects>/<slug>/<sessionId>/subagents/: under the
	// session id, not beside it. One level too high finds nothing and reports
	// a confident zero.
	SubagentsDirName = "subagents"
	// SubagentMetaSuffix is what makes an entry of subagents/ an agent. The
	// directory also holds agent-*.jsonl siblings and other files.
	SubagentMetaSuffix = ".meta.json"
	// SettingsFile is the Claude root's settings, read by claude/doctor for
	// the retention window only.
	SettingsFile = "settings.json"
)

// ---------------------------------------------------------------------------
// The registry record: sessions/<pid>.json. Read BY NAME, never by position:
// key order is unstable between records (measured).
// ---------------------------------------------------------------------------

const (
	KeySessionID = "sessionId"
	KeyPID       = "pid"
	KeyStatus    = "status"
	// KeyWaitingFor is optional: absent on a session waiting for nothing
	// nameable, and absence is NOT one of the values.
	KeyWaitingFor = "waitingFor"
	// KeyStatusUpdatedAt is when the session ENTERED its status, in epoch
	// MILLISECONDS (re-measured 2026-09-18: 13 digits on every live record).
	KeyStatusUpdatedAt = "statusUpdatedAt"
	// KeyUpdatedAt is when the record was last written, epoch milliseconds.
	// It moves on writes that are not status changes (D7).
	KeyUpdatedAt = "updatedAt"
	// KeyStartedAt is the process start, epoch milliseconds.
	KeyStartedAt  = "startedAt"
	KeyName       = "name"
	KeyNameSource = "nameSource"
	// KeyFormerNames is a list whose elements are bare strings or objects
	// {name, until, sessionId} (Claude Code 2.1.276 writes objects).
	KeyFormerNames         = "formerNames"
	KeyFormerNameName      = "name"
	KeyFormerNameUntil     = "until"
	KeyFormerNameSessionID = "sessionId"
	// KeyCWD is the session's LIVE working directory, never its launch
	// directory, and so never an input to EncodeProjectDir.
	KeyCWD = "cwd"
	// KeyTmux is "<session>:@<window>.%<pane>". The sigils are part of the
	// ids: strip them and a pane lookup silently matches nothing.
	KeyTmux                = "tmux"
	KeyVersion             = "version"
	KeyMessagingSocketPath = "messagingSocketPath"
	// KeyKind is ValueKindBG on a background session's own process, and
	// KeyJobID names the job it serves.
	KeyKind         = "kind"
	KeyJobID        = "jobId"
	KeyPeerProtocol = "peerProtocol"
	KeyPeerFeatures = "peerFeatures"

	// Measured on every live record and read by nothing. Named so the
	// doctor's key check is derived from one table (FR11).
	KeyProcStart  = "procStart"
	KeyEntrypoint = "entrypoint"
	KeyPIDDomain  = "pidDomain"
	KeyNameSince  = "nameSince"
)

// The registry status values.
const (
	ValueStatusIdle    = "idle"
	ValueStatusBusy    = "busy"
	ValueStatusShell   = "shell"
	ValueStatusWaiting = "waiting"
)

// The waitingFor values. "input needed" is the only one observed live (55 of
// 1920 observations); "permission prompt" is named in the vendor's strings and
// never seen; "dialog open" is FR4's third.
const (
	ValueWaitingInput      = "input needed"
	ValueWaitingPermission = "permission prompt"
	ValueWaitingDialog     = "dialog open"
	// permissionWord is what the pane controller's input gate has always
	// tested for: a CONTAINS test, so a reworded permission prompt is still a
	// permission dialog (LooksLikePermission).
	permissionWord = "permission"
)

// The registry kind values. A record with no kind is interactive.
const (
	ValueKindBG          = "bg"
	ValueKindInteractive = "interactive"
	// ValueKindBackground is the agents listing's word for a background entry.
	ValueKindBackground = "background"
)

// ---------------------------------------------------------------------------
// The job record: jobs/<dir>/state.json. Published by REPLACE (measured), so
// there is no torn-in-place window on this tree; a parse failure is still
// reported, never guessed at.
// ---------------------------------------------------------------------------

const (
	KeyDaemonShort     = "daemonShort"
	KeyState           = "state"
	KeyDetail          = "detail"
	KeyNeeds           = "needs"
	KeyTempo           = "tempo"
	KeyTokens          = "tokens"
	KeyCLIVersion      = "cliVersion"
	KeyChildren        = "children"
	KeyCreatedAt       = "createdAt"
	KeyReapedMidWorkAt = "reapedMidWorkAt"
	KeyLinkScanPath    = "linkScanPath"
	KeyResumeSessionID = "resumeSessionId"
	// A child is a string, or an object describing an ARTIFACT the job
	// produced (a pull request), never a child session id.
	KeyChildHref = "href"
	KeyChildKind = "kind"
	KeyChildID   = "id"

	// Measured on every state.json (tt's 19-key union) and never decoded.
	KeyInFlight        = "inFlight"
	KeyLinkScanOffset  = "linkScanOffset"
	KeyTemplate        = "template"
	KeyBackend         = "backend"
	KeyFirstTerminalAt = "firstTerminalAt"
	KeyBGIsolation     = "bgIsolation"
	KeyLastTerminalAt  = "lastTerminalAt"

	// The keys DELIBERATELY never decoded (FR8; tt:jobsfs/record.go:53). The
	// first three carry the user's own prompt, the agent's messages and a
	// draft reply written for the user. Job has no field for any of them,
	// which is the real guarantee: a field that exists is a field that leaks.
	KeyIntent         = "intent"
	KeyOutput         = "output"
	KeySuggestedReply = "suggestedReply"
	KeyRespawnFlags   = "respawnFlags"
	KeyProviderEnv    = "providerEnv"
)

// The job state and tempo values.
const (
	ValueStateWorking = "working"
	ValueStateBlocked = "blocked"
	ValueStateDone    = "done"
	ValueStateFailed  = "failed"
	// ValueTempoBlocked is what the permission bridge writes MID-turn while
	// leaving state alone: working + tempo blocked is BLOCKED (measured 33 h
	// on one job). ValueTempoActive keeps a settled job from ending.
	ValueTempoBlocked = "blocked"
	ValueTempoActive  = "active"
	ValueTempoIdle    = "idle"
	ValueTempoWorking = "working"
	ValueTempoDone    = "done"
	ValueTempoFailed  = "failed"
)

// The timeline line's keys (doctor only).
const (
	KeyTimelineAt     = "at"
	KeyTimelineDetail = "detail"
	KeyTimelineState  = "state"
	KeyTimelineText   = "text"
)

// KeyCleanupPeriodDays is settings.json's transcript retention, in days.
const KeyCleanupPeriodDays = "cleanupPeriodDays"

// ---------------------------------------------------------------------------
// The agents listing: `claude agents --json`, a FLAT ARRAY (not an object).
// Interactive entries carry pid + status; background ones id + state.
// startedAt is epoch MILLISECONDS here, unlike the job record's RFC3339.
// ---------------------------------------------------------------------------

const (
	KeyAgentID = "id"
)

// ---------------------------------------------------------------------------
// The transcript: one JSON record per line.
//
// REAL KEYS, INFERRED ENVELOPE. agentColor, isApiErrorMessage, is_error and
// the record types are measured; the envelope (type, message, content blocks,
// timestamp) is the conventional shape. A shape that is not there yields no
// opinion, never a guess.
// ---------------------------------------------------------------------------

const (
	KeyRecordType = "type"
	KeyMessage    = "message"
	KeyContent    = "content"
	// KeyTimestamp is an RFC3339 instant: a string, unlike the registry's
	// epoch-millisecond numbers.
	KeyTimestamp = "timestamp"
	KeySubtype   = "subtype"
	KeyModel     = "model"
	KeyUsage     = "usage"
	// KeyIsSidechain marks a side conversation; its usage is not this
	// session's context.
	KeyIsSidechain = "isSidechain"

	// KeyAgentColor is the session's colour, read ONLY from a
	// RecordTypeAgentColor record (D2). A record of any other type carrying
	// it is not a colour record, and a bare "color" key belongs to a subagent.
	KeyAgentColor = "agentColor"
	// KeyIsAPIErrorMessage is the API-error signal. KeyIsError is named so it
	// is recognisable and avoidable: it fires on RECOVERED tool failures, and
	// counting it reports healthy sessions as broken.
	KeyIsAPIErrorMessage = "isApiErrorMessage"
	KeyIsError           = "is_error"
	// KeySessionKind is a transcript's own word for its kind ("bg").
	KeySessionKind = "sessionKind"
	// KeyTranscriptSavedAt is a bookkeeping instant on records written with
	// no conversational turn behind them, which is why transcript activity is
	// not a proxy for session activity.
	KeyTranscriptSavedAt = "savedAt"

	// The three counts that together are "what the model last saw" (D3).
	KeyInputTokens         = "input_tokens"
	KeyCacheCreationTokens = "cache_creation_input_tokens"
	KeyCacheReadTokens     = "cache_read_input_tokens"
	KeyCompactMetadata     = "compactMetadata"
	KeyPostTokens          = "postTokens"

	// Content blocks.
	KeyBlockText = "text"
	KeyBlockID   = "id"
	KeyToolName  = "name"
	KeyToolInput = "input"
	KeyToolUseID = "tool_use_id"

	// Queue and attachment records (the pane controller's).
	KeyOperation        = "operation"
	KeyReason           = "reason"
	KeyAttachment       = "attachment"
	KeyAttachmentPrompt = "prompt"
)

// Transcript record types and their values.
const (
	ValueRecordUser           = "user"
	ValueRecordAssistant      = "assistant"
	ValueRecordSystem         = "system"
	ValueRecordAttachment     = "attachment"
	ValueRecordQueueOperation = "queue-operation"
	ValueRecordAgentColor     = "agent-color"

	ValueSubtypeStopHookSummary = "stop_hook_summary"
	ValueSubtypeCompactBoundary = "compact_boundary"

	ValueBlockText       = "text"
	ValueBlockToolUse    = "tool_use"
	ValueBlockToolResult = "tool_result"

	ValueOperationEnqueue      = "enqueue"
	ValueOperationRemove       = "remove"
	ValueReasonAbsorbedMidTurn = "absorbed_mid_turn"
	ValueAttachmentQueued      = "queued_command"

	// ValueModelSynthetic is written on records the client made up (an
	// interrupt, an error placeholder). Their usage is all zero and is not a
	// context size.
	ValueModelSynthetic = "<synthetic>"
	// ValueToolAgent is the tool that spawns a subagent. It is not "Task".
	ValueToolAgent = "Agent"
)

// The transcript record types measured to carry no timestamp (tt §0.4(d)).
// The timing scan skips every record without the key, so a new one costs scan
// distance; the doctor names them.
const (
	ValueRecordAgentName           = "agent-name"
	ValueRecordAITitle             = "ai-title"
	ValueRecordATISLatch           = "atis-latch"
	ValueRecordCostState           = "cost-state"
	ValueRecordFileHistorySnapshot = "file-history-snapshot"
	ValueRecordLastPrompt          = "last-prompt"
	ValueRecordMode                = "mode"
	ValueRecordPermissionMode      = "permission-mode"
)

// The subagent meta file's keys. Nothing opens a meta file (D1, C7); the names
// are here for lint and the doctor.
const (
	KeyTaskKind         = "taskKind"
	KeySubagentColor    = "color"
	KeySpawnDepth       = "spawnDepth"
	ValueTaskKindInProc = "in_process_teammate"
)

// tmuxPattern is anchored and colon-safe: a tmux session name may itself
// contain colons ("a:b:c"), so the window and pane are taken from the END, and
// the sigils are required. A value without that shape keeps only its raw form.
const tmuxPattern = `^(.+):(@\d+)\.(%\d+)$`
