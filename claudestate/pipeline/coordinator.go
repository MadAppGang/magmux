package pipeline

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/internal/claudecode"
	"github.com/MadAppGang/magmux/claudestate/internal/source"
	"github.com/MadAppGang/magmux/claudestate/join"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
	"github.com/MadAppGang/magmux/claudestate/verdict"
	"github.com/MadAppGang/magmux/claudestate/watch"
)

// maxJobArmRounds bounds the job-directory fixed point (tt's M5).
const maxJobArmRounds = 4

// coordinator is the pipeline's goroutine-owned state. Nothing here is
// reachable from any other goroutine.
type coordinator struct {
	p       *Pipeline
	cfg     Config
	watcher *watch.Watcher
	chain   []stage.Factory

	registry *source.Registry
	jobs     *source.Jobs
	agents   *source.Agents

	actors  map[model.Key]*actor
	located map[model.Key]model.Location // D5's cache rule: a single match, re-confirmed by one stat

	subs     []*subscriber
	latest   model.Fleet
	have     bool
	queued   []model.Problem // problems that ride out on the next Delivery
	agentsCh chan agentsDone
	waiters  []chan reply
	stats    Stats
	onDemand bool
	// listCancel ends the in-flight agents List, nil when none is in flight.
	// The List runs under this context, never a caller's (review r1 #6a).
	listCancel context.CancelFunc
}

type subscriber struct {
	sub     *Subscription
	dropped int // since the last delivery
}

type agentsDone struct {
	listing ports.Listing
	err     error
}

// cycleTrigger says why a cycle runs; only a self-wake cycle never self-wakes.
type cycleTrigger int

const (
	triggerOther cycleTrigger = iota
	triggerSelfWake
)

func newCoordinator(p *Pipeline, cfg Config, w *watch.Watcher) *coordinator {
	chain := make([]stage.Factory, 0, len(cfg.Chain)+3)
	if cfg.Locator != nil {
		chain = append(chain, paneGateFactory())
	}
	chain = append(chain, claudecode.Factory(claudecode.Config{
		Projects: cfg.Root.Projects, TailBytes: cfg.TailBytes, TimingTailBytes: cfg.TimingTailBytes, Now: cfg.Now,
	}))
	chain = append(chain, cfg.Chain...)
	vcfg := verdict.Config{Activity: cfg.Activity, Shell: cfg.Shell, Now: cfg.Now, Rules: cfg.Verdict}
	chain = append(chain, func(model.Key) stage.Stage { return verdict.New(vcfg) }) // last, always
	c := &coordinator{
		p: p, cfg: cfg, watcher: w, chain: chain,
		registry: source.NewRegistry(cfg.Root.Sessions, cfg.Clock, cfg.RetryDelay),
		jobs:     source.NewJobs(cfg.Root.Jobs),
		agents:   source.NewAgents(cfg.Agents),
		actors:   map[model.Key]*actor{},
		located:  map[model.Key]model.Location{},
		agentsCh: make(chan agentsDone, 1),
	}
	if w != nil {
		c.stats.Budget = w.Budget()
	}
	return c
}

func (c *coordinator) loop() {
	defer close(c.p.done)
	defer c.shutdown()
	var events <-chan watch.Event
	if c.watcher != nil {
		events = c.watcher.Events()
	}
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if c.absorb(ev) {
				c.cycle(triggerOther)
			}
		case <-c.p.refresh:
			c.cycle(triggerOther)
		case <-c.p.wake:
			c.cycle(triggerOther)
		case <-c.p.selfWake:
			c.cycle(triggerSelfWake)
		case d := <-c.agentsCh:
			c.agentsFinished(d)
		case m := <-c.p.in:
			if m.kind == cmdStop {
				return
			}
			c.command(m)
		}
	}
}

// absorb folds one watch event (and any already waiting) into the stats and
// the problem queue. It reports whether a cycle is due: an event naming only
// .key siblings changes no record.
func (c *coordinator) absorb(ev watch.Event) bool {
	due := false
	for {
		if c.fold(ev) {
			due = true
		}
		select {
		case more, ok := <-c.watcher.Events():
			if !ok {
				return due
			}
			ev = more
		default:
			return due
		}
	}
}

