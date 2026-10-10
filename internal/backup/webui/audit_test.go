package webui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
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
	updated := body
	updated.Retention = "7d"

	for _, c := range []receiverConfigCall{
		{"list", http.MethodGet, "/api/receiver-configs", admin, nil, http.StatusOK},
		{"non-admin create", http.MethodPost, "/api/receiver-configs", viewer, body, http.StatusForbidden},
		{"create", http.MethodPost, "/api/receiver-configs", admin, body, http.StatusCreated},
		{"update", http.MethodPut, "/api/receiver-configs/a", admin, updated, http.StatusNoContent},
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
		{"update", "a", http.StatusNoContent},
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

	if events[1].Detail == "" || len(events[1].Changes) != 0 {
		t.Errorf("failed audit event = %+v, want a detail and no changes", events[1])
	}

	checkAuditChanges(t, events, body.Path)
}

// checkAuditChanges checks the field changes TestAuditLogRecordsChanges'
// delete, update, and create (events[0], [2], [3]) recorded.
func checkAuditChanges(t *testing.T, events []auditEventJSON, path string) {
	t.Helper()

	if got, want := events[2].Changes, []store.AuditChange{{Field: "retention", New: "7d"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("update changes = %+v, want %+v", got, want)
	}

	created := changesByField(events[3].Changes)
	if c := created["path"]; c.Old != nil || c.New != path {
		t.Errorf("create's path change = %+v, want new %q", c, path)
	}

	if _, ok := created["retention"]; ok {
		t.Errorf("create lists unset retention: %+v", events[3].Changes)
	}

	deleted := changesByField(events[0].Changes)
	if c := deleted["retention"]; c.Old != "7d" || c.New != nil {
		t.Errorf("delete's retention change = %+v, want old 7d", c)
	}
}

func changesByField(changes []store.AuditChange) map[string]store.AuditChange {
	out := make(map[string]store.AuditChange, len(changes))
	for _, c := range changes {
		out[c.Field] = c
	}

	return out
}

func TestDiffSnapshots(t *testing.T) {
	t.Parallel()

	before := config.FileJob{
		Name: "db", Recipients: []string{"a@example.com"},
		Targets: []config.FileJobTarget{{Server: "s3", Bucket: "old"}, {Server: "nas"}},
	}
	after := before
	after.Recipients = []string{"a@example.com", "b@example.com"}
	after.Targets = []config.FileJobTarget{{Server: "s3", Bucket: "new"}}
	after.Armor = true

	got, err := diffSnapshots(before, after, nil)
	if err != nil {
		t.Fatal(err)
	}

	want := []store.AuditChange{
		{Field: "armor", New: true},
		{Field: "recipients", Old: []any{"a@example.com"}, New: []any{"a@example.com", "b@example.com"}},
		{Field: "targets[0].bucket", Old: "old", New: "new"},
		{Field: "targets[1].server", Old: "nas"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("diffSnapshots() = %+v, want %+v", got, want)
	}
}

func TestDiffSnapshotsRedactsSecrets(t *testing.T) {
	t.Parallel()

	secret := auditResources(nil, nil)["notification-configs"].secret
	before := notify.FileNotification{ID: "ops", Webhook: &notify.FileWebhook{URL: "https://hooks.example/T0/secret", Headers: map[string]string{"Authorization": "Bearer a", "X-Same": "s"}}}
	after := notify.FileNotification{ID: "ops", Webhook: &notify.FileWebhook{URL: "https://hooks.example/T0/other", Method: "POST", Headers: map[string]string{"Authorization": "Bearer b", "X-Same": "s"}}}

	got, err := diffSnapshots(before, after, secret)
	if err != nil {
		t.Fatal(err)
	}

	want := []store.AuditChange{
		{Field: "webhook.headers.Authorization", Old: redactedValue, New: redactedValue},
		{Field: "webhook.method", New: "POST"},
		{Field: "webhook.url", Old: redactedValue, New: redactedValue},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("diffSnapshots() = %+v, want %+v", got, want)
	}

	created, err := diffSnapshots(nil, after, secret)
	if err != nil {
		t.Fatal(err)
	}

	if c := changesByField(created)["webhook.headers.Authorization"]; c.Old != nil || c.New != redactedValue {
		t.Errorf("create's secret header change = %+v, want only a redacted new value", c)
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
