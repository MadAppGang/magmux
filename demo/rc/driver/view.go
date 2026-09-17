package main

// The drawing half: a size budget computed top-down, panels rendered into fixed
// boxes, and joins — never string concatenation, never len() for a width.
//
// The visual contract this screen is held to: a bounded value is a GRADIENT
// METER, a time series is a SPARKLINE, a discrete state is a BADGE, and prose
// lives only inside the response and log panels. If a panel here reads as more
// than half sentences, it is under-drawn.

import (
	"fmt"
	"image/color"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/barchart"
)

func lg(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

// showLog is a responsive decision, not a preference: the driver's own pane in
// the demo is fourteen rows, and a log panel there would leave four rows for
// the actions and the response together. Below the threshold the response panel
// takes the space and keeps the whole exchange.
func (m model) showLog() bool { return m.h-3 >= 14 }

// budget is the whole layout in one value, computed once from the terminal size
// and used by both SetSize and View. Two copies of this arithmetic drift the
// first time a panel is added, and the symptom is a frame one row too tall,
// which scrolls the header off every render.
type budget struct {
	actW, respW int
	topH, logH  int
	actListH    int // rows of the actions panel
	panesH      int // 0 when there is no room for a pane table
	opsH        int // 0 when there is no room for the op-class chart
}

func (m model) budget() budget {
	b := budget{}
	bodyH := m.h - headerHeight - 1
	if m.showLog() {
		// A FIFTH, not a third: at 80×24 a third of the body is seven rows of
		// log against four lines of it, and it costs the actions panel the three
		// rows that decide whether all thirteen are on screen at once.
		b.logH = clamp(bodyH/5, 4, 8)
	}
	b.topH = bodyH - b.logH
	b.actW = clamp(m.w*34/100, 26, 38)
	b.respW = m.w - b.actW

	// The left column fills TOP DOWN with as much as each panel has content
	// for, and spare rows go to the next panel rather than to empty space. A
	// panel padded out to a fixed height reads as data that stopped arriving.
	// +4 rather than +3: the panel costs a border, a padding column and a title
	// row, and the list itself keeps a row back for the filter input that `/`
	// opens. Without that row the thirteenth action is below the fold on a
	// screen with room for it, which is the kind of off-by-one only a
	// screenshot finds.
	b.actListH = b.topH
	spare := b.topH - (len(Actions) + 4)
	if spare >= 5 {
		// Panes: what actions 5, 6 and 7 are about, and the one panel that
		// changes without anybody pressing a key.
		b.actListH = len(Actions) + 4
		b.panesH = clamp(len(m.panes)+3, 5, min(spare, 12))
		spare -= b.panesH
		// Ops by class: the taxonomy that DECIDES the two refusals. A view
		// token reaches class read and nothing above it, so a bar chart of the
		// op table by class is the 403 in actions 8 and 9 drawn in advance.
		if spare >= 8 {
			b.opsH = min(spare, 9)
			spare -= b.opsH
		}
		// Whatever is still over goes back to the panes table, which is the
		// only one of the three that can usefully grow.
		b.panesH += spare
	}
	return b
}

func (m *model) layout() {
	if m.w < 20 || m.h < 8 {
		return
	}
	b := m.budget()
	// Every panel is a rounded border (1 cell a side) plus one column of
	// padding, so the content box is 4 narrower and 2 shorter than the panel,
	// and the title inside it costs one more row.
	m.actions.SetSize(b.actW-4, b.actListH-3)
	m.respvp.SetWidth(b.respW - 4)
	m.respvp.SetHeight(b.topH - 3)
	m.logvp.SetWidth(m.w - 4)
	m.logvp.SetHeight(max(1, b.logH-3))
	m.help.SetWidth(m.w)
	m.confirm.SetWidth(16)
	m.refreshResponse()
	m.logvp.SetContent(joinLines(m.logs))
	m.logvp.GotoBottom()
}

const headerHeight = 2

func (m model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	if m.w < 40 || m.h < 8 {
		v.Content = lg(m.th.Warn).Render("magmux rc driver needs 40×8")
		return v
	}
	b := m.budget()

	leftCol := m.th.Panel(m.actionsTitle(), m.actions.View(), b.actW, b.actListH, m.focus == focusActions)
	if b.panesH > 0 {
		leftCol = lipgloss.JoinVertical(lipgloss.Left, leftCol,
			m.th.Panel(m.panesTitle(), m.panesView(b.actW-4, b.panesH-3), b.actW, b.panesH, false))
	}
	if b.opsH > 0 {
		leftCol = lipgloss.JoinVertical(lipgloss.Left, leftCol,
			m.th.Panel(m.opsTitle(), m.opsView(b.actW-4, b.opsH-3), b.actW, b.opsH, false))
	}
	right := m.th.Panel(m.responseTitle(), m.respvp.View(), b.respW, b.topH, m.focus == focusResponse)
	rows := []string{m.header(), lipgloss.JoinHorizontal(lipgloss.Top, leftCol, right)}
	if b.logH > 0 {
		rows = append(rows, m.th.Panel(m.logTitle(), m.logvp.View(), m.w, b.logH, m.focus == focusLog))
	}
	rows = append(rows, m.footer())
	v.Content = lipgloss.JoinVertical(lipgloss.Left, rows...)
	return v
}

// panesView is magmux's own pane table, as badges rather than as a list of
// words: the id, a state badge, and a marker on the one this driver's telemetry
// is watching. It is the only panel that changes without a keypress, which is
// what makes it worth the rows.
func (m model) panesView(w, h int) string {
	th := m.th
	if len(m.panes) == 0 {
		return lg(th.Faint).Render("(no answer yet)")
	}
	var out []string
	for _, p := range m.panes {
		if len(out) >= h {
			break
		}
		hue := th.Info
		switch p.State {
		case "running":
			hue = th.OK
		case "panel":
			hue = th.Faint
		case "completed", "dead":
			hue = th.Dim
		case "failed":
			hue = th.Err
		}
		mark := " "
		if p.ID == m.target {
			mark = "◀"
		}
		label := p.Label
		if label == "" {
			label = p.Cmd
		}
		out = append(out, Clip(fmt.Sprintf("%s %s %s %s",
			lg(th.Accent).Render(mark),
			lg(th.Text).Bold(true).Render(fmt.Sprintf("%2d", p.ID)),
			th.Badge(p.State, hue),
			lg(th.Faint).Render(label)), w))
	}
	return joinLines(out)
}

// opClassOrder is magmux's own class ladder, least privileged first. A view
// token reaches `read` and stops.
var opClassOrder = []string{"read", "input", "control", "admin"}

// opsView is the op table as a BAR CHART BY CLASS, and it is the credential
// story drawn before anybody presses a key: the amber bar is what a read-only
// token can call, every violet bar is what only the session token can, and
// actions 8 and 9 are two specific violet ops being refused. It uses ntcharts
// because this is a genuine category distribution rather than a meter.
func (m model) opsView(w, h int) string {
	th := m.th
	if len(m.ops.Ops) == 0 {
		return lg(th.Faint).Render("(no answer yet)")
	}
	counts := map[string]int{}
	plugins := 0
	for _, o := range m.ops.Ops {
		counts[o.Class]++
		if o.Source != "" && o.Source != "magmux" {
			plugins++
		}
	}
	readable := lipgloss.NewStyle().Foreground(th.View)
	privileged := lipgloss.NewStyle().Foreground(th.Full)

	var bars []barchart.BarData
	for _, c := range opClassOrder {
		n := counts[c]
		if n == 0 {
			continue
		}
		style := privileged
		if c == "read" {
			style = readable
		}
		bars = append(bars, barchart.BarData{
			Label:  c,
			Values: []barchart.BarValue{{Name: c, Value: float64(n), Style: style}},
		})
	}
	if len(bars) == 0 {
		return lg(th.Faint).Render("(no classes)")
	}
	// The plugin badge is a row of the panel's budget, not a bonus on top of it:
	// the chart is drawn one row shorter when there is going to be one. Adding
	// it afterwards made the panel a row taller than it was sized for, and the
	// footer fell off the bottom of the screen the moment a plugin registered.
	chartH := max(3, h)
	if plugins > 0 {
		chartH = max(3, h-1)
	}
	bc := barchart.New(w, chartH)
	bc.PushAll(bars)
	bc.Draw()
	out := bc.View()
	if plugins > 0 {
		out = lipgloss.JoinVertical(lipgloss.Left, out,
			th.Badge(fmt.Sprintf("+%d plugin", plugins), th.Accent))
	}
	return out
}

func (m model) opsTitle() string {
	if len(m.ops.Ops) == 0 {
		return "ops by class"
	}
	return "ops by class " +
		lg(m.th.Faint).Render(fmt.Sprintf("%d · rev %d", len(m.ops.Ops), m.ops.Rev))
}

func (m model) panesTitle() string {
	return "panes " + lg(m.th.Faint).Render(fmt.Sprint(len(m.panes)))
}

// ── the header: two rows, and every number on them is measured ──────────────

func (m model) header() string {
	th := m.th
	dim := lg(th.Dim)

	// Row 1 — what the link is doing, and what it has carried.
	stateBadge := th.Badge(m.link.String(), linkColour(th, m.link))
	geom := "—"
	if m.pcols > 0 {
		geom = fmt.Sprintf("%d×%d", m.pcols, m.prows)
	}
	target := "—"
	if m.target >= 0 {
		target = fmt.Sprint(m.target)
	}
	l1 := []string{
		lg(th.Accent).Bold(true).Render("magmux rc"),
		stateBadge,
		dim.Render("pane ") + lg(th.Text).Bold(true).Render(target),
		dim.Render(geom),
		dim.Render("seq ") + lg(th.Text).Render(fmt.Sprint(m.seq)),
	}
	if m.alt {
		l1 = append(l1, th.Badge("ALT", th.Info))
	}
	if m.scrolled {
		l1 = append(l1, th.Badge("SCROLLED", th.Warn))
	}
	if m.running != nil {
		l1 = append(l1, lg(th.Accent).Render(m.spin.View()+" "+m.running.Key))
	}
	leftRow1 := strings.Join(l1, " ")

	// The right of row 1 is the two live instruments: the round-trip meter of
	// the last probe, and the frame sparkline. Both are shrunk before anything
	// is dropped, because a short meter still carries its value.
	budget := m.w - lipgloss.Width(leftRow1) - 2
	rightRow1 := m.instruments(budget)

	// Row 2 — THE CREDENTIALS. This is the demo's whole point, so it gets a
	// permanent row of its own and its two badges are never abbreviated away.
	full := th.Badge("FULL SESSION TOKEN", th.Full)
	view := th.Badge("READ-ONLY VIEW TOKEN", th.View)
	leftRow2 := dim.Render("▸ this driver ") + full
	rightRow2 := dim.Render("▸ every mirror ") + view
	if m.w >= 96 {
		leftRow2 += " " + lg(th.Faint).Render(Fingerprint(m.cfg.Token))
		rightRow2 += " " + lg(th.Faint).Render(Fingerprint(m.cfg.ViewToken))
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		spread(leftRow1, rightRow1, m.w),
		spread(leftRow2, rightRow2, m.w),
	)
}

// instruments draws the round-trip meter and the frame sparkline into whatever
// width is left, in that order of importance, and draws neither rather than
// drawing one badly.
func (m model) instruments(budget int) string {
	th := m.th
	dim := lg(th.Dim)
	var parts []string

	// Round trip of the last liveness probe: a bounded value, so a gradient
	// meter.
	if budget >= 22 && m.rtt > 0 {
		w := clamp(budget/4, 6, 12)
		parts = append(parts, dim.Render("rtt ")+
			th.Meter(latencyFrac(m.rtt), w, th.OK, th.Err)+" "+
			lg(th.Text).Render(FmtDur(m.rtt)))
		budget -= w + 11
	}
	// Frame arrivals per second over the last half minute. This is the
	// instrument that visibly reacts when action 2 bursts, and the reason the
	// header is worth having at all. Capped at 30 cells: wider is not more
	// information, it is a rule across the screen.
	if budget >= 18 {
		w := clamp(budget-12, 10, 24)
		series := m.frames[len(m.frames)-w:]
		maxv := 1.0
		for _, v := range series {
			if v > maxv {
				maxv = v
			}
		}
		parts = append(parts, dim.Render("frames ")+
			th.Spark(series, maxv, th.Info, th.Accent)+
			lg(th.Text).Render(fmt.Sprintf(" %d", m.frameCount)))
	}
	return strings.Join(parts, "  ")
}

// ── panel titles, which carry state rather than repeating the obvious ───────

func (m model) actionsTitle() string {
	t := "actions " + lg(m.th.Faint).Render(fmt.Sprint(len(Actions)))
	// A half-typed number is SHOWN. Otherwise pressing `1` on the way to `13`
	// looks like a key that did nothing.
	if m.digits != "" && time.Since(m.digitsAt) < digitWindow {
		t += " " + m.th.Badge(m.digits+"…", m.th.Accent)
	}
	return t
}

func (m model) responseTitle() string {
	th := m.th
	t := "request / response"
	if m.lastStatus > 0 {
		t += " " + th.Badge(fmt.Sprint(m.lastStatus), statusColour(th, m.lastStatus))
		if m.lastDur > 0 {
			// The last op's latency, as a meter: the same bounded-value rule as
			// the probe above, on the same scale, so the two are comparable.
			t += " " + th.Meter(latencyFrac(m.lastDur), 8, th.OK, th.Err) +
				lg(th.Faint).Render(" "+FmtDur(m.lastDur))
		}
	}
	return t
}

func (m model) logTitle() string {
	return "log " + lg(m.th.Faint).Render(fmt.Sprintf("%d actions · %s up",
		len(m.logs), time.Since(m.startedAt).Truncate(time.Second)))
}

// ── the footer ──────────────────────────────────────────────────────────────

func (m model) footer() string {
	th := m.th
	if m.pending != nil {
		return lipgloss.NewStyle().Width(m.w).Render(
			th.Badge("CONFIRM", th.Err) + " " +
				lg(th.Text).Render("type ") + lg(th.Err).Bold(true).Render(m.pending.Confirm) +
				lg(th.Text).Render(" and press enter — this ENDS THE RUN  ") +
				lg(th.Accent).Render(m.confirm.View()) + "  " +
				lg(th.Dim).Render("esc cancels"))
	}
	if m.link == LinkDead && m.linkReason != "" {
		return Clip(th.Badge("DEAD", th.Err)+" "+lg(th.Err).Render(m.linkReason), m.w)
	}
	m.help.Styles.ShortKey = lg(th.Accent)
	m.help.Styles.ShortDesc = lg(th.Dim)
	m.help.Styles.ShortSeparator = lg(th.Faint)
	// WHERE TO LOOK, permanently, on the right of the key bar. Every action says
	// it too, but an effect nobody notices is the same as no effect and the
	// whole claim of this demo is that two independent clients follow — so the
	// place to look cannot only exist in the response to something already
	// pressed. It is `cfg.Mirrors()` rather than a fixed phrase because the
	// mirror pane is optional: under RC_DEMO_MIRROR=0 the browser tab is the
	// only mirror, and telling somebody to watch a pane that was never created
	// is worse than telling them nothing.
	hint := m.help.View(m.keys)
	right := lg(th.Info).Render("↳ watch ") + lg(th.Dim).Render(m.cfg.MirrorsShort())
	if lipgloss.Width(hint)+lipgloss.Width(right)+2 > m.w {
		right = lg(th.Faint).Render(m.cfg.QuitHint)
	}
	return spread(hint, right, m.w)
}

// ── the response panel's content ────────────────────────────────────────────

func (m *model) refreshResponse() {
	w := m.respvp.Width()
	if w < 4 {
		w = 40
	}
	th := m.th
	var out []string
	for _, s := range m.steps {
		switch s.Role {
		case RoleRequest:
			cred := th.Tag("FULL", th.Full)
			if s.Cred == CredView {
				cred = th.Tag("VIEW", th.View)
			}
			out = append(out, lg(th.Accent).Bold(true).Render("▶ ")+
				lipgloss.NewStyle().Foreground(th.Text).Width(w-2).Render(s.Text))
			out = append(out, lg(th.Faint).Render("  · as the ")+cred+lg(th.Faint).Render(" token"))
		case RoleResponse:
			arrow := lg(th.OK).Bold(true).Render("◀ ")
			badge := th.Badge(fmt.Sprint(s.Status), statusColour(th, s.Status))
			if s.Status == 0 {
				arrow = lg(th.Err).Bold(true).Render("◀ ")
				badge = th.Badge("NO REPLY", th.Err)
			}
			if s.Status/100 != 2 {
				arrow = lg(th.Err).Bold(true).Render("◀ ")
			}
			out = append(out, arrow+badge+" "+
				th.Meter(latencyFrac(s.Dur), 6, th.OK, th.Err)+
				lg(th.Faint).Render(" "+FmtDur(s.Dur)))
			out = append(out, lipgloss.NewStyle().Foreground(th.Dim).Width(w).Render(s.Text))
		case RoleLook:
			out = append(out, lg(th.Info).Render("↳ ")+
				lipgloss.NewStyle().Foreground(th.Info).Width(w-2).Render(s.Text))
		case RoleHeading:
			out = append(out, lipgloss.NewStyle().Foreground(th.Text).Bold(true).Width(w).Render(s.Text))
		case RoleVerdict:
			// A REFUSAL THAT WAS THE POINT IS A SUCCESS. Actions 8 and 9 make
			// magmux return 403, and the 403 badge above stays red because that
			// is what magmux said — but the verdict badge beside it is GREEN,
			// because the boundary holding is the result the action set out to
			// prove. A red screen there would teach the opposite of the lesson.
			bg, glyph := th.OK, "✓ "
			if !s.OK {
				bg, glyph = th.Err, "✗ "
			}
			out = append(out, th.Badge(glyph+s.Badge, bg))
			out = append(out, lipgloss.NewStyle().Foreground(th.Text).Width(w).Render(s.Text))
		default:
			out = append(out, lipgloss.NewStyle().Foreground(th.Faint).Width(w).Render(s.Text))
		}
	}
	if len(out) == 0 {
		out = m.idleCard(w)
	}
	m.respvp.SetContent(joinLines(out))
	// The transcript follows the newest exchange; the idle card is read from the
	// top, because it is a legend and its first line is the instruction.
	if len(m.steps) == 0 {
		m.respvp.GotoTop()
	} else {
		m.respvp.GotoBottom()
	}
}

// idleCard is what the response panel shows before anything has been run. It is
// a LEGEND, not a welcome message: the left column's credential bar is the one
// piece of this screen that has to be taught once, and the transports it lists
// are what magmux actually answered on `/v1/capabilities` rather than a boast.
func (m model) idleCard(w int) []string {
	th := m.th
	var out []string
	// FIRST, because it is the one line that must be on screen at every size
	// this driver runs at — the demo's own pane is fourteen rows, where there
	// is no log panel to carry it. A screen full of actions that all fail
	// identically tells nobody which of the two tokens, the URL or the process
	// is at fault; this line says the session token was accepted before a
	// single action is offered.
	if m.caps.Protocol > 0 {
		out = append(out, Clip(strings.Join([]string{
			th.Badge("READY", th.OK),
			lg(th.Dim).Render(fmt.Sprintf("capabilities ok · protocol %d · readOnly=false · %d verbs",
				m.caps.Protocol, len(m.caps.Verbs))),
		}, " "), w))
		badges := []string{}
		for _, t := range sortedNames(m.caps.TransportNames()) {
			badges = append(badges, th.Badge(t, th.Info))
		}
		out = append(out, Clip(lg(th.Faint).Render("transports ")+strings.Join(badges, " "), w))
		out = append(out, "")
	}
	out = append(out, lg(th.Dim).Render("pick an action and press enter — every one is a"))
	out = append(out, lg(th.Dim).Render("real HTTP request. Nothing here is simulated."))
	out = append(out, "")
	out = append(out, lg(th.Dim).Render("the bar down the left of each action is the credential it spends:"))
	out = append(out, lg(th.Full).Render("▌")+lg(th.Text).Render(" the FULL session token — it can type"))
	out = append(out, lg(th.View).Render("▌")+lg(th.Text).Render(" the READ-ONLY view token — it cannot"))
	out = append(out, "")
	out = append(out, lg(th.Faint).Render("8 and 9 spend the read-only one on purpose, and magmux"))
	out = append(out, lg(th.Faint).Render("refuses them in its own words."))
	return out
}

// ── the action list's delegate ──────────────────────────────────────────────

// actionDelegate draws one action per row. The leftmost cell is a CREDENTIAL
// BAR — violet for the full token, amber for the read-only one — so which
// actions spend which credential is readable without reading a word, and the
// two refusals stand out in the column before anything has been pressed.
type actionDelegate struct{ th *Theme }

func (d actionDelegate) Height() int                         { return 1 }
func (d actionDelegate) Spacing() int                        { return 0 }
func (d actionDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d actionDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(actionItem)
	if !ok {
		return
	}
	th := d.th
	hue := th.Full
	if it.a.Cred == CredView {
		hue = th.View
	}
	sel := index == m.Index()
	keyStyle := lg(th.Dim)
	textStyle := lg(th.Dim)
	cursor := " "
	if sel {
		keyStyle = lg(th.Accent).Bold(true)
		textStyle = lg(th.Text).Bold(true)
		cursor = "▸"
	}
	line := lg(hue).Render("▌") + lg(th.Accent).Render(cursor) +
		keyStyle.Render(fmt.Sprintf("%2s ", it.a.Key)) +
		textStyle.Render(it.a.What)
	fmt.Fprint(w, Clip(line, m.Width()))
}

// ── small shared helpers ────────────────────────────────────────────────────

// spread puts `right` hard against the right edge of `w`, dropping it entirely
// rather than letting it wrap: a header that wraps costs a body row, and the
// body rows are what the demo is made of.
func spread(left, right string, w int) string {
	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if rw == 0 || lw+rw+1 > w {
		return Clip(left, w)
	}
	return left + strings.Repeat(" ", w-lw-rw) + right
}

func statusColour(th Theme, status int) color.Color {
	switch {
	case status == 0:
		return th.Err
	case status/100 == 2:
		return th.OK
	case status == 403 || status == 401:
		return th.Err
	case status == 502 || status == 504:
		return th.Warn
	case status/100 == 4:
		return th.Warn
	default:
		return th.Err
	}
}

func linkColour(th Theme, s LinkState) color.Color {
	switch s {
	case LinkLive:
		return th.OK
	case LinkStale:
		return th.Warn
	case LinkDead:
		return th.Err
	default:
		return th.Info
	}
}

// FmtDur prints a round trip at a resolution that can actually be read. A
// loopback op answers in a few hundred MICROSECONDS, so whole milliseconds
// print "0ms" for every honest measurement this demo makes — which reads as
// "not measured" rather than as "fast", and leaves the latency meter flat for
// ever.
func FmtDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Millisecond:
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}

// latencyFrac maps a round trip onto the meter, LOGARITHMICALLY, because the
// interesting range spans three decades: 0.3ms on loopback, 30ms across a
// Tailscale link, 3s when something is wrong. A linear scale with a ceiling
// high enough for the last is empty for the first two.
//
//	0.1ms → empty      3ms → a third      100ms → two thirds      3s → full
func latencyFrac(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	const lo, hi = 0.1, 3000.0 // milliseconds
	ms := float64(d.Microseconds()) / 1000
	f := (math.Log10(ms) - math.Log10(lo)) / (math.Log10(hi) - math.Log10(lo))
	return clampF(f, 0, 1)
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func joinLines(ss []string) string { return strings.Join(ss, "\n") }

func sortedNames(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func pad(s string, w int) string {
	if lipgloss.Width(s) >= w {
		return Clip(s, w)
	}
	return s + strings.Repeat(" ", w-lipgloss.Width(s))
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
