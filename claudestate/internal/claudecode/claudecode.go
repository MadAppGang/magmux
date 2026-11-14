// Package claudecode is the per-session transcript stage: it names the one
// file it will read before the arm wave (stage.Targeting), honours the read
// permit the cycle grants it, and reads that file AT MOST ONCE per cycle, the
// last max(TailBytes, TimingTailBytes) bytes, decoding the facts over the last
// TailBytes and the timing over the last TimingTailBytes of the same buffer
// (C20, NFR13 per key). A file whose size and mtime are unchanged since the
// last read is not read again (see decoded).
//
// It keeps no location across cycles: the coordinator hands it the cycle's
// Location in Targets. A located transcript is read only under a Grant (Armed
// or Pull) or when the cycle is Unwatched; any other read is refused with
// ProblemNotGranted and the facts stay nil (C17). An ambiguous location opens
// nothing (D5, C6).
package claudecode

import (
	"io/fs"
	"path/filepath"
	"time"

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
	// Now is the pipeline's clock, for the racy-file rule (see decoded).
	// nil keeps nothing between reads: every cycle reads, as before.
	Now func() time.Time
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

	// last is the last read's decoding, reused while the file is unchanged.
	last decoding

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
	m, err := st.decoded(loc.Path)
	if err != nil {
		st.problems = append(st.problems, model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemUnreadable,
			Path: loc.Path, Key: &key, Detail: "reading the transcript tail: " + err.Error()})
		s.Transcript = &model.TranscriptFacts{Location: loc}
		return s
	}
	f, tm, factsWin, timingWin := m.facts, m.timing, m.factsWin, m.timingWin
	subagents, evidence := claude.CountSubagents(st.cfg.Projects.FS, loc)

	apiErrors, compactions := f.APIErrors, f.Compactions
	s.Transcript = &model.TranscriptFacts{
		Location: loc, Window: factsWin,
		APIErrors: &apiErrors, LastAPIError: f.LastAPIError, Color: clonePtr(f.Color),
		Subagents: subagents, SubagentEvidence: evidence,
		ContextTokens: clonePtr(f.ContextTokens), Model: f.Model, Compactions: &compactions,
		TimingWindow: timingWin, LastActivityAt: clonePtr(tm.LastActivityAt),
		LatestText: tm.LatestText, LatestTextAt: clonePtr(tm.LatestTextAt),
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

// racyWindow is how old an mtime must be, against Now, to serve as a cache
// key: longer than the coarsest mtime granularity in use (HFS+'s one second).
const racyWindow = 2 * time.Second

// decoding is one read's decoded result, and the (path, size, mtime) it was
// read at.
type decoding struct {
	path                string
	size                int64
	mod                 time.Time
	facts               claude.Facts
	timing              claude.Timing
	factsWin, timingWin model.Window
}

// decoded returns the transcript's facts and timing, reading and decoding it
// only when it may have changed since the last read (review r1 #4a).
//
// A wake re-runs every session's chain, but most wakes change one file, and
// re-reading max(TailBytes, TimingTailBytes) of every transcript on each one
// is most of a busy machine's cost. So one fs.Stat comes first: a file whose
// size and mtime equal the last read's, at the same location, is not read
// again. That is ADR-D7's permitted use, "mtime and size are used as cache
// keys only"; nothing published is inferred from them.
//
//   - The Stat is taken BEFORE the read. A change between the two can only
//     leave the key older than the content, which forces the next read; it
//     can never leave a key newer than what was decoded.
//   - A read whose own size disagrees with the Stat (an append in between)
//     keeps no key at all.
//   - A zero mtime is never a key: it says nothing (an fstest.MapFS file, or
//     an FS that reports none), so such a file is read on every cycle, as
//     before.
//   - The racy-file rule (git's): an mtime within racyWindow of Now is not a
//     key either. A filesystem stamps mtime at its own granularity (a jiffy
//     on Linux, a whole second on HFS+), so a same-size rewrite in the same
//     tick as the Stat would keep the old key over new content. Once the
//     mtime is older than any tick a later write can share, every later
//     write moves it. A nil Now keeps no key at all.
//
// Everything else stays per cycle: the read permit and the subagent count.
// The implausible-instant Problem is raised every cycle too, but only because
// what it is raised FROM (ImplausibleInstants and ImplausibleField) rides in
// the cached Facts and Timing: a cache that kept less than the whole decoded
// values would drop it silently (review r2 #7).
func (st *Stage) decoded(path string) (decoding, error) {
	info, serr := fs.Stat(st.cfg.Projects.FS, path)
	if m := st.last; serr == nil && m.path == path && !m.mod.IsZero() &&
		info.Size() == m.size && info.ModTime().Equal(m.mod) {
		return m, nil
	}
	st.last = decoding{}
	n := max(st.cfg.TailBytes, st.cfg.TimingTailBytes)
	buf, size, err := source.Tail(st.cfg.Projects.FS, path, n)
	st.reads++
	if err != nil {
		return decoding{}, err
	}
	m := decoding{path: path, size: size}
	var factsTail, timingTail []byte
	factsTail, m.factsWin = suffix(buf, size, st.cfg.TailBytes)
	timingTail, m.timingWin = suffix(buf, size, st.cfg.TimingTailBytes)
	m.facts = claude.DecodeFacts(factsTail, m.factsWin)
	m.timing = claude.DecodeTiming(timingTail, m.timingWin)
	if serr == nil && size == info.Size() && !info.ModTime().IsZero() &&
		st.cfg.Now != nil && info.ModTime().Before(st.cfg.Now().Add(-racyWindow)) {
		m.mod = info.ModTime()
		st.last = m
	}
	return m, nil
}

// clonePtr is a fresh copy of *p: a remembered decoding is shared by every
// cycle that reuses it, so no session may hold a pointer into it.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
