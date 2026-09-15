package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// TestTokenAlphabetIsWhatAHeaderAndASubprotocolAccept pins the format, because
// the alphabet is not cosmetic: the token travels as `magmux.auth.<token>` in
// Sec-WebSocket-Protocol, where a comma or a space splits it into two protocol
// names and the browser drops the request without reporting why.
func TestTokenAlphabetIsWhatAHeaderAndASubprotocolAccept(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(tok) != 43 {
		t.Errorf("a generated token is 32 bytes as unpadded base64url = 43 chars, got %d (%q)", len(tok), tok)
	}
	if !ValidToken(tok) {
		t.Errorf("Generate produced a token ValidToken rejects: %q", tok)
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("base64url must not contain +, / or padding: %q", tok)
	}

	ok := strings.Repeat("a", 32)
	for _, tc := range []struct {
		name  string
		tok   string
		valid bool
	}{
		{"exactly the floor", ok, true},
		{"one under the floor", strings.Repeat("a", 31), false},
		{"exactly the ceiling", strings.Repeat("a", 256), true},
		{"one over the ceiling", strings.Repeat("a", 257), false},
		{"empty", "", false},
		{"every allowed punctuation", strings.Repeat("a", 28) + "._~-", true},
		{"a comma would split the subprotocol", strings.Repeat("a", 31) + ",", false},
		{"a space would split the header", strings.Repeat("a", 31) + " ", false},
		{"base64 standard padding", strings.Repeat("a", 31) + "=", false},
		{"a slash", strings.Repeat("a", 31) + "/", false},
		{"a newline", strings.Repeat("a", 31) + "\n", false},
		{"a NUL", strings.Repeat("a", 31) + "\x00", false},
		{"non-ASCII", strings.Repeat("a", 31) + "é", false},
	} {
		if got := ValidToken(tc.tok); got != tc.valid {
			t.Errorf("%s: ValidToken(%q) = %v, want %v", tc.name, tc.tok, got, tc.valid)
		}
	}

	// Two generations must differ. A constant token would pass every other test
	// in this file.
	other, _ := Generate()
	if other == tok {
		t.Error("two generated tokens are identical")
	}
}

// TestTokenFileRefusesAnythingThatIsNotASecret is the 0644 gate: a token another
// user can read is not a credential, and a symlink is somebody else's file.
func TestTokenFileRefusesAnythingThatIsNotASecret(t *testing.T) {
	dir := t.TempDir()
	tok, _ := Generate()

	good := filepath.Join(dir, "good.token")
	if err := os.WriteFile(good, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(good)
	if err != nil {
		t.Fatalf("a 0600 file owned by us must load: %v", err)
	}
	if got != tok {
		t.Errorf("LoadFile = %q, want %q (the trailing newline must be trimmed)", got, tok)
	}

	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666, 0o660} {
		p := filepath.Join(dir, "mode.token")
		if err := os.WriteFile(p, []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(p); err == nil {
			t.Errorf("mode %#o must be refused: a token anyone else can read is not a secret", mode)
		} else if !strings.Contains(err.Error(), "0600") {
			t.Errorf("mode %#o: the error should say what is wanted, got %v", mode, err)
		}
		os.Remove(p)
	}

	link := filepath.Join(dir, "link.token")
	if err := os.Symlink(good, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := LoadFile(link); err == nil {
		t.Error("a symlink must be refused even when its target is fine")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the refusal should name the reason, got %v", err)
	}

	sub := filepath.Join(dir, "adir.token")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(sub); err == nil {
		t.Error("a directory must be refused")
	}

	junk := filepath.Join(dir, "junk.token")
	if err := os.WriteFile(junk, []byte("not a token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(junk); err == nil {
		t.Error("a file whose content is not a valid token must be refused")
	}

	if _, err := LoadFile(filepath.Join(dir, "absent.token")); err == nil {
		t.Error("a missing --token-file must be an error, not a silent fallback to generation")
	}
}

// TestGeneratedTokenFileIsAtomicAnd0600 covers the write half: a temp file with
// O_EXCL|O_NOFOLLOW, renamed over the final name, mode 0600, and no temp file
// left behind.
func TestGeneratedTokenFileIsAtomicAnd0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "magmux-1.token")
	tok, _ := Generate()
	if err := WriteFile(path, tok); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %#o, want 0600", fi.Mode().Perm())
	}
	if got, err := LoadFile(path); err != nil || got != tok {
		t.Errorf("LoadFile after WriteFile = %q, %v; want %q", got, err, tok)
	}
	if _, err := os.Lstat(TempName(path, os.Getpid())); !os.IsNotExist(err) {
		t.Error("the temp file must be renamed away, not left beside the real one")
	}

	// Rewriting replaces, with no window where the path is missing. A stale
	// temp file from a killed process must not block it either.
	if err := os.WriteFile(TempName(path, os.Getpid()), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, tok); err == nil {
		t.Error("O_EXCL must refuse to write through an existing temp file")
	}
	os.Remove(TempName(path, os.Getpid()))

	// RemoveIfOurs removes only our own token.
	RemoveIfOurs(path, "some-other-token-entirely-that-is-long-enough")
	if _, err := os.Stat(path); err != nil {
		t.Error("RemoveIfOurs removed a file holding somebody else's token")
	}
	RemoveIfOurs(path, tok)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("RemoveIfOurs must remove a file still holding our token")
	}
}

