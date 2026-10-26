package receiver

import (
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
)

// replaySweepInterval is how often replayCache drops expired entries.
const replaySweepInterval = time.Minute

// replayCache remembers every request token jti a receiver has accepted
// until the token expires, so a captured token can't be sent a second
// time — not even for the same request, e.g. to put back a backup the
// sender has since replaced. Tokens are keyed by issuer and jti: the jti
// is only unique per sender. Its size is bounded by how many tokens
// trusted senders mint within one token lifetime. The zero value is ready
// to use. Safe for concurrent use.
type replayCache struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	lastSweep time.Time
}

// use records issuer's token id, valid until expiry, as used at now,
// reporting false if it already was. An entry is kept until expiry plus
// jwt.DefaultLeeway, the clock skew remoteauth.VerifyRemoteAuthToken
// still accepts an expired token for.
func (c *replayCache) use(issuer, id string, expiry, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.seen == nil {
		c.seen = make(map[string]time.Time)
	}

	if now.Sub(c.lastSweep) >= replaySweepInterval {
		for k, until := range c.seen {
			if now.After(until) {
				delete(c.seen, k)
			}
		}

		c.lastSweep = now
	}

	key := issuer + "\x00" + id
	if until, ok := c.seen[key]; ok && !now.After(until) {
		return false
	}

	c.seen[key] = expiry.Add(jwt.DefaultLeeway)

	return true
}
