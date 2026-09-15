package auth

import (
	"sync"
	"time"
)

// TicketTTL is how long a ticket is redeemable. Thirty seconds is long enough
// for a page to mint one and open an EventSource, and short enough that a
// ticket captured from a proxy log or a browser history is already spent by the
// time anyone reads it.
const TicketTTL = 30 * time.Second

// maxTickets bounds the outstanding set. A ticket is 43 bytes plus a struct, so
// this is trivial memory — the cap exists so a client that mints in a loop and
// never redeems cannot grow the map without limit. Past it, the OLDEST
// unredeemed ticket is dropped: a ticket that has been sitting unused while
// 1,024 others were minted is one nobody is going to use.
const maxTickets = 1024

// Tickets is the single-use credential exchange that keeps a raw token out of
// every URL.
//
// EventSource cannot set a header and a WebSocket in a browser cannot either,
// so both need their credential in the query string — where it lands in the
// server's access log, the browser's history, and every Referer the page emits.
// A ticket is what goes there instead: minted by an authenticated request,
// valid once, valid for TicketTTL, and carrying nothing but the KIND of the
// caller that minted it.
//
// A ticket inherits ReadOnly from its minter, so a viewer cannot mint its way
// up to the full token's capabilities.
type Tickets struct {
	mu  sync.Mutex
	m   map[string]ticket
	ttl time.Duration
	now func() time.Time
}

type ticket struct {
	kind    Kind
	expires time.Time
	minted  time.Time
}

// NewTickets returns an empty exchange. A ttl of 0 takes TicketTTL.
func NewTickets(ttl time.Duration) *Tickets {
	if ttl <= 0 {
		ttl = TicketTTL
	}
	return &Tickets{m: make(map[string]ticket), ttl: ttl, now: time.Now}
}

// TTL is how long a freshly minted ticket lasts. The HTTP reply reports it, so
// a client can schedule a re-mint rather than discovering the expiry as a 401.
func (t *Tickets) TTL() time.Duration { return t.ttl }

// Mint issues one ticket for a caller of the given kind.
func (t *Tickets) Mint(k Kind) (string, error) {
	s, err := Generate()
	if err != nil {
		return "", err
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(now)
	if len(t.m) >= maxTickets {
		t.dropOldestLocked()
	}
	t.m[s] = ticket{kind: k, expires: now.Add(t.ttl), minted: now}
	return s, nil
}

// Redeem spends a ticket and reports the kind it was minted for.
//
// It is single use in the strongest sense available here: the entry is deleted
// under the same lock that read it, so two concurrent redemptions of one ticket
// cannot both succeed.
//
// An expired or already-spent ticket is indistinguishable from one that never
// existed, and both answer None. The caller turns that into a 401, which
// EventSource treats as fatal — the client then mints a fresh ticket and opens
// a new stream, which is the documented reconnect (a new aggregate, new
// keyframes) rather than an endless retry loop against a spent credential.
func (t *Tickets) Redeem(s string) (Kind, bool) {
	if s == "" {
		return None, false
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.m[s]
	if !ok {
		return None, false
	}
	delete(t.m, s)
	if now.After(tk.expires) {
		return None, false
	}
	return tk.kind, true
}

// Outstanding is how many unspent, unexpired tickets exist. For tests and for
// the debug log; nothing branches on it.
func (t *Tickets) Outstanding() int {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(now)
	return len(t.m)
}

func (t *Tickets) sweepLocked(now time.Time) {
	for s, tk := range t.m {
		if now.After(tk.expires) {
			delete(t.m, s)
		}
	}
}

func (t *Tickets) dropOldestLocked() {
	var oldest string
	var at time.Time
	for s, tk := range t.m {
		if oldest == "" || tk.minted.Before(at) {
			oldest, at = s, tk.minted
		}
	}
	delete(t.m, oldest)
}
