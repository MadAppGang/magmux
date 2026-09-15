package firebase

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testKey is one generated RSA key, reused across the tests in this file. 2048
// bits and generated once because key generation is the slowest thing here by
// two orders of magnitude.
var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

// writeServiceAccount writes a credential file holding testKey and returns its
// path. tokenURI may be "" to omit the field.
func writeServiceAccount(t *testing.T, dir, tokenURI string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(testKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sa := map[string]any{
		"type":            "service_account",
		"project_id":      "magmux-test",
		"private_key_id":  "kid-1",
		"private_key":     string(keyPEM),
		"client_email":    "magmux@magmux-test.iam.gserviceaccount.com",
		"client_id":       "1",
		"auth_uri":        "https://accounts.google.com/o/oauth2/auth",
		"client_x509_url": "https://example.invalid",
	}
	if tokenURI != "" {
		sa["token_uri"] = tokenURI
	}
	b, err := json.MarshalIndent(sa, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSignJWTVerifies is the assertion's own proof: magmux's JWT verifies with
// crypto/rsa against the public half of the key it claims to be signed by, and
// its claims say what the Firebase token endpoint requires.
//
// The verification is done with rsa.VerifyPKCS1v15 and nothing else — no JWT
// library on either side — so the test is checking the bytes rather than
// checking a library against itself.
func TestSignJWTVerifies(t *testing.T) {
	dir := t.TempDir()
	sa, err := loadServiceAccount(writeServiceAccount(t, dir, TokenURI))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1780000000, 0)
	tok, err := signJWT(sa, now)
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT has three parts; got %d", len(parts))
	}
	// Every part is base64url with NO padding. A padded segment is the most
	// common way a hand-rolled JWT is rejected by a server that never says why.
	for i, p := range parts {
		if strings.ContainsAny(p, "+/=") {
			t.Fatalf("part %d is not unpadded base64url: %q", i, p)
		}
	}

	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("the signature does not verify against the key in the credential file: %v", err)
	}

	var hdr map[string]any
	decodeSeg(t, parts[0], &hdr)
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" || hdr["kid"] != "kid-1" {
		t.Fatalf("header = %v; want RS256/JWT/kid-1", hdr)
	}
	var claims map[string]any
	decodeSeg(t, parts[1], &claims)
	if claims["iss"] != sa.ClientEmail {
		t.Errorf("iss = %v, want %v", claims["iss"], sa.ClientEmail)
	}
	if claims["aud"] != TokenURI {
		t.Errorf("aud = %v, want the pinned %v", claims["aud"], TokenURI)
	}
	if got := claims["scope"]; got != scopeDatabase+" "+scopeEmail {
		t.Errorf("scope = %v", got)
	}
	if claims["iat"] != float64(now.Unix()) {
		t.Errorf("iat = %v, want %d", claims["iat"], now.Unix())
	}
	if claims["exp"] != float64(now.Unix()+3600) {
		t.Errorf("exp = %v, want iat+3600", claims["exp"])
	}
}

func decodeSeg(t *testing.T, seg string, into any) {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode %q: %v", seg, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
}

// TestTokenIsCachedAndRefreshedEarly: one exchange serves many requests, the
// cache is dropped five minutes before expiry rather than at it, and
// Invalidate — which is what a 401 and `auth_revoked` call — forces a new one.
func TestTokenIsCachedAndRefreshedEarly(t *testing.T) {
	var exchanges atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := exchanges.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", got)
		}
		if r.Form.Get("assertion") == "" {
			t.Error("no assertion in the exchange")
		}
		_, _ = w.Write([]byte(`{"access_token":"tok` +
			string(rune('0'+n)) + `","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	sa, err := loadServiceAccount(writeServiceAccount(t, dir, TokenURI))
	if err != nil {
		t.Fatal(err)
	}
	// The endpoint is pinned in production; the test points the SOURCE at its
	// own server, which is the one field a test may move.
	sa.TokenURI = srv.URL

	clock := time.Unix(1780000000, 0)
	ts := newSATokens(sa, srv.Client())
	ts.now = func() time.Time { return clock }

	first, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		got, err := ts.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("the cache handed out %q then %q", first, got)
		}
	}
	if n := exchanges.Load(); n != 1 {
		t.Fatalf("%d exchanges for 6 requests; the token is not cached", n)
	}

	// Four minutes before expiry the margin has bitten; a second earlier it has
	// not. Both are checked, because a margin that is off by its own sign looks
	// exactly like a margin that works.
	clock = clock.Add(3600*time.Second - refreshMargin - time.Second)
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := exchanges.Load(); n != 1 {
		t.Fatalf("refreshed at %s before expiry; the margin is %s", refreshMargin+time.Second, refreshMargin)
	}
	clock = clock.Add(2 * time.Second)
	second, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("the token was not refreshed inside the margin")
	}
	if n := exchanges.Load(); n != 2 {
		t.Fatalf("exchanges = %d, want 2", n)
	}

	ts.Invalidate()
	third, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third == second {
		t.Fatal("Invalidate did not drop the cached token")
	}
}

// TestEmulatorNeverLoadsACredential: emulator mode uses the documented admin
// bypass and never opens a credential file — so a config that points at
// 127.0.0.1 cannot leak a real service-account key by accident.
func TestEmulatorNeverLoadsACredential(t *testing.T) {
	cfg := &Config{
		Root:     "magmux",
		Host:     "test-host",
		Emulator: &EmulatorConfig{Host: "127.0.0.1:9000", NS: "demo-magmux"},
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	c, err := newClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.tokens.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != emulatorToken {
		t.Fatalf("emulator token = %q, want %q (firebase-tools 15.19.1 lib/emulator/hubExport.js:152-157)",
			tok, emulatorToken)
	}
	if _, ok := c.tokens.(staticToken); !ok {
		t.Fatalf("emulator mode uses %T, which can reach a credential", c.tokens)
	}
	// And the URL carries the namespace, which is how a local emulator is told
	// which database it is serving.
	if got := c.nodeURL("magmux/x", nil); !strings.Contains(got, "ns=demo-magmux") {
		t.Fatalf("emulator URL %q carries no ns", got)
	}
}
