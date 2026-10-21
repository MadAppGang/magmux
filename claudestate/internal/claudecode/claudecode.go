// Package claudecode is the per-session transcript stage: it names the one
// file it will read before the arm wave (stage.Targeting), honours the read
// permit the cycle grants it, and reads that file ONCE per cycle, the last
// max(TailBytes, TimingTailBytes) bytes, decoding the facts over the last
// TailBytes and the timing over the last TimingTailBytes of the same buffer
// (C20, NFR13 per key).
//
// It keeps no location across cycles: the coordinator hands it the cycle's
// Location in Targets. A located transcript is read only under a Grant (Armed
// or Pull) or when the cycle is Unwatched; any other read is refused with
// ProblemNotGranted and the facts stay nil (C17). An ambiguous location opens
// nothing (D5, C6).
package claudecode

import (
	"path/filepath"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/internal/source"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
)

// Name is the stage's name in the chain.
const Name = "claude-code"

// Config is how the stage is built.
type Config struct {
	Projects                   ports.Tree
	TailBytes, TimingTailBytes int // 0 => claude.FactsTailBytes, claude.TimingTailBytes
}

// Factory returns one stage per actor.
func Factory(cfg Config) stage.Factory {
	if cfg.TailBytes <= 0 {
		cfg.TailBytes = claude.FactsTailBytes
	}
	if cfg.TimingTailBytes <= 0 {
		cfg.TimingTailBytes = claude.TimingTailBytes
	}
	return func(key model.Key) stage.Stage { return &Stage{cfg: cfg, key: key} }
}

// Stage is one session's transcript reader.
type Stage struct {
	cfg Config
	key model.Key

	// This cycle's location, target and permit. Replaced on every Targets
	// call; nothing about location survives a cycle here.
	loc      model.Location
	target   string // absolute path of the located transcript; "" when unwatchable
	prepared bool
	grant    stage.Grant

	problems []model.Problem
	reads    int // transcript reads, for the one-read-per-cycle test
}

func (st *Stage) Name() string { return Name }

// Targets names the located transcript as a FILE, never a directory and never
// subagents/ (O9, C11). It reads no content.
func (st *Stage) Targets(s model.Session, loc model.Location) []model.WatchTarget {
	st.loc, st.prepared, st.target, st.grant = loc, true, "", stage.Grant{}
	if loc.Matches != 1 || loc.Path == "" || st.cfg.Projects.Dir == "" {
		return nil
	}
	kind := model.TargetTranscript
	if s.Job != nil {
		kind = model.TargetJobTranscript
	}
	st.target = filepath.Join(st.cfg.Projects.Dir, filepath.FromSlash(loc.Path))
	return []model.WatchTarget{{Path: st.target, Kind: kind}}
}

// Granted records this cycle's permit.
func (st *Stage) Granted(g stage.Grant) { st.grant = g }

func (st *Stage) permitted() bool {
	if st.grant.Unwatched {
		return true
	}
	if st.target == "" {
		return false
	}
	for _, list := range [][]model.WatchTarget{st.grant.Armed, st.grant.Pull} {
		for _, t := range list {
			if t.Path == st.target {
				return true
			}
		}
	}
	return false
}

// Observe adds what the session's own transcript says.
func (st *Stage) Observe(s model.Session) model.Session {
	if !st.prepared {
		return s
	}
	st.prepared = false
	loc := st.loc
	switch {
	case loc.Matches >= 2:
		s.Transcript = &model.TranscriptFacts{Location: loc, SubagentEvidence: model.EvidenceAmbiguous}
		return s
	case loc.Matches != 1 || loc.Path == "" || st.cfg.Projects.FS == nil:
		return s
	}
	key := s.Key
	if !st.permitted() {
		st.problems = append(st.problems, model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemNotGranted,
			Path: loc.Path, Key: &key, Detail: "the transcript was neither armed nor pulled this cycle; nothing was read"})
		s.Transcript = &model.TranscriptFacts{Location: loc}
		return s
	}
	n := max(st.cfg.TailBytes, st.cfg.TimingTailBytes)
	buf, size, err := source.Tail(st.cfg.Projects.FS, loc.Path, n)
	st.reads++
	if err != nil {
		st.problems = append(st.problems, model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemUnreadable,
			Path: loc.Path, Key: &key, Detail: "reading the transcript tail: " + err.Error()})
		s.Transcript = &model.TranscriptFacts{Location: loc}
		return s
	}
	factsTail, factsWin := suffix(buf, size, st.cfg.TailBytes)
	timingTail, timingWin := suffix(buf, size, st.cfg.TimingTailBytes)
	f := claude.DecodeFacts(factsTail, factsWin)
	tm := claude.DecodeTiming(timingTail, timingWin)
	subagents, evidence := claude.CountSubagents(st.cfg.Projects.FS, loc)

	apiErrors, compactions := f.APIErrors, f.Compactions
	s.Transcript = &model.TranscriptFacts{
		Location: loc, Window: factsWin,
		APIErrors: &apiErrors, LastAPIError: f.LastAPIError, Color: f.Color,
		Subagents: subagents, SubagentEvidence: evidence,
		ContextTokens: f.ContextTokens, Model: f.Model, Compactions: &compactions,
		TimingWindow: timingWin, LastActivityAt: tm.LastActivityAt,
		LatestText: tm.LatestText, LatestTextAt: tm.LatestTextAt,
	}
	errs := apiErrors
	s.Fault = model.Fault{APIErrors: &errs, Window: factsWin, LastMessage: f.LastAPIError}
	// One Problem per key per cycle per window, naming the first field (C4).
	if f.ImplausibleInstants > 0 {
		st.problems = append(st.problems, model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemImplausibleInstant,
			Path: loc.Path, Key: &key, Field: f.ImplausibleField, Detail: "implausible instants in the facts window"})
	} else if tm.ImplausibleInstants > 0 {
		st.problems = append(st.problems, model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemImplausibleInstant,
			Path: loc.Path, Key: &key, Field: tm.ImplausibleField, Detail: "implausible instants in the timing window"})
	}
	return s
}

// Problems drains what this cycle's observation could not do. The actor hands
// them to the coordinator with the session.
func (st *Stage) Problems() []model.Problem {
	p := st.problems
	st.problems = nil
	return p
}

// Reads is how many transcript reads this stage has made (tests only).
func (st *Stage) Reads() int { return st.reads }

// suffix is the last n bytes of one read and the window that describes them.
func suffix(buf []byte, size int64, n int) ([]byte, model.Window) {
	w := model.Window{TailBytes: n, FileSize: size, Partial: size > int64(n)}
	if len(buf) > n {
		buf = buf[len(buf)-n:]
	}
	return buf, w
}
