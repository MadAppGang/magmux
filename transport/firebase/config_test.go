package firebase

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigValidation is the table of everything refused before a socket is
// opened. Each row is a config a human could plausibly write, and the reason it
// is refused is the reason the message has to name the field.
func TestConfigValidation(t *testing.T) {
	base := func() *Config {
		return &Config{
			DatabaseURL: "https://p-default-rtdb.firebaseio.com",
			Root:        "magmux",
			Host:        "jacks-mac",
			Credentials: "~/.config/magmux/sa.json",
		}
	}
	cases := []struct {
		name string
		edit func(*Config)
		ok   bool
	}{
		{"the example config", func(*Config) {}, true},
		{"emulator alone", func(c *Config) {
			c.Credentials, c.DatabaseURL = "", ""
			c.Emulator = &EmulatorConfig{Host: "127.0.0.1:9000", NS: "demo-magmux"}
		}, true},
		{"credentials AND emulator", func(c *Config) {
			c.Emulator = &EmulatorConfig{Host: "127.0.0.1:9000", NS: "demo"}
		}, false},
		{"neither credentials nor emulator", func(c *Config) { c.Credentials = "" }, false},
		{"emulator plus a production URL", func(c *Config) {
			c.Credentials = ""
			c.Emulator = &EmulatorConfig{Host: "127.0.0.1:9000", NS: "demo"}
		}, false},
		{"an emulator host that is a URL", func(c *Config) {
			c.Credentials, c.DatabaseURL = "", ""
			c.Emulator = &EmulatorConfig{Host: "http://127.0.0.1:9000/x", NS: "demo"}
		}, false},
		{"root with a slash", func(c *Config) { c.Root = "mag/mux" }, false},
		{"root with a dot", func(c *Config) { c.Root = "mag.mux" }, false},
		{"empty host", func(c *Config) { c.Host = "" }, false},
		{"host with a space", func(c *Config) { c.Host = "jacks mac" }, false},
		{"a look-alike databaseURL", func(c *Config) {
			c.DatabaseURL = "https://x.firebaseio.com.evil.com"
		}, false},
		{"frameFps above the cap", func(c *Config) { c.FrameFPS = 60 }, false},
		{"a negative frameFps", func(c *Config) { c.FrameFPS = -1 }, false},
		{"a byte budget too small to carry a frame", func(c *Config) { c.ByteBudgetPerSec = 10 }, false},
		{"commands on with no owners", func(c *Config) {
			c.Commands = CommandsConfig{Enabled: true, AllowOps: []string{"list"}, KeyFile: "k"}
		}, false},
		{"commands on with no allowOps", func(c *Config) {
			c.Commands = CommandsConfig{Enabled: true, Owners: []string{"u"}, KeyFile: "k"}
		}, false},
		{"commands on with no keyFile", func(c *Config) {
			c.Commands = CommandsConfig{Enabled: true, Owners: []string{"u"}, AllowOps: []string{"list"}}
		}, false},
		{"a uid with a control character", func(c *Config) {
			c.Commands = CommandsConfig{Enabled: true, Owners: []string{"u\nid"}, AllowOps: []string{"list"}, KeyFile: "k"}
		}, false},
		{"an allowOps glob in the middle", func(c *Config) {
			c.Commands = CommandsConfig{Enabled: true, Owners: []string{"u"}, AllowOps: []string{"tick*.run"}, KeyFile: "k"}
		}, false},
		{"commands OFF, so the section is not checked at all", func(c *Config) {
			c.Commands = CommandsConfig{Owners: nil, AllowOps: nil, KeyFile: "/nonexistent"}
		}, true},
	}
	for _, c := range cases {
		cfg := base()
		c.edit(cfg)
		err := cfg.normalize()
		if c.ok && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

// TestConfigDefaults: an omitted rate and budget take the documented values
// rather than zero, because zero here means "mirror nothing" and would look
// exactly like a working mirror with a quiet session.
func TestConfigDefaults(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "https://p-default-rtdb.firebaseio.com",
		Root:        "magmux",
		Host:        "h",
		Credentials: "sa.json",
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if cfg.FrameFPS != DefaultFrameFPS {
		t.Errorf("frameFps = %d, want %d", cfg.FrameFPS, DefaultFrameFPS)
	}
	if cfg.ByteBudgetPerSec != DefaultByteBudget {
		t.Errorf("byteBudgetPerSec = %d, want %d", cfg.ByteBudgetPerSec, DefaultByteBudget)
	}
}

// TestKeyFilePermissions: the HMAC key gets the credential treatment, not the
// config treatment. It is the whole of magmux's own verification, so a
// world-readable one is refused at startup rather than used.
func TestKeyFilePermissions(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "cmd.key")
	if err := os.WriteFile(good, []byte("a-sufficiently-long-hmac-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose.key")
	if err := os.WriteFile(loose, []byte("a-sufficiently-long-hmac-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}

	cfg := func(path string) *Config {
		return &Config{
			DatabaseURL: "https://p-default-rtdb.firebaseio.com",
			Root:        "magmux", Host: "h", Credentials: "sa.json",
			Commands: CommandsConfig{Enabled: true, Owners: []string{"u1"},
				AllowOps: []string{"list"}, KeyFile: path},
		}
	}
	c := cfg(good)
	if err := c.normalize(); err != nil {
		t.Fatalf("a 0600 key file was refused: %v", err)
	}
	// Trimmed: every editor adds a trailing newline, and a key that silently
	// differed by one byte would fail every HMAC with no way to see why.
	if string(c.Key()) != "a-sufficiently-long-hmac-key" {
		t.Fatalf("key = %q; the trailing newline was not trimmed", c.Key())
	}
	for name, path := range map[string]string{
		"mode 0644": loose,
		"too short": short,
		"a symlink": link,
		"missing":   filepath.Join(dir, "nope.key"),
	} {
		if err := cfg(path).normalize(); err == nil {
			t.Errorf("a key file that is %s was accepted", name)
		}
	}
}

// TestAllowOps is the allowlist's three shapes and nothing else. A general glob
// would make this a language, and a language in an allowlist is a place for a
// rule to mean more than its author read.
func TestAllowOps(t *testing.T) {
	cfg := &Config{Commands: CommandsConfig{AllowOps: []string{"list", "capture", "ticket.*"}}}
	yes := []string{"list", "capture", "ticket.run_ticket", "ticket.status"}
	no := []string{"send", "input", "open_pane", "ticket", "ticketx.run", "other.run_ticket", ""}
	for _, op := range yes {
		if !cfg.AllowsOp(op) {
			t.Errorf("AllowsOp(%q) = false", op)
		}
	}
	for _, op := range no {
		if cfg.AllowsOp(op) {
			t.Errorf("AllowsOp(%q) = true", op)
		}
	}
	star := &Config{Commands: CommandsConfig{AllowOps: []string{"*"}}}
	if !star.AllowsOp("open_pane") {
		t.Error(`"*" did not allow everything`)
	}
}

// TestShippedExamplesAreValid reads the files this phase ships. They are
// documentation a human copies, so a typo in one is a support request, not a
// cosmetic defect.
func TestShippedExamplesAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "firebase")

	var cfg Config
	readJSON(t, filepath.Join(dir, "config.example.json"), &cfg)
	// The example ships with commands OFF and a placeholder uid, so it must
	// validate as it stands — that is the whole point of shipping it.
	if err := cfg.normalize(); err != nil {
		t.Fatalf("config.example.json does not validate: %v", err)
	}
	if cfg.Commands.Enabled {
		t.Error("the shipped example enables commands; it must not")
	}

	var fb struct {
		Database  map[string]any `json:"database"`
		Emulators map[string]any `json:"emulators"`
	}
	readJSON(t, filepath.Join(dir, "firebase.json"), &fb)
	if fb.Database["rules"] != "database.rules.json" {
		t.Errorf("firebase.json does not load the shipped rules: %v", fb.Database)
	}
	if _, ok := fb.Emulators["database"]; !ok {
		t.Error("firebase.json declares no database emulator")
	}

	var rules map[string]any
	readJSON(t, filepath.Join(dir, "database.rules.json"), &rules)
	if _, ok := rules["rules"]; !ok {
		t.Error("database.rules.json has no top-level rules block")
	}
	// The rules must read the SAME owner list magmux checks, through the
	// $root/$host wildcards, or a deployment with a different `root` would
	// silently authorise nobody.
	raw, err := os.ReadFile(filepath.Join(dir, "database.rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"$root", "$host", "/owners/' + auth.uid", "!data.exists()"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("database.rules.json does not contain %q", want)
		}
	}
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
