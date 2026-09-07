package report

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// wantSchedule parses spec the same way ResolveSettings does, for a
// test to compare a resolved Settings.Schedule against.
func wantSchedule(t *testing.T, spec string) cron.Schedule {
	t.Helper()

	sched, err := cron.ParseStandard(spec)
	if err != nil {
		t.Fatalf("cron.ParseStandard(%q) error: %v", spec, err)
	}

	return sched
}

func TestResolveSettingsDisabledByDefault(t *testing.T) {
	t.Parallel()

	got, err := ResolveSettings(FileReport{}, nil)
	if err != nil {
		t.Fatalf("ResolveSettings() error: %v", err)
	}

	if got.Enabled {
		t.Errorf("ResolveSettings() enabled = true, want false for an unset report:")
	}
}

func TestResolveSettingsDefaults(t *testing.T) {
	t.Parallel()

	ops := notify.Notification{ID: "ops-email", Email: &notify.Email{To: []string{"ops@example.com"}, From: "backups@example.com"}}
	notifications := map[string]notify.Notification{"ops-email": ops}

	cfg := FileReport{
		Enabled:       true,
		Notifications: []string{"ops-email"},
	}

	got, err := ResolveSettings(cfg, notifications)
	if err != nil {
		t.Fatalf("ResolveSettings() error: %v", err)
	}

	want := Settings{
		Enabled:       true,
		Schedule:      wantSchedule(t, defaultReportSchedule),
		Notifications: []notify.Notification{ops},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveSettings() = %+v, want %+v", got, want)
	}
}

func TestResolveSettingsExplicitSchedule(t *testing.T) {
	t.Parallel()

	ops := notify.Notification{ID: "ops-email", Email: &notify.Email{To: []string{"a@example.com"}, From: "reports@example.com"}}
	notifications := map[string]notify.Notification{"ops-email": ops}

	cfg := FileReport{
		Enabled:       true,
		Schedule:      "45 23 * * *",
		Notifications: []string{"ops-email"},
	}

	got, err := ResolveSettings(cfg, notifications)
	if err != nil {
		t.Fatalf("ResolveSettings() error: %v", err)
	}

	wantNext := time.Date(2026, 8, 28, 23, 45, 0, 0, time.Local)
	if next := got.Schedule.Next(time.Date(2026, 8, 28, 0, 0, 0, 0, time.Local)); !next.Equal(wantNext) {
		t.Errorf("schedule.Next() = %v, want %v (report.schedule 23:45 parsed as 45 23 * * *)", next, wantNext)
	}
}

func TestResolveSettingsMultipleNotifications(t *testing.T) {
	t.Parallel()

	email := notify.Notification{ID: "ops-email", Email: &notify.Email{To: []string{"a@example.com"}, From: "reports@example.com"}}
	webhook := notify.Notification{ID: "ops-webhook", Webhook: &notify.Webhook{URL: "https://example.com/hook", Method: "POST"}}
	notifications := map[string]notify.Notification{"ops-email": email, "ops-webhook": webhook}

	cfg := FileReport{Enabled: true, Notifications: []string{"ops-email", "ops-webhook"}}

	got, err := ResolveSettings(cfg, notifications)
	if err != nil {
		t.Fatalf("ResolveSettings() error: %v", err)
	}

	want := []notify.Notification{email, webhook}
	if !reflect.DeepEqual(got.Notifications, want) {
		t.Errorf("Notifications = %+v, want %+v", got.Notifications, want)
	}
}

func TestResolveSettingsErrors(t *testing.T) {
	t.Parallel()

	ops := notify.Notification{ID: "ops-email", Email: &notify.Email{To: []string{"a@example.com"}, From: "reports@example.com"}}
	notifications := map[string]notify.Notification{"ops-email": ops}

	cases := []struct {
		name          string
		cfg           FileReport
		notifications map[string]notify.Notification
		wantErrHas    string
	}{
		{
			name:       "missing notifications",
			cfg:        FileReport{Enabled: true},
			wantErrHas: "report.notifications",
		},
		{
			name:          "unknown notification id",
			cfg:           FileReport{Enabled: true, Notifications: []string{"nope"}},
			notifications: notifications,
			wantErrHas:    `unknown notification id "nope"`,
		},
		{
			name:          "bad schedule",
			cfg:           FileReport{Enabled: true, Notifications: []string{"ops-email"}, Schedule: "not-a-cron-expr"},
			notifications: notifications,
			wantErrHas:    "report.schedule",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ResolveSettings(tc.cfg, tc.notifications)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Fatalf("ResolveSettings() error = %v, want it to mention %q", err, tc.wantErrHas)
			}
		})
	}
}
