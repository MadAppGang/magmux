package main

// The TUI: state, messages, and the update loop. `view.go` draws it.
//
// Elm rules, kept strictly, because the failure mode of a dashboard is a screen
// that disagrees with itself: the model owns every fact, View is a pure
// function of it, and ALL I/O happens in commands. An action takes seconds
// (action 11 holds a stalled subscriber while it types five times), so it runs
// on its own goroutine and reports through a channel; nothing blocks Update.

import (
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// How much history the frame sparkline keeps. One bucket is one second, so this
// is the last minute of the pane's life — long enough that action 2's burst is
// still visible while you read the explanation of it.
const sparkBuckets = 60

// How long a half-typed action number stays half-typed. Long enough to press
// `1` then `3` deliberately, short enough that two separate choices a second
// apart are two choices.
const digitWindow = 900 * time.Millisecond

// ── messages ────────────────────────────────────────────────────────────────

type (
	tickMsg  time.Time
	panesMsg struct{ panes []Pane }
	opsMsg   struct{ table OpTable }
	capsMsg  struct {
		caps Capabilities
		err  error
	}
	actionDoneMsg struct {
		key string
		err error
	}
)

// chanSink hands an action's evidence to the UI goroutine. Buffered deeply
// enough that a chatty action never waits on rendering, and BLOCKING rather
// than dropping when it is full: a demo that silently loses the response it
// just received is worse than one that renders a beat late.
type chanSink struct{ ch chan any }

func (s chanSink) Emit(l Line) { s.ch <- l }

// ── the model ───────────────────────────────────────────────────────────────

type keymap struct {
	Pick, Up, Down, Run, Focus, Filter, Next, Prev, Refresh, Quit key.Binding
}

func (k keymap) ShortHelp() []key.Binding {
	// FIVE, not every binding: the key bar shares its row with "where to look",
	// and that hint is the one thing on this screen a first-time reader needs.
	// `/` and `r` stay bound and stay in FullHelp.
	return []key.Binding{k.Pick, k.Up, k.Run, k.Focus, k.Next, k.Quit}
}
func (k keymap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Pick, k.Up, k.Down, k.Run}, {k.Focus, k.Filter, k.Refresh}, {k.Next, k.Prev, k.Quit}}
}

var keys = keymap{
	Pick:    key.NewBinding(key.WithKeys("0", "1", "2", "3", "4", "5", "6", "7", "8", "9"), key.WithHelp("0-9", "pick")),
	Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑↓", "move")),
	Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓", "down")),
	Run:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "run")),
	Focus:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus")),
	Filter:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Next:    key.NewBinding(key.WithKeys("]"), key.WithHelp("]/[", "retarget")),
	Prev:    key.NewBinding(key.WithKeys("["), key.WithHelp("[", "prev pane")),
	Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
}

type focusArea int

const (
	focusActions focusArea = iota
	focusResponse
	focusLog
	focusCount
)

type model struct {
	cfg  Config
	args Args
	th   Theme
	w, h int

	api  *API
	tel  *Telemetry
	msgs chan any

	actions list.Model
	respvp  viewport.Model
	logvp   viewport.Model
	spin    spinner.Model
	help    help.Model
	confirm textinput.Model
	keys    keymap
	focus   focusArea

	// evidence
	steps []Line
	logs  []string

	// telemetry, every field of it MEASURED
	link       LinkState
	linkReason string
	seq        uint64
	prows      int
	pcols      int
	alt        bool
	scrolled   bool
	frames     []float64
	bucketAt   time.Time
	frameCount int
	lastFrame  time.Time
	rtt        time.Duration
	lastDur    time.Duration
	lastStatus int

	// the run
	caps    Capabilities
	target  int
	panes   []Pane
	ops     OpTable
	opened  int
	stalled *StalledWS
	over    bool
	running *Action
	pending *Action // waiting for its confirm word
	// digits is the half-typed action number. Four of the thirteen actions have
	// two-digit keys, so a digit cannot run one on its own: it SELECTS, and
	// enter runs. The alternative — run on the first digit and hope — makes 10
	// through 13 unreachable by number, which is how every banner in this demo
	// tells a human to drive it.
	digits    string
	digitsAt  time.Time
	quitting  bool
	startedAt time.Time
}

// actionItem adapts an Action to the list's Item interface. FilterValue is the
// key AND the words, so `/8` and `/view` both find the refusals.
type actionItem struct{ a Action }

func (i actionItem) FilterValue() string { return i.a.Key + " " + i.a.What + " " + i.a.How }

