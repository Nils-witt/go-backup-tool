package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestResolveDatabase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    fileDatabase
		expected DatabaseConfig
		wantErr  string
	}{
		{name: "unset defaults to sqlite", expected: DatabaseConfig{Type: DatabaseSQLite}},
		{name: "explicit sqlite", input: fileDatabase{Type: "sqlite"}, expected: DatabaseConfig{Type: DatabaseSQLite}},
		{name: "sqlite with postgres settings", input: fileDatabase{Type: "sqlite", Host: "db"}, wantErr: "type sqlite takes no other settings"},
		{name: "sqlite with dsn", input: fileDatabase{DSN: "postgres://db/x"}, wantErr: "type sqlite takes no other settings"},
		{name: "unknown type", input: fileDatabase{Type: "mysql"}, wantErr: `database.type "mysql"`},
		{
			name:     "postgres dsn passed through",
			input:    fileDatabase{Type: "postgres", DSN: " host=db user=u dbname=x "},
			expected: DatabaseConfig{Type: DatabasePostgres, PostgresDSN: "host=db user=u dbname=x"},
		},
		{name: "postgres dsn and fields", input: fileDatabase{Type: "postgres", DSN: "postgres://db/x", Host: "db"}, wantErr: "not both"},
		{name: "postgres without connection", input: fileDatabase{Type: "postgres"}, wantErr: "needs either dsn or host, user, and dbname"},
		{name: "postgres missing fields", input: fileDatabase{Type: "postgres", Host: "db"}, wantErr: "needs user, dbname"},
		{
			name:     "postgres fields with defaults",
			input:    fileDatabase{Type: "postgres", Host: "db", User: "backup", DBName: "state"},
			expected: DatabaseConfig{Type: DatabasePostgres, PostgresDSN: "postgres://backup@db:5432/state?sslmode=require"},
		},
		{
			name:     "postgres fields escape password",
			input:    fileDatabase{Type: "postgres", Host: "db", Port: 6543, User: "backup", Password: "p@ss/w:rd?#", DBName: "state", SSLMode: "verify-full"}, //nolint:gosec // dummy credential for the URL escaping check
			expected: DatabaseConfig{Type: DatabasePostgres, PostgresDSN: "postgres://backup:p%40ss%2Fw%3Ard%3F%23@db:6543/state?sslmode=verify-full"},         //nolint:gosec // dummy credential for the URL escaping check
		},
		{
			name:     "postgres ipv6 host",
			input:    fileDatabase{Type: "postgres", Host: "::1", User: "backup", DBName: "state", SSLMode: "disable"},
			expected: DatabaseConfig{Type: DatabasePostgres, PostgresDSN: "postgres://backup@[::1]:5432/state?sslmode=disable"},
		},
		{name: "postgres bad port", input: fileDatabase{Type: "postgres", Host: "db", Port: 70000, User: "u", DBName: "x"}, wantErr: "database.port 70000"},
		{name: "postgres bad sslmode", input: fileDatabase{Type: "postgres", Host: "db", User: "u", DBName: "x", SSLMode: "on"}, wantErr: `database.sslmode "on"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveDatabase(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveDatabase() error = %v, want one containing %q", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveDatabase() error: %v", err)
			}

			if got != tt.expected {
				t.Errorf("resolveDatabase() = %+v, want %+v", got, tt.expected)
			}
		})
	}
}

func TestResolveDatabaseErrorOmitsPassword(t *testing.T) {
	t.Parallel()

	_, err := resolveDatabase(fileDatabase{Type: "postgres", Host: "db", User: "u", Password: "hunter2", DBName: "x", SSLMode: "bogus"})
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("resolveDatabase() error = %v, want one without the password", err)
	}
}

func TestParseFlagsDatabase(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
database:
  type: postgres
  host: db
  user: backup
  password: secret
  dbname: state
jobs:
  - name: test
    files: [{command: echo}]
    key: backup.gpg
    targets: [{server: local, bucket: b}]
    recipients: [a@example.com]
servers:
  - name: local
    type: local
    path: /tmp
commands:
  - id: echo
    cmd: echo hi
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() error: %v", err)
	}

	expected := DatabaseConfig{Type: DatabasePostgres, PostgresDSN: "postgres://backup:secret@db:5432/state?sslmode=require"} //nolint:gosec // dummy credential for the URL escaping check
	if rc.Database != expected {
		t.Errorf("ParseFlags() Database = %+v, want %+v", rc.Database, expected)
	}
}
