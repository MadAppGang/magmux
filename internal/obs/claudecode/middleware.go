package claudecode

import (
	"fmt"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/adapters"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
)

// LaunchDirs answers where a session was LAUNCHED — not where it is working
// now.
//
// The two are usually the same and the difference is the whole story of
// transcript resolution: a correct encoder fed a session's LIVE working
// directory resolves about 96% of sessions, and the 4% that miss land in a
// directory that exists and holds somebody else's transcripts. magmux is
// exempt from that ceiling for one reason — it SPAWNED the sessions it
// observes, so it holds the value it passed to the child. That value, and
// nothing else, is what this port returns.
//
// ok is false when this process has no record of the session, which is the
// normal answer for a session magmux did not start. That is not a failure: the
// resolver falls back to scanning for the session id, which for a session that
// moved after startup is not a fallback at all but the only correct answer.
//
// # It must be safe for concurrent use
//
// ONE implementation serves every session's middleware, and each middleware
// runs on its own actor's goroutine. That is the opposite of the rule one step
// downstream — a middleware instance is never shared — and it is the rule that
// -race found broken the first time it was missed, in phase 3's PaneLocator.
type LaunchDirs interface {
	LaunchDir(pid int) (string, bool)
}

// Config is how the middleware is built. It is a plain value, copied on the
// way in.
type Config struct {
	// Root is Claude Code's projects directory, normally
	// $CLAUDE_CONFIG_DIR/projects or ~/.claude/projects.
	//
	// An empty Root yields a middleware that adds nothing at all, rather than
	// one that reads the developer's home directory. Every test in this tree
	// points at testdata, and a default that silently reached ~/.claude would
	// make that impossible to enforce.
	Root string

	// TailBytes bounds each read window. Non-positive takes
	// [adapters.DefaultTailBytes], which is the measured 200 KB.
	TailBytes int

	// LaunchDirs answers the launch-directory question. nil means nobody can,
	// and every session is then resolved by scanning for its session id —
	// correct, and one directory listing more expensive.
	LaunchDirs LaunchDirs
}

// Middleware returns the factory for one Claude Code middleware per session.
//
// Add it to [pipeline.Config.Chain]. It sits AFTER the pane gate, which is
// prepended automatically: the gate answers which pane a session is in, and
// nothing here needs that, but everything downstream of both wants them in one
// fact set.
func Middleware(cfg Config) pipeline.Factory {
	return func(sessionID string) pipeline.Middleware {
		return &middleware{
			tr:   adapters.Transcripts{Root: cfg.Root, TailBytes: cfg.TailBytes},
			dirs: cfg.LaunchDirs,
			key:  sessionID,
		}
	}
}

// middleware is ONE session's transcript reader.
//
// Everything it remembers is about that one session, which is why a shared
// instance would be a silent wrong answer rather than a merely inefficient
// one.
type middleware struct {
	tr   adapters.Transcripts
	dirs LaunchDirs
	key  string

	// loc is the transcript once found. A HIT is remembered and a MISS is not,
	// deliberately, and for the same reason the pane gate keeps a match and
	// re-asks a miss: which file belongs to a session id cannot change — the
	// id is in the file's name — while a session that has written nothing yet
	// writes its first record seconds after it starts. Caching that miss would
	// leave a session unexamined for as long as it lived.
	loc adapters.Location

	// read is the last reading, kept so that an unchanged file is not read
	// again. size and mod are the CACHE KEY and nothing else; see
	// [adapters.Transcripts.Read] on why they are not an activity signal.
	read     adapters.Reading
	haveRead bool
}

func (m *middleware) Name() string { return "claude-code" }