// TestStoreClassifiesBothTokens is the comparison matrix.
func TestStoreClassifiesBothTokens(t *testing.T) {
	full, _ := Generate()
	view, _ := Generate()
	s := NewStore(full, view, nil)

	for _, tc := range []struct {
		name string
		tok  string
		want Kind
	}{
		{"the session token", full, Full},
		{"the view token", view, View},
		{"nothing", "", None},
		{"a wrong token", strings.Repeat("z", 43), None},
		{"the session token with one character changed", flip(full), None},
		{"a prefix of the session token", full[:20], None},
	} {
		if got := s.Check(tc.tok); got != tc.want {
			t.Errorf("%s: Check = %v, want %v", tc.name, got, tc.want)
		}
	}

	if !s.HasView() {
		t.Error("HasView must be true when a view token was configured")
	}
	if NewStore(full, "", nil).Check(view) != None {
		t.Error("with no view token configured, the view token must not authenticate")
	}
	if NewStore(full, "", nil).HasView() {
		t.Error("HasView must be false when no view token was configured")
	}
	if Full.ReadOnly() {
		t.Error("the session token is not read-only")
	}
	if !View.ReadOnly() {
		t.Error("the view token is read-only")
	}
}

// TestViewTokenReachesReadBuiltinsAndNothingElse is D1's whole rule, plus
// --view-op.
func TestViewTokenReachesReadBuiltinsAndNothingElse(t *testing.T) {
	full, _ := Generate()
	view, _ := Generate()
	s := NewStore(full, view, []string{"ticket.status", "not-qualified", "too.many.dots", ".leading", "trailing."})

	spec := func(name, source string, class protocol.OpClass) protocol.OpSpec {
		return protocol.OpSpec{Name: name, Source: source, Class: class}
	}

	for _, tc := range []struct {
		name    string
		kind    Kind
		spec    protocol.OpSpec
		allowed bool
	}{
		{"full token, control op", Full, spec("send", protocol.SourceBuiltin, protocol.ClassControl), true},
		{"full token, input op", Full, spec("input", protocol.SourceBuiltin, protocol.ClassInput), true},
		{"view token, read built-in", View, spec("capture", protocol.SourceBuiltin, protocol.ClassRead), true},
		{"view token, list", View, spec("list", protocol.SourceBuiltin, protocol.ClassRead), true},
		{"view token, control built-in", View, spec("send", protocol.SourceBuiltin, protocol.ClassControl), false},
		{"view token, input", View, spec("input", protocol.SourceBuiltin, protocol.ClassInput), false},
		{"view token, display built-in", View, spec("tint", protocol.SourceBuiltin, protocol.ClassDisplay), false},
		// The plugin rules: a plugin's self-declared class NEVER widens the view
		// token, so even a read-class plugin op is refused unless --view-op
		// named it.
		{"view token, read-class plugin op it was not granted", View,
			spec("ticket.list", "ticket", protocol.ClassRead), false},
		{"view token, the granted plugin op", View,
			spec("ticket.status", "ticket", protocol.ClassRead), true},
		{"view token, a granted plugin op of class control", View,
			spec("ticket.status", "ticket", protocol.ClassControl), true},
	} {
		err := s.Authorize(tc.kind, tc.spec, true)
		if (err == nil) != tc.allowed {
			t.Errorf("%s: Authorize = %v, want allowed=%v", tc.name, err, tc.allowed)
		}
		if err != nil && protocol.CodeOf(err) != protocol.CodeForbidden {
			t.Errorf("%s: the refusal must be `forbidden`, got %q", tc.name, protocol.CodeOf(err))
		}
	}

	// An unregistered op is not this layer's to refuse: the registry answers
	// unknown_verb, and a viewer that could tell "forbidden" from "no such op"
	// would have an op-name oracle.
	if err := s.Authorize(View, protocol.OpSpec{}, false); err != nil {
		t.Errorf("an unregistered op must fall through to the registry, got %v", err)
	}

	// Only fully-qualified names are kept, so this flag can never reach a
	// built-in — no built-in name has a dot.
	for _, bad := range []string{"not-qualified", "too.many.dots", ".leading", "trailing.", "send"} {
		if s.GrantedToViewers(bad) {
			t.Errorf("--view-op %q must have been ignored; it is not a plugin.op name", bad)
		}
	}
	if !s.GrantedToViewers("ticket.status") {
		t.Error("--view-op ticket.status must be kept")
	}

	// EffectiveReadOnly carries Authorize's decision across the boundary: a
	// granted op runs with ReadOnly cleared so the hub's own class check does
	// not refuse what the operator just allowed.
	if !s.EffectiveReadOnly(View, "capture") {
		t.Error("an ordinary read op still runs as a read-only caller")
	}
	if s.EffectiveReadOnly(View, "ticket.status") {
		t.Error("a --view-op grant must clear ReadOnly for that op")
	}
	if s.EffectiveReadOnly(Full, "capture") {
		t.Error("a full-token caller is never read-only")
	}
}

