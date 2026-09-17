package firebase

// The config file, and every check that happens before a socket is opened.
//
// All of it runs before mux.init(), so every failure here is a plain line on
// stderr and an exit 1 while magmux still owns a normal terminal. That is why
// the messages name the field and the remedy rather than wrapping a JSON
// decoder error: this is the one moment a human is looking.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Defaults. Both are deliberately modest: this is a mirror at walking pace, not
// a remote terminal.
const (
	// DefaultFrameFPS is two frames a second. A phone watching a build wants to
	// see it move; it does not want 30 fps of a scrolling compiler.
	DefaultFrameFPS = 2
	// MaxFrameFPS caps what a config may ask for. Past this the budget is spent
	// on frames nothing renders.
	MaxFrameFPS = 10
	// DefaultByteBudget is 256 KB/s, shared across every pane. Frames are the
	// only thing it sheds.
	DefaultByteBudget = 262144
	// MinByteBudget keeps a misconfiguration from silently disabling frames
	// altogether while still reporting success.
	MinByteBudget = 4096
)

// Config is the JSON file --firebase names.
//
// Credentials and Emulator are MUTUALLY EXCLUSIVE, and that is the most
// important line in this struct: the emulator accepts an admin bypass header
// that is not a credential at all, so a config that named both would leave it
// ambiguous whether a real service-account key was about to be sent to
// 127.0.0.1.
type Config struct {
	DatabaseURL string `json:"databaseURL"`
	// Root is the top-level node magmux writes under, so several unrelated
	// projects can share one database.
	Root string `json:"root"`
	// Host names THIS machine. It is a path segment and part of the HMAC
	// canonical string, which is why its grammar is narrow.
	Host string `json:"host"`
	// Credentials is a service-account JSON file. Production only.
	Credentials string `json:"credentials"`
	// Emulator points at a local RTDB emulator. Development only; it never
	// reads a credential.
	Emulator *EmulatorConfig `json:"emulator"`

	FrameFPS         int `json:"frameFps"`
	ByteBudgetPerSec int `json:"byteBudgetPerSec"`

	Commands CommandsConfig `json:"commands"`

	// key is the HMAC key loaded from Commands.KeyFile. It is unexported so it
	// cannot be set from the file it guards, and it is nil unless commands are
	// enabled.
	key []byte
}

// EmulatorConfig is the local-emulator half. Host is `host:port` and NS is the
// namespace the emulator serves, which travels as `?ns=`.
type EmulatorConfig struct {
	Host string `json:"host"`
	NS   string `json:"ns"`
}

// CommandsConfig is the inbound half, and it is OFF by default.
//
// Every field is required when Enabled: an owner list that is empty would
// authorise nobody and is more likely a typo than an intention, and a key file
// that is missing makes every command unverifiable. Both refuse to start rather
// than degrade, because the degraded mode of an inbound command channel is
// "anyone who can write to the database gets a shell".
type CommandsConfig struct {
	Enabled bool `json:"enabled"`
	// Owners are Firebase uids. The SAME list is written to the database, where
	// the security rules read it, so magmux and the rules check one owner set.
	Owners []string `json:"owners"`
	// KeyFile holds the HMAC key. It must be a regular file, owned by this
	// user, mode 0600.
	KeyFile string `json:"keyFile"`
	// AllowOps is an allowlist of op names: an exact name, `<plugin>.*`, or
	// `*`. There are no other globs, so a pattern can never widen by accident.
	AllowOps []string `json:"allowOps"`
}