// fold folds ONE event into the stats and the problem queue and reports
// whether it alone makes a cycle due. A loss is due on ANY event of a drain,
// not only on the last: the lost Event's paths are unknown, so only a full
// cycle can cover them, and an event naming only .key siblings must not hide
// it (review r1 #1).
func (c *coordinator) fold(ev watch.Event) bool {
	c.stats.WatchEvents++
	c.stats.WatchDropped += ev.Dropped
	due := ev.Dropped > 0
	if ev.Err != nil {
		c.queued = append(c.queued, model.Problem{Scope: model.ScopeWatch, Kind: model.ProblemWatchArm,
			Detail: "the watcher reported: " + ev.Err.Error()})
		due = true
	}
	for _, p := range ev.Paths {
		if c.wakes(p) {
			due = true
		}
	}
	return due
}

// wakes reports whether a changed path can change the document.
//
//   - A .key sibling changes no record.
//   - A direct child of Root.Base that is neither the sessions nor the jobs
//     directory changes nothing either. Base is armed (M12) only so that
//     creating jobs/ fires; the rest of the Claude root (history.jsonl,
//     settings.json, …) is written on every prompt, and each write would
//     otherwise be a full cycle over every source. Deeper paths are not
//     filtered: an armed transcript lives under projects/ and must wake.
func (c *coordinator) wakes(p string) bool {
	if strings.HasSuffix(p, claude.KeyFileSuffix) {
		return false
	}
	r := c.cfg.Root
	if r.Base == "" {
		return true
	}
	p = filepath.Clean(p)
	if filepath.Dir(p) != filepath.Clean(r.Base) {
		return true
	}
	return (r.Sessions.Dir != "" && p == filepath.Clean(r.Sessions.Dir)) ||
		(r.Jobs.Dir != "" && p == filepath.Clean(r.Jobs.Dir))
}

func (c *coordinator) command(m command) {
	switch m.kind {
	case cmdSubscribe:
		buf := m.buffer
		if buf == 0 {
			buf = c.cfg.SubscriberBuffer
		}
		sub := &Subscription{name: m.name, c: make(chan Delivery, buf)}
		c.subs = append(c.subs, &subscriber{sub: sub})
		m.reply <- reply{sub: sub}
	case cmdUnsubscribe:
		for i, s := range c.subs {
			if s.sub == m.sub {
				c.subs = slices.Delete(c.subs, i, i+1)
				close(s.sub.c)
				break
			}
		}
		m.reply <- reply{}
	case cmdStats:
		s := c.stats
		s.Subscribers = len(c.subs)
		s.Watching = c.watcher != nil && !c.onDemand
		m.reply <- reply{stats: s}
	case cmdExec:
		ctx := context.WithValue(m.ctx, reentry{}, c.p)
		m.reply <- reply{err: m.fn(ctx, ops{c: c})}
	case cmdRefreshSources:
		c.refreshSources(m)
	}
}

// refreshSources is the ONE path to the agents listing (FR23): never from a
// watch cycle. The List runs on its own goroutine under a context the
// coordinator owns, so one caller giving up neither cancels it nor counts as
// the source failing for everyone (each caller's ctx bounds only its own
// wait); shutdown cancels it and waits for it. A second request joins the
// first; the gates are timestamps on Config.Now.
func (c *coordinator) refreshSources(m command) {
	wantAgents := false
	for _, id := range m.ids {
		if id == model.SourceAgents {
			wantAgents = true
		}
	}
	if !wantAgents {
		c.p.Refresh()
		m.reply <- reply{}
		return
	}
	if !c.agents.On() {
		m.reply <- reply{err: ErrSourceOff}
		return
	}
	start, joined, skipped := c.agents.Begin(c.cfg.Now())
	switch {
	case skipped != "":
		m.reply <- reply{outcome: RefreshOutcome{Skipped: skipped}}
	case joined:
		c.waiters = append(c.waiters, m.reply)
	case start:
		c.waiters = append(c.waiters, m.reply)
		ctx, cancel := context.WithCancel(context.Background())
		c.listCancel = cancel
		port, out := c.agents.Port(), c.agentsCh
		go func() {
			l, err := port.List(ctx)
			out <- agentsDone{listing: l, err: err}
		}()
	}
}

func (c *coordinator) agentsFinished(d agentsDone) {
	c.endList()
	l, problems, err := c.agents.Finish(c.cfg.Now(), d.listing, d.err)
	c.queued = append(c.queued, problems...)
	var out RefreshOutcome
	if err != nil {
		out.Err = err
	} else {
		out.Ran, out.At, out.Entries = true, l.At, len(l.Entries)
	}
	c.cycle(triggerOther)
	for _, w := range c.waiters {
		w <- reply{outcome: out}
	}
	c.waiters = nil
}

