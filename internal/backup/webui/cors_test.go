package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetCORSMiddleware covers webui.cors-get-origins: only GET/HEAD (and
// their preflights) on /api/... from an allowed origin get CORS headers;
// writes, other paths and other origins get none.
func TestGetCORSMiddleware(t *testing.T) {
	t.Parallel()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	tests := []struct {
		name          string
		allowed       []string
		method        string
		path          string
		origin        string
		preflightFor  string
		wantAllow     string
		wantStatus    int
		wantPreflight bool
	}{
		{name: "allowed origin GET", allowed: []string{"https://a.example"}, method: http.MethodGet, path: "/api/status", origin: "https://a.example", wantAllow: "https://a.example", wantStatus: http.StatusTeapot},
		{name: "allowed origin HEAD", allowed: []string{"https://a.example"}, method: http.MethodHead, path: "/api/status", origin: "https://a.example", wantAllow: "https://a.example", wantStatus: http.StatusTeapot},
		{name: "wildcard GET", allowed: []string{"*"}, method: http.MethodGet, path: "/api/status", origin: "https://b.example", wantAllow: "*", wantStatus: http.StatusTeapot},
		{name: "other origin GET", allowed: []string{"https://a.example"}, method: http.MethodGet, path: "/api/status", origin: "https://b.example", wantStatus: http.StatusTeapot},
		{name: "no origin", allowed: []string{"*"}, method: http.MethodGet, path: "/api/status", wantStatus: http.StatusTeapot},
		{name: "POST gets no CORS", allowed: []string{"*"}, method: http.MethodPost, path: "/api/tokens", origin: "https://a.example", wantStatus: http.StatusTeapot},
		{name: "non-api path", allowed: []string{"*"}, method: http.MethodGet, path: "/", origin: "https://a.example", wantStatus: http.StatusTeapot},
		{name: "GET preflight", allowed: []string{"https://a.example"}, method: http.MethodOptions, path: "/api/status", origin: "https://a.example", preflightFor: http.MethodGet, wantAllow: "https://a.example", wantStatus: http.StatusNoContent, wantPreflight: true},
		{name: "DELETE preflight refused", allowed: []string{"*"}, method: http.MethodOptions, path: "/api/tokens/x", origin: "https://a.example", preflightFor: http.MethodDelete, wantStatus: http.StatusTeapot},
		{name: "preflight from other origin", allowed: []string{"https://a.example"}, method: http.MethodOptions, path: "/api/status", origin: "https://b.example", preflightFor: http.MethodGet, wantStatus: http.StatusTeapot},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			if tt.preflightFor != "" {
				req.Header.Set("Access-Control-Request-Method", tt.preflightFor)
			}

			rec := httptest.NewRecorder()
			getCORSMiddleware(tt.allowed, next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tt.wantAllow {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantAllow)
			}

			if got := rec.Header().Get("Access-Control-Allow-Methods"); (got != "") != tt.wantPreflight {
				t.Errorf("Access-Control-Allow-Methods = %q, want preflight headers: %v", got, tt.wantPreflight)
			}
		})
	}
}