// LoadConfig reads and validates a config file.
//
// The path is expanded for `~` because this file names a credential and a key,
// and both live in ~/.config by convention; a config that has to spell out a
// home directory is a config that cannot be copied between machines.
func LoadConfig(path string) (*Config, error) {
	p, err := expandHome(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("firebase config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("firebase config %s: %w", path, err)
	}
	if err := cfg.normalize(); err != nil {
		return nil, fmt.Errorf("firebase config %s: %w", path, err)
	}
	return &cfg, nil
}

// normalize validates and fills in defaults. It is the only place a Config
// becomes usable, and it is called by LoadConfig and by the tests that build a
// Config in memory.
func (c *Config) normalize() error {
	if (c.Credentials != "") == (c.Emulator != nil) {
		return fmt.Errorf(`exactly one of "credentials" and "emulator" must be set`)
	}
	if !validSegment(c.Root) {
		return fmt.Errorf(`"root" must match [A-Za-z0-9_-]{1,64} (got %q)`, c.Root)
	}
	if !validSegment(c.Host) {
		return fmt.Errorf(`"host" must match [A-Za-z0-9_-]{1,64} (got %q)`, c.Host)
	}

	if c.Emulator != nil {
		if c.Emulator.Host == "" {
			return fmt.Errorf(`"emulator.host" is required (e.g. 127.0.0.1:9000)`)
		}
		if strings.ContainsAny(c.Emulator.Host, "/?#@") {
			return fmt.Errorf(`"emulator.host" must be host:port, not a URL (got %q)`, c.Emulator.Host)
		}
		if !validSegment(c.Emulator.NS) {
			return fmt.Errorf(`"emulator.ns" must match [A-Za-z0-9_-]{1,64} (got %q)`, c.Emulator.NS)
		}
		// The production URL is not consulted in emulator mode, and it is an
		// error to supply one: a file carrying both invites the reader to
		// believe the wrong one is in use.
		if c.DatabaseURL != "" {
			return fmt.Errorf(`"databaseURL" and "emulator" cannot both be set`)
		}
	} else if err := checkDatabaseURL(c.DatabaseURL); err != nil {
		return err
	}

	switch {
	case c.FrameFPS == 0:
		c.FrameFPS = DefaultFrameFPS
	case c.FrameFPS < 0 || c.FrameFPS > MaxFrameFPS:
		return fmt.Errorf(`"frameFps" must be 1..%d (got %d)`, MaxFrameFPS, c.FrameFPS)
	}
	switch {
	case c.ByteBudgetPerSec == 0:
		c.ByteBudgetPerSec = DefaultByteBudget
	case c.ByteBudgetPerSec < MinByteBudget:
		return fmt.Errorf(`"byteBudgetPerSec" must be at least %d (got %d)`, MinByteBudget, c.ByteBudgetPerSec)
	}

	return c.normalizeCommands()
}

// normalizeCommands is the inbound half's validation. With commands disabled it
// checks nothing and loads nothing — in particular it does not open the key
// file, so a config that has the section filled in but switched off is not a
// reason to read a secret off disk.
func (c *Config) normalizeCommands() error {
	if !c.Commands.Enabled {
		return nil
	}
	if len(c.Commands.Owners) == 0 {
		return fmt.Errorf(`"commands.enabled" needs at least one uid in "commands.owners"`)
	}
	for _, uid := range c.Commands.Owners {
		if !validUID(uid) {
			return fmt.Errorf(`"commands.owners" holds %q, which is not a uid (1-128 printable ASCII, no spaces)`, uid)
		}
	}
	if len(c.Commands.AllowOps) == 0 {
		return fmt.Errorf(`"commands.enabled" needs at least one entry in "commands.allowOps"`)
	}
	for _, pat := range c.Commands.AllowOps {
		if !validAllowOp(pat) {
			return fmt.Errorf(`"commands.allowOps" holds %q; entries are an exact op name, "<plugin>.*", or "*"`, pat)
		}
	}
	if c.Commands.KeyFile == "" {
		return fmt.Errorf(`"commands.enabled" needs a "commands.keyFile"`)
	}
	key, err := loadKeyFile(c.Commands.KeyFile)
	if err != nil {
		return err
	}
	c.key = key
	return nil
}

// Key is the HMAC key, or nil when commands are disabled.
func (c *Config) Key() []byte { return c.key }

// loadKeyFile reads the HMAC key under the same rules as a bearer token: no
// symlink, a regular file, owned by this euid, no group or other bits.
//
// The key is the whole of magmux's own verification — the rules layer stops a
// non-owner, and this stops an owner's stolen session from being replayed or
// forged — so it gets the credential treatment rather than the config
// treatment.
func loadKeyFile(path string) ([]byte, error) {
	p, err := expandHome(path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("commands.keyFile %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("commands.keyFile %s is a symlink; magmux will not follow one to a secret", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("commands.keyFile %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("commands.keyFile %s has mode %#o; it must be 0600 (no group or other bits)", path, perm)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && uint32(st.Uid) != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("commands.keyFile %s is owned by uid %d, not by this process (uid %d)",
			path, st.Uid, os.Geteuid())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("commands.keyFile %s: %w", path, err)
	}
	// Trimmed, because every editor adds a trailing newline and a key that
	// silently differed by one byte from the one the client used would fail
	// every HMAC with no way to see why.
	key := []byte(strings.TrimSpace(string(b)))
	if len(key) < 16 {
		return nil, fmt.Errorf("commands.keyFile %s holds %d bytes; an HMAC key must be at least 16", path, len(key))
	}
	return key, nil
}

// AllowsOp reports whether op passes the allowlist.
//
// Three shapes and no more: `*` is everything, `<plugin>.*` is one plugin's
// ops, and anything else is an exact name. A general glob would make the
// allowlist a language, and a language in an allowlist is a place for a rule
// to mean more than its author read.
func (c *Config) AllowsOp(op string) bool {
	for _, pat := range c.Commands.AllowOps {
		switch {
		case pat == "*":
			return true
		case strings.HasSuffix(pat, ".*"):
			if strings.HasPrefix(op, pat[:len(pat)-1]) && len(op) > len(pat)-1 {
				return true
			}
		case pat == op:
			return true
		}
	}
	return false
}

// IsOwner reports whether uid is in the configured owner set. The comparison is
// an ordinary one: a uid is not a secret, and the secret half of the check is
// the constant-time HMAC compare in verify().
func (c *Config) IsOwner(uid string) bool {
	for _, o := range c.Commands.Owners {
		if o == uid {
			return true
		}
	}
	return false
}

// validSegment is the grammar for a path segment magmux composes into a URL:
// root, host and the emulator namespace. It is narrow on purpose — these end up
// in a REST path and in the HMAC canonical string, and a segment that could
// hold a `/` or a newline could forge either.
func validSegment(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// validUID is the Firebase uid grammar as magmux enforces it: 1-128 printable
// ASCII with no space. Firebase's own limit is 128 characters; the printable
// restriction is magmux's, because a uid with a control character in it would
// be a field that could forge a line of the HMAC canonical string.
func validUID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// validAllowOp is the allowlist entry grammar.
func validAllowOp(pat string) bool {
	if pat == "*" {
		return true
	}
	name := strings.TrimSuffix(pat, ".*")
	if name == "" {
		return false
	}
	// Both halves of a qualified name, and a bare built-in, use the same
	// characters. Checking the string rather than the split keeps one rule.
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '.':
		default:
			return false
		}
	}
	return !strings.HasPrefix(name, ".") && !strings.HasSuffix(name, ".")
}

// expandHome resolves a leading `~/`. Nothing else is expanded: a config file
// is not a shell.
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand %q: %w", p, err)
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")), nil
	}
	return p, nil
}
