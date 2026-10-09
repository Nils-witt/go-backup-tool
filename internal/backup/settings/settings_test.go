package settings

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

var discardLogger = slog.New(slog.DiscardHandler)

func openTestStateDB(t *testing.T) *store.Store {
	t.Helper()

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

type testManager struct {
	*Manager

	registry *notify.Registry
	live     *report.Live
	db       *store.Store
}

// newTestManager builds a Manager over db, storing jobs in it first.
func newTestManager(t *testing.T, db *store.Store, jobs ...store.JobConfig) testManager {
	t.Helper()

	for _, j := range jobs {
		if err := db.CreateJobConfig(t.Context(), j); err != nil {
			t.Fatalf("CreateJobConfig() error: %v", err)
		}
	}

	registry := notify.NewRegistry(nil)
	live := report.NewLive(report.Settings{})
	smtp := notify.SMTPSettings{Host: "smtp.example.com", Port: 587, Username: "backups@example.com"}

	return testManager{
		Manager:  NewManager(db, registry, live, smtp, notify.GPGSettings{}, discardLogger),
		registry: registry, live: live, db: db,
	}
}

func webhookInput(id, url string, headers map[string]*string) NotificationInput {
	return NotificationInput{ID: id, Webhook: &WebhookInput{URL: url, Headers: headers}}
}

func TestLoadImportsConfigFile(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()

	// Stored before: edits made in the web UI win over the config file.
	if err := db.CreateNotificationConfig(ctx, store.NotificationConfig{ID: "ops", Webhook: &notify.FileWebhook{URL: "https://edited"}}); err != nil {
		t.Fatal(err)
	}

	// Invalid once loaded: an email with no smtp: in this config file.
	if err := db.CreateNotificationConfig(ctx, store.NotificationConfig{ID: "mail", Email: &notify.FileEmail{}}); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, db)
	m.Load(ctx,
		[]notify.FileNotification{{ID: "ops", Webhook: &notify.FileWebhook{URL: "https://yaml"}}, {ID: "slack", Webhook: &notify.FileWebhook{URL: "https://slack"}}},
		report.FileReport{Enabled: true, Schedule: "@daily", Notifications: []string{"ops"}},
	)

	checkLoadedState(t, m)

	list, err := m.ListNotifications(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListNotifications() = %+v, %v", list, err)
	}

	if list[0].ID != "mail" || list[0].Error == "" || list[1].ID != "ops" || len(list[1].UsedBy) != 1 || list[1].UsedBy[0] != "report" {
		t.Errorf("ListNotifications() = %+v, want mail with an error and ops used by the report", list)
	}
}

// checkLoadedState checks the live state TestLoadImportsConfigFile's Load
// should leave behind.
func checkLoadedState(t *testing.T, m testManager) {
	t.Helper()

	if n, ok := m.registry.Get("ops"); !ok || n.Webhook.URL != "https://edited" {
		t.Errorf("registry ops = %+v, %v, want the edited url", n, ok)
	}

	if _, ok := m.registry.Get("slack"); !ok {
		t.Error("imported notification slack not active")
	}

	if _, ok := m.registry.Get("mail"); ok {
		t.Error("invalid notification mail is active")
	}

	if s := m.live.Get(); !s.Enabled || len(s.Notifications) != 1 {
		t.Errorf("live report = %+v, want enabled with ops", s)
	}
}

func TestNotificationHeadersAreWriteOnly(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))
	ctx := t.Context()

	if err := m.CreateNotification(ctx, "erin", webhookInput("hook", "https://x", map[string]*string{"Authorization": nil})); !errors.Is(err, ErrInvalid) {
		t.Errorf("create with a kept-but-unknown header error = %v, want ErrInvalid", err)
	}

	if err := m.CreateNotification(ctx, "erin", webhookInput("hook", "https://x", map[string]*string{"Authorization": new("Bearer secret"), "X-A": new("1")})); err != nil {
		t.Fatalf("CreateNotification() error: %v", err)
	}

	// Keep Authorization (nil), drop X-A (omitted), add X-B.
	if err := m.UpdateNotification(ctx, "frank", webhookInput("hook", "https://y", map[string]*string{"Authorization": nil, "X-B": new("2")})); err != nil {
		t.Fatalf("UpdateNotification() error: %v", err)
	}

	n, _ := m.registry.Get("hook")
	if n.Webhook.URL != "https://y" || n.Webhook.Headers["Authorization"] != "Bearer secret" || n.Webhook.Headers["X-B"] != "2" || len(n.Webhook.Headers) != 2 {
		t.Errorf("registry hook = %+v, want the kept secret plus X-B on the new url", n.Webhook)
	}
}