func runTUI(cfg Config, args Args) error {
	// Diagnostics go to a FILE. A fmt.Println from inside a running TUI paints
	// over the frame with no way to repaint it.
	if f, err := tea.LogToFile(filepath.Join(cfg.State, "rc-driver.log"), "driver"); err == nil {
		defer f.Close() //nolint:errcheck
	} else {
		log.SetOutput(io.Discard)
	}

	th := resolveTheme(args.Theme)
	msgs := make(chan any, 1024)
	sink := chanSink{ch: msgs}

	items := make([]list.Item, 0, len(Actions))
	for _, a := range Actions {
		items = append(items, actionItem{a})
	}
	l := list.New(items, actionDelegate{th: &th}, 20, 10)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(true)
	l.SetShowFilter(true)
	l.FilterInput.Prompt = "/"

	sp := spinner.New(spinner.WithSpinner(spinner.Dot))

	ci := textinput.New()
	ci.Prompt = ""
	ci.CharLimit = 12

	m := model{
		cfg: cfg, args: args, th: th,
		api: NewAPI(cfg, sink), msgs: msgs,
		actions:   l,
		respvp:    viewport.New(),
		logvp:     viewport.New(),
		spin:      sp,
		help:      help.New(),
		confirm:   ci,
		keys:      keys,
		frames:    make([]float64, sparkBuckets),
		target:    args.Pane,
		opened:    -1,
		link:      LinkConnecting,
		bucketAt:  time.Now(),
		startedAt: time.Now(),
	}
	m.tel = NewTelemetry(cfg, msgs)

	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, waitFor(m.msgs), tickEvery(), m.loadCaps(), m.loadPanes(), m.loadOps())
}

// waitFor turns the one channel the goroutines write into a stream of messages.
// It is re-issued after every message, which is the documented Bubble Tea
// pattern for an external event source.
func waitFor(ch chan any) tea.Cmd {
	return func() tea.Msg {
		v, ok := <-ch
		if !ok {
			return nil
		}
		return v
	}
}

func tickEvery() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) loadCaps() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		c, err := api.Capabilities()
		return capsMsg{c, err}
	}
}

func (m model) loadPanes() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		p, err := api.Panes()
		if err != nil {
			return panesMsg{nil}
		}
		return panesMsg{p}
	}
}