// cycleState is one cycle's working set. It lives for one call of cycle.
type cycleState struct {
	problems []model.Problem
	arm      watch.ArmReport
	spine    []model.WatchTarget
	jobsAbs  bool
	jobDirs  []string
	jobArmed map[string]bool
	jobPull  map[string]bool
	notConv  bool
	reg      source.RegistryResult
	jobsRes  source.JobsResult
	jobsList source.Listing
	sessions []model.Session
	locator  ports.PaneLocator
	locs     map[model.Key]model.Location
	targets  map[model.Key][]model.WatchTarget
	over     map[string]bool
	failed   map[string]bool
	shed     map[string]bool
	// unarmed counts the targets this cycle asked to read under a watch that
	// were not held when the read was granted: their arm or re-arm failed.
	unarmed   int
	published []model.Session
	effects   []EffectItem
	doc       model.Fleet
}

// cycle is ONE cycle, in §3.1's order. The method names are G7's landmarks:
// every arm precedes the first verifyIdentity, which precedes readSources;
// releaseOnce is the only release, after publish; deliverEffects is last.
func (c *coordinator) cycle(trigger cycleTrigger) {
	cs := &cycleState{}
	c.armSpine(cs)
	c.discoverJobDirs(cs)
	c.armJobDirs(cs)
	c.verifyIdentity(cs)
	c.readSources(cs)
	c.joinSessions(cs)
	c.retireActors(cs)
	c.panesForCycle(cs)
	c.locateTranscripts(cs)
	c.prepareTargets(cs)
	c.armTargets(cs)
	c.verifyIdentity(cs)
	grants := c.grant(cs)
	c.observeAll(cs, grants)
	c.collect(cs)
	c.publish(cs)
	c.releaseOnce(cs)
	c.deliverEffects(cs, trigger)
}

func (c *coordinator) watching() bool { return c.watcher != nil && !c.onDemand }

// armSpine arms the registry directory, and the jobs directory if it exists;
// while it does not, the Claude root itself, so creating jobs/ fires an event
// (tt's M12).
func (c *coordinator) armSpine(cs *cycleState) {
	r := c.cfg.Root
	if !r.Sessions.IsZero() && r.Sessions.Dir != "" {
		cs.spine = append(cs.spine, model.WatchTarget{Path: r.Sessions.Dir, Dir: true, Kind: model.TargetSpine})
	}
	cs.jobsAbs = r.Jobs.IsZero()
	if !r.Jobs.IsZero() {
		if _, err := fs.Stat(r.Jobs.FS, "."); err != nil {
			cs.jobsAbs = true
		} else if r.Jobs.Dir != "" {
			cs.spine = append(cs.spine, model.WatchTarget{Path: r.Jobs.Dir, Dir: true, Kind: model.TargetSpine})
		}
	}
	if cs.jobsAbs && r.Base != "" {
		cs.spine = append(cs.spine, model.WatchTarget{Path: r.Base, Dir: true, Kind: model.TargetSpine})
	}
	if c.watcher == nil {
		return
	}
	rep := c.watcher.Arm(cs.spine)
	c.mergeArm(cs, rep)
	// The spine alone over the claim: on-demand, loudly (D15).
	c.onDemand = false
	for _, t := range rep.Over {
		if t.Kind == model.TargetSpine {
			c.onDemand = true
		}
	}
	if c.onDemand {
		cs.problems = append(cs.problems, model.Problem{Scope: model.ScopeWatch, Kind: model.ProblemOverBudget,
			Detail: "the spine alone exceeds the descriptor claim; every cycle is an unwatched pull"})
	}
}

// discoverJobDirs lists the jobs root. Discovery is separate from arming.
func (c *coordinator) discoverJobDirs(cs *cycleState) {
	cs.jobsList = c.jobs.List()
	cs.jobDirs = cs.jobsList.Dirs
	cs.jobArmed, cs.jobPull = map[string]bool{}, map[string]bool{}
}

