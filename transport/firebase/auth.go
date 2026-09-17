package firebase

// Service-account auth: a self-signed JWT exchanged for an access token.
//
// There is no Google SDK here and there will not be one — magmux's dependency
// list is two golang.org/x packages, and the whole of this flow is a signature
// and a form POST. What it costs is that the three things an SDK would get
// right have to be got right here: the assertion is signed with the key from
// the file and nothing else, the token endpoint is PINNED rather than read out
// of that file, and the token is cached with a refresh margin so a long session
// does not re-authenticate once a second.

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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Scopes the assertion asks for. Both are what the RTDB REST API documents for
// an admin credential; userinfo.email is what makes `auth.uid` resolvable in
// the security rules.
const (
	scopeDatabase = "https://www.googleapis.com/auth/firebase.database"
	scopeEmail    = "https://www.googleapis.com/auth/userinfo.email"

	// jwtTTL is the assertion's own lifetime. One hour is the maximum Google
	// accepts.
	jwtTTL = time.Hour
	// refreshMargin is how long before expiry a cached token is replaced. Five
	// minutes is far longer than any request here can take, so a token handed
	// out is a token that will still be valid when it arrives.
	refreshMargin = 5 * time.Minute
	// emulatorToken is the RTDB emulator's admin bypass. It is not in the
	// public documentation; the source is firebase-tools 15.19.1,
	// lib/emulator/hubExport.js:152-157, which uses exactly this header for the
	// emulator's own export. It is a magic string and not a credential — which
	// is the point: emulator mode never reads a credential file at all.
	emulatorToken = "owner"
)

// tokenSource hands out the bearer token for one RTDB request.
//
// It is an interface with two implementations because the difference between
// production and the emulator is exactly this and nothing else: one signs a JWT
// and talks to Google, the other returns a constant. Everything downstream — the
// REST client, the mirror, the command listener — is written once.
type tokenSource interface {
	// Token returns a bearer token, refreshing if the cached one is close to
	// expiry.
	Token(ctx context.Context) (string, error)
	// Invalidate drops the cached token. Called on a 401 and on the SSE
	// `auth_revoked` event, which are the two ways a token can stop working
	// before its stated expiry.
	Invalidate()
}

// staticToken is the emulator's source.
type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }
func (staticToken) Invalidate()                             {}

// serviceAccount is the part of the JSON file magmux uses.
type serviceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`

	key *rsa.PrivateKey
}

// loadServiceAccount reads and checks a service-account file.
//
// The token_uri check happens HERE, at load, rather than at the first request:
// this function is called before mux.init(), where a failure is a readable line
// on a normal terminal, and a credential file that names somebody else's token
// endpoint is a thing a human needs to be told about immediately.
func loadServiceAccount(path string) (*serviceAccount, error) {
	p, err := expandHome(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("firebase credentials %s: %w", path, err)
	}
	var sa serviceAccount
	if err := json.Unmarshal(b, &sa); err != nil {
		return nil, fmt.Errorf("firebase credentials %s: %w", path, err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, fmt.Errorf("firebase credentials %s: needs client_email and private_key", path)
	}
	if sa.TokenURI != "" && sa.TokenURI != TokenURI {
		return nil, fmt.Errorf("firebase credentials %s: token_uri is %q; magmux only ever exchanges a JWT at %s",
			path, sa.TokenURI, TokenURI)
	}
	sa.TokenURI = TokenURI
	key, err := parsePrivateKey(sa.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("firebase credentials %s: %w", path, err)
	}
	sa.key = key
	return &sa, nil
}

// parsePrivateKey decodes the PEM block a service-account file carries.
//
// PKCS#8 is what Google issues, and x509.ParsePKCS8PrivateKey is what the
// architecture names. PKCS#1 is accepted too because a key round-tripped
// through openssl comes back in that form, and refusing it would be a riddle
// with no security value — both are the same RSA key.
func parsePrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("private_key is not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private_key is %T; Google service accounts use RSA", k)
		}
		return rk, nil
	}
	rk, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private_key could not be parsed as PKCS#8 or PKCS#1: %w", err)
	}
	return rk, nil
}

// signJWT builds and signs the assertion for the given instant.
//
// now is a parameter rather than a call to time.Now so the test can check the
// claims it produced, byte for byte, against a signature it verified with
// rsa.VerifyPKCS1v15.
func signJWT(sa *serviceAccount, now time.Time) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	if sa.PrivateKeyID != "" {
		header["kid"] = sa.PrivateKeyID
	}
	iat := now.Unix()
	claims := map[string]any{
		"iss":   sa.ClientEmail,
		"scope": scopeDatabase + " " + scopeEmail,
		"aud":   sa.TokenURI,
		"iat":   iat,
		"exp":   iat + int64(jwtTTL/time.Second),
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64(hb) + "." + b64(cb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, sa.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// b64 is JWT's base64: URL alphabet, no padding.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// saTokens is the production token source: one cached access token behind a
// mutex, refreshed early.
type saTokens struct {
	sa *serviceAccount
	hc *http.Client
	// now is time.Now, replaceable in a test that needs an expired cache
	// without sleeping.
	now func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newSATokens(sa *serviceAccount, hc *http.Client) *saTokens {
	return &saTokens{sa: sa, hc: hc, now: time.Now}
}

// Token returns a cached token or fetches a new one.
//
// The whole fetch happens under the mutex. That serialises two goroutines that
// both found the cache stale into one HTTP request instead of two, which is
// what you want from a credential endpoint — and the lock is never held across
// anything else, because this is the only method that takes it.
func (t *saTokens) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Before(t.expires.Add(-refreshMargin)) {
		return t.token, nil
	}
	tok, ttl, err := t.fetch(ctx)
	if err != nil {
		return "", err
	}
	t.token, t.expires = tok, t.now().Add(ttl)
	return tok, nil
}

// Invalidate drops the cache so the next Token fetches.
func (t *saTokens) Invalidate() {
	t.mu.Lock()
	t.token, t.expires = "", time.Time{}
	t.mu.Unlock()
}

// fetch exchanges a fresh assertion for an access token. Caller holds t.mu.
func (t *saTokens) fetch(ctx context.Context) (string, time.Duration, error) {
	assertion, err := signJWT(t.sa, t.now())
	if err != nil {
		return "", 0, fmt.Errorf("firebase: signing the assertion: %w", err)
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("firebase: token exchange: %w", err)
	}
	defer resp.Body.Close()
	// Bounded: an error page from a hijacked endpoint is not something to read
	// into memory without a limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("firebase: token exchange: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("firebase: token exchange returned %d: %s", resp.StatusCode, trimForLog(body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", 0, fmt.Errorf("firebase: token exchange returned unparseable JSON: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("firebase: token exchange returned no access_token")
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = jwtTTL
	}
	return out.AccessToken, ttl, nil
}

// trimForLog bounds an upstream error body before it reaches a log line. A
// token endpoint's failure body is small; anything large is a proxy's HTML.
func trimForLog(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
