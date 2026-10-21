package watch

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

// DefaultWindow is the coalescing window (D8): 50 ms for both hosts.
//
// Measured, not chosen: a session was observed rewriting its record twice,
// thirteen seconds apart, and those are two genuine changes the window must
// NOT merge, while one rewrite that fsnotify reports as several events must
// produce one wake-up. 50 ms is far below the former and above the latter.
// (tt's 250 ms rests on arithmetic that is wrong; ADR-D8.)
const DefaultWindow = 50 * time.Millisecond

// DefaultBuffer is the Events buffer.
const DefaultBuffer = 64

// closeDrainWait bounds the teardown drain, on the Clock. The drain normally
// finishes in microseconds; the bound exists so a future fsnotify that never
// closes its channel costs a pause, not a hung Stop.
const closeDrainWait = 2 * time.Second

// Config is how a watcher is built. It is copied by value into the watcher.
type Config struct {
	Clock  ports.Clock   // required: the coalescer and closeAndDrain's bound use it
	Window time.Duration // 0 => DefaultWindow
	Budget Budget        // zero => RuntimeBudget()
	Buffer int           // 0 => DefaultBuffer
	// Never lists absolute directories no directory target may equal or lie
	// under: the projects tree, whose directories are never armed (C11, D15).
	// The watcher knows no Claude layout, so the coordinator names the tree.
	Never []string
}

// Event is one coalesced report: something under these paths changed.
type Event struct {
	Paths   []string // absolute, de-duplicated, in arrival order
	Dropped int      // Events lost since the previous delivered Event (FR18.6)
	Err     error    // a backend error; the coordinator turns it into a ScopeWatch Problem
}

// Degradation is why a wave did not get everything it asked for.
type Degradation uint8

const (
	DegradedNone Degradation = iota
	DegradedFileCap
	DegradedJobDirCap
	DegradedJobTranscriptCap
	DegradedBudget
	DegradedUnavailable
	DegradedNotConverged
)

var degradationNames = []string{
	"none", "file-cap", "job-dir-cap", "job-transcript-cap", "budget", "unavailable", "not-converged",
}

func (d Degradation) String() string {
	if int(d) >= len(degradationNames) {
		return fmt.Sprintf("Degradation(%d)", d)
	}
	return degradationNames[d]
}

// ArmReport is what one Arm, Verify or Release did.
//
// The ON-DEMAND signal: when the spine alone exceeds the claim, Arm arms
// nothing new, reports DegradedBudget, and puts the spine targets in Over.
// A spine target in Over means "degrade to on-demand" (D15); a spine target
// is never shed one by one and never appears in Shed.
type ArmReport struct {
	Armed, Held, Released int
	// Cost is the claim now, each held object once: darwin descriptors (a
	// directory is 1 + its entries, a file 1), linux watches.
	Cost int
	// Over: refused by the budget or a ceiling at Arm, in shedding order.
	// Granted as Pull for this cycle; offered to Arm again next cycle.
	Over []model.WatchTarget
	// Shed: held, found over budget by Verify. Still armed for this cycle's
	// read; the cycle's Release must not keep them.
	Shed     []model.WatchTarget
	Failed   []model.Problem // ScopeWatch: ProblemWatchArm, ProblemWatchLimit
	Degraded []Degradation
}

func (r *ArmReport) degrade(d Degradation) {
	if !slices.Contains(r.Degraded, d) {
		r.Degraded = append(r.Degraded, d)
	}
}

// Watcher is one fsnotify watcher and the held set behind it.
type Watcher struct {
	cfg    Config
	claim  int
	fsw    *fsnotify.Watcher
	events chan Event
	stop   chan struct{}
	done   chan struct{}

	// The coordinator's alone (Arm, Verify, Release).
	held map[string]held
	// add and remove are fsw.Add and fsw.Remove; a package test replaces
	// them to inject EMFILE and ENOSPC.
	add, remove func(string) error
}

type held struct {
	t    model.WatchTarget
	id   identity
	cost int
}