// armJobDirs arms every listed job directory and re-lists, to a fixed point of
// at most maxJobArmRounds. Unwatched, the listing IS the read set.
func (c *coordinator) armJobDirs(cs *cycleState) {
	if !c.watching() || c.cfg.Root.Jobs.Dir == "" {
		return
	}
	for round := 0; round < maxJobArmRounds; round++ {
		var want []model.WatchTarget
		for _, d := range cs.jobDirs {
			if !cs.jobArmed[d] && !cs.jobPull[d] {
				want = append(want, model.WatchTarget{Path: filepath.Join(c.cfg.Root.Jobs.Dir, d), Dir: true, Kind: model.TargetJobDir})
			}
		}
		if len(want) == 0 {
			return // converged
		}
		rep := c.watcher.Arm(want)
		c.mergeArm(cs, rep)
		over, failed := pathSet(rep.Over), problemPaths(rep.Failed)
		for _, t := range want {
			d := filepath.Base(t.Path)
			switch {
			case over[t.Path]:
				cs.jobPull[d] = true
			case !failed[t.Path]:
				cs.jobArmed[d] = true
			}
		}
		relist := c.jobs.List()
		cs.jobsList, cs.jobDirs = relist, relist.Dirs
	}
	for _, d := range cs.jobDirs {
		if !cs.jobArmed[d] && !cs.jobPull[d] {
			cs.notConv = true
		}
	}
	if cs.notConv {
		cs.arm.Degraded = append(cs.arm.Degraded, watch.DegradedNotConverged)
		cs.problems = append(cs.problems, model.Problem{Scope: model.ScopeWatch, Kind: model.ProblemNotConverged,
			Detail: fmt.Sprintf("job directories kept appearing for %d rounds; the unarmed ones are read next cycle", maxJobArmRounds)})
	}
}

// verifyIdentity re-checks every held target's (dev, ino), re-arms a replaced
// one BEFORE it is read, and recounts the budget. An overrun is shed: still
// read this cycle (it was armed before the read), released at releaseOnce.
func (c *coordinator) verifyIdentity(cs *cycleState) {
	if c.watcher == nil {
		return
	}
	rep := c.watcher.Verify()
	c.mergeArm(cs, rep)
	if cs.shed == nil {
		cs.shed = map[string]bool{}
	}
	for _, t := range rep.Shed {
		cs.shed[t.Path] = true
	}
}

// readSources reads the registry, the permitted job records and the applied
// listing. Each file once. The agents source never spawns here (FR23).
func (c *coordinator) readSources(cs *cycleState) {
	cs.reg = c.registry.Read()
	unwatched := !c.watching()
	// Armed means HELD after the verify that just ran, not "armJobDirs armed
	// it": a directory replaced in between whose re-arm failed is not watched,
	// and its record waits for the next cycle (review r2 #4).
	permit := func(dir string) bool {
		switch {
		case unwatched || cs.jobPull[dir]:
			return true
		case !cs.jobArmed[dir]:
			return false
		case c.watcher.Holds(filepath.Join(c.cfg.Root.Jobs.Dir, dir)):
			return true
		}
		cs.unarmed++
		return false
	}
	if c.cfg.Root.Jobs.Dir == "" {
		permit = func(string) bool { return true } // a tree that cannot be armed is read as a pull
	}
	cs.jobsRes = c.jobs.Read(cs.jobDirs, permit)
	cs.problems = append(cs.problems, cs.reg.Problems...)
	cs.problems = append(cs.problems, cs.jobsList.Problems...)
	cs.problems = append(cs.problems, cs.jobsRes.Problems...)
}

// joinSessions asks the oracle once per distinct pid, then joins (§3.4).
func (c *coordinator) joinSessions(cs *cycleState) {
	listing := c.agents.Listing()
	alive := map[int]model.Liveness{}
	probe := func(pid int) {
		if _, done := alive[pid]; pid <= 0 || done {
			return
		}
		if c.cfg.Oracle == nil {
			alive[pid] = model.LivenessUnknown
			return
		}
		alive[pid] = c.cfg.Oracle.Alive(pid)
	}
	for _, r := range cs.reg.Records {
		probe(r.PID)
	}
	if listing != nil {
		for _, e := range listing.Entries {
			if e.PID != nil && claude.IsInteractiveEntry(e) {
				probe(*e.PID)
			}
		}
	}
	sessions, problems := join.Build(join.Inputs{
		Records: cs.reg.Records, Jobs: cs.jobsRes.Jobs, Agents: listing,
		Alive: func(pid int) model.Liveness { return alive[pid] },
	})
	cs.sessions = sessions
	cs.problems = append(cs.problems, problems...)
}

// retireActors starts an actor for every new key and STOPS every actor whose
// key is absent from this cycle's join (C14): its loop exits, its timer is
// stopped, its undelivered effects are dropped.
func (c *coordinator) retireActors(cs *cycleState) {
	live := make(map[model.Key]bool, len(cs.sessions))
	for _, s := range cs.sessions {
		live[s.Key] = true
		if c.actors[s.Key] == nil {
			c.actors[s.Key] = startActor(s.Key, c.chain, c.cfg.Clock, c.wakeFunc())
			c.stats.ActorsStarted++
		}
	}
	for k, a := range c.actors {
		if !live[k] {
			a.stop()
			delete(c.actors, k)
			delete(c.located, k)
			c.stats.ActorsStopped++
		}
	}
}

