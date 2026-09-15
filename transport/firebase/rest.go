package firebase

// The REST client: every byte that leaves this package goes through here.
//
// Two things in it are not ordinary HTTP plumbing.
//
//   - **The redirect.** RTDB answers a request to the project URL with a 307 to
//     the instance that actually holds the data. Go strips Authorization on a
//     cross-host redirect, which is right, so the header has to be put back —
//     and putting it back unconditionally would hand an admin credential to
//     whoever answered the redirect. `authFor` is the same predicate that
//     admitted the original URL, so a redirect can only ever move the token
//     between hosts magmux would have talked to anyway.
//
//   - **The status classes.** A 400 from RTDB means one PATH in the batch is
//     unacceptable, and the whole batch is refused for it; a 401 means the token
//     died early; 429 and 5xx mean try later. They are three different remedies,
//     so they are three different types rather than one error string the caller
//     has to pattern-match.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpError is a non-2xx answer from RTDB, with enough of the body to diagnose
// it and none of the body's ability to fill a log.
type httpError struct {
	Status int
	Body   string
	URL    string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("firebase: %s returned %d: %s", e.URL, e.Status, e.Body)
}

// retryable reports whether waiting could plausibly help: rate limiting, a
// server fault, or a gateway in between.
func (e *httpError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// badPath reports the 400 case: RTDB rejected something about the request's
// own shape — a key with a `$` in it, an ancestor and a descendant in one
// multi-path write, a value too deep. Retrying the same batch cannot help, and
// the caller's remedy is to find which path it was.
func (e *httpError) badPath() bool { return e.Status == http.StatusBadRequest }

// unauthorized reports the case where the credential has to be replaced rather
// than the request retried.
func (e *httpError) unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// asHTTPError is the unwrap, written once.
func asHTTPError(err error) (*httpError, bool) {
	var he *httpError
	if errors.As(err, &he) {
		return he, true
	}
	return nil, false
}

// client is the RTDB REST surface magmux uses: a multi-path PATCH, a PUT, a
// GET and an SSE stream.
type client struct {
	base   *url.URL
	ns     string // emulator namespace, "" in production
	tokens tokenSource
	hc     *http.Client
	// authFor decides whether the Authorization header may be presented to a
	// URL. In production it is isDatabaseURL; in emulator mode it is
	// same-host, which is what lets the fake RTDB in the tests exercise the
	// real redirect path.
	authFor func(*url.URL) bool
}

// newClient builds the client a Config describes. It performs no I/O: the
// predicate has already run over databaseURL in normalize(), and this is only
// the assembly.
func newClient(cfg *Config, hc *http.Client) (*client, error) {
	c := &client{hc: hc}
	if cfg.Emulator != nil {
		u, err := url.Parse("http://" + cfg.Emulator.Host)
		if err != nil {
			return nil, fmt.Errorf("firebase: emulator.host %q: %w", cfg.Emulator.Host, err)
		}
		host := u.Host
		c.base = u
		c.ns = cfg.Emulator.NS
		c.tokens = staticToken(emulatorToken)
		// Same host only. The emulator is a local process; a redirect off it is
		// not a thing that happens, and if it did it would not be getting a
		// header.
		c.authFor = func(t *url.URL) bool { return t != nil && t.Host == host }
	} else {
		u, err := url.Parse(cfg.DatabaseURL)
		if err != nil {
			return nil, fmt.Errorf("firebase: databaseURL %q: %w", cfg.DatabaseURL, err)
		}
		sa, err := loadServiceAccount(cfg.Credentials)
		if err != nil {
			return nil, err
		}
		c.base = u
		c.tokens = newSATokens(sa, hc)
		c.authFor = isDatabaseURL
	}
	if c.hc == nil {
		c.hc = newHTTPClient(c.authFor)
	}
	return c, nil
}

// newHTTPClient builds the http.Client with the redirect rule installed.
//
// The timeout is per REQUEST and deliberately absent: the SSE stream is a
// request that never ends, and a client-level timeout would cut it every 30
// seconds. Every call site passes a ctx instead, which bounds exactly the calls
// that should be bounded.
func newHTTPClient(authFor func(*url.URL) bool) *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("firebase: stopped after 5 redirects")
			}
			// Go has already removed Authorization if the host changed. Put it
			// back only for a host the predicate admits — and read it off the
			// ORIGINAL request, because that is the only one that ever had it.
			if req.Header.Get("Authorization") == "" && authFor(req.URL) {
				if auth := via[0].Header.Get("Authorization"); auth != "" {
					req.Header.Set("Authorization", auth)
				}
			}
			return nil
		},
	}
}

