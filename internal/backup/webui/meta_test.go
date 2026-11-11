package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

func TestHandleHealth(t *testing.T) {
	t.Parallel()

	closed := openTestStateDB(t)
	if err := closed.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	tests := []struct {
		name       string
		db         store.Store
		wantCode   int
		wantStatus string
		wantDB     string
	}{
		{name: "reachable db", db: openTestStateDB(t), wantCode: http.StatusOK, wantStatus: healthOK, wantDB: healthOK},
		{name: "no db", db: nil, wantCode: http.StatusServiceUnavailable, wantStatus: healthDegraded, wantDB: healthUnavailable},
		{name: "closed db", db: closed, wantCode: http.StatusServiceUnavailable, wantStatus: healthDegraded, wantDB: healthUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/health", nil)
			rec := httptest.NewRecorder()

			handleHealth(tt.db, discardLogger)(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status code = %d, want %d", rec.Code, tt.wantCode)
			}

			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q, want JSON", ct)
			}

			var got healthJSON
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding response body: %v", err)
			}

			if got.Status != tt.wantStatus || got.Database != tt.wantDB {
				t.Errorf("body = %+v, want status %q, database %q", got, tt.wantStatus, tt.wantDB)
			}
		})
	}
}