// panesForCycle snapshots the pane table once. On error the cycle uses
// UnknownLocator, never the configured locator or the previous table.
func (c *coordinator) panesForCycle(cs *cycleState) {
	cs.locator = c.cfg.Locator
	if cl, ok := c.cfg.Locator.(ports.CycleLocator); ok {
		loc, err := cl.ForCycle(context.Background())
		if err != nil || loc == nil {
			loc = ports.UnknownLocator
		}
		cs.locator = loc
	}
}

// locateTranscripts computes each session's Location, by its speakers:
//   - a job speaker: the job's declared linkScanPath alone, under tt's lexical
//     containment rule, by one stat; never a glob;
//   - otherwise a session id tt would glob: claude.LocateTranscript's by-id
//     glob. D5's cache rule (the orchestrator's decision of 2026-10-21): a
//     single match is kept while one stat confirms it still exists, and the
//     glob runs only for a new, vanished or ambiguous id;
//   - otherwise nothing is looked up.
func (c *coordinator) locateTranscripts(cs *cycleState) {
	cs.locs = map[model.Key]model.Location{}
	projects := c.cfg.Root.Projects
	if projects.IsZero() {
		return
	}
	for _, s := range cs.sessions {
		key := s.Key
		switch {
		case s.Job != nil:
			delete(c.located, key)
			p := s.Job.LinkScanPath
			if p == "" {
				continue
			}
			rel, ok := claude.ProjectsRel(projects.Path, p)
			if !ok {
				cs.problems = append(cs.problems, model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemOutsideRoot,
					Key: &key, Path: p, Detail: "the job's linkScanPath is outside the projects tree; nothing was located"})
				continue
			}
			loc, err := claude.LocateTranscript(projects.FS, claude.LocateRequest{DeclaredPath: rel})
			if err != nil {
				cs.problems = append(cs.problems, model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemUnreadable,
					Key: &key, Path: rel, Detail: err.Error()})
				continue
			}
			cs.locs[key] = loc
		case claude.ValidSessionID(key.SessionID):
			if prev, ok := c.located[key]; ok && prev.Matches == 1 && stillThere(projects.FS, prev.Path) {
				cs.locs[key] = prev
				continue
			}
			delete(c.located, key)
			loc, err := claude.LocateTranscript(projects.FS, claude.LocateRequest{SessionID: key.SessionID})
			c.stats.TranscriptGlobs++
			if err != nil {
				cs.problems = append(cs.problems, model.Problem{Scope: model.ScopeSource, Kind: model.ProblemUnreadable,
					Key: &key, Detail: "locating the transcript: " + err.Error()})
				continue
			}
			if loc.Matches >= 2 {
				cs.problems = append(cs.problems, model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemAmbiguousTranscript,
					Key: &key, Detail: fmt.Sprintf("%d project directories hold this session's transcript; none is read", loc.Matches)})
			}
			if loc.Matches == 1 {
				c.located[key] = loc
			}
			cs.locs[key] = loc
		}
	}
}

func stillThere(fsys fs.FS, rel string) bool {
	info, err := fs.Stat(fsys, rel)
	return err == nil && info.Mode().IsRegular()
}

// prepareTargets asks every actor which files its stages will read.
func (c *coordinator) prepareTargets(cs *cycleState) {
	cs.targets = map[model.Key][]model.WatchTarget{}
	replies := map[model.Key]chan actorReply{}
	for _, s := range cs.sessions {
		a := c.actors[s.Key]
		r := make(chan actorReply, 1)
		if a.send(actorMsg{kind: msgPrepare, session: s, loc: cs.locs[s.Key], reply: r}) {
			replies[s.Key] = r
		}
	}
	for k, r := range replies {
		if v, ok := c.actors[k].await(r); ok {
			cs.targets[k] = v.targets
		}
	}
}

// armTargets arms the union of the targets, within the budget. Refused by the
// budget is Pull; failed to arm is a ScopeWatch Problem and not granted.
func (c *coordinator) armTargets(cs *cycleState) {
	cs.over, cs.failed = map[string]bool{}, map[string]bool{}
	if !c.watching() {
		return
	}
	var all []model.WatchTarget
	for _, s := range cs.sessions {
		all = append(all, cs.targets[s.Key]...)
	}
	if len(all) == 0 {
		return
	}
	rep := c.watcher.Arm(all)
	c.mergeArm(cs, rep)
	cs.over, cs.failed = pathSet(rep.Over), problemPaths(rep.Failed)
	if len(rep.Over) > 0 {
		cs.problems = append(cs.problems, model.Problem{Scope: model.ScopeWatch, Kind: model.ProblemOverBudget,
			Detail: fmt.Sprintf("%d transcripts are over the descriptor budget and are read unwatched this cycle", len(rep.Over))})
	}
}

