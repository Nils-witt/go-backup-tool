package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/telemetry"
)

func TestHandleClientConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sentry   config.SentrySettings
		expected *clientSentryJSON
	}{
		{name: "sentry disabled"},
		{
			name:     "sentry enabled",
			sentry:   config.SentrySettings{DSN: "https://k@o1.ingest.sentry.io/1", FrontendDSN: "https://k@o1.ingest.sentry.io/2", Environment: "prod"},
			expected: &clientSentryJSON{DSN: "https://k@o1.ingest.sentry.io/2", Environment: "prod", Release: telemetry.Release()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			handleClientConfig(tt.sentry)(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/client-config", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("GET /api/client-config = %d, want %d", rec.Code, http.StatusOK)
			}

			var got clientConfigJSON
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
			}

			switch {
			case tt.expected == nil && got.Sentry != nil:
				t.Errorf("sentry = %+v, want it omitted", *got.Sentry)
			case tt.expected != nil && (got.Sentry == nil || *got.Sentry != *tt.expected):
				t.Errorf("sentry = %+v, want %+v", got.Sentry, *tt.expected)
			}
		})
	}
}