func TestValidViewOpShape(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"ticket.run_ticket", true},
		{"a.b", true},
		{"send", false},
		{"", false},
		{".op", false},
		{"plugin.", false},
		{"a.b.c", false},
	} {
		if got := ValidViewOp(tc.in); got != tc.ok {
			t.Errorf("ValidViewOp(%q) = %v, want %v", tc.in, got, tc.ok)
		}
	}
}

// TestTicketsAreSingleUseAndExpire covers the two properties the whole
// mechanism exists for.
func TestTicketsAreSingleUseAndExpire(t *testing.T) {
	tk := NewTickets(50 * time.Millisecond)

	s, err := tk.Mint(Full)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidToken(s) {
		t.Errorf("a ticket must look like a token: %q", s)
	}
	if k, ok := tk.Redeem(s); !ok || k != Full {
		t.Fatalf("first redemption = %v, %v; want Full, true", k, ok)
	}
	if _, ok := tk.Redeem(s); ok {
		t.Error("a ticket must be single use; the second redemption succeeded")
	}

	// A viewer's ticket inherits ReadOnly, so a ticket is not a way up.
	v, _ := tk.Mint(View)
	if k, ok := tk.Redeem(v); !ok || k != View {
		t.Errorf("a view ticket must redeem as View, got %v, %v", k, ok)
	}

	exp, _ := tk.Mint(Full)
	time.Sleep(80 * time.Millisecond)
	if _, ok := tk.Redeem(exp); ok {
		t.Error("an expired ticket must be refused")
	}
	if _, ok := tk.Redeem("never-minted-but-well-formed-value-here"); ok {
		t.Error("an unknown ticket must be refused")
	}
	if n := tk.Outstanding(); n != 0 {
		t.Errorf("expired tickets must be swept, %d left", n)
	}
	if tk.TTL() != 50*time.Millisecond {
		t.Errorf("TTL = %v", tk.TTL())
	}
}