// grant hands each actor what it may read. Armed is what the watcher HOLDS
// after the second verifyIdentity, not what armTargets reported: a target
// replaced in between whose re-arm failed is no longer covered, and granting
// it would read a file no watch covers (review r2 #4). Either way an
// unarmed target is not read, and the Watch row says so.
func (c *coordinator) grant(cs *cycleState) map[model.Key]stage.Grant {
	out := map[model.Key]stage.Grant{}
	for _, s := range cs.sessions {
		g := stage.Grant{Unwatched: !c.watching()}
		for _, t := range cs.targets[s.Key] {
			switch {
			case g.Unwatched:
			case cs.over[t.Path]:
				g.Pull = append(g.Pull, t)
			case !cs.failed[t.Path] && c.watcher.Holds(t.Path):
				g.Armed = append(g.Armed, t)
			default:
				cs.unarmed++
			}
		}
		out[s.Key] = g
	}
	return out
}

// observeAll runs every chain, each on its own actor goroutine.
func (c *coordinator) observeAll(cs *cycleState, grants map[model.Key]stage.Grant) {
	for _, s := range cs.sessions {
		if !c.actors[s.Key].send(actorMsg{kind: msgObserve, session: s, grant: grants[s.Key], locator: cs.locator}) {
			c.stats.ObservationsDropped++
		}
	}
}

// collect asks each actor for its frozen session and effects, and builds the
// document (freeze point 2).
func (c *coordinator) collect(cs *cycleState) {
	replies := make([]chan actorReply, len(cs.sessions))
	for i, s := range cs.sessions {
		replies[i] = make(chan actorReply, 1)
		c.actors[s.Key].send(actorMsg{kind: msgCollect, reply: replies[i]})
	}
	for i, s := range cs.sessions {
		v, ok := c.actors[s.Key].await(replies[i])
		published := v.session
		if !ok || published.Key != s.Key {
			published = s
		}
		cs.published = append(cs.published, published)
		cs.problems = append(cs.problems, v.problems...)
		for _, e := range v.effects {
			cs.effects = append(cs.effects, EffectItem{Key: s.Key, Effect: e, Published: published})
		}
	}
	doc := model.Fleet{Epoch: c.cfg.Now(), Sessions: cs.published, Sources: c.sources(cs)}
	doc.Sort()
	cs.doc = doc.Clone()
	for i := range cs.effects {
		for _, s := range cs.doc.Sessions {
			if s.Key == cs.effects[i].Key {
				cs.effects[i].Published = s.Clone()
			}
		}
	}
	c.stats.Cycles++
}

// sources is §3.1's table: always four, in order.
func (c *coordinator) sources(cs *cycleState) []model.SourceReport {
	now := c.cfg.Now()
	at := &now
	reg := model.SourceReport{ID: model.SourceRegistry, State: cs.reg.State, Detail: cs.reg.Detail, At: at}
	jobs := model.SourceReport{ID: model.SourceJobs, State: cs.jobsList.State, Detail: cs.jobsList.Detail}
	if jobs.State == model.SourceOK && cs.jobsRes.State == model.SourceDegraded {
		jobs.State = model.SourceDegraded
	}
	if jobs.State == model.SourceOK && cs.notConv {
		jobs.State, jobs.Detail = model.SourceDegraded, "not converged"
	}
	if jobs.State != model.SourceOff {
		t := now
		jobs.At = &t
	}
	w := model.SourceReport{ID: model.SourceWatch}
	switch {
	case c.watcher == nil:
		w.State = model.SourceOff
	case c.onDemand || len(cs.arm.Over) > 0 || len(cs.arm.Shed) > 0 || cs.notConv || cs.unarmed > 0:
		var parts []string
		if c.onDemand || len(cs.arm.Over) > 0 || len(cs.arm.Shed) > 0 || cs.notConv {
			parts = append(parts, degradedDetail(cs.arm, c.onDemand))
		}
		if cs.unarmed > 0 {
			parts = append(parts, fmt.Sprintf("%d targets could not be armed and were not read", cs.unarmed))
		}
		w.State, w.Detail = model.SourceDegraded, strings.Join(parts, ", ")
	default:
		w.State = model.SourceOK
	}
	if w.State == model.SourceOK && slices.Contains(cs.arm.Degraded, watch.DegradedUnavailable) {
		w.State, w.Detail = model.SourceFailed, "the watcher has stopped"
		cs.problems = append(cs.problems, model.Problem{Scope: model.ScopeWatch, Kind: model.ProblemWatchArm, Detail: w.Detail})
	}
	reports := []model.SourceReport{reg, jobs, c.agents.Report(), w}
	for _, r := range reports {
		if (r.State == model.SourceDegraded || r.State == model.SourceFailed) && !c.hasProblemFor(cs, r.ID) {
			cs.problems = append(cs.problems, model.Problem{Scope: scopeOf(r.ID), Kind: kindOf(r.ID),
				Detail: fmt.Sprintf("the %s source is %s: %s", r.ID, r.State, r.Detail)})
		}
	}
	return reports
}