// Start creates the fsnotify watcher and its event loop. It arms nothing:
// the coordinator's first Arm does. The caller must Stop the result.
func Start(cfg Config) (*Watcher, error) {
	if cfg.Clock == nil {
		return nil, errors.New("watch: Config.Clock is required")
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultBuffer
	}
	if cfg.Budget == (Budget{}) {
		b, err := RuntimeBudget()
		if err != nil {
			return nil, err
		}
		cfg.Budget = b
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		if errors.Is(err, syscall.EMFILE) {
			return nil, fmt.Errorf("watch: %s: %w", limitEMFILE, err)
		}
		return nil, fmt.Errorf("watch: %w", err)
	}
	w := &Watcher{
		cfg:    cfg,
		claim:  claimOf(cfg.Budget),
		fsw:    fsw,
		events: make(chan Event, cfg.Buffer),
		stop:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		held:   map[string]held{},
		add:    fsw.Add,
		remove: fsw.Remove,
	}
	go w.loop()
	return w, nil
}

// Budget is what this watcher was given (or what RuntimeBudget computed).
func (w *Watcher) Budget() Budget { return w.cfg.Budget }

// Holds reports whether path is held NOW: armed by the last Arm that admitted
// it, and kept by every Verify since. A target whose identity changed and
// whose re-arm failed is no longer held, and neither is anything once the
// watcher has stopped. This, not an earlier Arm's report, is what a read
// "under a watch" may rely on (review r2 #4). Caller's goroutine only, like
// Arm and Verify.
func (w *Watcher) Holds(path string) bool {
	if w.stopped() {
		return false
	}
	_, ok := w.held[path]
	return ok
}

// Events yields coalesced reports. It is closed when the watcher stops.
func (w *Watcher) Events() <-chan Event { return w.events }

// Stop shuts the watcher down and waits for its loop. Idempotent, and safe
// from any goroutine. Stopping is a signal rather than a close, because a
// second close panics and guarding it needs a sync.Once (G5).
func (w *Watcher) Stop() {
	select {
	case w.stop <- struct{}{}:
	case <-w.done:
	}
	<-w.done
}

// Stopped reports, without blocking, whether the loop has ended — by Stop, or
// because fsnotify closed a channel under it. A stopped watcher holds
// nothing and never starts again, so its owner must stop reading "under a
// watch" and pull instead (review r3 #1). Safe from any goroutine: it is a
// receive on a closed-or-open channel.
func (w *Watcher) Stopped() bool { return w.stopped() }

func (w *Watcher) stopped() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// armPriority is the order Arm admits targets in: the spine first, because a
// lost spine makes a whole family undiscoverable; then job directories (the
// only watch that sees a job record appear), job transcripts, and interactive
// transcripts last. Shedding is the reverse (D15).
func armPriority(k model.TargetKind) int {
	switch k {
	case model.TargetSpine:
		return 0
	case model.TargetJobDir:
		return 1
	case model.TargetJobTranscript:
		return 2
	default:
		return 3
	}
}

// sortForArm orders targets by priority, then path.
func sortForArm(ts []model.WatchTarget) {
	slices.SortStableFunc(ts, func(a, b model.WatchTarget) int {
		if c := armPriority(a.Kind) - armPriority(b.Kind); c != 0 {
			return c
		}
		return compareStrings(a.Path, b.Path)
	})
}

