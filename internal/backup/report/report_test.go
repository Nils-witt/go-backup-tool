package report

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// wantSchedule parses spec the same way ResolveSettings does (pinned to
// UTC), for a test to compare a resolved Settings.Schedule against.
func wantSchedule(t *testing.T, spec string) cron.Schedule {
	t.Helper()

	sched, err := cron.ParseStandard(spec)
	if err != nil {
		t.Fatalf("cron.ParseStandard(%q) error: %v", spec, err)
	}

	if s, ok := sched.(*cron.SpecSchedule); ok {
		s.Location = time.UTC
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
	notifications := notify.NewRegistry(map[string]notify.Notification{"ops-email": ops})

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
		Schedule:      wantSchedule(t, DefaultSchedule),
		Notifications: []string{"ops-email"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveSettings() = %+v, want %+v", got, want)
	}
}

func TestResolveSettingsExplicitSchedule(t *testing.T) {
	t.Parallel()

	ops := notify.Notification{ID: "ops-email", Email: &notify.Email{To: []string{"a@example.com"}, From: "reports@example.com"}}
	notifications := notify.NewRegistry(map[string]notify.Notification{"ops-email": ops})

	cfg := FileReport{
		Enabled:       true,
		Schedule:      "45 23 * * *",
		Notifications: []string{"ops-email"},
	}

	got, err := ResolveSettings(cfg, notifications)
	if err != nil {
		t.Fatalf("ResolveSettings() error: %v", err)
	}

	// The schedule is evaluated in UTC whatever zone the reference time is
	// in: from 00:00 at UTC+2 (22:00 UTC the day before), the next 23:45 is
	// 23:45 UTC that same previous day, not 23:45 at UTC+2.
	plus2 := time.FixedZone("UTC+2", 2*60*60)

	wantNext := time.Date(2026, 8, 27, 23, 45, 0, 0, time.UTC)
	if next := got.Schedule.Next(time.Date(2026, 8, 28, 0, 0, 0, 0, plus2)); !next.Equal(wantNext) {
		t.Errorf("schedule.Next() = %v, want %v (45 23 * * * evaluated in UTC)", next, wantNext)
	}
}

func TestResolveSettingsDescriptorScheduleIsUTC(t *testing.T) {
	t.Parallel()

	notifications := notify.NewRegistry(map[string]notify.Notification{"ops-email": {ID: "ops-email", Email: &notify.Email{To: []string{"a@example.com"}}}})

	got, err := ResolveSettings(FileReport{Enabled: true, Schedule: "@daily", Notifications: []string{"ops-email"}}, notifications)
	if err != nil {
		t.Fatalf("ResolveSettings() error: %v", err)
	}

	from := time.Date(2026, 8, 28, 12, 0, 0, 0, time.FixedZone("UTC-5", -5*60*60))

	wantNext := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	if next := got.Schedule.Next(from); !next.Equal(wantNext) {
		t.Errorf("@daily Next() = %v, want UTC midnight %v", next, wantNext)
	}
}

func TestResolveSettingsMultipleNotifications(t *testing.T) {
	t.Parallel()

	email := notify.Notification{ID: "ops-email", Email: &notify.Email{To: []string{"a@example.com"}, From: "reports@example.com"}}
	webhook := notify.Notification{ID: "ops-webhook", Webhook: &notify.Webhook{URL: "https://example.com/hook", Method: "POST"}}
	notifications := notify.NewRegistry(map[string]notify.Notification{"ops-email": email, "ops-webhook": webhook})

	cfg := FileReport{Enabled: true, Notifications: []string{"ops-email", "ops-webhook"}}

	got, err := ResolveSettings(cfg, notifications)
	if err != nil {
		t.Fatalf("ResolveSettings() error: %v", err)
	}

	want := []string{"ops-email", "ops-webhook"}
	if !reflect.DeepEqual(got.Notifications, want) {
		t.Errorf("Notifications = %+v, want %+v", got.Notifications, want)
	}
}

func TestResolveSettingsDisabledStillChecksSchedule(t *testing.T) {
	t.Parallel()

	if _, err := ResolveSettings(FileReport{Schedule: "nope"}, nil); err == nil {
		t.Fatal("ResolveSettings() of a disabled report with a bad schedule succeeded")
	}
}

func TestLiveSetWakesChanged(t *testing.T) {
	t.Parallel()

	live := NewLive(Settings{})
	changed := live.Changed()

	live.Set(Settings{Enabled: true})

	select {
	case <-changed:
	default:
		t.Fatal("Changed() not closed after Set()")
	}

	if !live.Get().Enabled {
		t.Error("Get() after Set() = disabled, want enabled")
	}
}

func TestResolveSettingsErrors(t *testing.T) {
	t.Parallel()

	ops := notify.Notification{ID: "ops-email", Email: &notify.Email{To: []string{"a@example.com"}, From: "reports@example.com"}}
	notifications := notify.NewRegistry(map[string]notify.Notification{"ops-email": ops})

	cases := []struct {
		name          string
		cfg           FileReport
		notifications *notify.Registry
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
		{
			name:          "time zone prefix",
			cfg:           FileReport{Enabled: true, Notifications: []string{"ops-email"}, Schedule: "CRON_TZ=Europe/Berlin 0 7 * * *"},
			notifications: notifications,
			wantErrHas:    "always evaluated in UTC",
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