// hasProblemFor reports whether this Delivery already explains a degraded or
// failed source: every such row comes with at least one Problem (§3.1).
func (c *coordinator) hasProblemFor(cs *cycleState, id model.SourceID) bool {
	for _, p := range append(slices.Clone(c.queued), cs.problems...) {
		switch id {
		case model.SourceWatch:
			if p.Scope == model.ScopeWatch {
				return true
			}
		case model.SourceAgents:
			if p.Kind == model.ProblemAgentsListing {
				return true
			}
		default:
			if p.Scope == model.ScopeSource || p.Scope == model.ScopeRecord {
				return true
			}
		}
	}
	return false
}

func scopeOf(id model.SourceID) model.Scope {
	if id == model.SourceWatch {
		return model.ScopeWatch
	}
	return model.ScopeSource
}

func kindOf(id model.SourceID) model.ProblemKind {
	switch id {
	case model.SourceAgents:
		return model.ProblemAgentsListing
	case model.SourceWatch:
		return model.ProblemOverBudget
	default:
		return model.ProblemUnreadable
	}
}

func degradedDetail(rep watch.ArmReport, onDemand bool) string {
	var parts []string
	if onDemand {
		parts = append(parts, "on-demand")
	}
	for _, d := range rep.Degraded {
		parts = append(parts, d.String())
	}
	if len(parts) == 0 {
		parts = append(parts, "over budget")
	}
	slices.Sort(parts)
	return strings.Join(slices.Compact(parts), ", ")
}

// publish hands the document to every subscriber (freeze point 3: a copy
// each, Problems and Changes included). The hub never blocks: a full buffer
// drops its oldest delivery and the subscriber is resynced with this one.
func (c *coordinator) publish(cs *cycleState) {
	problems := append(c.queued, cs.problems...)
	c.queued = nil
	var changes []join.Change
	if c.have {
		changes = join.Diff(c.latest, cs.doc)
	} else {
		changes = join.Diff(model.Fleet{}, cs.doc)
	}
	c.latest, c.have = cs.doc, true
	for _, s := range c.subs {
		d := Delivery{Doc: cs.doc.Clone(), Problems: cloneProblems(problems), Changes: cloneChanges(changes), Dropped: s.dropped}
		d.Resync = s.dropped > 0
		select {
		case s.sub.c <- d:
			s.dropped = 0
			continue
		default:
		}
		select { // full: drop the oldest, deliver this one as a resync
		case old := <-s.sub.c:
			// The dropped delivery's own losses are this one's too.
			s.dropped += old.Dropped + 1
			c.stats.HubDropped++
		default:
		}
		d.Dropped, d.Resync = s.dropped, true
		select {
		case s.sub.c <- d:
			s.dropped = 0
			c.stats.Resyncs++
		default:
			s.dropped++
			c.stats.HubDropped++
		}
	}
	c.stats.Published++
}

// cloneProblems is freeze point 3 for Problems: the slice AND what each
// Problem points at (Key, PIDs), so one sink's edit cannot reach another's.
func cloneProblems(ps []model.Problem) []model.Problem {
	if ps == nil {
		return nil
	}
	out := make([]model.Problem, len(ps))
	for i, p := range ps {
		if p.Key != nil {
			k := *p.Key
			p.Key = &k
		}
		p.PIDs = slices.Clone(p.PIDs)
		out[i] = p
	}
	return out
}

// cloneChanges is freeze point 3 for Changes: the slice and each Fields.
func cloneChanges(cs []join.Change) []join.Change {
	if cs == nil {
		return nil
	}
	out := make([]join.Change, len(cs))
	for i, c := range cs {
		c.Fields = slices.Clone(c.Fields)
		out[i] = c
	}
	return out
}

