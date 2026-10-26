package config

import (
	"strings"
	"testing"
)

func TestResolveSentry(t *testing.T) {
	t.Parallel()

	const (
		dsn         = "https://abc123@o1.ingest.sentry.io/42"
		frontendDSN = "https://def456@o1.ingest.sentry.io/43"
	)

	tests := []struct {
		name     string
		input    fileSentry
		expected SentrySettings
		wantErr  string
	}{
		{name: "unset disables"},
		{name: "dsn only", input: fileSentry{DSN: " " + dsn + " "}, expected: SentrySettings{DSN: dsn, FrontendDSN: dsn}},
		{
			name:     "frontend dsn and environment",
			input:    fileSentry{DSN: dsn, FrontendDSN: frontendDSN, Environment: "production"},
			expected: SentrySettings{DSN: dsn, FrontendDSN: frontendDSN, Environment: "production"},
		},
		{name: "frontend dsn without dsn", input: fileSentry{FrontendDSN: frontendDSN}, wantErr: "need sentry.dsn"},
		{name: "environment without dsn", input: fileSentry{Environment: "production"}, wantErr: "need sentry.dsn"},
		{name: "bad scheme", input: fileSentry{DSN: "ftp://abc@host/1"}, wantErr: "sentry.dsn: must be an http"},
		{name: "missing key", input: fileSentry{DSN: "https://host/1"}, wantErr: "sentry.dsn: missing public key"},
		{name: "missing project", input: fileSentry{DSN: "https://abc@host/"}, wantErr: "sentry.dsn: missing project id"},
		{name: "missing host", input: fileSentry{DSN: "https:///1"}, wantErr: "sentry.dsn: missing host"},
		{name: "bad frontend dsn", input: fileSentry{DSN: dsn, FrontendDSN: "https://host/1"}, wantErr: "sentry.frontend-dsn: missing public key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveSentry(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveSentry() error = %v, want one containing %q", err, tt.wantErr)
				}

				if strings.Contains(err.Error(), "abc") {
					t.Errorf("resolveSentry() error %q echoes the DSN", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveSentry() error = %v", err)
			}

			if got != tt.expected {
				t.Errorf("resolveSentry() = %+v, want %+v", got, tt.expected)
			}

			if got.Enabled() != (tt.expected.DSN != "") {
				t.Errorf("Enabled() = %v, want %v", got.Enabled(), tt.expected.DSN != "")
			}
		})
	}
}
