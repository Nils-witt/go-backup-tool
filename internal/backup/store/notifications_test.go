package store

import (
	"errors"
	"fmt"
	"reflect"
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

	if err := db.SaveReportSettings(ctx, ReportSettings{Enabled: true, Schedule: "@daily", Notifications: []string{"ops"}, UpdatedBy: "erin"}); err != nil {
		t.Fatalf("first SaveReportSettings() error: %v", err)
	}

	if err := db.SaveReportSettings(ctx, ReportSettings{Enabled: false, Schedule: "0 6 * * *", UpdatedBy: "erin"}); err != nil {
		t.Fatalf("SaveReportSettings() error: %v", err)
	}

	got, ok, err := db.GetReportSettings(ctx)
	if err != nil || !ok || got.Enabled || got.Schedule != "0 6 * * *" || len(got.Notifications) != 0 || got.UpdatedBy != "erin" {
		t.Errorf("GetReportSettings() = %+v, %v, %v", got, ok, err)
	}
}

// storedNotificationFixtures pin the exact JSON the notifications table
// holds for each channel shape, captured from what the store wrote before
// its row types were decoupled from package notify. This is the on-disk
// contract: rows like these already sit in users' state dbs, so a failing
// test here means a change needs a migration, not a fixture update. A nil
// channel is stored as NULL.
var storedNotificationFixtures = []struct {
	name    string
	webhook *notify.FileWebhook
	email   *notify.FileEmail
	rawHook *string
	rawMail *string
}{
	{
		name: "every field set",
		webhook: &notify.FileWebhook{
			URL: "https://hooks.example/x", Method: "POST",
			Headers: map[string]string{"X-Env": "prod", "Authorization": "Bearer t"}, Body: `{"text":"{job}"}`,
		},
		email: &notify.FileEmail{
			To: []string{"ops@example.com", "oncall@example.com"}, From: "backup@example.com",
			Subject: "[{server_name}] {job} failed", Body: "see logs",
			Encrypt: &notify.FileEmailEncrypt{Recipients: []string{"ABCDEF0123456789"}},
		},
		rawHook: new(`{"url":"https://hooks.example/x","method":"POST","headers":{"Authorization":"Bearer t","X-Env":"prod"},"body":"{\"text\":\"{job}\"}"}`),
		rawMail: new(`{"to":["ops@example.com","oncall@example.com"],"from":"backup@example.com","subject":"[{server_name}] {job} failed","body":"see logs","encrypt":{"recipients":["ABCDEF0123456789"]}}`),
	},
	{
		name:    "webhook only, optional fields empty",
		webhook: &notify.FileWebhook{URL: "https://hooks.example/y"},
		rawHook: new(`{"url":"https://hooks.example/y","method":"","body":""}`),
	},
	{
		name:    "email only, optional fields empty",
		email:   &notify.FileEmail{To: []string{"a@example.com"}},
		rawMail: new(`{"to":["a@example.com"],"from":"","subject":"","body":""}`),
	},
}

// storedNotificationColumns reads id's raw webhook/email column values,
// nil for NULL.
func storedNotificationColumns(t *testing.T, db *Store, id string) (webhook, email *string) {
	t.Helper()

	var row struct{ Webhook, Email *string }
	if err := db.read.Raw("SELECT webhook, email FROM notifications WHERE id = ?", id).Scan(&row).Error; err != nil {
		t.Fatalf("reading raw notification row: %v", err)
	}

	return row.Webhook, row.Email
}

func TestNotificationConfigStoredFormat(t *testing.T) {
	t.Parallel()

	for i, tt := range storedNotificationFixtures {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := openTestStore(t)
			now := time.Now()
			id := fmt.Sprintf("n%d", i)

			if err := db.CreateNotificationConfig(t.Context(), NotificationConfig{ID: id, Webhook: tt.webhook, Email: tt.email, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("CreateNotificationConfig() error: %v", err)
			}

			assertStoredNotification(t, db, id, tt.rawHook, tt.rawMail, "after create")

			if err := db.UpdateNotificationConfig(t.Context(), NotificationConfig{ID: id, Webhook: tt.webhook, Email: tt.email, UpdatedAt: now}); err != nil {
				t.Fatalf("UpdateNotificationConfig() error: %v", err)
			}

			assertStoredNotification(t, db, id, tt.rawHook, tt.rawMail, "after update")
		})
	}
}

