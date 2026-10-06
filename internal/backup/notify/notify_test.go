package notify

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestResolveSMTPUnset(t *testing.T) {
	t.Parallel()

	got, err := ResolveSMTP(FileSMTP{})
	if err != nil {
		t.Fatalf("ResolveSMTP() error: %v", err)
	}

	if got != (SMTPSettings{}) {
		t.Errorf("ResolveSMTP() = %+v, want the zero value for an unset smtp:", got)
	}
}

func TestResolveSMTPDefaults(t *testing.T) {
	t.Setenv("NOTIFY_TEST_PW", "s3cr3t")

	got, err := ResolveSMTP(FileSMTP{Host: "smtp.example.com", Username: "backups@example.com", PasswordEnv: "NOTIFY_TEST_PW"}) //nolint:gosec // PasswordEnv names an env var, not a credential itself
	if err != nil {
		t.Fatalf("ResolveSMTP() error: %v", err)
	}

	want := SMTPSettings{
		Host:     "smtp.example.com",
		Port:     587, // default for starttls
		Username: "backups@example.com",
		Password: "s3cr3t",
		Security: SMTPSecurityStartTLS,
	}

	if got != want {
		t.Errorf("ResolveSMTP() = %+v, want %+v", got, want)
	}
}

func TestResolveSMTPDirectPassword(t *testing.T) {
	t.Parallel()

	got, err := ResolveSMTP(FileSMTP{Host: "smtp.example.com", Username: "backups@example.com", Password: "hunter2"})
	if err != nil {
		t.Fatalf("ResolveSMTP() error: %v", err)
	}

	if got.Password != "hunter2" {
		t.Errorf("password = %q, want the literal smtp.password", got.Password)
	}
}

func TestResolveSMTPTLSDefaultPort(t *testing.T) {
	t.Parallel()

	got, err := ResolveSMTP(FileSMTP{Host: "smtp.example.com", Security: "tls"})
	if err != nil {
		t.Fatalf("ResolveSMTP() error: %v", err)
	}

	if got.Port != 465 {
		t.Errorf("port = %d, want 465 default for tls", got.Port)
	}
}

func TestResolveSMTPExplicitPort(t *testing.T) {
	t.Parallel()

	got, err := ResolveSMTP(FileSMTP{Host: "smtp.example.com", Port: 2525, Security: "none"})
	if err != nil {
		t.Fatalf("ResolveSMTP() error: %v", err)
	}

	if got.Port != 2525 {
		t.Errorf("port = %d, want explicit 2525", got.Port)
	}

	if got.Security != SMTPSecurityNone {
		t.Errorf("security = %q, want none", got.Security)
	}
}

func TestResolveSMTPErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cfg        FileSMTP
		wantErrHas string
	}{
		{
			name:       "bad security",
			cfg:        FileSMTP{Host: "smtp.example.com", Security: "ssl"},
			wantErrHas: "smtp.security",
		},
		{
			name:       "username without password-env",
			cfg:        FileSMTP{Host: "smtp.example.com", Username: "u"},
			wantErrHas: "password-env",
		},
		{
			name:       "password-env without username",
			cfg:        FileSMTP{Host: "smtp.example.com", PasswordEnv: "SOME_ENV"},
			wantErrHas: "password-env",
		},
		{
			name:       "password and password-env both set",
			cfg:        FileSMTP{Host: "smtp.example.com", Username: "u", Password: "p", PasswordEnv: "SOME_ENV"},
			wantErrHas: "mutually exclusive",
		},
		{
			name:       "password-env not set in environment",
			cfg:        FileSMTP{Host: "smtp.example.com", Username: "u", PasswordEnv: "NOTIFY_TEST_UNSET_VAR"}, //nolint:gosec // PasswordEnv names an env var, not a credential itself
			wantErrHas: "NOTIFY_TEST_UNSET_VAR",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ResolveSMTP(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Fatalf("ResolveSMTP() error = %v, want it to mention %q", err, tc.wantErrHas)
			}
		})
	}
}

