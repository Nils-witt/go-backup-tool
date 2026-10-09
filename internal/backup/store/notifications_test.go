package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

func TestNotificationConfigCRUD(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	now := time.Now().Truncate(time.Second)

	n := NotificationConfig{
		ID:        "ops",
		Webhook:   &notify.FileWebhook{URL: "https://x", Headers: map[string]string{"Authorization": "Bearer t"}},
		CreatedAt: now, CreatedBy: "erin", UpdatedAt: now, UpdatedBy: "erin",
	}

	if err := db.CreateNotificationConfig(ctx, n); err != nil {
		t.Fatalf("CreateNotificationConfig() error: %v", err)
	}

	if err := db.CreateNotificationConfig(ctx, n); !errors.Is(err, ErrNotificationExists) {
		t.Errorf("duplicate CreateNotificationConfig() error = %v, want ErrNotificationExists", err)
	}

	n.Webhook, n.Email, n.UpdatedBy = nil, &notify.FileEmail{To: []string{"o@example.com"}}, "frank"
	if err := db.UpdateNotificationConfig(ctx, n); err != nil {
		t.Fatalf("UpdateNotificationConfig() error: %v", err)
	}

	got, ok, err := db.GetNotificationConfig(ctx, "ops")
	if err != nil || !ok {
		t.Fatalf("GetNotificationConfig() = %v, %v", ok, err)
	}

	if got.Webhook != nil || got.Email == nil || !slices.Equal(got.Email.To, []string{"o@example.com"}) || got.CreatedBy != "erin" || got.UpdatedBy != "frank" {
		t.Errorf("after update = %+v", got)
	}

	checkDeleteNotificationConfig(t, db, "ops")
}

// checkDeleteNotificationConfig deletes stored notification id and checks
// updating or deleting it afterwards reports it missing.
func checkDeleteNotificationConfig(t *testing.T, db *Store, id string) {
	t.Helper()

	ctx := t.Context()

	if err := db.DeleteNotificationConfig(ctx, id); err != nil {
		t.Fatalf("DeleteNotificationConfig() error: %v", err)
	}

	if err := db.DeleteNotificationConfig(ctx, id); !errors.Is(err, ErrNotificationNotFound) {
		t.Errorf("second DeleteNotificationConfig() error = %v, want ErrNotificationNotFound", err)
	}

	if err := db.UpdateNotificationConfig(ctx, NotificationConfig{ID: id}); !errors.Is(err, ErrNotificationNotFound) {
		t.Errorf("UpdateNotificationConfig(deleted) error = %v, want ErrNotificationNotFound", err)
	}
}

func TestImportNotificationConfigsKeepsExistingRows(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	if err := db.CreateNotificationConfig(ctx, NotificationConfig{ID: "a", Webhook: &notify.FileWebhook{URL: "https://edited"}}); err != nil {
		t.Fatal(err)
	}

	imported, err := db.ImportNotificationConfigs(ctx, []NotificationConfig{
		{ID: "a", Webhook: &notify.FileWebhook{URL: "https://from-yaml"}},
		{ID: "b", Webhook: &notify.FileWebhook{URL: "https://b"}},
	})
	if err != nil || !slices.Equal(imported, []string{"b"}) {
		t.Fatalf("ImportNotificationConfigs() = %v, %v, want [b]", imported, err)
	}

	list, err := db.ListNotificationConfigs(ctx)
	if err != nil || len(list) != 2 || list[0].Webhook.URL != "https://edited" {
		t.Errorf("notifications = %+v, %v, want a (edited url kept) and b", list, err)
	}
}

func TestReportSettingsUnsetOnFreshDB(t *testing.T) {
	t.Parallel()

	if _, ok, err := openTestStore(t).GetReportSettings(t.Context()); err != nil || ok {
		t.Fatalf("GetReportSettings() on a fresh db = %v, %v, want none", ok, err)
	}
}

func TestReportSettings(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	imported, err := db.ImportReportSettings(ctx, ReportSettings{Enabled: true, Schedule: "@daily", Notifications: []string{"ops"}, UpdatedBy: "config file"})
	if err != nil || !imported {
		t.Fatalf("ImportReportSettings() = %v, %v, want imported", imported, err)
	}

	if imported, err := db.ImportReportSettings(ctx, ReportSettings{Schedule: "@hourly"}); err != nil || imported {
		t.Errorf("second ImportReportSettings() = %v, %v, want skipped", imported, err)
	}

	if err := db.SaveReportSettings(ctx, ReportSettings{Enabled: false, Schedule: "0 6 * * *", UpdatedBy: "erin"}); err != nil {
		t.Fatalf("SaveReportSettings() error: %v", err)
	}

	got, ok, err := db.GetReportSettings(ctx)
	if err != nil || !ok || got.Enabled || got.Schedule != "0 6 * * *" || len(got.Notifications) != 0 || got.UpdatedBy != "erin" {
		t.Errorf("GetReportSettings() = %+v, %v, %v", got, ok, err)
	}
}