// TestResolveOrderAndThatAnEnvTokenIsNeverWritten pins the source precedence
// and the one rule that cannot be inferred from it.
func TestResolveOrderAndThatAnEnvTokenIsNeverWritten(t *testing.T) {
	dir := t.TempDir()
	envTok, _ := Generate()
	fileTok, _ := Generate()
	viewTok, _ := Generate()

	fileP := filepath.Join(dir, "from-file.token")
	if err := os.WriteFile(fileP, []byte(fileTok), 0o600); err != nil {
		t.Fatal(err)
	}
	viewP := filepath.Join(dir, "view.token")
	if err := os.WriteFile(viewP, []byte(viewTok), 0o600); err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(dir, "magmux-1.token")

	// 1. The environment wins over the file, and NOTHING is written.
	r, err := Resolve(Config{EnvToken: envTok, TokenFile: fileP, DefaultPath: def})
	if err != nil {
		t.Fatal(err)
	}
	if r.Token != envTok {
		t.Errorf("MAGMUX_TOKEN must win over --token-file")
	}
	if r.GeneratedPath != "" {
		t.Errorf("an env token owns no file, got GeneratedPath %q", r.GeneratedPath)
	}
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Fatal("MAGMUX_TOKEN was written to disk; it must never be")
	}
	// Belt and braces: no file anywhere under the directory holds it.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if strings.Contains(string(b), envTok) {
			t.Fatalf("MAGMUX_TOKEN leaked into %s", e.Name())
		}
	}

	// 2. --token-file when the environment is empty, and still no write.
	r, err = Resolve(Config{TokenFile: fileP, DefaultPath: def})
	if err != nil {
		t.Fatal(err)
	}
	if r.Token != fileTok || r.GeneratedPath != "" {
		t.Errorf("--token-file: token=%q generated=%q", r.Token, r.GeneratedPath)
	}
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Error("an operator's --token-file must not cause a second file to be written")
	}

	// 3. Neither: generate, write 0600, and own the removal.
	r, err = Resolve(Config{DefaultPath: def})
	if err != nil {
		t.Fatal(err)
	}
	if r.GeneratedPath != def {
		t.Fatalf("GeneratedPath = %q, want %q", r.GeneratedPath, def)
	}
	onDisk, err := LoadFile(def)
	if err != nil || onDisk != r.Token {
		t.Fatalf("the generated token must be readable at its final name: %q, %v", onDisk, err)
	}
	r.Cleanup()
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Error("Cleanup must remove a generated token file")
	}

	// 4. A --token-file magmux did not write is never removed.
	r, _ = Resolve(Config{TokenFile: fileP, DefaultPath: def})
	r.Cleanup()
	if _, err := os.Stat(fileP); err != nil {
		t.Error("Cleanup removed the operator's own --token-file")
	}

	// 5. The view token, from both sources; never generated.
	r, err = Resolve(Config{EnvToken: envTok, ViewTokenFile: viewP, DefaultPath: def})
	if err != nil {
		t.Fatal(err)
	}
	if r.ViewToken != viewTok {
		t.Errorf("--view-token-file not loaded: %q", r.ViewToken)
	}
	r, err = Resolve(Config{EnvToken: envTok, DefaultPath: def})
	if err != nil {
		t.Fatal(err)
	}
	if r.ViewToken != "" {
		t.Error("a view token must never be generated; with no source, viewers are disabled")
	}
	if r.Store.HasView() {
		t.Error("the store must report no view token")
	}

	// 6. A malformed env token is fatal rather than a silent fallback.
	if _, err := Resolve(Config{EnvToken: "short", DefaultPath: def}); err == nil {
		t.Error("a malformed MAGMUX_TOKEN must be an error")
	}
	if _, err := Resolve(Config{EnvToken: envTok, EnvViewToken: "short", DefaultPath: def}); err == nil {
		t.Error("a malformed MAGMUX_VIEW_TOKEN must be an error")
	}

	// 7. The same value for both is refused: a read-only credential that is
	//    also the full one grants everything while claiming not to.
	if _, err := Resolve(Config{EnvToken: envTok, EnvViewToken: envTok, DefaultPath: def}); err == nil {
		t.Error("the view token must not be allowed to equal the session token")
	}
	// And that refusal must not leave a generated file behind.
	os.Remove(def)
	if _, err := Resolve(Config{EnvViewToken: "short", DefaultPath: def}); err == nil {
		t.Error("expected a failure")
	}
	if _, err := os.Stat(def); err == nil {
		// EnvViewToken is validated after generation, so this is the path that
		// proves the cleanup on the failure branch works.
		t.Error("a failure after the token was generated must remove the file")
	}
}

// TestStoreDoesNotKeepTheRawToken is a small structural guarantee: nothing in
// the Store can be marshalled back into a usable credential, so a debug dump of
// one is not a leak.
func TestStoreDoesNotKeepTheRawToken(t *testing.T) {
	tok, _ := Generate()
	s := NewStore(tok, "", nil)
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), tok) {
		t.Errorf("the raw token survives in the Store: %s", b)
	}
	if string(b) != "{}" {
		t.Errorf("the Store has no exported fields, so it marshals as {}; got %s", b)
	}
}

// flip changes one character of a token, to build a near-miss.
func flip(s string) string {
	b := []byte(s)
	if b[0] == 'a' {
		b[0] = 'b'
	} else {
		b[0] = 'a'
	}
	return string(b)
}
