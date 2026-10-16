package mux

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MadAppGang/magmux/sockdir"
)

// ── the editor-mode override: the merge and the private settings copy ───────
//
// Claude Code keeps only the LAST --settings it is given (research F1), so the
// override cannot add a second flag beside a caller's: it MERGES editorMode
// "normal" into the one that takes effect. claude_launch.go finds that flag
// and replaces its span; this file produces what goes in the span.

// settingsFileMax bounds a caller's settings file. A settings file is a few
// hundred bytes; anything near this is not one, and reading it whole into a
// launch is not something to do on the way to a fork.
const settingsFileMax = 1 << 20

// errNotObject is a settings value that is valid JSON and not an object.
var errNotObject = errors.New("settings are not a JSON object")

// mergeEditorMode decodes a settings JSON object, sets editorMode to "normal"
// and re-encodes it compactly. Every other key survives: the values are
// json.RawMessage, so nothing is re-interpreted, and json.Marshal sorts the
// keys, so the output is deterministic.
func mergeEditorMode(raw []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil { // `null`
		return nil, errNotObject
	}
	m["editorMode"] = json.RawMessage(`"normal"`)
	return json.Marshal(m)
}

// shellQuoteJSON is Q: compact JSON as ONE single-quoted shell word that holds
// no whitespace and no single quote. Compact JSON has whitespace only inside
// strings, and a JSON string cannot hold a raw control character, so the only
// bytes left to remove are the space (0x20) and the quote (0x27); each becomes
// its \u escape, which is legal exactly where it can occur — inside a JSON
// string. POSIX single quotes keep the backslashes literal, which is why the
// rewrite needs a POSIX-family shell.
func shellQuoteJSON(compact []byte) string {
	s := strings.ReplaceAll(string(compact), " ", escSpace)
	s = strings.ReplaceAll(s, "'", escQuote)
	return "'" + s + "'"
}

// escSpace and escQuote are the JSON escapes for 0x20 and 0x27, spelled as a
// backslash plus the hex so no editor or tool can fold them back into the
// bytes they stand for.
const (
	escSpace = `\` + "u0020"
	escQuote = `\` + "u0027"
)

// readSettingsFile reads a caller's settings file in the caller's own context:
// it must be a regular file (following a symlink is fine — it is the caller's
// path) of at most settingsFileMax bytes.
func readSettingsFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > settingsFileMax {
		return nil, fmt.Errorf("%s is %d bytes, over the %d-byte limit", path, fi.Size(), settingsFileMax)
	}
	b, err := io.ReadAll(io.LimitReader(f, settingsFileMax+1))
	if err != nil {
		return nil, err
	}
	if len(b) > settingsFileMax {
		return nil, fmt.Errorf("%s grew past the %d-byte limit while being read", path, settingsFileMax)
	}
	return b, nil
}

// writeSettingsCopy writes merged settings to a new 0600 file in the private
// directory dir, pid-named so the dead-owner sweep can reap it, and returns its
// path. The directory is created and VERIFIED first (sockdir.EnsurePrivateDir:
// the token-file rule applied to a directory); a directory that fails is no
// file at all, and the caller falls back to unparsed.
//
// The path is emitted single-quoted into a shell string, so a path holding a
// quote or whitespace is refused before anything is created.
func writeSettingsCopy(dir string, merged []byte) (string, error) {
	if dir == "" {
		return "", errors.New("no settings directory")
	}
	if strings.ContainsAny(dir, "' \t\n") {
		return "", fmt.Errorf("settings directory %q cannot be single-quoted safely", dir)
	}
	if err := sockdir.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	// os.CreateTemp is O_RDWR|O_CREATE|O_EXCL at 0600.
	f, err := os.CreateTemp(dir, sockdir.SettingsFilePattern(os.Getpid()))
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.Write(merged); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// removeSettingsFile deletes the private settings copy this pane owns, if
// any. settingsFile is write-once before publication, so it needs no lock;
// the callers are the reap (once cmd.Wait has returned, whichever goroutine
// that was) and the spawn-failure path.
func (p *Pane) removeSettingsFile() {
	if p == nil || p.settingsFile == "" {
		return
	}
	if err := os.Remove(p.settingsFile); err != nil && !errors.Is(err, os.ErrNotExist) && dbgFile != nil {
		fmt.Fprintf(dbgFile, "[editor] remove %s: %v\n", p.settingsFile, err)
	}
}

// sweepSettings is the startup half of the private directory's lifecycle: it
// removes the copies a magmux that died badly left behind. Synchronous, like
// ReapStale, and bounded by the same deadline.
func (m *Magmux) sweepSettings() {
	dir := settingsDir(m.socketDir())
	if n := sockdir.ReapSettings(dir, sockdir.ReapDeadline); n > 0 && dbgFile != nil {
		fmt.Fprintf(dbgFile, "reaped %d stale settings file(s) in %s\n", n, dir)
	}
}

// removeOwnSettings is the exit half: every copy THIS process still owns, for
// a pane whose child was never reaped.
func (m *Magmux) removeOwnSettings() {
	sockdir.RemoveOwnSettings(settingsDir(m.socketDir()))
}
