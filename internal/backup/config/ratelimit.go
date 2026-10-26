package config

import (
	"fmt"
	"math"
)

// Defaults for webui.rate-limit: (see fileWebUIRateLimit). The request
// limit leaves room for a dashboard page load's burst of parallel fetches
// and the 2s status polling the dashboard falls back to without its live
// WebSocket; the failure limit lets a user whose token just expired retry
// a few times while stopping token guessing and log flooding.
const (
	DefaultRateLimitRequestsPerSecond     = 20
	DefaultRateLimitBurst                 = 100
	DefaultRateLimitAuthFailuresPerMinute = 10
	DefaultRateLimitAuthFailureBurst      = 20
)

// fileWebUIRateLimit is webui.rate-limit:, the per-client limits on the
// web UI's and receiver API's /api/... requests (see the webui package's
// rateLimit middleware). A client is its address as clientAddr records
// it, so behind a reverse proxy set webui.trust-proxy-headers, or every
// client shares the proxy's limits. Each zero field takes its default.
type fileWebUIRateLimit struct {
	// Disabled turns both limits off, e.g. when a reverse proxy or WAF in
	// front already rate-limits.
	Disabled bool `yaml:"disabled"`

	// RequestsPerSecond and Burst limit every /api/... request a client
	// makes, as a token bucket: Burst requests at once, refilled at
	// RequestsPerSecond. A client over it gets 429 Too Many Requests.
	RequestsPerSecond float64 `yaml:"requests-per-second"`
	Burst             int     `yaml:"burst"`

	// AuthFailuresPerMinute and AuthFailureBurst limit failed
	// authentications (any 401 response: a rejected SSO or API token, or a
	// rejected receiver request token). Once a client has used up
	// AuthFailureBurst failures, every /api/... request it makes gets 429
	// until the bucket refills at AuthFailuresPerMinute.
	AuthFailuresPerMinute float64 `yaml:"auth-failures-per-minute"`
	AuthFailureBurst      int     `yaml:"auth-failure-burst"`
}

// RateLimitSettings is webui.rate-limit: after validation, with every
// default filled in.
type RateLimitSettings struct {
	Disabled              bool
	RequestsPerSecond     float64
	Burst                 int
	AuthFailuresPerMinute float64
	AuthFailureBurst      int
}

// resolveRateLimit validates cfg and fills in its defaults. Negative,
// infinite or NaN values are rejected rather than silently disabling a
// limit; Disabled is the one way to turn the limits off.
func resolveRateLimit(cfg fileWebUIRateLimit) (RateLimitSettings, error) {
	s := RateLimitSettings(cfg)

	for _, f := range []struct {
		name string
		v    float64
	}{
		{"requests-per-second", s.RequestsPerSecond},
		{"burst", float64(s.Burst)},
		{"auth-failures-per-minute", s.AuthFailuresPerMinute},
		{"auth-failure-burst", float64(s.AuthFailureBurst)},
	} {
		if f.v < 0 || math.IsInf(f.v, 0) || math.IsNaN(f.v) {
			return RateLimitSettings{}, fmt.Errorf("webui.rate-limit.%s must be a positive number, or unset for its default (use disabled: true to turn the limits off)", f.name)
		}
	}

	return s.WithDefaults(), nil
}

// WithDefaults returns s with every zero limit replaced by its default, so
// the zero RateLimitSettings means "the default limits".
func (s RateLimitSettings) WithDefaults() RateLimitSettings {
	if s.RequestsPerSecond <= 0 {
		s.RequestsPerSecond = DefaultRateLimitRequestsPerSecond
	}

	if s.Burst <= 0 {
		s.Burst = DefaultRateLimitBurst
	}

	if s.AuthFailuresPerMinute <= 0 {
		s.AuthFailuresPerMinute = DefaultRateLimitAuthFailuresPerMinute
	}

	if s.AuthFailureBurst <= 0 {
		s.AuthFailureBurst = DefaultRateLimitAuthFailureBurst
	}

	return s
}