// releaseOnce is the single releasing call. It arms nothing.
//
//	keep = (spine ∪ armed job dirs ∪ Armed targets of published sessions) ∖ shed
func (c *coordinator) releaseOnce(cs *cycleState) {
	if c.watcher == nil {
		return
	}
	var keep []model.WatchTarget
	for _, t := range cs.spine {
		keep = append(keep, t)
	}
	for d := range cs.jobArmed {
		keep = append(keep, model.WatchTarget{Path: filepath.Join(c.cfg.Root.Jobs.Dir, d), Dir: true, Kind: model.TargetJobDir})
	}
	for _, s := range cs.sessions {
		for _, t := range cs.targets[s.Key] {
			if !cs.over[t.Path] && !cs.failed[t.Path] && c.watching() {
				keep = append(keep, t)
			}
		}
	}
	keep = slices.DeleteFunc(keep, func(t model.WatchTarget) bool { return cs.shed[t.Path] && t.Kind != model.TargetSpine })
	c.mergeArm(cs, c.watcher.Release(keep))
	c.stats.LastArm = cs.arm
}

// deliverEffects hands the cycle's effects to the host, each with its session
// as published. Its problems ride out on the next Delivery, which this
// coordinator triggers itself, once: a self-wake cycle never self-wakes.
func (c *coordinator) deliverEffects(cs *cycleState, trigger cycleTrigger) {
	if c.cfg.OnEffects == nil || len(cs.effects) == 0 {
		return
	}
	ctx := context.WithValue(context.Background(), reentry{}, c.p)
	problems := c.cfg.OnEffects(ctx, EffectBatch{Epoch: cs.doc.Epoch, Items: cs.effects})
	if len(problems) == 0 {
		return
	}
	c.queued = append(c.queued, problems...)
	if trigger != triggerSelfWake {
		select {
		case c.p.selfWake <- struct{}{}:
		default:
		}
	}
}

// mergeArm folds one watcher report into the cycle's merged report and its
// problems.
func (c *coordinator) mergeArm(cs *cycleState, rep watch.ArmReport) {
	cs.arm.Armed += rep.Armed
	cs.arm.Held = rep.Held
	cs.arm.Released += rep.Released
	cs.arm.Cost = rep.Cost
	cs.arm.Over = append(cs.arm.Over, rep.Over...)
	cs.arm.Shed = append(cs.arm.Shed, rep.Shed...)
	cs.arm.Failed = append(cs.arm.Failed, rep.Failed...)
	cs.arm.Degraded = append(cs.arm.Degraded, rep.Degraded...)
	cs.problems = append(cs.problems, rep.Failed...)
}

func (c *coordinator) wakeFunc() func() {
	wake := c.p.wake
	return func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// endList releases the finished List's context.
func (c *coordinator) endList() {
	if c.listCancel != nil {
		c.listCancel()
		c.listCancel = nil
	}
}

func (c *coordinator) shutdown() {
	// An in-flight List is the pipeline's goroutine: end it and wait for it,
	// because Stop waits for every goroutine it owns. ports.AgentsListing
	// obliges List to return once its context is done.
	if c.listCancel != nil {
		c.endList()
		<-c.agentsCh
	}
	if c.watcher != nil {
		c.watcher.Stop()
	}
	for k, a := range c.actors {
		a.stop()
		delete(c.actors, k)
		c.stats.ActorsStopped++
	}
	for _, s := range c.subs {
		close(s.sub.c)
	}
	c.subs = nil
	for _, w := range c.waiters {
		w <- reply{err: ErrStopped}
	}
}

func pathSet(ts []model.WatchTarget) map[string]bool {
	m := map[string]bool{}
	for _, t := range ts {
		m[t.Path] = true
	}
	return m
}

func problemPaths(ps []model.Problem) map[string]bool {
	m := map[string]bool{}
	for _, p := range ps {
		m[p.Path] = true
	}
	return m
}

// ops is the coordinator seen from inside Exec. It is only ever called on the
// coordinator goroutine, during fn.
type ops struct{ c *coordinator }

func (o ops) Latest() (model.Fleet, bool) {
	if !o.c.have {
		return model.Fleet{}, false
	}
	return o.c.latest.Clone(), true
}

func (o ops) Session(k model.Key) (model.Session, bool) {
	for _, s := range o.c.latest.Sessions {
		if s.Key == k {
			return s.Clone(), true
		}
	}
	return model.Session{}, false
}

func (o ops) Call(ctx context.Context, k model.Key, fn func(model.Session, func(string) (stage.Stage, bool)) error) error {
	a := o.c.actors[k]
	if a == nil {
		return ErrNoSuchKey
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return callOn(ctx, a, fn)
}
