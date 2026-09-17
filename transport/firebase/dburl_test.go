package firebase

import (
	"net/http"
	"testing"
)

// TestIsDatabaseURL is the host predicate's table.
//
// The rejections are the point, and each one is a real way a suffix check or a
// Contains check gets this wrong: a look-alike host that ENDS in the right
// string, a look-alike that CONTAINS it, userinfo that moves the real host past
// a careless reader, a plaintext scheme, a non-443 port, a query and a
// fragment.
func TestIsDatabaseURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
		why  string
	}{
		{"https://p-default-rtdb.firebaseio.com", true, "the classic shape"},
		{"https://p-default-rtdb.firebaseio.com/", true, "a bare slash is still the root"},
		{"https://p-default-rtdb.firebaseio.com:443", true, "the default port, spelled out"},
		{"https://P-Default-RTDB.FIREBASEIO.COM", true, "hostnames are case-insensitive"},
		{"https://magmux.europe-west1.firebasedatabase.app", true, "the regional shape"},

		{"https://evil.com", false, "not Firebase at all"},
		{"https://x.firebaseio.com.evil.com", false, "a look-alike that ENDS elsewhere; a suffix check passes it"},
		{"https://evil.com/x.firebaseio.com", false, "a look-alike in the PATH; a Contains check passes it"},
		{"https://firebaseio.com", false, "no namespace label"},
		{"https://a.b.firebaseio.com", false, "an extra label: exact position, not a suffix"},
		{"https://magmux.firebasedatabase.app", false, "the regional shape needs a region label"},
		{"https://a.b.c.firebasedatabase.app.evil.com", false, "four labels, wrong ones"},
		{"https://p-default-rtdb.firebaseio.com@evil.com/", false, "userinfo: the real host is evil.com"},
		{"https://user:pw@p-default-rtdb.firebaseio.com", false, "userinfo at all"},
		{"http://p-default-rtdb.firebaseio.com", false, "plaintext: a token must never cross it"},
		{"https://p-default-rtdb.firebaseio.com:8443", false, "a non-443 port"},
		{"https://p-default-rtdb.firebaseio.com/?auth=x", false, "a query"},
		{"https://p-default-rtdb.firebaseio.com/#x", false, "a fragment"},
		{"https://p-default-rtdb.firebaseio.com/sub/path", false, "a path below the root"},
		{"https://-bad.firebaseio.com", false, "a label may not start with a hyphen"},
		{"https://bad-.firebaseio.com", false, "a label may not end with a hyphen"},
		{"https://p_default.firebaseio.com", false, "underscore is not a DNS label character"},
		{"https://p-default-rtdb.firebaseio.com.", false, "a trailing dot is a different string"},
		{"", false, "empty"},
		{"://nonsense", false, "unparseable"},
		{"magmux", false, "not a URL"},
		{"https://127.0.0.1:9000", false, "the emulator is not reached this way"},
	}
	for _, c := range cases {
		if got := IsDatabaseURL(c.url); got != c.want {
			t.Errorf("IsDatabaseURL(%q) = %v, want %v — %s", c.url, got, c.want, c.why)
		}
	}
}

// refusingTransport fails any request at all. It is how the next test proves
// the predicate runs BEFORE any I/O rather than merely alongside it.
type refusingTransport struct{ t *testing.T }

func (rt refusingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.t.Fatalf("a request was made to %s before the host predicate refused the config", r.URL)
	return nil, nil
}

// TestBadDatabaseURLNeverReachesTheNetwork pins the ordering that matters: a
// config naming a look-alike host is refused with no socket opened, so an admin
// credential is never presented to it.
func TestBadDatabaseURLNeverReachesTheNetwork(t *testing.T) {
	hc := &http.Client{Transport: refusingTransport{t}}
	for _, bad := range []string{
		"https://x.firebaseio.com.evil.com",
		"http://p-default-rtdb.firebaseio.com",
		"https://p-default-rtdb.firebaseio.com:8443",
		"https://p-default-rtdb.firebaseio.com@evil.com/",
	} {
		cfg := &Config{
			DatabaseURL: bad,
			Root:        "magmux",
			Host:        "test-host",
			Credentials: "/nonexistent/sa.json",
		}
		if err := cfg.normalize(); err == nil {
			t.Fatalf("normalize accepted %q", bad)
		}
		// And the client, which is the thing that would open the socket, is
		// never built for it.
		if _, err := New(cfg, Options{Hub: nil, SID: "s-1", HTTPClient: hc}); err == nil {
			t.Fatalf("New accepted %q", bad)
		}
	}
}

// TestTokenURIIsPinned: a credential file that names somebody else's token
// endpoint is refused at load, because the file is precisely the thing an
// attacker who can write it controls.
func TestTokenURIIsPinned(t *testing.T) {
	dir := t.TempDir()
	sa := writeServiceAccount(t, dir, "https://oauth2.evil.com/token")
	if _, err := loadServiceAccount(sa); err == nil {
		t.Fatal("loadServiceAccount accepted a foreign token_uri")
	}
	ok := writeServiceAccount(t, dir+"/ok", TokenURI)
	if _, err := loadServiceAccount(ok); err != nil {
		t.Fatalf("loadServiceAccount refused the pinned token_uri: %v", err)
	}
}
