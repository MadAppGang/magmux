package claude

// KeySet is the keys of one format, split by how often a record carries them.
type KeySet struct{ Universal, Optional []string }

// Vocab is the decoders' own table, partitioned by source and role, so
// claude/doctor and lint derive every set they need from it and none can drift
// from what is actually decoded (FR11, H1). Every entry is a constant from
// schema.go.
type Vocab struct {
	// Registry.Universal is the 19 keys every measured live record carried.
	Registry KeySet
	// Jobs.Universal is the MEASURED 19-key union over every state.json
	// (tt:doctorfs.go:65-70), not the set DecodeJob reads: it includes
	// JobsNeverRead, so jobs-keys does not warn on every real record.
	Jobs                                KeySet
	AgentsInteractive, AgentsBackground KeySet
	// SubagentMeta stays although nothing opens a meta file: lint needs the
	// names.
	SubagentMeta KeySet
	// Timeline is timeline.jsonl's keys (doctor only).
	Timeline KeySet
	// JobsNeverRead is the state.json keys deliberately never decoded.
	JobsNeverRead  []string
	TranscriptKeys []string

	RegistryStatus, WaitingFor, JobState, Tempo, Kind []string
	UntimestampedTypes                                []string
	// Homographs are decode words that are also wire words of a host
	// (magmux's status words and turn roles): banned only in decode position.
	Homographs []string
}

// Vocabulary returns a fresh copy of the table.
func Vocabulary() Vocab {
	return Vocab{
		Registry: KeySet{
			Universal: []string{
				KeyCWD, KeyEntrypoint, KeyKind, KeyMessagingSocketPath, KeyName, KeyNameSince,
				KeyNameSource, KeyPeerFeatures, KeyPeerProtocol, KeyPID, KeyPIDDomain,
				KeyProcStart, KeySessionID, KeyStartedAt, KeyStatus, KeyStatusUpdatedAt,
				KeyTmux, KeyUpdatedAt, KeyVersion,
			},
			Optional: []string{KeyFormerNames, KeyWaitingFor, KeyJobID},
		},
		Jobs: KeySet{
			Universal: []string{
				KeyBackend, KeyChildren, KeyCreatedAt, KeyCWD, KeyDaemonShort, KeyDetail,
				KeyFirstTerminalAt, KeyIntent, KeyLinkScanOffset, KeyLinkScanPath, KeyOutput,
				KeyProviderEnv, KeyRespawnFlags, KeyResumeSessionID, KeySessionID, KeyState,
				KeyTemplate, KeyTempo, KeyUpdatedAt,
			},
			Optional: []string{
				KeyName, KeyNameSource, KeyCLIVersion, KeyTokens, KeyNeeds, KeyReapedMidWorkAt,
				KeyInFlight, KeyBGIsolation, KeyLastTerminalAt, KeySuggestedReply,
			},
		},
		AgentsInteractive: KeySet{
			Universal: []string{KeyCWD, KeyKind, KeyName, KeyPID, KeySessionID, KeyStartedAt, KeyStatus},
			Optional:  []string{KeyWaitingFor},
		},
		AgentsBackground: KeySet{
			Universal: []string{KeyCWD, KeyAgentID, KeyKind, KeyName, KeySessionID, KeyStartedAt, KeyState},
		},
		SubagentMeta: KeySet{
			Universal: []string{KeySpawnDepth},
			Optional:  []string{KeyTaskKind, KeySubagentColor},
		},
		Timeline: KeySet{
			Universal: []string{KeyTimelineAt, KeyTimelineDetail, KeyTimelineState, KeyTimelineText},
		},
		JobsNeverRead: []string{KeyIntent, KeyOutput, KeySuggestedReply, KeyRespawnFlags, KeyProviderEnv},
		TranscriptKeys: []string{
			KeyRecordType, KeyMessage, KeyContent, KeyTimestamp, KeySubtype, KeyModel, KeyUsage,
			KeyIsSidechain, KeyAgentColor, KeyIsAPIErrorMessage, KeyCWD, KeyOperation, KeyReason,
			KeyAttachment, KeyAttachmentPrompt, KeyInputTokens, KeyCacheCreationTokens,
			KeyCacheReadTokens, KeyCompactMetadata, KeyPostTokens, KeyBlockText, KeyBlockID,
			KeyToolName, KeyToolInput, KeyToolUseID,
		},
		RegistryStatus: []string{ValueStatusBusy, ValueStatusIdle, ValueStatusShell, ValueStatusWaiting},
		WaitingFor:     []string{ValueWaitingInput, ValueWaitingPermission, ValueWaitingDialog},
		JobState:       []string{ValueStateBlocked, ValueStateDone, ValueStateFailed, ValueStateWorking},
		Tempo: []string{
			ValueTempoActive, ValueTempoBlocked, ValueTempoDone, ValueTempoFailed, ValueTempoIdle, ValueTempoWorking,
		},
		Kind: []string{ValueKindBG, ValueKindInteractive, ValueKindBackground},
		UntimestampedTypes: []string{
			ValueRecordAgentName, ValueRecordAITitle, ValueRecordATISLatch, ValueRecordCostState,
			ValueRecordFileHistorySnapshot, ValueRecordLastPrompt, ValueRecordMode, ValueRecordPermissionMode,
		},
		Homographs: []string{
			ValueStatusIdle, ValueStatusBusy, ValueStatusShell, ValueStatusWaiting,
			ValueRecordUser, ValueRecordAssistant,
		},
	}
}
