package webui

import (
	"bytes"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// TestWriteJSONHidesEncodingError checks that a value that can't be
// encoded answers 500 with a generic message, while the encoding error
// itself is only logged.
func TestWriteJSONHidesEncodingError(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer

	log := slog.New(slog.NewTextHandler(&logged, nil))
	h := logRequests(log, rateLimit(config.RateLimitSettings{}.WithDefaults(), false, log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, math.Inf(1))
	})), false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/status", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	if body := rec.Body.String(); strings.Contains(body, "json") || strings.Contains(body, "Inf") {
		t.Errorf("body = %q, want a generic message", body)
	}

	if !strings.Contains(logged.String(), "unsupported value") {
		t.Errorf("log = %q, want the encoding error", logged.String())
	}
}
