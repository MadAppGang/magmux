package mux

// Where --firebase becomes a running mirror.
//
// This file is the second and last place mux and a transport package meet, and
// it follows remote.go's shape exactly: the direction is one way, mux registers
// itself INTO the adapter through two callbacks (the aggregate, the log), and
// transport/firebase never hears of a Pane, a Screen or a treeMu.
//
// The ORDER is a security order, like remote.go's. Config parsing, the
// host-predicate check on databaseURL, loading the service account and loading
// the HMAC key ALL happen before mux.init(), so a bad config is a readable line
// on a normal terminal and an exit 1 — not a message painted into an alternate
// screen nobody can scroll back to. Nothing in this file touches the network
// before Start, which runs after the layout exists.

import (
	"fmt"
	"os"
	"time"

	"github.com/MadAppGang/magmux/buildinfo"
	"github.com/MadAppGang/magmux/transport/firebase"
)

// firebaseConfigPath resolves the flag and the environment variable, in that
// order. --firebase wins over MAGMUX_FIREBASE, as every other flag does over
// its variable.
func firebaseConfigPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("MAGMUX_FIREBASE")
}

// prepareFirebase loads and validates the config and builds the adapter. It
// performs no I/O against the database.
func (m *Magmux) prepareFirebase(path string) (*firebase.Adapter, error) {
	cfg, err := firebase.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return firebase.New(cfg, firebase.Options{
		Hub: m.bus(),
		// The SAME bytes every other subscriber's first line is built from, so
		// the mirror's seed and a WebSocket client's aggregate can never
		// disagree about a pane.
		Aggregate: m.remoteAggregate,
		SID:       firebase.MakeSID(m.remoteID(), time.Now()),
		PID:       os.Getpid(),
		Version:   buildinfo.Version,
		// A function, not a value: this runs before init(), where the geometry
		// is resolved, so a size read here is always 0x0.
		Geometry: m.geometry,
		// dbgFile or nothing, resolved at WRITE time: a headless magmux writes
		// zero bytes to stdout and an attached one has an alternate screen in
		// the way, so a diagnostic printed anywhere else corrupts a frame.
		Log: func(line string) {
			if dbgFile != nil {
				fmt.Fprintln(dbgFile, line)
			}
		},
	})
}

// geometry is the terminal size the session meta reports. It is read under
// treeMu like every other layout fact.
func (m *Magmux) geometry() (rows, cols int) {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.rows, m.cols
}

// startFirebase brings the mirror up. It is called after markLayoutReady, for
// the same reason the connect-time aggregate is published there: a mirror of
// panes that do not exist yet is a mirror of nothing.
func (m *Magmux) startFirebase(a *firebase.Adapter) {
	if a == nil {
		return
	}
	m.fbMu.Lock()
	m.firebase = a
	m.fbMu.Unlock()
	a.Start()
}

// finalizeFirebase is the mirror's half of teardown: the last flush, carrying
// `results` and `alive:false`.
//
// It runs AFTER Hub.Finalize, which handed the mirror `results` through the
// ordinary Write path, and it is bounded by the same deadline — so the mirror
// can never be the thing that makes magmux slow to exit.
func (m *Magmux) finalizeFirebase(d time.Duration) {
	m.fbMu.Lock()
	a := m.firebase
	m.fbMu.Unlock()
	if a != nil {
		a.Finalize(d)
	}
}

// cleanupFirebase stops the flusher, the listener and the executors.
func (m *Magmux) cleanupFirebase() {
	m.fbMu.Lock()
	a := m.firebase
	m.fbMu.Unlock()
	if a != nil {
		a.Close()
	}
}

// firebaseNote is the one line magmux prints about a mirror it opened.
//
// On stderr, never stdout, and it names the node rather than the credential.
// Commands are called out explicitly: "this session is taking instructions from
// a database" is not something to discover from a config file a week later.
func firebaseNote(a *firebase.Adapter) {
	if a == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "magmux: mirroring to %s\n", a.URL())
	if a.CommandsEnabled() {
		fmt.Fprintf(os.Stderr, "magmux: firebase commands are ENABLED (owner uid + HMAC required)\n")
	}
}
