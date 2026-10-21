package pipeline

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
)

// Every test here builds its own Claude trees (an fstest.MapFS, the corpus
// read through os.DirFS, or a t.TempDir()), injects a fake oracle, and never
// reads the real Claude root, probes a pid or speaks to tmux.

var base = time.Date(2026, 9, 18, 2, 40, 11, 204e6, time.UTC)

const wait = 10 * time.Second // a bounded wait: fsnotify and goroutines are real

// oracle answers from a map; an absent pid is alive.
type oracle map[int]model.Liveness

func (o oracle) Alive(pid int) model.Liveness {
	if l, ok := o[pid]; ok {
		return l
	}
	return model.LivenessYes
}

func mapTree(path string, fsys fs.FS) ports.Tree { return ports.Tree{Path: path, FS: fsys} }

// rec is a registry record's bytes, built from claude's constants.
func rec(pid int, id, status string, extra ...string) []byte {
	parts := []string{
		fmt.Sprintf("%q:%d", claude.KeyPID, pid),
		fmt.Sprintf("%q:%q", claude.KeyStatus, status),
		fmt.Sprintf("%q:%d", claude.KeyStatusUpdatedAt, base.Add(-time.Minute).UnixMilli()),
	}
	if id != "" {
		parts = append(parts, fmt.Sprintf("%q:%q", claude.KeySessionID, id))
	}
	parts = append(parts, extra...)
	return []byte("{" + strings.Join(parts, ",") + "}")
}

func kv(k, v string) string { return fmt.Sprintf("%q:%q", k, v) }

func jobRec(id, short, state, tempo, link string) []byte {
	parts := []string{kv(claude.KeyState, state), kv(claude.KeyTempo, tempo), kv(claude.KeyDaemonShort, short),
		kv(claude.KeyUpdatedAt, base.Add(-time.Minute).Format(time.RFC3339Nano)), kv(claude.KeyLinkScanPath, link)}
	if id != "" {
		parts = append(parts, kv(claude.KeySessionID, id))
	}
	return []byte("{" + strings.Join(parts, ",") + "}")
}

func transcriptLines(colour string) []byte {
	return []byte(`{"` + claude.KeyRecordType + `":"` + claude.ValueRecordUser + `","` + claude.KeyTimestamp + `":"2026-09-18T02:00:00Z"}` + "\n" +
		`{"` + claude.KeyRecordType + `":"` + claude.ValueRecordAgentColor + `","` + claude.KeyAgentColor + `":"` + colour + `"}` + "\n")
}

type harness struct {
	t   *testing.T
	p   *Pipeline
	sub *Subscription
	clk *ports.FakeClock
}

// start builds a pipeline with Watch off, a fake clock fixed at base, and one
// subscription.
func start(t *testing.T, cfg Config) *harness {
	t.Helper()
	clk := ports.NewFakeClock(base)
	if cfg.Clock == nil {
		cfg.Clock = clk
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return base }
	}
	p, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Stop)
	sub, err := p.Subscribe(context.Background(), "test", 16)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, p: p, sub: sub, clk: clk}
}

// next waits for one Delivery.
func (h *harness) next() Delivery {
	h.t.Helper()
	select {
	case d, ok := <-h.sub.C():
		if !ok {
			h.t.Fatal("the subscription closed")
		}
		return d
	case <-time.After(wait):
		h.t.Fatal("no delivery")
	}
	return Delivery{}
}

// cycle asks for one cycle and returns its delivery.
func (h *harness) cycle() Delivery {
	h.t.Helper()
	if !h.p.Refresh() {
		h.t.Fatal("Refresh refused")
	}
	return h.next()
}

// quiet asserts no delivery arrives within d.
func (h *harness) quiet(d time.Duration) {
	h.t.Helper()
	select {
	case got := <-h.sub.C():
		h.t.Fatalf("an unexpected delivery: %d sessions, %d problems %v", len(got.Doc.Sessions), len(got.Problems), got.Problems)
	case <-time.After(d):
	}
}

func (h *harness) stats() Stats {
	h.t.Helper()
	s, err := h.p.Stats(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func byKey(d Delivery, k model.Key) (model.Session, bool) {
	for _, s := range d.Doc.Sessions {
		if s.Key == k {
			return s, true
		}
	}
	return model.Session{}, false
}

func hasProblem(ps []model.Problem, kind model.ProblemKind) bool {
	for _, p := range ps {
		if p.Kind == kind {
			return true
		}
	}
	return false
}

func sourceRow(d Delivery, id model.SourceID) model.SourceReport {
	for _, s := range d.Doc.Sources {
		if s.ID == id {
			return s
		}
	}
	return model.SourceReport{}
}

// corpusRoot is a scenario root read through os.DirFS under the canonical
// /testdata paths: readable, not watchable.
func corpusRoot(scenario string) ports.Root {
	tree := func(name string) ports.Tree {
		return ports.Tree{Path: "/testdata/" + name, FS: os.DirFS(filepath.Join("..", "testdata", "roots", scenario, name))}
	}
	return ports.Root{Sessions: tree("sessions"), Jobs: tree("jobs"), Projects: tree("projects")}
}

// countingFS counts content opens, by name. Opens happen on actor goroutines,
// so they travel on a channel and are tallied on the test goroutine: no lock.
type countingFS struct {
	fs.FS
	ch    chan string
	tally map[string]int
}

func newCounting(fsys fs.FS) *countingFS {
	return &countingFS{FS: fsys, ch: make(chan string, 4096), tally: map[string]int{}}
}

func (c *countingFS) Open(name string) (fs.File, error) {
	c.ch <- name
	return c.FS.Open(name)
}

// opens drains the channel and returns the tally. Test goroutine only, after a
// synchronising receive.
func (c *countingFS) opens() map[string]int {
	for {
		select {
		case n := <-c.ch:
			c.tally[n]++
		default:
			return c.tally
		}
	}
}
func (c *countingFS) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(c.FS, name) }
func (c *countingFS) Stat(name string) (fs.FileInfo, error)      { return fs.Stat(c.FS, name) }

// effect is a host effect type, asserted back in OnEffects.
type effect struct{ n int }

func (effect) EffectName() string { return "test" }

// hostStage is a configurable host stage.
type hostStage struct {
	name    string
	observe func(model.Session) model.Session
	effects []stage.Effect
	emit    bool
	n       int
}

func (h *hostStage) Name() string { return h.name }
func (h *hostStage) Observe(s model.Session) model.Session {
	h.n++
	if h.emit {
		h.effects = append(h.effects, effect{n: h.n})
	}
	if h.observe != nil {
		return h.observe(s)
	}
	return s
}
func (h *hostStage) Effects() []stage.Effect {
	e := h.effects
	h.effects = nil
	return e
}

var _ fstest.MapFS
