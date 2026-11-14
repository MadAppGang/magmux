// Package doctor is FR11's contract checks: it reads one live example of each
// Claude format through the fs.FS values it is handed and reports, per
// assumption, whether that assumption still holds.
//
// It reads, and only reads: every read goes through an fs.FS in Env, and it
// calls no os function (§2.3), so a doctor run cannot touch anything it was not
// handed. Bytes are decoded by claude's decoders, and raw key names come from
// claude's constants, never from literals (G1). A doctor run is a diagnostic:
// it runs no cycle and arms nothing, as tt's doctor does.
//
// It is a port of tt's doctorfs minus terminal-state-gate and observe-server,
// which stay in tt.
package doctor

import (
	"io/fs"
	"time"

	"github.com/MadAppGang/magmux/claudestate/ports"
)

// Verdict is one check's answer. A Skip is never reported as a pass: "nobody
// could look" and "it is fine" are different claims.
type Verdict uint8

const (
	VerdictUnknown Verdict = iota
	VerdictOK
	VerdictWarn
	VerdictFail
	VerdictSkip
)

var verdictNames = []string{"unknown", "ok", "warn", "fail", "skip"}

func (v Verdict) String() string {
	if int(v) < len(verdictNames) {
		return verdictNames[v]
	}
	return "Verdict(?)"
}

// Env is what a run reads.
type Env struct {
	Root ports.Root
	// Settings is the Claude root, read only for settings.json; nil when the
	// trees were given separately.
	Settings fs.FS
	// Agents is one listing the host took, or nil: the agents check then
	// skips, which is a different claim from ok.
	Agents *ports.Listing
	// Now is the run's single instant: two clock reads in one report can
	// disagree, and a diagnostic that contradicts itself is worse than none.
	Now time.Time
	// Retention > 0 overrides settings.json's cleanupPeriodDays.
	Retention time.Duration
}

// Result is one check's report.
type Result struct {
	Check    string
	Verdict  Verdict
	Detail   string
	Evidence []string
}

// The check ids, in FR11's order.
const (
	CheckRegistryKeys             = "registry-keys"
	CheckRegistryStatusValues     = "registry-status-values"
	CheckRegistryWaitingFor       = "registry-waiting-for"
	CheckRegistryTmuxShape        = "registry-tmux-shape"
	CheckRegistryMtimeTracks      = "registry-mtime-tracks-status"
	CheckRegistryVersionSpread    = "registry-version-spread"
	CheckJobsKeys                 = "jobs-keys"
	CheckJobsStateValues          = "jobs-state-values"
	CheckJobsDirMtimeTracks       = "jobs-dir-mtime-tracks-record"
	CheckJobsUpdatedAtNotEarlier  = "jobs-updatedat-not-earlier-than-timeline"
	CheckJobsTimelineShape        = "jobs-timeline-shape"
	CheckJobsLinkScanPath         = "jobs-linkscanpath"
	CheckJobsSignalKeys           = "jobs-signal-keys"
	CheckTranscriptRetentionFloor = "transcript-retention-floor"
	CheckTranscriptDeepPopulation = "transcript-deep-population"
	CheckTranscriptUntimestamped  = "transcript-untimestamped-types"
	CheckTranscriptAgentColor     = "transcript-agent-color-shape"
	CheckTranscriptAPIErrorKey    = "transcript-api-error-key"
	CheckSubagentMetaFiles        = "subagent-meta-files"
	CheckAgentsCLIShape           = "agents-cli-shape"
)

// Checks returns the 20 FR11 ids, in FR11's order.
func Checks() []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.id
	}
	return out
}

// Run returns exactly one Result per Checks() entry, in that order. It never
// returns an error: a check that cannot run says so with a verdict of its own,
// because an error would collapse twenty independent answers into one.
func Run(env Env) []Result {
	d := &run{env: env}
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		r := c.fn(d)
		r.Check = c.id
		out = append(out, r)
	}
	return out
}

type check struct {
	id string
	fn func(*run) Result
}

var checks = []check{
	{CheckRegistryKeys, (*run).registryKeys},
	{CheckRegistryStatusValues, (*run).registryStatusValues},
	{CheckRegistryWaitingFor, (*run).registryWaitingFor},
	{CheckRegistryTmuxShape, (*run).registryTmuxShape},
	{CheckRegistryMtimeTracks, (*run).registryMtimeTracksStatus},
	{CheckRegistryVersionSpread, (*run).registryVersionSpread},
	{CheckJobsKeys, (*run).jobsKeys},
	{CheckJobsStateValues, (*run).jobsStateValues},
	{CheckJobsDirMtimeTracks, (*run).jobsDirMtimeTracksRecord},
	{CheckJobsUpdatedAtNotEarlier, (*run).jobsUpdatedAtNotEarlier},
	{CheckJobsTimelineShape, (*run).jobsTimelineShape},
	{CheckJobsLinkScanPath, (*run).jobsLinkScanPath},
	{CheckJobsSignalKeys, (*run).jobsSignalKeys},
	{CheckTranscriptRetentionFloor, (*run).transcriptRetentionFloor},
	{CheckTranscriptDeepPopulation, (*run).transcriptDeepPopulation},
	{CheckTranscriptUntimestamped, (*run).transcriptUntimestampedTypes},
	{CheckTranscriptAgentColor, (*run).transcriptAgentColorShape},
	{CheckTranscriptAPIErrorKey, (*run).transcriptAPIErrorKey},
	{CheckSubagentMetaFiles, (*run).subagentMetaFiles},
	{CheckAgentsCLIShape, (*run).agentsCLIShape},
}

func result(v Verdict, detail string, evidence ...string) Result {
	return Result{Verdict: v, Detail: detail, Evidence: evidence}
}

func skip(why string, evidence ...string) Result { return result(VerdictSkip, why, evidence...) }
