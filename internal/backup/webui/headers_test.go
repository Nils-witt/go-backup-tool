package webui

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

func TestContentSecurityPolicyConnectSrc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
		want string
	}{
		{name: "issuer origin", opts: Options{OIDC: config.OIDCSettings{Issuer: "https://sso.example/realms/backup"}}, want: "connect-src 'self' https://sso.example;"},
		{name: "issuer with port", opts: Options{OIDC: config.OIDCSettings{Issuer: "http://localhost:8081/application/o/gbt/"}}, want: "connect-src 'self' http://localhost:8081;"},
		{name: "no issuer", want: "connect-src 'self';"},
		{name: "not a URL", opts: Options{OIDC: config.OIDCSettings{Issuer: "sso.example"}}, want: "connect-src 'self';"},
		{
			name: "sentry dsn without its key",
			opts: Options{
				OIDC:   config.OIDCSettings{Issuer: "https://sso.example/realms/backup"},
				Sentry: config.SentrySettings{DSN: "https://k@o1.ingest.sentry.io/1", FrontendDSN: "https://k@o1.ingest.sentry.io/2"},
			},
			want: "connect-src 'self' https://sso.example https://o1.ingest.sentry.io;",
		},
		{
			name: "shared origin listed once",
			opts: Options{OIDC: config.OIDCSettings{Issuer: "https://example.com/sso"}, Sentry: config.SentrySettings{FrontendDSN: "https://k@example.com/1"}},
			want: "connect-src 'self' https://example.com;",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := contentSecurityPolicy(cspConnectOrigins(tt.opts)); !strings.Contains(got, tt.want) {
				t.Errorf("contentSecurityPolicy() = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	tests := []struct {
		name         string
		path         string
		trustProxy   bool
		tls          bool
		header       map[string]string
		wantNoStore  bool
		wantHSTS     bool
		wantNextCode int
	}{
		{name: "dashboard over http", path: "/"},
		{name: "api over http", path: "/api/status", wantNoStore: true},
		{name: "api over tls", path: "/api/status", tls: true, wantNoStore: true, wantHSTS: true},
		{name: "x-forwarded-proto trusted", path: "/", trustProxy: true, header: map[string]string{"X-Forwarded-Proto": "https"}, wantHSTS: true},
		{name: "forwarded proto trusted", path: "/", trustProxy: true, header: map[string]string{"Forwarded": "for=192.0.2.1;proto=https"}, wantHSTS: true},
		{name: "forwarded proto http wins over x-forwarded-proto", path: "/", trustProxy: true, header: map[string]string{"Forwarded": "proto=http", "X-Forwarded-Proto": "https"}},
		{name: "x-forwarded-proto untrusted", path: "/", header: map[string]string{"X-Forwarded-Proto": "https"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}

			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}

			rec := httptest.NewRecorder()
			securityHeaders([]string{"https://sso.example"}, tt.trustProxy, next).ServeHTTP(rec, req)

			if rec.Code != http.StatusTeapot {
				t.Errorf("status = %d, want the wrapped handler's %d", rec.Code, http.StatusTeapot)
			}

			for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Cross-Origin-Opener-Policy"} {
				if rec.Header().Get(h) == "" {
					t.Errorf("%s header missing", h)
				}
			}

			if got := rec.Header().Get("Cache-Control") == "no-store"; got != tt.wantNoStore {
				t.Errorf("Cache-Control no-store = %v, want %v", got, tt.wantNoStore)
			}

			if got := rec.Header().Get("Strict-Transport-Security") != ""; got != tt.wantHSTS {
				t.Errorf("Strict-Transport-Security set = %v, want %v", got, tt.wantHSTS)
			}
		})
	}
}

func TestLoopbackOnly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", false},
		{"::", false},
		{"192.0.2.10", false},
	} {
		if got := loopbackOnly(&net.TCPAddr{IP: net.ParseIP(tc.ip), Port: 8080}); got != tc.want {
			t.Errorf("loopbackOnly(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}
