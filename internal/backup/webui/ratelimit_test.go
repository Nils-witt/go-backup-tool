package webui

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// fakeClock is a manually advanced clock for rateLimitWithClock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

func newRateLimitTest(s config.RateLimitSettings, status int) (http.Handler, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
	log := slog.New(slog.DiscardHandler)

	return rateLimitWithClock(s, false, log, clock.Now, next), clock
}

func serveFrom(t *testing.T, h http.Handler, remoteAddr, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func TestRateLimitRequests(t *testing.T) {
	t.Parallel()

	h, clock := newRateLimitTest(config.RateLimitSettings{RequestsPerSecond: 1, Burst: 3}, http.StatusOK)

	for i := range 3 {
		if rec := serveFrom(t, h, "192.0.2.1:1000", "/api/status"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 within the burst", i, rec.Code)
		}
	}

	rec := serveFrom(t, h, "192.0.2.1:1001", "/api/status")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request over the burst: status = %d, want 429", rec.Code)
	}

	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}

	if rec := serveFrom(t, h, "192.0.2.2:1000", "/api/status"); rec.Code != http.StatusOK {
		t.Errorf("other client: status = %d, want 200 (limits are per client)", rec.Code)
	}

	if rec := serveFrom(t, h, "192.0.2.1:1000", "/assets/index.js"); rec.Code != http.StatusOK {
		t.Errorf("static asset: status = %d, want 200 (only /api/ is limited)", rec.Code)
	}

	clock.Advance(time.Second)

	if rec := serveFrom(t, h, "192.0.2.1:1000", "/api/status"); rec.Code != http.StatusOK {
		t.Errorf("after refill: status = %d, want 200", rec.Code)
	}
}

func TestRateLimitAuthFailures(t *testing.T) {
	t.Parallel()

	s := config.RateLimitSettings{RequestsPerSecond: 1000, Burst: 1000, AuthFailuresPerMinute: 6, AuthFailureBurst: 2}
	h, clock := newRateLimitTest(s, http.StatusUnauthorized)

	for i := range 2 {
		if rec := serveFrom(t, h, "192.0.2.1:1000", "/api/me"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want the handler's 401", i, rec.Code)
		}
	}

	rec := serveFrom(t, h, "192.0.2.1:1000", "/api/me")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after the failure burst: status = %d, want 429", rec.Code)
	}

	// 6 per minute refills one failure every 10s.
	if got, _ := strconv.Atoi(rec.Header().Get("Retry-After")); got != 10 {
		t.Errorf("Retry-After = %d, want 10", got)
	}

	clock.Advance(10 * time.Second)

	if rec := serveFrom(t, h, "192.0.2.1:1000", "/api/me"); rec.Code != http.StatusUnauthorized {
		t.Errorf("after refill: status = %d, want the handler's 401 again", rec.Code)
	}
}

func TestRateLimitSuccessDoesNotCountAsFailure(t *testing.T) {
	t.Parallel()

	s := config.RateLimitSettings{RequestsPerSecond: 1000, Burst: 1000, AuthFailuresPerMinute: 1, AuthFailureBurst: 1}
	h, _ := newRateLimitTest(s, http.StatusForbidden)

	for i := range 5 {
		if rec := serveFrom(t, h, "192.0.2.1:1000", "/api/tokens"); rec.Code != http.StatusForbidden {
			t.Fatalf("request %d: status = %d, want 403 (only 401 counts as an authentication failure)", i, rec.Code)
		}
	}
}

func TestRateLimitDisabled(t *testing.T) {
	t.Parallel()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := rateLimit(config.RateLimitSettings{Disabled: true, RequestsPerSecond: 1, Burst: 1}, false, slog.New(slog.DiscardHandler), next)

	for i := range 5 {
		if rec := serveFrom(t, h, "192.0.2.1:1000", "/api/status"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 with the limits disabled", i, rec.Code)
		}
	}
}

func TestRateLimitKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		want string
	}{
		{addr: "192.0.2.1:4711", want: "192.0.2.1"},
		{addr: "192.0.2.1", want: "192.0.2.1"},
		{addr: "[::ffff:192.0.2.1]:4711", want: "192.0.2.1"},
		{addr: "[2001:db8:cafe:1:2:3:4:5]:4711", want: "2001:db8:cafe:1::/64"},
		{addr: "2001:db8:cafe:1::17", want: "2001:db8:cafe:1::/64"},
		{addr: "[2001:db8:cafe:1::17]", want: "2001:db8:cafe:1::/64"},
		{addr: "unknown", want: "unknown"},
	}

	for _, tt := range tests {
		if got := rateLimitKey(tt.addr); got != tt.want {
			t.Errorf("rateLimitKey(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}

func TestClientLimiterSweepsIdleClients(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newClientLimiter(config.RateLimitSettings{}, start)

	l.get("a", start)

	// Each get sweeps at most once per rateLimitSweepInterval.
	l.get("b", start.Add(rateLimitIdleAfter-time.Second))

	if _, ok := l.clients["a"]; !ok {
		t.Fatal("client a was swept before it was idle for rateLimitIdleAfter")
	}

	l.get("b", start.Add(rateLimitIdleAfter-time.Second+rateLimitSweepInterval))

	if _, ok := l.clients["a"]; ok {
		t.Error("client a is still tracked after being idle for rateLimitIdleAfter")
	}
}
