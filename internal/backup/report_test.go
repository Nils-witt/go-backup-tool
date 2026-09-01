package backup

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// wantSchedule parses spec the same way resolveReportSettings does, for a
// test to compare a resolved ReportSettings.Schedule against.
func wantSchedule(t *testing.T, spec string) cron.Schedule {
	t.Helper()

	sched, err := cron.ParseStandard(spec)
	if err != nil {
		t.Fatalf("cron.ParseStandard(%q) error: %v", spec, err)
	}

	return sched
}

func TestResolveReportSettingsDisabledByDefault(t *testing.T) {
	t.Parallel()

	got, err := resolveReportSettings(fileReport{})
	if err != nil {
		t.Fatalf("resolveReportSettings() error: %v", err)
	}

	if got.Enabled {
		t.Errorf("resolveReportSettings() enabled = true, want false for an unset report:")
	}
}

func TestResolveReportSettingsDefaults(t *testing.T) {
	t.Setenv("REPORT_TEST_PW", "s3cr3t")

	cfg := fileReport{
		Enabled: true,
		To:      []string{"ops@example.com"},
		SMTP: fileSMTP{
			Host:        "smtp.example.com",
			Username:    "backups@example.com",
			PasswordEnv: "REPORT_TEST_PW",
		},
	}

	got, err := resolveReportSettings(cfg)
	if err != nil {
		t.Fatalf("resolveReportSettings() error: %v", err)
	}

	want := ReportSettings{
		Enabled:  true,
		To:       []string{"ops@example.com"},
		From:     "backups@example.com", // defaults to smtp.username
		Schedule: wantSchedule(t, defaultReportSchedule),
		SMTP: SMTPSettings{
			Host:     "smtp.example.com",
			Port:     587, // default for starttls
			Username: "backups@example.com",
			Password: "s3cr3t",
			Security: SMTPSecurityStartTLS,
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveReportSettings() = %+v, want %+v", got, want)
	}
}

func TestResolveReportSettingsExplicitFields(t *testing.T) {
	t.Parallel()

	cfg := fileReport{
		Enabled:  true,
		To:       []string{"a@example.com", "b@example.com"},
		From:     "reports@example.com",
		Schedule: "45 23 * * *",
		SMTP: fileSMTP{
			Host:     "smtp.example.com",
			Port:     2525,
			Security: "none",
		},
	}

	got, err := resolveReportSettings(cfg)
	if err != nil {
		t.Fatalf("resolveReportSettings() error: %v", err)
	}

	if got.From != "reports@example.com" {
		t.Errorf("from = %q, want explicit report.from", got.From)
	}

	wantNext := time.Date(2026, 8, 28, 23, 45, 0, 0, time.Local)
	if next := got.Schedule.Next(time.Date(2026, 8, 28, 0, 0, 0, 0, time.Local)); !next.Equal(wantNext) {
		t.Errorf("schedule.Next() = %v, want %v (report.schedule 23:45 parsed as 45 23 * * *)", next, wantNext)
	}

	if got.SMTP.Port != 2525 {
		t.Errorf("port = %d, want explicit 2525", got.SMTP.Port)
	}

	if got.SMTP.Security != SMTPSecurityNone {
		t.Errorf("security = %q, want none", got.SMTP.Security)
	}
}

func TestResolveReportSettingsDirectPassword(t *testing.T) {
	t.Parallel()

	cfg := fileReport{
		Enabled: true,
		To:      []string{"a@example.com"},
		From:    "reports@example.com",
		SMTP: fileSMTP{
			Host:     "smtp.example.com",
			Username: "backups@example.com",
			Password: "hunter2",
		},
	}

	got, err := resolveReportSettings(cfg)
	if err != nil {
		t.Fatalf("resolveReportSettings() error: %v", err)
	}

	if got.SMTP.Password != "hunter2" {
		t.Errorf("password = %q, want the literal report.smtp.password", got.SMTP.Password)
	}
}

func TestResolveReportSettingsTLSDefaultPort(t *testing.T) {
	t.Parallel()

	cfg := fileReport{
		Enabled: true,
		To:      []string{"a@example.com"},
		From:    "reports@example.com",
		SMTP:    fileSMTP{Host: "smtp.example.com", Security: "tls"},
	}

	got, err := resolveReportSettings(cfg)
	if err != nil {
		t.Fatalf("resolveReportSettings() error: %v", err)
	}

	if got.SMTP.Port != 465 {
		t.Errorf("port = %d, want 465 default for tls", got.SMTP.Port)
	}
}

func TestResolveReportSettingsErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cfg        fileReport
		wantErrHas string
	}{
		{
			name:       "missing to",
			cfg:        fileReport{Enabled: true, SMTP: fileSMTP{Host: "smtp.example.com"}},
			wantErrHas: "report.to",
		},
		{
			name:       "empty to entry",
			cfg:        fileReport{Enabled: true, To: []string{" "}, SMTP: fileSMTP{Host: "smtp.example.com"}},
			wantErrHas: "report.to[0]",
		},
		{
			name:       "missing smtp host",
			cfg:        fileReport{Enabled: true, To: []string{"a@example.com"}},
			wantErrHas: "report.smtp.host",
		},
		{
			name: "bad schedule",
			cfg: fileReport{
				Enabled: true, To: []string{"a@example.com"}, Schedule: "not-a-cron-expr",
				SMTP: fileSMTP{Host: "smtp.example.com"},
			},
			wantErrHas: "report.schedule",
		},
		{
			name: "bad security",
			cfg: fileReport{
				Enabled: true, To: []string{"a@example.com"},
				SMTP: fileSMTP{Host: "smtp.example.com", Security: "ssl"},
			},
			wantErrHas: "report.smtp.security",
		},
		{
			name: "username without password-env",
			cfg: fileReport{
				Enabled: true, To: []string{"a@example.com"},
				SMTP: fileSMTP{Host: "smtp.example.com", Username: "u"},
			},
			wantErrHas: "password-env",
		},
		{
			name: "password-env without username",
			cfg: fileReport{
				Enabled: true, To: []string{"a@example.com"},
				SMTP: fileSMTP{Host: "smtp.example.com", PasswordEnv: "SOME_ENV"},
			},
			wantErrHas: "password-env",
		},
		{
			name: "password and password-env both set",
			cfg: fileReport{
				Enabled: true, To: []string{"a@example.com"},
				SMTP: fileSMTP{Host: "smtp.example.com", Username: "u", Password: "p", PasswordEnv: "SOME_ENV"},
			},
			wantErrHas: "mutually exclusive",
		},
		{
			name: "password-env not set in environment",
			cfg: fileReport{
				Enabled: true, To: []string{"a@example.com"},
				SMTP: fileSMTP{Host: "smtp.example.com", Username: "u", PasswordEnv: "REPORT_TEST_UNSET_VAR"}, //nolint:gosec // PasswordEnv names an env var, not a credential itself
			},
			wantErrHas: "REPORT_TEST_UNSET_VAR",
		},
		{
			name: "no from and no username",
			cfg: fileReport{
				Enabled: true, To: []string{"a@example.com"},
				SMTP: fileSMTP{Host: "smtp.example.com"},
			},
			wantErrHas: "report.from",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := resolveReportSettings(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Fatalf("resolveReportSettings() error = %v, want it to mention %q", err, tc.wantErrHas)
			}
		})
	}
}