// loadOps is re-issued after every action, not on a timer: the op table only
// changes when a plugin registers or dies, and action 10 is the only thing here
// that makes that happen.
func (m model) loadOps() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		t, err := api.OpsTable()
		if err != nil {
			return opsMsg{}
		}
		return opsMsg{t}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()

	case tea.BackgroundColorMsg:
		// The terminal answered. `auto` defers to it; an explicit --theme or
		// MAGMUX_THEME does not, because that is what a screenshot harness and
		// a magmux-opened pane use and neither can be asked.
		if m.args.Theme == "" || m.args.Theme == "auto" {
			m.th = NewTheme(msg.IsDark())
			m.actions.SetDelegate(actionDelegate{th: &m.th})
		}

	case tea.KeyPressMsg:
		var cmd tea.Cmd
		m, cmd = m.onKey(msg)
		cmds = append(cmds, cmd)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)

	case tickMsg:
		m.rollBuckets(time.Time(msg))
		if m.link == LinkLive && !m.lastFrame.IsZero() && time.Since(m.lastFrame) > staleAfter {
			m.link = LinkStale
		}
		cmds = append(cmds, tickEvery())

	case capsMsg:
		// The credential is proved BEFORE a menu of things that need it is
		// usable. A screen whose every action fails identically tells nobody
		// which of the two tokens, the URL or the process is the problem.
		switch {
		case msg.err != nil:
			m.pushLog(m.th.Badge("NO ANSWER", m.th.Err) + " " +
				lg(m.th.Err).Render("magmux refused the session token: "+msg.err.Error()))
		case msg.caps.ReadOnly:
			m.pushLog(m.th.Badge("WRONG TOKEN", m.th.Err) + " " +
				lg(m.th.Err).Render("the file at magmux-*.token reports readOnly=true; this driver needs the session token"))
		default:
			m.caps = msg.caps
			m.pushLog(m.th.Badge("READY", m.th.OK) + " " + lg(m.th.Dim).Render(fmt.Sprintf(
				"capabilities ok · protocol %d · readOnly=false · %d verbs · transports %s · %s",
				msg.caps.Protocol, len(msg.caps.Verbs),
				strings.Join(sortedNames(msg.caps.TransportNames()), " "), m.cfg.URL)))
			m.refreshResponse()
		}

	case opsMsg:
		if len(msg.table.Ops) > 0 {
			m.ops = msg.table
		}

	case panesMsg:
		m.panes = msg.panes
		if m.target < 0 {
			m.target = Watched(msg.panes)
			if m.target >= 0 {
				go m.tel.Run(m.target)
			}
		}
		if m.opened >= 0 && !hasPane(msg.panes, m.opened) {
			m.opened = -1
		}

	case Line:
		m.steps = append(m.steps, msg)
		if msg.Role == RoleResponse {
			m.lastStatus, m.lastDur = msg.Status, msg.Dur
		}
		m.refreshResponse()
		cmds = append(cmds, waitFor(m.msgs))

	case linkMsg:
		m.link, m.linkReason = msg.State, msg.Reason
		if msg.State == LinkDead && msg.Reason != "" {
			m.pushLog(m.th.Badge("DEAD", m.th.Err) + " " + msg.Reason)
		}
		cmds = append(cmds, waitFor(m.msgs))

	case frameMsg:
		m.seq, m.prows, m.pcols = msg.Seq, msg.Rows, msg.Cols
		m.alt, m.scrolled = msg.Alt, msg.Scrolled
		m.lastFrame = msg.At
		m.frameCount++
		m.frames[len(m.frames)-1]++
		if m.link != LinkDead {
			m.link = LinkLive
		}
		cmds = append(cmds, waitFor(m.msgs))

	case probeMsg:
		m.rtt = msg.RTT
		cmds = append(cmds, waitFor(m.msgs))

	case actionDoneMsg:
		m.finishAction(msg)
		cmds = append(cmds, m.loadPanes(), m.loadOps())
		if m.over {
			m.quitting = true
			cmds = append(cmds, tea.Quit)
		}
	}

	// The focused panel gets the navigation keys; the others are static.
	switch m.focus {
	case focusActions:
		var cmd tea.Cmd
		m.actions, cmd = m.actions.Update(msg)
		cmds = append(cmds, cmd)
	case focusResponse:
		var cmd tea.Cmd
		m.respvp, cmd = m.respvp.Update(msg)
		cmds = append(cmds, cmd)
	case focusLog:
		var cmd tea.Cmd
		m.logvp, cmd = m.logvp.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m model) onKey(msg tea.KeyPressMsg) (model, tea.Cmd) {
	// The confirm box owns the keyboard while it is up. Action 12 ends the run,
	// so it is the one thing here that must not be reachable by a stray enter.
	if m.pending != nil {
		switch msg.String() {
		case "esc":
			m.steps = append(m.steps, verdict(false, "CANCELLED", "nothing was killed"))
			m.refreshResponse()
			m.pending = nil
			return m, nil
		case "enter":
			act := m.pending
			ok := m.confirm.Value() == act.Confirm
			m.pending = nil
			m.confirm.SetValue("")
			m.confirm.Blur()
			if !ok {
				m.steps = append(m.steps, verdict(false, "CANCELLED", "nothing was killed"))
				m.refreshResponse()
				return m, nil
			}
			return m.start(*act, true)
		default:
			var cmd tea.Cmd
			m.confirm, cmd = m.confirm.Update(msg)
			return m, cmd
		}
	}
	// While the list is filtering, every printable key belongs to the filter.
	if m.actions.SettingFilter() {
		var cmd tea.Cmd
		m.actions, cmd = m.actions.Update(msg)
		return m, cmd
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		m.quitting = true
		return m, tea.Quit
	case key.Matches(msg, m.keys.Focus):
		m.focus = (m.focus + 1) % focusCount
		if m.focus == focusLog && !m.showLog() {
			m.focus = focusActions
		}
		return m, nil
	case key.Matches(msg, m.keys.Refresh):
		return m, tea.Batch(m.loadPanes(), m.loadOps())
	case key.Matches(msg, m.keys.Next):
		return m, m.retarget(+1)
	case key.Matches(msg, m.keys.Prev):
		return m, m.retarget(-1)
	case key.Matches(msg, m.keys.Run) && m.focus == focusActions:
		if m.running != nil {
			return m, nil
		}
		it, ok := m.actions.SelectedItem().(actionItem)
		if !ok {
			return m, nil
		}
		if it.a.Confirm != "" {
			m.pending = &Actions[indexOf(it.a.Key)]
			m.confirm.SetValue("")
			return m, m.confirm.Focus()
		}
		return m.start(it.a, false)
	}

	// A bare digit SELECTS, and enter runs — because the driver this replaced
	// was a numbered menu and the number is what every banner in this demo tells
	// a human to press, and because four of the thirteen numbers are two digits
	// long. A digit that ran its action immediately would make 10 through 13
	// unreachable by number at all.
	if k := msg.String(); len(k) == 1 && k[0] >= '0' && k[0] <= '9' {
		if time.Since(m.digitsAt) > digitWindow || len(m.digits) >= 2 {
			m.digits = ""
		}
		m.digits += k
		m.digitsAt = time.Now()
		if a := ActionByKey(m.digits); a != nil {
			m.actions.Select(indexOf(m.digits))
		} else if a := ActionByKey(k); a != nil {
			m.digits = k
			m.actions.Select(indexOf(k))
		}
	}
	return m, nil
}

