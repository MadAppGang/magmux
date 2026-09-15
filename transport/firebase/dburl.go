package firebase

// The host predicate: the one gate between a service-account credential and the
// network.

import (
	"fmt"
	"net/url"
	"strings"
)

// TokenURI is the ONLY token endpoint magmux will exchange a JWT at.
//
// It is pinned rather than read from the service-account file because the file
// is the thing an attacker who can write it controls: a `token_uri` pointing at
// their own host would hand them a signed assertion for the real account. The
// file may omit the field or state exactly this value; anything else is
// refused before a single byte leaves the process.
const TokenURI = "https://oauth2.googleapis.com/token"

// IsDatabaseURL reports whether s is a URL magmux may present an admin
// credential to.
//
// It is an EXACT-LABEL match and not a suffix match, which is the whole point:
// `x.firebaseio.com.evil.com` ends in nothing suspicious to a naive
// strings.HasSuffix, and `evil.com/x.firebaseio.com` fools a naive Contains.
// The two shapes Firebase actually serves are
//
//	https://<ns>.firebaseio.com
//	https://<ns>.<region>.firebasedatabase.app
//
// and everything else — a different scheme, userinfo, a port that is not 443, a
// query, a fragment, a path deeper than "/" — is refused. Userinfo matters more
// than it looks: `https://p-default-rtdb.firebaseio.com@evil.com/` parses with
// Host "evil.com", and a check that read u.User and ignored it would send the
// token there.
//
// It is used twice, and both are load-bearing: once on the configured
// databaseURL before any I/O, and once per redirect target, because Go strips
// the Authorization header on a cross-host redirect and re-adding it is only
// safe for a host that passes this.
func IsDatabaseURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return isDatabaseURL(u)
}

// isDatabaseURL is the parsed twin, so CheckRedirect does not re-serialise a
// *url.URL just to re-parse it.
func isDatabaseURL(u *url.URL) bool {
	switch {
	case u == nil:
		return false
	case u.Scheme != "https":
		return false
	case u.User != nil:
		// See above: userinfo moves the real host somewhere a careless reader
		// never looks.
		return false
	case u.RawQuery != "" || u.ForceQuery:
		return false
	case u.Fragment != "" || u.RawFragment != "":
		return false
	case u.Opaque != "":
		return false
	case u.Path != "" && u.Path != "/":
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	return isDatabaseHost(u.Hostname())
}

// isDatabaseHost is the label walk. The hostname arrives from url.Hostname(),
// which has already stripped the port and the brackets of an IPv6 literal — and
// an IPv6 literal has no dots, so it falls out at the label count.
func isDatabaseHost(host string) bool {
	host = strings.ToLower(host)
	// A trailing dot is a legal FQDN and a different string to every other
	// check in this file. Refuse rather than normalise: one spelling.
	if host == "" || strings.HasSuffix(host, ".") {
		return false
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if !validLabel(l) {
			return false
		}
	}
	switch len(labels) {
	case 3:
		return labels[1] == "firebaseio" && labels[2] == "com"
	case 4:
		return labels[2] == "firebasedatabase" && labels[3] == "app"
	}
	return false
}

// validLabel is the DNS label grammar narrowed to what these two domains use:
// [a-z0-9-], 1..63 characters, and not starting or ending with a hyphen.
func validLabel(l string) bool {
	if len(l) == 0 || len(l) > 63 {
		return false
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

// checkDatabaseURL is IsDatabaseURL with a reason, for the config error a human
// reads once at startup.
func checkDatabaseURL(s string) error {
	if s == "" {
		return fmt.Errorf("databaseURL is required")
	}
	if !IsDatabaseURL(s) {
		return fmt.Errorf("databaseURL %q is not a Firebase Realtime Database URL "+
			"(want https://<ns>.firebaseio.com or https://<ns>.<region>.firebasedatabase.app, "+
			"with no userinfo, port, query, fragment or path)", s)
	}
	return nil
}