func assertStoredNotification(t *testing.T, db *Store, id string, expectedHook, expectedMail *string, when string) {
	t.Helper()

	gotHook, gotMail := storedNotificationColumns(t, db, id)
	if !reflect.DeepEqual(gotHook, expectedHook) {
		t.Errorf("%s: webhook column = %s, want %s", when, derefOrNull(gotHook), derefOrNull(expectedHook))
	}

	if !reflect.DeepEqual(gotMail, expectedMail) {
		t.Errorf("%s: email column = %s, want %s", when, derefOrNull(gotMail), derefOrNull(expectedMail))
	}
}

func derefOrNull(s *string) string {
	if s == nil {
		return "NULL"
	}

	return *s
}

func TestNotificationConfigDecodesStoredRows(t *testing.T) {
	t.Parallel()

	for i, tt := range storedNotificationFixtures {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := openTestStore(t)
			id := fmt.Sprintf("n%d", i)

			err := db.db.Exec(
				"INSERT INTO notifications (id, webhook, email, created_at, created_by, updated_at, updated_by) VALUES (?, ?, ?, ?, '', ?, '')",
				id, tt.rawHook, tt.rawMail, time.Now().UTC(), time.Now().UTC(),
			).Error
			if err != nil {
				t.Fatalf("inserting raw notification row: %v", err)
			}

			got, ok, err := db.GetNotificationConfig(t.Context(), id)
			if err != nil || !ok {
				t.Fatalf("GetNotificationConfig() = _, %v, %v, want a stored row", ok, err)
			}

			if !reflect.DeepEqual(got.Webhook, tt.webhook) {
				t.Errorf("Webhook = %+v, want %+v", got.Webhook, tt.webhook)
			}

			if !reflect.DeepEqual(got.Email, tt.email) {
				t.Errorf("Email = %+v, want %+v", got.Email, tt.email)
			}
		})
	}
}

// TestNotificationRowsMirrorNotifyTypes fails when notify's channel types
// gain, lose or rename a field, or change a JSON name, without the store's
// row types following. Fix it deliberately: decide whether the new field is
// stored, update the row type and its mapping, and add it to
// storedNotificationFixtures — rows already on disk must keep decoding.
func TestNotificationRowsMirrorNotifyTypes(t *testing.T) {
	t.Parallel()

	pairs := []struct{ notifyType, rowType reflect.Type }{
		{reflect.TypeFor[notify.FileWebhook](), reflect.TypeFor[webhookRow]()},
		{reflect.TypeFor[notify.FileEmail](), reflect.TypeFor[emailRow]()},
		{reflect.TypeFor[notify.FileEmailEncrypt](), reflect.TypeFor[emailEncryptRow]()},
	}

	for _, p := range pairs {
		t.Run(p.notifyType.Name(), func(t *testing.T) {
			t.Parallel()

			if p.notifyType.NumField() != p.rowType.NumField() {
				t.Fatalf("%s has %d fields, %s has %d", p.notifyType, p.notifyType.NumField(), p.rowType, p.rowType.NumField())
			}

			for i := range p.notifyType.NumField() {
				nf, rf := p.notifyType.Field(i), p.rowType.Field(i)
				if nf.Name != rf.Name || nf.Tag.Get("json") != rf.Tag.Get("json") {
					t.Errorf("field %d: %s %q vs %s %q", i, nf.Name, nf.Tag.Get("json"), rf.Name, rf.Tag.Get("json"))
				}
			}
		})
	}
}