// start dispatches an action onto its own goroutine. Every fact it needs is
// copied in; the model is not touched from over there.
func (m model) start(a Action, confirmed bool) (model, tea.Cmd) {
	m.running = &a
	m.steps = []Line{heading(a.Key + " · " + a.What), note(a.How)}
	m.refreshResponse()

	target := m.target
	if m.args.Pane >= 0 {
		target = m.args.Pane
	}
	ctx := &Ctx{
		cfg: m.cfg, api: m.api, sink: chanSink{ch: m.msgs}, Target: target,
		Opened: &m.opened, Stalled: &m.stalled, Over: &m.over,
		Confirmed: confirmed, Telemetry: m.tel,
	}
	ch := m.msgs
	return m, tea.Batch(m.spin.Tick, func() tea.Msg {
		err := a.Run(ctx)
		ch <- actionDoneMsg{key: a.Key, err: err}
		return nil
	}, waitFor(ch))
}

func (m *model) finishAction(msg actionDoneMsg) {
	a := m.running
	m.running = nil
	if a == nil {
		return
	}
	status := ""
	if m.lastStatus > 0 {
		status = m.th.Badge(fmt.Sprint(m.lastStatus), statusColour(m.th, m.lastStatus))
	}
	outcome := m.th.Badge("DONE", m.th.Info)
	for _, s := range m.steps {
		if s.Role == RoleVerdict {
			if s.OK {
				outcome = m.th.Badge("✓ "+s.Badge, m.th.OK)
			} else {
				outcome = m.th.Badge("✗ "+s.Badge, m.th.Err)
			}
		}
	}
	if msg.err != nil {
		outcome = m.th.Badge("✗ ERROR", m.th.Err)
		m.steps = append(m.steps, verdict(false, "ERROR", msg.err.Error()))
		m.refreshResponse()
	}
	dim := lg(m.th.Dim)
	m.pushLog(fmt.Sprintf("%s %s %s %s %s",
		dim.Render(time.Now().Format("15:04:05")),
		m.th.Tag(fmt.Sprintf("%2s", a.Key), m.th.Accent),
		dim.Render(pad(a.What, 30)),
		status, outcome))
}

// retarget moves the DRIVER's own watch, which is `unwatch` then `watch` on its
// own connection. There is no op that points somebody else's stream somewhere
// new, and inventing one for a demo would contradict the API the demo exists to
// show — so this moves only this client, exactly as the mirrors' own pickers
// move only themselves.
func (m *model) retarget(delta int) tea.Cmd {
	var live []int
	for _, p := range m.panes {
		if p.State != "panel" {
			live = append(live, p.ID)
		}
	}
	if len(live) < 2 {
		return nil
	}
	at := 0
	for i, id := range live {
		if id == m.target {
			at = i
		}
	}
	next := live[((at+delta)%len(live)+len(live))%len(live)]
	if next == m.target {
		return nil
	}
	m.target = next
	m.seq, m.lastFrame = 0, time.Time{}
	m.tel.Retarget(next)
	return nil
}

// rollBuckets advances the sparkline's one-second buckets. It is the only place
// the series moves, so a burst cannot be counted twice or lost between ticks.
func (m *model) rollBuckets(now time.Time) {
	for now.Sub(m.bucketAt) >= time.Second {
		m.bucketAt = m.bucketAt.Add(time.Second)
		copy(m.frames, m.frames[1:])
		m.frames[len(m.frames)-1] = 0
	}
}

func (m *model) pushLog(s string) {
	m.logs = append(m.logs, s)
	if len(m.logs) > 500 {
		m.logs = m.logs[len(m.logs)-500:]
	}
	m.logvp.SetContent(joinLines(m.logs))
	m.logvp.GotoBottom()
}

func hasPane(panes []Pane, id int) bool {
	for _, p := range panes {
		if p.ID == id {
			return true
		}
	}
	return false
}

func indexOf(key string) int {
	for i := range Actions {
		if Actions[i].Key == key {
			return i
		}
	}
	return 0
}