// Observe adds what the session's own record says.
//
// A session whose transcript cannot be found leaves every field this stage
// owns exactly as it arrived — undetermined. That is the honest answer and it
// is a different one from zero: "this session has no errors and no subagents"
// and "nobody has been able to look" are the two answers a user has to be able
// to tell apart, and the model spells both.
func (m *middleware) Observe(s obs.Session) obs.Session {
	if m.tr.Root == "" || s.ID == "" {
		return s
	}

	// The launch directory: whatever an earlier stage already knew, else the
	// port. Recorded onto the fact set when the port answers, because it is
	// the input to the tool's own on-disk layout and a consumer that wants to
	// find anything else of this session's cannot re-derive it.
	launchDir := s.LaunchDir
	if launchDir == "" && m.dirs != nil && s.PID > 0 {
		if dir, ok := m.dirs.LaunchDir(s.PID); ok && dir != "" {
			launchDir, s.LaunchDir = dir, dir
		}
	}

	if !m.loc.Found() {
		// An error here is indistinguishable from a miss in the fact set, and
		// that is a known gap rather than a choice: pipeline.Middleware has no
		// channel for a problem. The consequence is bounded — the fields stay
		// undetermined, which is what a caller must already handle — and it is
		// recorded in the phase log.
		loc, err := m.tr.Locate(s.ID, launchDir)
		if err == nil && loc.Found() {
			m.loc = loc
		}
	}
	if !m.loc.Found() {
		return s
	}

	// SUBAGENTS — from the files, never from the transcript, and re-counted on
	// every observation because subagents appear and finish inside one turn.
	//
	// A session whose subagents directory does not exist counts as ZERO, not as
	// undetermined: the directory is created lazily on the first spawn, so its
	// absence is the ordinary shape of a session that has spawned none. 94% of
	// live sessions are that shape, which is why the other reading left the
	// count blank almost everywhere. See adapters.Transcripts.Subagents.
	if sub, err := m.tr.Subagents(m.loc); err == nil && sub.Known {
		s.SubagentCount = obs.Ptr(sub.Count)
	}

	// COLOUR and FAULTS — from a bounded window of the transcript.
	if !m.haveRead || m.changed() {
		read, err := m.tr.Read(m.loc)
		if err != nil {
			// The file was located once and cannot be read now: rotated,
			// compacted, or removed. The previous reading is still the last
			// thing this session actually said, so it is kept rather than
			// replaced with a zero — but nothing new is claimed.
			if !m.haveRead {
				return s
			}
		} else {
			m.read, m.haveRead = read, true
		}
	}
	if !m.haveRead {
		return s
	}

	if m.read.Color != nil {
		// A fresh pointer, never the one this stage is holding: a fact set is
		// frozen by the actor after the chain runs, and a stage that hands out
		// its own pointer has handed out a route into the next reading.
		colour := *m.read.Color
		s.Color = &colour
	}
	s.Fault.APIErrors = obs.Ptr(m.read.APIErrors)
	if detail := m.faultDetail(); detail != "" {
		s.Fault.Detail = detail
	}
	return s
}

// changed reports whether the file has moved on since the last reading.
//
// This is cache validity and nothing else. It is NOT "has the session done
// something": records that carry no conversational turn touch the file, so a
// change here means "read it again", never "the session is busy". The
// inference in the other direction — that an unchanged file means an idle
// session — is the one that manufactured a 1.37-hour staleness out of a
// correctly idle session, and nothing here makes it.
func (m *middleware) changed() bool {
	st, err := m.tr.Stat(m.loc)
	if err != nil {
		// Cannot tell. Re-read: the cost is one bounded window, and the
		// alternative is serving a stale answer because a stat failed.
		return true
	}
	return st.Size != m.read.Size || st.ModTime != m.read.ModTime
}

// faultDetail is magmux's own words about the most recent fault, and — on a
// ZERO as well as on a count — how much of the transcript the count covers.
//
// # The count is RECENT, and that was decided rather than discovered
//
// The error count is taken from the last 200 KB of the transcript. It is
// therefore the number of API-level errors in the RECENT part of the session,
// and it is published as that and not as "the errors this session has hit".
// Decided in fix round 1 in the user's absence and recorded as reversible; the
// alternative — scan each transcript whole once, then tail it — would make
// attention rule 3 fire for any session that had EVER hit an error, the same
// sticky-forever shape phase 8 rejected for exit statuses. Attention is the
// consumer that acts on this field, and an error from twenty megabytes ago is
// not a reason to interrupt a human now.
//
// # Why the zero needs the disclosure most
//
// This function used to return "" for a zero before it reached the clause that
// disclosed the window, so a truncated read published a bare, DETERMINED 0.
// Live validation on 2026-09-19 found that for 13 of 36 sessions: each had real
// errors, every one outside the window, and each was published as apiErrors 0
// with no detail at all. A zero over a window is the claim most easily
// over-read, because nothing about it looks partial. A whole-file zero is exact
// and stays unqualified.
func (m *middleware) faultDetail() string {
	window := ""
	if m.read.Truncated {
		window = fmt.Sprintf("the last %s of a %s transcript",
			sizeText(m.read.Window), sizeText(m.read.Size))
	}
	if m.read.APIErrors == 0 {
		if window == "" {
			return ""
		}
		return "no API-level error in " + window + "; errors before that are not counted"
	}
	detail := m.read.LastError
	if detail == "" {
		detail = "an API-level error was recorded"
	}
	if window != "" {
		detail += " (counted in " + window + "; errors before that are not counted)"
	}
	return detail
}

// sizeText is a byte count for a person: "200 KB", "5.4 MB". Binary units,
// because DefaultTailBytes is 200<<10 and "204.8 KB" would read as a different
// number from the one every document calls it.
func sizeText(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", n>>10)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
}
