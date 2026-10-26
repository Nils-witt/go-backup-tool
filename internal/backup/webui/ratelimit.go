package webui

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

const (
	// rateLimitSweepInterval is how often clientLimiter drops idle clients.
	rateLimitSweepInterval = time.Minute

	// rateLimitIdleAfter is how long a client must have made no request
	// before clientLimiter forgets it. By then both its buckets have
	// refilled under any sensible settings, so forgetting it loses nothing.
	rateLimitIdleAfter = 10 * time.Minute

	// maxRateLimitClients bounds how many clients clientLimiter tracks one
	// by one; beyond it (e.g. a flood from many addresses), new clients
	// share one overflow entry until idle ones are swept.
	maxRateLimitClients = 100_000

	// ipv6RateLimitPrefix groups IPv6 clients by their /64 network, since a
	// single host usually controls a whole /64.
	ipv6RateLimitPrefix = 64
)

// clientLimits are one client's two token buckets (see rateLimit), plus
// what clientLimiter needs to log and sweep it.
type clientLimits struct {
	requests *rate.Limiter
	failures *rate.Limiter
	lastSeen time.Time

	// limited is set once the request limit rejects this client and
	// cleared by its next allowed request, so each episode logs only once.
	limited atomic.Bool
}

// clientLimiter tracks clientLimits per client key (see rateLimitKey).
// Safe for concurrent use.
type clientLimiter struct {
	settings config.RateLimitSettings

	mu        sync.Mutex
	clients   map[string]*clientLimits
	overflow  *clientLimits
	lastSweep time.Time
}

func newClientLimiter(s config.RateLimitSettings, now time.Time) *clientLimiter {
	l := &clientLimiter{settings: s.WithDefaults(), clients: make(map[string]*clientLimits), lastSweep: now}
	l.overflow = l.newLimits()

	return l
}

func (l *clientLimiter) newLimits() *clientLimits {
	return &clientLimits{
		requests: rate.NewLimiter(rate.Limit(l.settings.RequestsPerSecond), l.settings.Burst),
		failures: rate.NewLimiter(rate.Limit(l.settings.AuthFailuresPerMinute/60), l.settings.AuthFailureBurst),
	}
}

// get returns key's limits, creating them on first use, and sweeps idle
// clients at most once per rateLimitSweepInterval.
func (l *clientLimiter) get(key string, now time.Time) *clientLimits {
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) >= rateLimitSweepInterval {
		for k, c := range l.clients {
			if now.Sub(c.lastSeen) >= rateLimitIdleAfter {
				delete(l.clients, k)
			}
		}

		l.lastSweep = now
	}

	c, ok := l.clients[key]
	if !ok {
		if len(l.clients) >= maxRateLimitClients {
			c = l.overflow
		} else {
			c = l.newLimits()
			l.clients[key] = c
		}
	}

	c.lastSeen = now

	return c
}

// rateLimit wraps next, limiting each client's /api/... requests (the web
// UI's JSON API and the receiver API alike) with two token buckets from s:
// one for every request, and one for failed authentications, which next
// reports as a 401 response. A client over the request limit, or with no
// failures left, gets 429 Too Many Requests with a Retry-After header
// instead of reaching next — so a client guessing tokens, or flooding the
// login log with rejected ones, is held off without slowing anyone whose
// requests succeed. The dashboard shell and its static assets aren't
// limited. A client is keyed by its address (see clientAddr and
// rateLimitKey). s.Disabled returns next unchanged.
func rateLimit(s config.RateLimitSettings, trustProxyHeaders bool, log *slog.Logger, next http.Handler) http.Handler {
	if s.Disabled {
		return next
	}

	return rateLimitWithClock(s, trustProxyHeaders, log, time.Now, next)
}

func rateLimitWithClock(s config.RateLimitSettings, trustProxyHeaders bool, log *slog.Logger, now func() time.Time, next http.Handler) http.Handler {
	limiter := newClientLimiter(s, now())

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		t := now()
		key := rateLimitKey(clientAddr(r, trustProxyHeaders))
		c := limiter.get(key, t)

		if c.failures.TokensAt(t) < 1 {
			tooManyRequests(w, c.failures, t)
			return
		}

		if !c.requests.AllowN(t, 1) {
			if !c.limited.Swap(true) {
				log.Warn("web UI: client exceeded the request rate limit", "client", key, "path", r.URL.Path)
			}

			tooManyRequests(w, c.requests, t)

			return
		}

		c.limited.Store(false)

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		if sw.status != http.StatusUnauthorized {
			return
		}

		c.failures.AllowN(t, 1)

		if c.failures.TokensAt(t) < 1 {
			log.Warn("web UI: client blocked after repeated authentication failures", "client", key, "path", r.URL.Path)
		}
	})
}

// tooManyRequests answers a rate-limited request: 429, with Retry-After
// set to the whole seconds until lim has a token again.
func tooManyRequests(w http.ResponseWriter, lim *rate.Limiter, now time.Time) {
	wait := 1.0
	if perSecond := float64(lim.Limit()); perSecond > 0 {
		wait = math.Max(1, math.Ceil((1-lim.TokensAt(now))/perSecond))
	}

	w.Header().Set("Retry-After", strconv.Itoa(int(wait)))
	http.Error(w, "too many requests", http.StatusTooManyRequests)
}

// rateLimitKey turns a client address from clientAddr ("host:port", a
// bare IP, or anything a proxy header held) into the key rateLimit counts
// it under: the IP alone, with an IPv4-mapped address as plain IPv4 and an
// IPv6 address reduced to its /64 network. A value that isn't an IP is
// used as it is.
func rateLimitKey(addr string) string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}

	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return host
	}

	ip = ip.Unmap().WithZone("")
	if ip.Is4() {
		return ip.String()
	}

	prefix, err := ip.Prefix(ipv6RateLimitPrefix)
	if err != nil {
		return ip.String()
	}

	return prefix.String()
}
