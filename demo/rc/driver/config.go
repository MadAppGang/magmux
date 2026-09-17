package main

// Where the running demo left its identity, and what to say when it is not
// there. Ported verbatim in behaviour from the driver this replaced, because
// its diagnosis is checked by demo/rc/selftest.ts and is the one thing in the
// driver that runs when magmux is already gone.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config is the run's identity: a URL and the two credentials, plus the words
// that make a hint true where this process happens to be running.
type Config struct {
	URL string
	// Token is the FULL session token. Only this process holds it.
	Token string
	// ViewToken is the read-only token. Every mirror holds this one.
	ViewToken string

	ID    string
	State string

	// Place is "pane" when the driver is stacked under the mirror, "window"
	// otherwise; Mirror is "pane", "window" or "none". Telling somebody to look
	// at a pane that was never created is worse than saying nothing.
	Place     string
	Mirror    string
	MirrorKey string
	Prefix    string
	QuitHint  string
}

// Fingerprint is how a token is shown on screen. Never the whole thing: this
// pane is in a screenshot in a README the first time anybody demos it.
func Fingerprint(t string) string {
	if len(t) < 12 {
		return "…"
	}
	return t[:6] + "…" + t[len(t)-4:]
}

// Mirrors names where to look, in the words the current layout makes true.
func (c Config) Mirrors() string {
	switch c.Mirror {
	case "none":
		return "the browser tab"
	case "window":
		return fmt.Sprintf("the mirror window (%s %s) and the browser tab", c.Prefix, c.MirrorKey)
	default:
		return "the mirror pane above and the browser tab"
	}
}

// MirrorsShort is the same fact in the fewest cells, for the key bar — where
// the alternative to a short phrase is no phrase at all, because the bar has to
// share a row with the bindings.
func (c Config) MirrorsShort() string {
	switch c.Mirror {
	case "none":
		return "the browser tab"
	case "window":
		return "the mirror window + the browser tab"
	default:
		return "the mirror pane + the browser tab"
	}
}

// MirrorKeys is how to reach the mirror's own keyboard, for the two actions
// that ask a human to press a key over there.
func (c Config) MirrorKeys() string {
	switch c.Mirror {
	case "none":
		return "(there is no mirror pane — RC_DEMO_MIRROR=0)"
	case "window":
		return fmt.Sprintf("(%s %s)", c.Prefix, c.MirrorKey)
	default:
		return fmt.Sprintf("(%s ↑)", c.Prefix)
	}
}

var routineLine = regexp.MustCompile(`^magmux: (listening on |token in )`)

// magmuxReason is what magmux itself said, when the file this driver needs is
// not there.
//
// A MISSING TOKEN IS A SYMPTOM. magmux writes its token before it binds and
// removes it at exit, so "the token is gone" is the shape of every way magmux
// can stop — a layout it refuses, a port it could not keep, a crash — and none
// of those is a problem with credentials, files or permissions, which is where
// "cannot read the session token" sends a reader. magmux's own reason is
// sitting in $STATE/magmux.err the whole time, so it is read here and reported
// FIRST.
//
// Returns "" when there is genuinely no evidence either way, which is the only
// case where "is the demo running?" is the right thing to ask.
func magmuxReason(state string) string {
	raw, err := os.ReadFile(filepath.Join(state, "magmux.err"))
	if err != nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var said []string
	for _, l := range lines {
		if !routineLine.MatchString(l) {
			said = append(said, l)
		}
	}
	if len(said) > 0 {
		if len(said) > 6 {
			said = said[len(said)-6:]
		}
		return strings.Join(said, "\n")
	}
	// Only routine lines, and yet the file the driver needs is gone: magmux DID
	// come up here and has since exited without a word. Still evidence, and
	// still a better answer than a question about whether the demo is running.
	for _, l := range lines {
		if strings.HasPrefix(l, "magmux: listening on ") {
			return "(magmux announced a listener here and then exited without a message)"
		}
	}
	return ""
}

// LoadConfig reads the run's identity off disk, exactly as an operator would.
//
// Both tokens come from FILES and neither is ever on a command line: a value on
// a command line is in every `ps` listing on the machine, which is why magmux
// itself has --token-file and no --token.
func LoadConfig(a Args) (Config, error) {
	read := func(file, what string) (string, error) {
		raw, err := os.ReadFile(file)
		v := strings.TrimSpace(string(raw))
		if err == nil && v == "" {
			err = fmt.Errorf("it is empty")
		}
		if err == nil {
			return v, nil
		}
		if reason := magmuxReason(a.State); reason != "" {
			var indented []string
			for _, l := range strings.Split(reason, "\n") {
				indented = append(indented, "         "+l)
			}
			return "", fmt.Errorf(
				"magmux exited — this is its own reason, from %s:\n%s\n\n"+
					"       The %s at %s is gone BECAUSE of that: magmux writes that file\n"+
					"       before it binds and removes it at exit, so a missing token is the\n"+
					"       symptom and the lines above are the cause.\n"+
					"       (%v)",
				filepath.Join(a.State, "magmux.err"), strings.Join(indented, "\n"), what, file, err)
		}
		return "", fmt.Errorf(
			"cannot read the %s at %s: %v\n"+
				"       there is no %s either, so there is no evidence\n"+
				"       magmux ever ran here — is the demo running? start it with 'task demo:rc'.",
			what, file, err, filepath.Join(a.State, "magmux.err"))
	}

	url := a.URL
	if url == "" {
		v, err := read(filepath.Join(a.State, "magmux.url"), "magmux URL")
		if err != nil {
			return Config{}, err
		}
		url = v
	}
	tokenFile := a.TokenFile
	if tokenFile == "" {
		tokenFile = filepath.Join(a.State, fmt.Sprintf("magmux-%s.token", a.ID))
	}
	viewFile := a.ViewFile
	if viewFile == "" {
		viewFile = filepath.Join(a.State, "view.token")
	}
	token, err := read(tokenFile, "session token")
	if err != nil {
		return Config{}, err
	}
	view, err := read(viewFile, "view token")
	if err != nil {
		return Config{}, err
	}
	return Config{
		URL:       strings.TrimRight(url, "/"),
		Token:     token,
		ViewToken: view,
		ID:        a.ID,
		State:     a.State,
		Place:     a.Place,
		Mirror:    a.Mirror,
		MirrorKey: a.MirrorKey,
		Prefix:    a.Prefix,
		QuitHint:  a.QuitHint,
	}, nil
}