// sortForShed is shedding order: interactive transcripts, then job
// transcripts, then job directories, then the spine; within a kind the last
// admitted goes first.
func sortForShed(ts []model.WatchTarget) {
	slices.SortStableFunc(ts, func(a, b model.WatchTarget) int {
		if c := armPriority(b.Kind) - armPriority(a.Kind); c != 0 {
			return c
		}
		return compareStrings(b.Path, a.Path)
	})
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// refusal says why a target may never be armed, or "".
func (w *Watcher) refusal(t model.WatchTarget) string {
	switch {
	case t.Path == "" || !filepath.IsAbs(t.Path):
		return "a watch target must be an absolute path"
	case t.Kind == model.TargetUnknown || t.Kind > model.TargetJobTranscript:
		return "a watch target must have a known kind"
	case t.Dir && t.Kind != model.TargetSpine && t.Kind != model.TargetJobDir:
		return "only the spine and job directories are armed as directories; a transcript is armed as a FILE (C11)"
	case t.Dir && w.underNever(t.Path):
		return "a directory of the projects tree is never armed (C11, D15)"
	}
	return ""
}

func watchProblem(kind model.ProblemKind, path, detail string) model.Problem {
	return model.Problem{Scope: model.ScopeWatch, Kind: kind, Path: path, Detail: detail}
}

// armProblem maps an Add failure: EMFILE and ENOSPC name the limit.
func armProblem(path string, err error) model.Problem {
	switch {
	case errors.Is(err, syscall.EMFILE):
		return watchProblem(model.ProblemWatchLimit, path, limitEMFILE+": "+err.Error())
	case errors.Is(err, syscall.ENOSPC):
		return watchProblem(model.ProblemWatchLimit, path, limitENOSPC+": "+err.Error())
	}
	return watchProblem(model.ProblemWatchArm, path, err.Error())
}

func (w *Watcher) unavailable() ArmReport {
	return ArmReport{
		Degraded: []Degradation{DegradedUnavailable},
		Failed:   []model.Problem{watchProblem(model.ProblemWatchArm, "", "the watcher has stopped")},
	}
}

// recount re-lists every held directory and returns the claim now.
func (w *Watcher) recount() int {
	total := 0
	for p, h := range w.held {
		h.cost = targetCost(h.t)
		w.held[p] = h
		total += h.cost
	}
	return total
}

// arm adds one target, recording the identity it had across the Add.
func (w *Watcher) arm(t model.WatchTarget, cost int) *model.Problem {
	before, fi, err := identityOf(t.Path)
	if err != nil {
		p := armProblem(t.Path, err)
		return &p
	}
	if fi.IsDir() != t.Dir {
		p := watchProblem(model.ProblemWatchArm, t.Path, fmt.Sprintf("the target says Dir=%v but the path is %s", t.Dir, fi.Mode().Type()))
		return &p
	}
	if err := w.add(t.Path); err != nil {
		p := armProblem(t.Path, err)
		return &p
	}
	after, _, err := identityOf(t.Path)
	if err != nil || after != before {
		// Replaced between the stat and the Add: record no identity, so the
		// next Verify re-arms it before it is read.
		after = identity{}
	}
	w.held[t.Path] = held{t: t, id: after, cost: cost}
	return nil
}

// drop releases one held target. Its error is deliberately ignored: the
// expected one is ErrNonExistentWatch, because fsnotify removes its own watch
// when the watched path is removed or renamed, and any other leaves the
// descriptor with fsnotify, which closes it at Stop. Either way the target is
// no longer held, which is the only fact the protocol needs.
func (w *Watcher) drop(path string) {
	_ = w.remove(path)
	delete(w.held, path)
}

// Arm arms targets, within budget. It ARMS ONLY: nothing held is released or
// re-opened here unless its identity changed. A held target with the same
// identity counts in Held and costs nothing again.
func (w *Watcher) Arm(targets []model.WatchTarget) ArmReport {
	if w.stopped() {
		return w.unavailable()
	}
	var rep ArmReport

	// De-duplicate by path; refuse what may never be armed.
	seen := map[string]bool{}
	var want []model.WatchTarget
	for _, t := range targets {
		if seen[t.Path] {
			continue
		}
		seen[t.Path] = true
		if why := w.refusal(t); why != "" {
			rep.Failed = append(rep.Failed, watchProblem(model.ProblemWatchArm, t.Path, why))
			continue
		}
		want = append(want, t)
	}
	sortForArm(want)

	cost := w.recount()

	// The spine is admitted whole or not at all: if it does not fit, the
	// watcher degrades to on-demand and arms nothing new.
	spineCost := 0
	for _, h := range w.held {
		if h.t.Kind == model.TargetSpine {
			spineCost += h.cost
		}
	}
	for _, t := range want {
		if _, ok := w.held[t.Path]; !ok && t.Kind == model.TargetSpine {
			spineCost += targetCost(t)
		}
	}
	onDemand := w.claim > 0 && spineCost > w.claim

	count := map[model.TargetKind]int{}
	for _, h := range w.held {
		count[h.t.Kind]++
	}

	for _, t := range want {
		if h, ok := w.held[t.Path]; ok {
			id, _, err := identityOf(t.Path)
			if err == nil && id.ok && id == h.id {
				rep.Held++
				continue
			}
			// The path's identity changed under the watch: close, then
			// open, in that order.
			w.drop(t.Path)
			cost -= h.cost
			count[h.t.Kind]--
			if p := w.arm(t, h.cost); p != nil {
				rep.Failed = append(rep.Failed, *p)
				continue
			}
			cost += h.cost
			count[t.Kind]++
			rep.Armed++
			continue
		}
		if onDemand {
			rep.Over = append(rep.Over, t)
			continue
		}
		if d, capped := w.capped(t.Kind, count); capped {
			rep.Over = append(rep.Over, t)
			rep.degrade(d)
			continue
		}
		c := targetCost(t)
		if w.claim > 0 && cost+c > w.claim {
			rep.Over = append(rep.Over, t)
			rep.degrade(DegradedBudget)
			continue
		}
		if p := w.arm(t, c); p != nil {
			rep.Failed = append(rep.Failed, *p)
			continue
		}
		cost += c
		count[t.Kind]++
		rep.Armed++
	}
	if onDemand {
		rep.degrade(DegradedBudget)
	}
	sortForShed(rep.Over)
	rep.Cost = cost
	return rep
}

// capped applies tt's ceilings. Files bounds every armed transcript.
func (w *Watcher) capped(k model.TargetKind, count map[model.TargetKind]int) (Degradation, bool) {
	b := w.cfg.Budget
	switch k {
	case model.TargetJobDir:
		if b.JobDirs > 0 && count[k] >= b.JobDirs {
			return DegradedJobDirCap, true
		}
	case model.TargetJobTranscript:
		if b.JobTranscripts > 0 && count[k] >= b.JobTranscripts {
			return DegradedJobTranscriptCap, true
		}
		fallthrough
	case model.TargetTranscript:
		if b.Files > 0 && count[model.TargetTranscript]+count[model.TargetJobTranscript] >= b.Files {
			return DegradedFileCap, true
		}
	}
	return DegradedNone, false
}

// Verify compares each held target's identity with what its path names now,
// re-arms a replaced target, drops a vanished one, and recounts the budget.
// It runs BEFORE the read the targets cover.
//
// An overrun puts held targets into Shed, in shedding order, until the rest
// fits. Shed targets stay armed for this cycle's read (they were armed before
// it); the cycle's Release must not keep them. The spine is never shed: if it
// alone exceeds the claim, Verify reports DegradedBudget and the next Arm
// degrades to on-demand.
func (w *Watcher) Verify() ArmReport {
	if w.stopped() {
		return w.unavailable()
	}
	var rep ArmReport
	paths := make([]string, 0, len(w.held))
	for p := range w.held {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		h := w.held[p]
		id, _, err := identityOf(p)
		switch {
		case err != nil:
			w.drop(p)
			rep.Released++
			rep.Failed = append(rep.Failed, armProblem(p, err))
		case !id.ok || id != h.id:
			w.drop(p)
			if prob := w.arm(h.t, h.cost); prob != nil {
				rep.Released++
				rep.Failed = append(rep.Failed, *prob)
				continue
			}
			rep.Armed++
		default:
			rep.Held++
		}
	}

	cost := w.recount()
	if w.claim > 0 && cost > w.claim {
		var candidates []model.WatchTarget
		for _, h := range w.held {
			if h.t.Kind != model.TargetSpine {
				candidates = append(candidates, h.t)
			}
		}
		sortForShed(candidates)
		for _, t := range candidates {
			if cost <= w.claim {
				break
			}
			rep.Shed = append(rep.Shed, t)
			cost -= w.held[t.Path].cost
		}
		rep.degrade(DegradedBudget)
	}
	rep.Cost = w.recount()
	return rep
}

// Release releases every held target not in keep. It arms nothing, not even
// a replacement for a kept target whose identity changed: that re-arm is the
// next cycle's Verify, before that cycle's read.
func (w *Watcher) Release(keep []model.WatchTarget) ArmReport {
	var rep ArmReport
	want := map[string]bool{}
	for _, t := range keep {
		want[t.Path] = true
	}
	for p := range w.held {
		if want[p] {
			continue
		}
		w.drop(p)
		rep.Released++
	}
	for _, h := range w.held {
		rep.Cost += h.cost
	}
	rep.Held = len(w.held)
	if w.stopped() {
		rep.degrade(DegradedUnavailable)
	}
	return rep
}

// closeAndDrain releases the fsnotify watcher and DRAINS its channels until
// the events channel closes, bounded by closeDrainWait on the Clock.
//
// The drain is the descriptor release, and it is MEASURED: fsnotify v1.9.0's
// Close on darwin returns before its reader goroutine has finished, and Close
// alone in a start/stop loop leaves descriptors behind (20 cycles on a
// three-record directory moved the lowest free descriptor from 4 to 85, and it
// settled only to 81). Draining until the channel closes releases every one
// (G12). It is written over channels, not the watcher, so the bound can be
// tested against a channel that never closes.
func closeAndDrain(closeFn func() error, events <-chan fsnotify.Event, errs <-chan error, clock ports.Clock) {
	_ = closeFn()
	bound := clock.NewTimer(closeDrainWait)
	defer bound.Stop()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-errs:
		case <-bound.C():
			return
		}
	}
}