// nodeURL builds the REST URL for one node path.
//
// The `.json` suffix is RTDB's; `?ns=` is the emulator's way of naming the
// namespace, since a local emulator has no per-project hostname to carry it.
func (c *client) nodeURL(path string, q url.Values) string {
	u := *c.base
	u.Path = "/" + strings.TrimPrefix(path, "/") + ".json"
	if q == nil {
		q = url.Values{}
	}
	if c.ns != "" {
		q.Set("ns", c.ns)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// request builds an authenticated request. It is separate from do so the SSE
// listener, which does not read the body to completion, can share the auth.
func (c *client) request(ctx context.Context, method, rawURL string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do runs one request and reads its body.
//
// A 401 invalidates the cached token and retries ONCE. That is not a retry loop
// in disguise: a token that was refused twice is a configuration problem, and
// hammering an auth endpoint is how a misconfigured client gets a project rate
// limited.
func (c *client) do(ctx context.Context, method, path string, q url.Values, body []byte) ([]byte, error) {
	out, err := c.do1(ctx, method, path, q, body)
	if he, ok := asHTTPError(err); ok && he.unauthorized() {
		c.tokens.Invalidate()
		return c.do1(ctx, method, path, q, body)
	}
	return out, err
}

func (c *client) do1(ctx context.Context, method, path string, q url.Values, body []byte) ([]byte, error) {
	rawURL := c.nodeURL(path, q)
	req, err := c.request(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpError{Status: resp.StatusCode, Body: trimForLog(out), URL: redact(rawURL)}
	}
	return out, nil
}

// patch sends one multi-path update, silently.
//
// `print=silent` makes RTDB answer 204 with no body. On a session that writes
// twice a second for hours, echoing every value back would double the bytes for
// information nothing reads.
func (c *client) patch(ctx context.Context, path string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPatch, path, url.Values{"print": {"silent"}}, b)
	return err
}

// put replaces one node wholesale.
func (c *client) put(ctx context.Context, path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPut, path, url.Values{"print": {"silent"}}, b)
	return err
}

// get reads one node.
func (c *client) get(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, nil, nil)
}

// stream opens an SSE subscription on a node. The caller owns the response body
// and must close it; the ctx is what ends the stream.
func (c *client) stream(ctx context.Context, path string) (*http.Response, error) {
	rawURL := c.nodeURL(path, nil)
	req, err := c.request(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return nil, &httpError{Status: resp.StatusCode, Body: trimForLog(body), URL: redact(rawURL)}
	}
	return resp, nil
}

// redact strips the query from a URL before it reaches a log line. The query
// carries `?auth=` in the emulator harness and `?ns=`, neither of which belongs
// in a debug file that a human will paste into an issue.
func redact(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// backoff is the retry schedule for a retryable failure: 1 s doubling to 60 s,
// with jitter so several sessions that lost the same network do not come back
// in lockstep.
type backoff struct {
	cur time.Duration
	// jitter is a function so a test can make the schedule exact.
	jitter func(time.Duration) time.Duration
}

const (
	backoffMin = 1 * time.Second
	backoffMax = 60 * time.Second
)

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = backoffMin
	} else {
		b.cur *= 2
		if b.cur > backoffMax {
			b.cur = backoffMax
		}
	}
	if b.jitter != nil {
		return b.jitter(b.cur)
	}
	return b.cur
}

func (b *backoff) reset() { b.cur = 0 }
