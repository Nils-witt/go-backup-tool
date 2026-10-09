package webui

import (
	"net/http"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/settings"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// startSettingsWebUI starts an SSO-enabled web UI whose notifications and
// report are managed by a fresh settings.Manager over its own state db, with
// one job "db" whose failure-notifications reference "ops".
func startSettingsWebUI(t *testing.T, idp *testIDP) (*Server, *notify.Registry) {
	t.Helper()

	db := openTestStateDB(t)
	registry := notify.NewRegistry(nil)

	if err := db.CreateJobConfig(t.Context(), store.JobConfig{Name: "db", FailureNotifications: []string{"ops"}}); err != nil {
		t.Fatalf("CreateJobConfig() error: %v", err)
	}

	manager := settings.NewManager(db, registry, report.NewLive(report.Settings{}), notify.SMTPSettings{}, notify.GPGSettings{}, discardLogger)
	statusStore, _ := newTestStore()

	srv := StartWebUI("127.0.0.1:0", statusStore, nil, nil, nil, nil, nil, nil, manager, nil, discardLogger, db, nil, idp.settings(), nil, false, false, "", nil, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	return srv, registry
}

func TestNotificationConfigAPI(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, registry := startSettingsWebUI(t, idp)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	secret := "Bearer secret"
	hook := settings.NotificationInput{ID: "ops", Webhook: &settings.WebhookInput{URL: "https://hooks.example.com/x", Headers: map[string]*string{"Authorization": &secret}}}
	free := settings.NotificationInput{ID: "free", Webhook: &settings.WebhookInput{URL: "https://hooks.example.com/y"}}
	mail := settings.NotificationInput{ID: "mail", Email: &notify.FileEmail{To: []string{"o@example.com"}}}
	kept := settings.NotificationInput{Webhook: &settings.WebhookInput{URL: "https://hooks.example.com/z", Headers: map[string]*string{"Authorization": nil}}}

	for _, c := range []receiverConfigCall{
		{"non-admin list", http.MethodGet, "/api/notification-configs", viewer, nil, http.StatusForbidden},
		{"non-admin create", http.MethodPost, "/api/notification-configs", viewer, hook, http.StatusForbidden},
		{"admin create", http.MethodPost, "/api/notification-configs", admin, hook, http.StatusCreated},
		{"admin create free", http.MethodPost, "/api/notification-configs", admin, free, http.StatusCreated},
		{"duplicate", http.MethodPost, "/api/notification-configs", admin, hook, http.StatusConflict},
		{"email without smtp", http.MethodPost, "/api/notification-configs", admin, mail, http.StatusBadRequest},
		{"update keeping secret", http.MethodPut, "/api/notification-configs/ops", admin, kept, http.StatusNoContent},
		{"update changing id", http.MethodPut, "/api/notification-configs/ops", admin, free, http.StatusBadRequest},
		{"update missing", http.MethodPut, "/api/notification-configs/missing", admin, kept, http.StatusNotFound},
		{"delete in use by job", http.MethodDelete, "/api/notification-configs/ops", admin, nil, http.StatusConflict},
		{"delete free", http.MethodDelete, "/api/notification-configs/free", admin, nil, http.StatusNoContent},
		{"delete free again", http.MethodDelete, "/api/notification-configs/free", admin, nil, http.StatusNotFound},
	} {
		c.run(t, srv)
	}

	if n, _ := registry.Get("ops"); n.Webhook == nil || n.Webhook.URL != "https://hooks.example.com/z" || n.Webhook.Headers["Authorization"] != secret {
		t.Errorf("registry ops = %+v, want the new url with the kept secret", n.Webhook)
	}

	var list notificationConfigListJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/notification-configs", admin, &list); code != http.StatusOK {
		t.Fatalf("admin list = %d", code)
	}

	if list.SMTPConfigured || len(list.Notifications) != 1 {
		t.Fatalf("list = %+v, want just ops and smtp unconfigured", list)
	}

	got := list.Notifications[0]
	if v, ok := got.Webhook.Headers["Authorization"]; !ok || v != nil {
		t.Errorf("listed Authorization header = %v, %v, want present but masked (null)", v, ok)
	}

	if len(got.UsedBy) != 1 || got.UsedBy[0] != "job db" {
		t.Errorf("used_by = %v, want [job db]", got.UsedBy)
	}
}

func TestReportConfigAPI(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, _ := startSettingsWebUI(t, idp)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	hook := settings.NotificationInput{ID: "ops", Webhook: &settings.WebhookInput{URL: "https://hooks.example.com/x"}}

	for _, c := range []receiverConfigCall{
		{"non-admin get", http.MethodGet, "/api/report-config", viewer, nil, http.StatusForbidden},
		{"create notification", http.MethodPost, "/api/notification-configs", admin, hook, http.StatusCreated},
		{"unknown notification", http.MethodPut, "/api/report-config", admin, report.FileReport{Enabled: true, Notifications: []string{"nope"}}, http.StatusBadRequest},
		{"bad schedule", http.MethodPut, "/api/report-config", admin, report.FileReport{Schedule: "soon"}, http.StatusBadRequest},
		{"non-admin update", http.MethodPut, "/api/report-config", viewer, report.FileReport{}, http.StatusForbidden},
		{"admin update", http.MethodPut, "/api/report-config", admin, report.FileReport{Enabled: true, Schedule: "0 6 * * *", Notifications: []string{"ops"}}, http.StatusNoContent},
	} {
		c.run(t, srv)
	}

	var got reportConfigJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/report-config", admin, &got); code != http.StatusOK {
		t.Fatalf("admin get = %d", code)
	}

	if !got.Enabled || got.Schedule != "0 6 * * *" || got.UpdatedBy != "erin" || got.NextRun == nil ||
		got.DefaultSchedule != report.DefaultSchedule || len(got.NotificationIDs) != 1 {
		t.Errorf("report config = %+v", got)
	}
}