func TestNotificationValidation(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))

	for name, in := range map[string]NotificationInput{
		"no channel":       {ID: "a"},
		"relative url":     webhookInput("a", "/hook", nil),
		"non-http url":     webhookInput("a", "file:///etc/passwd", nil),
		"bad header name":  webhookInput("a", "https://x", map[string]*string{"Bad Name": new("v")}),
		"multiline header": webhookInput("a", "https://x", map[string]*string{"X": new("a\r\nInjected: 1")}),
		"email no address": {ID: "a", Email: &notify.FileEmail{}},
		"blank id":         webhookInput(" ", "https://x", nil),
	} {
		if err := m.CreateNotification(t.Context(), "erin", in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: CreateNotification() error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestDeleteNotificationRefusedWhileInUse(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()
	m := newTestManager(t, db, store.JobConfig{Name: "db", FailureNotifications: []string{"jobs"}})

	for _, id := range []string{"jobs", "recv", "rep", "free"} {
		if err := m.CreateNotification(ctx, "erin", webhookInput(id, "https://x", nil)); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.CreateReceiverConfig(ctx, store.ReceiverConfig{ID: "a", PublicKey: "pem", Path: "/srv/a", DownloadNotifications: []string{"recv"}}); err != nil {
		t.Fatal(err)
	}

	if err := m.UpdateReport(ctx, "erin", report.FileReport{Notifications: []string{"rep"}}); err != nil {
		t.Fatal(err)
	}

	for id, user := range map[string]string{"jobs": "job db", "recv": "receiver a", "rep": "report"} {
		err := m.DeleteNotification(ctx, "erin", id)
		if !errors.Is(err, ErrNotificationInUse) || !strings.Contains(err.Error(), user) {
			t.Errorf("DeleteNotification(%s) error = %v, want in use by %s", id, err, user)
		}
	}

	if err := m.DeleteNotification(ctx, "erin", "free"); err != nil {
		t.Fatalf("DeleteNotification(free) error: %v", err)
	}

	if _, ok := m.registry.Get("free"); ok {
		t.Error("deleted notification still in registry")
	}

	if err := m.DeleteNotification(ctx, "erin", "free"); !errors.Is(err, store.ErrNotificationNotFound) {
		t.Errorf("second delete error = %v, want ErrNotificationNotFound", err)
	}
}

func TestUpdateReport(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))
	ctx := t.Context()

	if err := m.CreateNotification(ctx, "erin", webhookInput("ops", "https://x", nil)); err != nil {
		t.Fatal(err)
	}

	changed := m.live.Changed()

	for name, fr := range map[string]report.FileReport{
		"unknown notification": {Enabled: true, Notifications: []string{"nope"}},
		"enabled, none":        {Enabled: true},
		"bad schedule":         {Schedule: "whenever"},
	} {
		if err := m.UpdateReport(ctx, "erin", fr); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: UpdateReport() error = %v, want ErrInvalid", name, err)
		}
	}

	if err := m.UpdateReport(ctx, "erin", report.FileReport{Enabled: true, Schedule: " 0 6 * * * ", Notifications: []string{"ops"}}); err != nil {
		t.Fatalf("UpdateReport() error: %v", err)
	}

	select {
	case <-changed:
	default:
		t.Error("UpdateReport() didn't wake the report loop")
	}

	got, err := m.GetReport(ctx)
	if err != nil || !got.Enabled || got.Schedule != "0 6 * * *" || got.UpdatedBy != "erin" {
		t.Errorf("GetReport() = %+v, %v", got, err)
	}

	if _, ok := m.NextReport(got.UpdatedAt); !ok {
		t.Error("NextReport() = disabled, want a next run")
	}
}

func TestWithoutStateDB(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	m.Load(t.Context(), []notify.FileNotification{{ID: "ops", Webhook: &notify.FileWebhook{URL: "https://x"}}}, report.FileReport{Enabled: true, Notifications: []string{"ops"}})

	if _, ok := m.registry.Get("ops"); !ok {
		t.Error("config file notification not active without a state db")
	}

	if !m.live.Get().Enabled {
		t.Error("config file report not active without a state db")
	}

	if err := m.CreateNotification(t.Context(), "erin", webhookInput("b", "https://x", nil)); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("CreateNotification() without a state db error = %v, want ErrStoreUnavailable", err)
	}
}
