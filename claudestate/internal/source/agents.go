package source

import (
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

// The agents source's gates (FR23, tt:refresh.go). They are TIMESTAMPS
// compared against the coordinator's now, never timers, so the source arms
// nothing and an idle pipeline holds no timer for it (C2).
const (
	// MinInterval: a refresh within this long of the last completed listing
	// is not run again.
	MinInterval = 2 * time.Second
	// backoffBase and backoffCap are tt's 2s, 4s, 8s ... 5 min after
	// consecutive failures.
	backoffBase = 2 * time.Second
	backoffCap  = 5 * time.Minute
)

// The skip words RefreshOutcome.Skipped carries.
const (
	SkippedMinInterval = "min-interval"
	SkippedBackoff     = "backoff"
)

// Agents is the agents-listing source's state. The port does the spawn, the
// cache and the lock; this owns only when a call may be made and what the
// last good listing was.
type Agents struct {
	port     ports.AgentsListing
	listing  *claude.AgentsListing // the applied listing; nil = none ever
	lastDone time.Time             // when the last List completed, success or not
	failures int
	nextOK   time.Time // backoff: no call before this
	lastErr  error
	inflight bool
}

// NewAgents builds the source; a nil port is the source turned off.
func NewAgents(port ports.AgentsListing) *Agents { return &Agents{port: port} }

// On reports whether the source exists at all.
func (a *Agents) On() bool { return a.port != nil }

// Port is the host's port, for the coordinator's one List goroutine.
func (a *Agents) Port() ports.AgentsListing { return a.port }

// Begin decides whether a refresh may run at now. joined: a List is already
// in flight, and the caller waits for it. skipped: the rate limit or the
// backoff refused, which is not an error.
func (a *Agents) Begin(now time.Time) (start, joined bool, skipped string) {
	switch {
	case a.inflight:
		return false, true, ""
	case !a.nextOK.IsZero() && now.Before(a.nextOK):
		return false, false, SkippedBackoff
	case a.listing != nil && a.failures == 0 && !a.lastDone.IsZero() && now.Sub(a.lastDone) < MinInterval:
		return false, false, SkippedMinInterval
	}
	a.inflight = true
	return true, false, ""
}

// Finish applies one List result at now. A listing that does not decode is a
// failure exactly like a failed List: malformed is never empty (C12).
func (a *Agents) Finish(now time.Time, l ports.Listing, err error) (*claude.AgentsListing, []model.Problem, error) {
	a.inflight = false
	a.lastDone = now
	var problems []model.Problem
	if err == nil {
		var dl claude.AgentsListing
		dl, problems, err = claude.DecodeAgents(l.Raw, l.At)
		if err == nil {
			a.listing, a.failures, a.nextOK, a.lastErr = &dl, 0, time.Time{}, nil
			return a.listing, problems, nil
		}
	}
	a.failures++
	a.lastErr = err
	delay := backoffBase << min(a.failures-1, 8)
	if delay > backoffCap {
		delay = backoffCap
	}
	a.nextOK = now.Add(delay)
	problems = append(problems, model.Problem{Scope: model.ScopeSource, Kind: model.ProblemAgentsListing,
		Detail: fmt.Sprintf("the agents listing failed (%v); the next attempt is not before %s", err, delay)})
	return nil, problems, err
}

// Listing is the listing the next join applies; nil = none.
func (a *Agents) Listing() *claude.AgentsListing { return a.listing }

// Report is the Sources row (§3.1 table).
func (a *Agents) Report() model.SourceReport {
	r := model.SourceReport{ID: model.SourceAgents}
	switch {
	case a.port == nil:
		r.State = model.SourceOff
	case a.lastErr != nil && a.listing == nil:
		r.State, r.Detail = model.SourceFailed, a.lastErr.Error()
	case a.lastErr != nil:
		r.State, r.Detail = model.SourceDegraded, "the last listing failed; an older one is applied: "+a.lastErr.Error()
		at := a.listing.At
		r.At = &at
	case a.listing != nil:
		r.State = model.SourceOK
		at := a.listing.At
		r.At = &at
	default:
		r.State, r.Detail = model.SourceStateUnknown, "no listing has been taken"
	}
	return r
}

// Tail reads the last n bytes of one file in ONE read, and its size at the
// moment of the read. It seeks when the file can (os.DirFS's files do);
// otherwise it reads the file and keeps the tail, which only an in-memory test
// FS reaches.
func Tail(fsys fs.FS, name string, n int) ([]byte, int64, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	if s, ok := f.(io.Seeker); ok && size > int64(n) {
		if _, err := s.Seek(size-int64(n), io.SeekStart); err != nil {
			return nil, size, err
		}
		b, err := io.ReadAll(io.LimitReader(f, int64(n)))
		return b, size, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, size, err
	}
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return b, size, nil
}
