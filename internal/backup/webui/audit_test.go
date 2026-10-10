package webui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// TestAuditLogRecordsChanges checks, end to end, that every change an
// admin makes through the API — successful or not — lands in the audit log
// with who made it and what it targeted, that reads and forbidden attempts
// don't, and that only a session allowed to see the audit log can read it.
func TestAuditLogRecordsChanges(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, _, baseDir := startReceiverConfigWebUI(t, idp)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	body := config.FileReceiver{ID: "a", PublicKey: testPublicKeyPEM(t), Path: filepath.Join(baseDir, "a")}

	for _, c := range []receiverConfigCall{
		{"list", http.MethodGet, "/api/receiver-configs", admin, nil, http.StatusOK},
		{"non-admin create", http.MethodPost, "/api/receiver-configs", viewer, body, http.StatusForbidden},
		{"create", http.MethodPost, "/api/receiver-configs", admin, body, http.StatusCreated},
		{"delete missing", http.MethodDelete, "/api/receiver-configs/missing", admin, nil, http.StatusNotFound},
		{"delete", http.MethodDelete, "/api/receiver-configs/a", admin, nil, http.StatusNoContent},
		{"non-admin read", http.MethodGet, "/api/audit-events", viewer, nil, http.StatusForbidden},
	} {
		c.run(t, srv)
	}

	var events []auditEventJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/audit-events", admin, &events); code != http.StatusOK {
		t.Fatalf("GET /api/audit-events = %d", code)
	}

	want := []struct {
		action, target string
		status         int
	}{
		{"delete", "a", http.StatusNoContent},
		{"delete", "missing", http.StatusNotFound},
		{"create", "a", http.StatusCreated},
	}

	if len(events) != len(want) {
		t.Fatalf("audit events = %+v, want %d", events, len(want))
	}

	for i, w := range want {
		ev := events[i]
		if ev.Username != "erin" || ev.Resource != "receiver-configs" || ev.Action != w.action || ev.Target != w.target ||
			ev.Status != w.status || ev.Success != (w.status < http.StatusBadRequest) {
			t.Errorf("audit event %d = %+v, want %s %s (%d) by erin", i, ev, w.action, w.target, w.status)
		}
	}

	if events[1].Detail == "" {
		t.Errorf("failed audit event has no detail: %+v", events[1])
	}
}

func TestDescribeChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern, method, path    string
		action, resource, target string
	}{
		{"POST /api/receiver-configs", http.MethodPost, "/api/receiver-configs", "create", "receiver-configs", ""},
		{"PUT /api/job-configs/{key}", http.MethodPut, "/api/job-configs/db", "update", "job-configs", "db"},
		{"DELETE /api/tokens/{id}", http.MethodDelete, "/api/tokens/t1", "delete", "tokens", "t1"},
		{"POST /api/jobs/{name}/retry", http.MethodPost, "/api/jobs/db/retry", "retry", "jobs", "db"},
		{"PUT /api/report-config", http.MethodPut, "/api/report-config", "update", "report-config", ""},
	}

	for _, tt := range tests {
		mux := http.NewServeMux()

		var action, resource, target string

		mux.HandleFunc(tt.pattern, func(_ http.ResponseWriter, r *http.Request) {
			action, resource, target = describeChange(r)
		})

		req, err := http.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
		if err != nil {
			t.Fatal(err)
		}

		mux.ServeHTTP(httptest.NewRecorder(), req)

		if action != tt.action || resource != tt.resource || target != tt.target {
			t.Errorf("describeChange(%s) = %q, %q, %q; want %q, %q, %q", tt.pattern, action, resource, target, tt.action, tt.resource, tt.target)
		}
	}
}