// loop owns the fsnotify channels, the pending set and the one window timer.
// Every send to Events is non-blocking: a loop that blocked on a consumer
// would stop draining fsnotify, and a watcher that stops draining fsnotify
// goes silent with no error anywhere.
//
// A loss is never silent either. An Event that does not fit is counted, and
// the count rides on the next Event that does; while it is outstanding the
// loop also OFFERS a bare Event{Dropped: n} on a send case of its own select,
// so the count is delivered the moment the consumer frees a slot even when no
// further change ever comes. Burst-then-quiet is exactly how a session
// finishes, and a count that waited for the next write would wait forever.
// The offer is a select case, not a send, so the loop still never blocks.
func (w *Watcher) loop() {
	defer close(w.done)
	defer close(w.events)
	defer closeAndDrain(w.fsw.Close, w.fsw.Events, w.fsw.Errors, w.cfg.Clock)

	var (
		timer   ports.Timer
		timerC  <-chan time.Time
		pending []string
		inSet   = map[string]bool{}
		dropped int
	)
	disarm := func() {
		if timer != nil {
			timer.Stop()
			timer, timerC = nil, nil
		}
	}
	publish := func(ev Event) {
		ev.Dropped = dropped
		select {
		case w.events <- ev:
			dropped = 0
		default:
			dropped++
		}
	}

	for {
		// report is Events while a loss is outstanding, else nil: a nil
		// channel's case is never chosen.
		var report chan<- Event
		if dropped > 0 {
			report = w.events
		}
		select {
		case <-w.stop:
			disarm()
			return

		case report <- Event{Dropped: dropped}:
			dropped = 0

		case ev, ok := <-w.fsw.Events:
			if !ok {
				disarm()
				publish(Event{Err: errors.New("watch: the fsnotify event channel closed unexpectedly")})
				return
			}
			name := filepath.Clean(ev.Name)
			if !inSet[name] {
				inSet[name] = true
				pending = append(pending, name)
			}
			// ONE timer for the whole window: the consumer rebuilds one
			// document, so a burst across three files wants one wake-up.
			if timer == nil {
				timer = w.cfg.Clock.NewTimer(w.cfg.Window)
				timerC = timer.C()
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				disarm()
				publish(Event{Err: errors.New("watch: the fsnotify error channel closed unexpectedly")})
				return
			}
			publish(Event{Err: fmt.Errorf("watch: %w", err)})

		case <-timerC:
			// One-shot: finished with. The next event arms a fresh one.
			timer.Stop()
			timer, timerC = nil, nil
			publish(Event{Paths: pending})
			pending, inSet = nil, map[string]bool{}
		}
	}
}

// underNever reports a path equal to or inside one of Config.Never.
func (w *Watcher) underNever(p string) bool {
	p = filepath.Clean(p)
	for _, n := range w.cfg.Never {
		n = filepath.Clean(n)
		if p == n || strings.HasPrefix(p, n+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
