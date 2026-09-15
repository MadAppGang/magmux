package mux

// Where magmux and the plugin host meet.
//
// This file is to `plugin` what remote.go is to `transport/httpapi`: the ONE
// place the two packages touch, and the direction is strictly one way. The host
// is handed three callbacks — take a snapshot, a plugin died, where to log —
// and never hears of a Pane, a Screen or treeMu. That is what lets the whole
// host be tested against a bare hub with no terminal, and what makes
// TestImportDirection mean something.

import (
	"fmt"
	"os"

	"github.com/MadAppGang/magmux/plugin"
	"github.com/MadAppGang/magmux/protocol"
)

// plugins returns this magmux's plugin host, building it on first use.
//
// Lazy for the same reason the hub is: every unit test in this package builds a
// Magmux as a struct literal and never calls init(), and a socket connection
// reaches for the host on every line. A host with no spawned processes is
// nearly free and still serves a plugin an operator started by hand.
func (m *Magmux) plugins() *plugin.Host {
	m.pluginOnce.Do(func() {
		if m.pluginHost == nil {
			m.pluginHost = plugin.New(plugin.Config{
				Hub: m.bus(),
				// The session token, so a plugin a developer runs by hand can
				// register with the credential they already have. Empty without
				// --listen, and then only plugins magmux spawned can register.
				Token: m.remoteToken,
				// The same filtered environment a pane gets: a plugin inherits
				// the developer's PATH and toolchain and none of the session's
				// secrets. The host appends the three MAGMUX_PLUGIN_* values.
				Env:      paneEnviron(),
				LogDir:   m.socketDir(),
				ID:       m.remoteID(),
				Debug:    debugWriter{},
				Snapshot: m.applyPluginSnapshot,
				OnExit:   m.pluginGone,
			})
		}
	})
	return m.pluginHost
}

// applyPluginSnapshot routes one `controller.snapshot` to the pane it describes.
//
// It is the OWNERSHIP check, and it is here rather than in the host because
// only this side knows what a pane is. A plugin may report on a pane whose
// controller is its own and on no other — the panel's provenance model rests on
// `◀ IN` rows being magmux's own observation of a session, and a plugin that
// could describe a pane it never opened would be putting words in another
// session's mouth.
//
// Every refusal names which of the three things was wrong, because they send a
// plugin author to three different places: the pane is gone, the pane has no
// plugin controller at all, or it has one belonging to somebody else.
func (m *Magmux) applyPluginSnapshot(pluginName string, snap protocol.ControllerSnapshot) error {
	if snap.Pane == nil {
		return sockErrf(sockCodeBadRequest, "controller.snapshot needs a pane")
	}
	idx := *snap.Pane
	p := m.paneByID(idx)
	if p == nil {
		return sockErrf(sockCodeNoSuchPane, "no pane %d (it may have been closed)", idx)
	}
	// p.controller is written by attachController / OpenPane while the pane is
	// still private and never written again, so this is the same unlocked read
	// pollControllers and buildPaneResults already make.
	pc, ok := p.controller.(*pluginController)
	if !ok {
		return sockErrf(sockCodeForbidden,
			"pane %d is not observed by a plugin, so plugin %q cannot report on it; open the pane "+
				`with {"controller":"self"} to claim it`, idx, pluginName)
	}
	if pc.plugin != pluginName {
		return sockErrf(sockCodeForbidden,
			"pane %d is observed by plugin %q, not by %q", idx, pc.plugin, pluginName)
	}
	return pc.push(snap)
}

// pluginGone degrades every pane a dead plugin was observing.
//
// The controller is NOT detached, because p.controller is write-once: a pane
// that lost its observer keeps it, and the controller keeps merging the
// terminal's own idle detection (mergeTerminalIdle). So the pane goes on
// reporting state — from the screen rather than from the tool — instead of
// freezing on whatever the plugin said last, which is the difference between a
// degraded observation and a lie.
func (m *Magmux) pluginGone(pluginName string) {
	for _, p := range m.livePanes() {
		pc, ok := p.controller.(*pluginController)
		if !ok || pc.plugin != pluginName {
			continue
		}
		pc.markGone()
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[ctrl] pane %d lost its observer (%s); terminal-only from here\n",
				p.id, pc.Name())
		}
	}
}

// startPlugins spawns every --plugin command.
//
// It runs AFTER the socket is bound and MAGMUX_SOCK is published, because the
// first thing a plugin does is dial it — and BEFORE the layout is announced, so
// a plugin that opens its own panes at startup is not racing a client's
// connect-time aggregate.
//
// A failure is a plain line on stderr and NOT fatal: a magmux that refused to
// start because one plugin's command was mistyped would take the session's real
// work down with it. The plugin is simply absent, its ops are not in the table,
// and a caller asking for one gets unknown_verb.
func (m *Magmux) startPlugins(cmds []string) {
	if len(cmds) == 0 {
		return
	}
	if err := m.plugins().Spawn(cmds, m.sockPath); err != nil {
		fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
	}
}

// stopPlugins ends every spawned plugin.
//
// It runs after waitSocketShutdown, and that order is the whole of a plugin's
// graceful exit: the hub's Finalize has already given every subscriber —
// plugins included — results, then shutdown, then EOF, so a well-behaved plugin
// has already decided to leave and the SIGTERM below is a formality.
func (m *Magmux) stopPlugins() { m.pluginHost.Shutdown() }