func TestBuildNotifications(t *testing.T) {
	t.Parallel()

	smtp := SMTPSettings{Host: "smtp.example.com", Username: "backups@example.com"}

	fileNotifications := []FileNotification{
		{ID: "webhook-only", Webhook: &FileWebhook{URL: "https://example.com/hook"}},
		{ID: "email-only", Email: &FileEmail{To: []string{"ops@example.com"}}},
		{ID: "both", Webhook: &FileWebhook{URL: "https://example.com/hook2", Method: "put"}, Email: &FileEmail{To: []string{"a@example.com"}, From: "reports@example.com"}},
	}

	got, err := Build(fileNotifications, smtp, GPGSettings{Bin: "gpg"})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	want := map[string]Notification{
		"webhook-only": {ID: "webhook-only", Webhook: &Webhook{URL: "https://example.com/hook", Method: http.MethodPost}},
		"email-only":   {ID: "email-only", Email: &Email{To: []string{"ops@example.com"}, From: "backups@example.com", SMTP: smtp}},
		"both": {
			ID:      "both",
			Webhook: &Webhook{URL: "https://example.com/hook2", Method: http.MethodPut},
			Email:   &Email{To: []string{"a@example.com"}, From: "reports@example.com", SMTP: smtp},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Build() = %+v, want %+v", got, want)
	}
}

func TestBuildNotificationsEmailEncrypt(t *testing.T) {
	t.Parallel()

	smtp := SMTPSettings{Host: "smtp.example.com", Username: "backups@example.com"}

	fileNotifications := []FileNotification{
		{
			ID: "encrypted",
			Email: &FileEmail{
				To:      []string{"ops@example.com"},
				Encrypt: &FileEmailEncrypt{Recipients: []string{"ops@example.com", " sibling@example.com "}},
			},
		},
	}

	gpg := GPGSettings{Bin: "/usr/local/bin/gpg", Homedir: "/etc/gbt/gnupg"}

	got, err := Build(fileNotifications, smtp, gpg)
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	want := map[string]Notification{
		"encrypted": {
			ID: "encrypted",
			Email: &Email{
				To: []string{"ops@example.com"}, From: "backups@example.com", SMTP: smtp,
				Encrypt: &EmailEncrypt{Recipients: []string{"ops@example.com", "sibling@example.com"}, GPGBin: "/usr/local/bin/gpg", GPGHomedir: "/etc/gbt/gnupg"},
			},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Build() = %+v, want %+v", got, want)
	}
}

func TestBuildNotificationsErrors(t *testing.T) {
	t.Parallel()

	smtpConfigured := SMTPSettings{Host: "smtp.example.com"}

	cases := []struct {
		name       string
		fns        []FileNotification
		smtp       SMTPSettings
		wantErrHas string
	}{
		{
			name:       "missing id",
			fns:        []FileNotification{{Webhook: &FileWebhook{URL: "https://example.com"}}},
			wantErrHas: "id is required",
		},
		{
			name: "duplicate id",
			fns: []FileNotification{
				{ID: "dup", Webhook: &FileWebhook{URL: "https://example.com/1"}},
				{ID: "dup", Webhook: &FileWebhook{URL: "https://example.com/2"}},
			},
			wantErrHas: `duplicate notification id "dup"`,
		},
		{
			name:       "neither webhook nor email",
			fns:        []FileNotification{{ID: "empty"}},
			wantErrHas: "at least one of webhook or email",
		},
		{
			name:       "webhook missing url",
			fns:        []FileNotification{{ID: "bad-webhook", Webhook: &FileWebhook{}}},
			wantErrHas: "url is required",
		},
		{
			name:       "email missing to",
			fns:        []FileNotification{{ID: "bad-email", Email: &FileEmail{}}},
			smtp:       smtpConfigured,
			wantErrHas: "to is required",
		},
		{
			name:       "email without smtp configured",
			fns:        []FileNotification{{ID: "bad-email", Email: &FileEmail{To: []string{"a@example.com"}}}},
			wantErrHas: "smtp: is not configured",
		},
		{
			name:       "email without from or smtp username",
			fns:        []FileNotification{{ID: "bad-email", Email: &FileEmail{To: []string{"a@example.com"}}}},
			smtp:       smtpConfigured,
			wantErrHas: "neither from nor smtp.username",
		},
		{
			name: "email encrypt without recipients",
			fns: []FileNotification{
				{ID: "bad-encrypt", Email: &FileEmail{To: []string{"a@example.com"}, From: "f@example.com", Encrypt: &FileEmailEncrypt{}}},
			},
			smtp:       smtpConfigured,
			wantErrHas: "recipients is required",
		},
		{
			name: "email encrypt with empty recipient",
			fns: []FileNotification{
				{
					ID:    "bad-encrypt",
					Email: &FileEmail{To: []string{"a@example.com"}, From: "f@example.com", Encrypt: &FileEmailEncrypt{Recipients: []string{" "}}},
				},
			},
			smtp:       smtpConfigured,
			wantErrHas: "recipients[0] is empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Build(tc.fns, tc.smtp, GPGSettings{Bin: "gpg"})
			if err == nil || !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Fatalf("Build() error = %v, want it to mention %q", err, tc.wantErrHas)
			}
		})
	}
}
