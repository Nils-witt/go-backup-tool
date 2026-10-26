package config

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

// writeConfigFile writes contents to a config.yaml inside t.TempDir() and
// returns its path.
func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	return path
}

// singleJob asserts rc holds exactly one job and returns it.
func singleJob(t *testing.T, rc *RunConfig) *Config {
	t.Helper()

	if len(rc.Jobs) != 1 {
		t.Fatalf("ParseFlags() jobs = %d, want 1", len(rc.Jobs))
	}

	return rc.Jobs[0]
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		env     map[string]string
		wantErr string // substring expected in the error, "" means no error
	}{
		{
			// Jobs may be stored in the state db instead; the "nothing to
			// run" check happens once they're loaded (see app.Run).
			name: "missing jobs list",
			yaml: "servers:\n  - name: s\n    type: local\n    path: /mnt/backups\nrecipients: [me@example.com]\n",
		},
		{
			name: "missing jobs list allowed with listen set",
			yaml: "webui:\n  enabled: true\n  listen: :8080\nservers:\n  - name: s\n    type: local\n    path: /mnt/backups\nrecipients: [me@example.com]\n",
		},
		{
			name:    "missing command",
			yaml:    "servers:\n  - name: s\n    type: local\n    path: /mnt/backups\njobs:\n  - name: test\n    targets: [{server: s, bucket: b}]\n    recipients: [me@example.com]\n",
			wantErr: "at least one file",
		},
		{
			name:    "missing targets",
			yaml:    "commands:\n  - id: job-test\n    cmd: echo hi\njobs:\n  - name: test\n    files: [{command: job-test}]\n    recipients: [me@example.com]\n",
			wantErr: "at least one target is required",
		},
		{
			name:    "target references unknown server",
			yaml:    "commands:\n  - id: job-test\n    cmd: echo hi\njobs:\n  - name: test\n    files: [{command: job-test}]\n    recipients: [me@example.com]\n    targets: [{server: nope, bucket: b}]\n",
			wantErr: `no server named "nope"`,
		},
		{
			name:    "no recipients",
			yaml:    "servers:\n  - name: s\n    type: local\n    path: /mnt/backups\ncommands:\n  - id: job-test\n    cmd: echo hi\njobs:\n  - name: test\n    files: [{command: job-test}]\n    targets: [{server: s, bucket: b}]\n",
			wantErr: "specify at least one recipient",
		},
		{
			name: "valid recipient config",
			yaml: "servers:\n  - name: s\n    type: local\n    path: /mnt/backups\ncommands:\n  - id: job-test\n    cmd: echo hi\njobs:\n  - name: test\n    files: [{command: job-test}]\n    targets: [{server: s, bucket: b}]\n    recipients: [me@example.com]\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			path := writeConfigFile(t, tt.yaml)

			rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseFlags() unexpected error: %v", err)
				}

				if rc == nil {
					t.Fatal("ParseFlags() returned nil config with no error")
				}

				return
			}

			if err == nil {
				t.Fatalf("ParseFlags() expected error containing %q, got nil", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseFlags() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestParseFlagsHelp(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	_, err := ParseFlags([]string{"-h"}, &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("ParseFlags(-h) error = %v, want flag.ErrHelp", err)
	}

	if out.Len() == 0 {
		t.Error("ParseFlags(-h) wrote no usage output")
	}
}

// Not parallel: t.Chdir changes the process's working directory, so this test can't have parallel ancestors.
func TestParseFlagsNoConfigFile(t *testing.T) {
	t.Chdir(t.TempDir())

	_, err := ParseFlags(nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no config file found") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "no config file found")
	}
}

func TestParseFlagsKeyTimeNotYetSubstituted(t *testing.T) {
	t.Parallel()

	// ParseFlags leaves {time} in the key unresolved: substituteKeyTime
	// (app.go) resolves it fresh immediately before every run, so a
	// repeating job (interval) gets a distinct key on every run instead
	// of overwriting the same object.
	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    key: "prefix-{time}-suffix.gpg"
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	if cfg.Key != "prefix-{time}-suffix.gpg" {
		t.Errorf("ParseFlags() key = %q, want unresolved template %q", cfg.Key, "prefix-{time}-suffix.gpg")
	}
}

func TestParseFlagsMultipleRecipients(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients:
      - a@example.com
      - b@example.com
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := []string{"a@example.com", "b@example.com"}
	if len(cfg.Recipients) != len(want) {
		t.Fatalf("ParseFlags() recipients = %v, want %v", cfg.Recipients, want)
	}

	for i, r := range want {
		if cfg.Recipients[i] != r {
			t.Errorf("ParseFlags() recipients[%d] = %q, want %q", i, cfg.Recipients[i], r)
		}
	}
}

func TestParseFlagsConfigFile(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
timeout: 5m
webui:
  enabled: true
  listen: ":8080"

servers:
  - name: primary
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo from-file"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: primary, bucket: file-bucket}]
    recipients:
      - file@example.com
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	if cfg.Sources[0].Cmd != "echo from-file" {
		t.Errorf("cfg.Sources[0].Cmd = %q, want %q", cfg.Sources[0].Cmd, "echo from-file")
	}

	want := Target{ServerName: "primary", Kind: ServerKindLocal, Bucket: "file-bucket", LocalPath: "/mnt/backups"}
	if len(cfg.Targets) != 1 || cfg.Targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.Targets, want)
	}

	if len(cfg.Recipients) != 1 || cfg.Recipients[0] != "file@example.com" {
		t.Errorf("cfg.recipients = %v, want [file@example.com]", cfg.Recipients)
	}

	if rc.Timeout != 5*time.Minute {
		t.Errorf("rc.Timeout = %v, want 5m", rc.Timeout)
	}

	if rc.Listen != ":8080" {
		t.Errorf("rc.Listen = %q, want %q", rc.Listen, ":8080")
	}
}

func TestParseFlagsConfigFileListenUnset(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.Listen != "" {
		t.Errorf("rc.Listen = %q, want empty (web UI disabled by default)", rc.Listen)
	}
}

func TestParseFlagsWebUIEnabledRequiresListen(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "webui.listen is not set") {
		t.Fatalf("ParseFlags() error = %v, want it to mention webui.listen is not set", err)
	}
}

func TestParseFlagsWebUIDisabledIgnoresListen(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  listen: ":8080"

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.Listen != "" {
		t.Errorf("rc.Listen = %q, want empty (webui.enabled unset/false)", rc.Listen)
	}
}

func TestParseFlagsRejectsRemovedLocalLogin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		webuiYAML  string
		wantErrHas string
	}{
		{name: "username/password", webuiYAML: "\n  username: \"admin\"\n  password: \"secret\"", wantErrHas: "webui.username"},
		{name: "client-secret", webuiYAML: "\n  oidc:\n    enabled: true\n    issuer: \"https://idp.example.com\"\n    client-id: \"c\"\n    client-secret: \"s\"", wantErrHas: "client-secret"},
		{name: "redirect-url", webuiYAML: "\n  oidc:\n    enabled: true\n    issuer: \"https://idp.example.com\"\n    client-id: \"c\"\n    redirect-url: \"https://x/cb\"", wantErrHas: "redirect-url"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"`+tc.webuiYAML+`

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

			_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Fatalf("ParseFlags() error = %v, want substring %q", err, tc.wantErrHas)
			}
		})
	}
}

func TestParseFlagsOIDCGroupPermissions(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  oidc:
    enabled: true
    issuer: "https://idp.example.com"
    client-id: "my-client"
    groups-claim: "roles"
    button-label: "Log in with Keycloak"
    group-permissions:
      backup-admins: [admin]
      auditors: [login-log, download-log]

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	want := map[string]permission.Permission{
		"backup-admins": permission.PermissionAdmin,
		"auditors":      permission.PermissionViewLoginLog | permission.PermissionViewDownloadLog,
	}
	if !reflect.DeepEqual(rc.OIDC.GroupPermissions, want) {
		t.Errorf("rc.OIDC.GroupPermissions = %v, want %v", rc.OIDC.GroupPermissions, want)
	}

	if rc.OIDC.GroupsClaim != "roles" || rc.OIDC.ButtonLabel != "Log in with Keycloak" {
		t.Errorf("rc.OIDC groupsClaim/buttonLabel = %q/%q", rc.OIDC.GroupsClaim, rc.OIDC.ButtonLabel)
	}
}

func TestParseFlagsOIDCGroupPermissionsRejectsUnknown(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  oidc:
    enabled: true
    issuer: "https://idp.example.com"
    client-id: "my-client"
    group-permissions:
      ops: [delete]

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	if _, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("ParseFlags() with an unknown webui.oidc.group-permissions entry = nil error, want one")
	}
}

func TestParseFlagsLogViewerDefaultsToDisabled(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.LogViewer {
		t.Error("rc.LogViewer = true, want false (log viewer disabled by default)")
	}
}

func TestParseFlagsLogViewerEnabled(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  log-viewer: true

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if !rc.LogViewer {
		t.Error("rc.LogViewer = false, want true (enable-log-viewer: true set in config file)")
	}
}

// TestParseFlagsEventLogLimit covers webui.event-log-limit: defaulting to
// DefaultEventLogLimit when unset, being taken as given in range,
// and being rejected outside 1..maxEventLogLimit.
func TestParseFlagsEventLogLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setting string
		want    int
		wantErr bool
	}{
		{name: "unset uses default", setting: "", want: DefaultEventLogLimit},
		{name: "explicit value", setting: "event-log-limit: 1000", want: 1000},
		{name: "maximum", setting: "event-log-limit: 10000", want: 10000},
		{name: "negative rejected", setting: "event-log-limit: -1", wantErr: true},
		{name: "above maximum rejected", setting: "event-log-limit: 10001", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  `+tt.setting+`

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

			rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseFlags() error = nil, want an error")
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseFlags() unexpected error: %v", err)
			}

			if rc.EventLogLimit != tt.want {
				t.Errorf("rc.EventLogLimit = %d, want %d", rc.EventLogLimit, tt.want)
			}
		})
	}
}

// TestParseFlagsCORSGetOrigins covers webui.cors-get-origins: accepting
// "*" and bare origins (normalized), and rejecting anything a browser's
// Origin header could never match.
func TestParseFlagsCORSGetOrigins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setting string
		want    []string
		wantErr bool
	}{
		{name: "unset", setting: "", want: nil},
		{name: "wildcard", setting: `cors-get-origins: ["*"]`, want: []string{"*"}},
		{name: "origins normalized", setting: `cors-get-origins: [" HTTPS://Status.Example.com/ ", "http://localhost:5173"]`, want: []string{"https://status.example.com", "http://localhost:5173"}},
		{name: "path rejected", setting: `cors-get-origins: ["https://example.com/app"]`, wantErr: true},
		{name: "missing scheme rejected", setting: `cors-get-origins: ["example.com"]`, wantErr: true},
		{name: "other scheme rejected", setting: `cors-get-origins: ["ftp://example.com"]`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  `+tt.setting+`

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

			rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseFlags() error = nil, want an error")
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseFlags() unexpected error: %v", err)
			}

			if !slices.Equal(rc.CORSGetOrigins, tt.want) {
				t.Errorf("rc.CORSGetOrigins = %q, want %q", rc.CORSGetOrigins, tt.want)
			}
		})
	}
}

func TestParseFlagsConfigFileLogLevel(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
log-level: debug

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.LogLevel != slog.LevelDebug {
		t.Errorf("rc.LogLevel = %v, want %v", rc.LogLevel, slog.LevelDebug)
	}
}

func TestParseFlagsLogLevelFlagOverridesConfigFile(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
log-level: debug

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path, "-log-level", "error"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.LogLevel != slog.LevelError {
		t.Errorf("rc.LogLevel = %v, want %v (explicit -log-level should win)", rc.LogLevel, slog.LevelError)
	}
}

func TestParseFlagsConfigFileBadLogLevel(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
log-level: "not-a-level"

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("ParseFlags() expected error for invalid config file log-level, got nil")
	}
}

func TestParseFlagsConfigFileMissingExplicit(t *testing.T) {
	t.Parallel()

	_, err := ParseFlags([]string{
		"-config", filepath.Join(t.TempDir(), "does-not-exist.yaml"),
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("ParseFlags() expected error for missing explicit -config file, got nil")
	}
}

func TestLoadFileConfigMissingDefaultIgnored(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	fc, err := loadFileConfig(path, false)
	if err != nil {
		t.Fatalf("loadFileConfig(explicit=false) unexpected error for missing file: %v", err)
	}

	if fc != nil {
		t.Errorf("loadFileConfig(explicit=false) = %+v, want nil for missing file", fc)
	}
}

func TestLoadFileConfigMissingExplicit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	if _, err := loadFileConfig(path, true); err == nil {
		t.Fatal("loadFileConfig(explicit=true) expected error for missing file, got nil")
	}
}

func TestParseFlagsConfigFileBadTimeout(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
timeout: "not-a-duration"

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("ParseFlags() expected error for invalid config file timeout, got nil")
	}
}

func TestParseFlagsMultiJob(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
recipients:
  - default@example.com

servers:
  - name: primary
    type: local
    path: /mnt/primary
  - name: secondary
    type: local
    path: /mnt/secondary

commands:
  - id: job-database
    cmd: "mysqldump db"
  - id: job-files
    cmd: "tar czf - /data"

jobs:
  - name: database
    files: [{command: job-database}]
    targets: [{server: primary, bucket: db-bucket}]
  - name: files
    files: [{command: job-files}]
    targets: [{server: secondary, bucket: files-bucket}]
    recipients:
      - files-only@example.com
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if len(rc.Jobs) != 2 {
		t.Fatalf("ParseFlags() jobs = %d, want 2", len(rc.Jobs))
	}

	db, files := rc.Jobs[0], rc.Jobs[1]

	if db.Name != "database" || db.Sources[0].Cmd != "mysqldump db" {
		t.Errorf("db job = %+v", db)
	}

	wantDBTarget := Target{ServerName: "primary", Kind: ServerKindLocal, Bucket: "db-bucket", LocalPath: "/mnt/primary"}
	if len(db.Targets) != 1 || db.Targets[0] != wantDBTarget {
		t.Errorf("db.targets = %+v, want [%+v]", db.Targets, wantDBTarget)
	}

	// db job inherits the shared recipients default.
	if len(db.Recipients) != 1 || db.Recipients[0] != "default@example.com" {
		t.Errorf("db.recipients = %v, want inherited [default@example.com]", db.Recipients)
	}

	// files job targets a different server and overrides recipients.
	wantFilesTarget := Target{ServerName: "secondary", Kind: ServerKindLocal, Bucket: "files-bucket", LocalPath: "/mnt/secondary"}
	if len(files.Targets) != 1 || files.Targets[0] != wantFilesTarget {
		t.Errorf("files.targets = %+v, want [%+v]", files.Targets, wantFilesTarget)
	}

	if len(files.Recipients) != 1 || files.Recipients[0] != "files-only@example.com" {
		t.Errorf("files.recipients = %v, want override [files-only@example.com]", files.Recipients)
	}
}

func TestParseFlagsMultipleTargets(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: primary
    type: local
    path: /mnt/primary
  - name: offsite
    type: remote
    endpoint: "https://minio.example.com"

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    recipients: [me@example.com]
    targets:
      - server: primary
        bucket: primary-bucket
      - server: offsite
        bucket: secondary-bucket
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := []Target{
		{ServerName: "primary", Kind: ServerKindLocal, Bucket: "primary-bucket", LocalPath: "/mnt/primary"},
		{ServerName: "offsite", Kind: ServerKindRemote, Bucket: "secondary-bucket", Endpoint: "https://minio.example.com"},
	}

	if len(cfg.Targets) != len(want) {
		t.Fatalf("cfg.targets = %+v, want %+v", cfg.Targets, want)
	}

	for i := range want {
		if cfg.Targets[i] != want[i] {
			t.Errorf("cfg.targets[%d] = %+v, want %+v", i, cfg.Targets[i], want[i])
		}
	}
}

func TestParseFlagsTargetMissingServer(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    recipients: [me@example.com]
    targets:
      - bucket: b
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "targets[0]: server is required") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "targets[0]: server is required")
	}
}

func TestParseFlagsTargetMissingBucket(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    recipients: [me@example.com]
    targets:
      - server: s
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "targets[0]: bucket is required") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "targets[0]: bucket is required")
	}
}

func TestParseFlagsServerRequiresName(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "servers[0]: name is required") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "servers[0]: name is required")
	}
}

func TestParseFlagsServerDuplicateName(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: dup
    type: local
    path: /mnt/a
  - name: dup
    type: local
    path: /mnt/b

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: dup, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `duplicate server name "dup"`) {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, `duplicate server name "dup"`)
	}
}

func TestParseFlagsServerTypeRequired(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "type is required") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "type is required")
	}
}

func TestParseFlagsLocalServer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: `+dir+`

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: nas, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "b", LocalPath: dir}
	if len(cfg.Targets) != 1 || cfg.Targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.Targets, want)
	}
}

func TestParseFlagsLocalServerRequiresPath(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: nas, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "path is required for type: local") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "path is required for type: local")
	}
}

func TestParseFlagsLocalServerRetention(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: `+dir+`
    retention: 168h

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: nas, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "b", LocalPath: dir, Retention: 168 * time.Hour}
	if len(cfg.Targets) != 1 || cfg.Targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.Targets, want)
	}
}

func TestParseFlagsLocalServerRetentionInDays(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: `+dir+`
    retention: 7d

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: nas, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "b", LocalPath: dir, Retention: 7 * 24 * time.Hour}
	if len(cfg.Targets) != 1 || cfg.Targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.Targets, want)
	}
}

func TestParseFlagsLocalServerRetentionRejectsNegative(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: /mnt/backups
    retention: -1h

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: nas, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "retention must not be negative") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "retention must not be negative")
	}
}

func TestParseFlagsJobTargetRetention(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		retentionLine string // "retention: ..." line under the job's targets: entry, or "" to omit it
		wantRetention time.Duration
	}{
		{
			name:          "overrides the server's own retention",
			retentionLine: "retention: 30d",
			wantRetention: 30 * 24 * time.Hour,
		},
		{
			name:          "unset falls back to the server's retention",
			wantRetention: 7 * 24 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: `+dir+`
    retention: 7d

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: nas, bucket: b, `+tt.retentionLine+`}]
    recipients: [me@example.com]
`)

			rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
			if err != nil {
				t.Fatalf("ParseFlags() unexpected error: %v", err)
			}

			cfg := singleJob(t, rc)

			want := Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "b", LocalPath: dir, Retention: tt.wantRetention}
			if len(cfg.Targets) != 1 || cfg.Targets[0] != want {
				t.Errorf("cfg.targets = %+v, want [%+v]", cfg.Targets, want)
			}
		})
	}
}

func TestParseFlagsJobTargetRetentionErrors(t *testing.T) {
	t.Parallel()

	tests := []parseErrorCase{
		{
			name: "negative duration",
			yaml: `
servers:
  - name: nas
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: nas, bucket: b, retention: -1h}]
    recipients: [me@example.com]
`,
			wantErr: "retention must not be negative",
		},
		{
			name: "remote server",
			yaml: `
servers:
  - name: sib
    type: remote
    endpoint: "https://backup2.example.com:8443"

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: sib, bucket: b, retention: 168h}]
    recipients: [me@example.com]
`,
			wantErr: `retention is not valid for server "sib" (type remote; local only)`,
		},
	}

	checkParseErrors(t, tests)
}

func TestParseFlagsLocalServerRejectsRemoteFields(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: /mnt/backups
    endpoint: "https://example.com"

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: nas, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "is not valid for type: local") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "is not valid for type: local")
	}
}

func TestParseFlagsServerUnknownType(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: ftp

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `unknown type "ftp"`) {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, `unknown type "ftp"`)
	}
}

func TestParseFlagsMultiJobRequiresName(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-unnamed-0
    cmd: "echo hi"

jobs:
  - files: [{command: job-unnamed-0}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "name is required")
	}
}

func TestParseFlagsMultiJobDuplicateName(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-dup
    cmd: "echo hi"
  - id: job-dup-2
    cmd: "echo bye"

jobs:
  - name: dup
    files: [{command: job-dup}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
  - name: dup
    files: [{command: job-dup-2}]
    targets: [{server: s, bucket: b2}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "duplicate job name") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "duplicate job name")
	}
}

func TestParseFlagsJobFilter(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-database
    cmd: "mysqldump db"
  - id: job-files
    cmd: "tar czf - /data"

jobs:
  - name: database
    files: [{command: job-database}]
    targets: [{server: s, bucket: db-bucket}]
    recipients: [me@example.com]
  - name: files
    files: [{command: job-files}]
    targets: [{server: s, bucket: files-bucket}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path, "-job", "files"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.Name != "files" {
		t.Errorf("cfg.name = %q, want %q", cfg.Name, "files")
	}
}

func TestParseFlagsJobFilterUnknownJob(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-database
    cmd: "mysqldump db"

jobs:
  - name: database
    files: [{command: job-database}]
    targets: [{server: s, bucket: db-bucket}]
    recipients: [me@example.com]
`)

	// The job may be stored in the state db instead, so an unknown -job is
	// only reported once jobs are loaded (see jobs.Manager.Load).
	rc, err := ParseFlags([]string{"-config", path, "-job", "nope"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if len(rc.Jobs) != 0 || rc.JobFilter != "nope" {
		t.Errorf("rc.Jobs = %d, rc.JobFilter = %q; want 0, %q", len(rc.Jobs), rc.JobFilter, "nope")
	}

	if len(rc.FileJobs) != 1 || rc.FileJobs[0].Name != "database" {
		t.Errorf("rc.FileJobs = %+v, want the database job", rc.FileJobs)
	}
}

func TestParseFlagsFlattensFileJobs(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
gpg-bin: /usr/bin/gpg2
files: [{command: shared}]
recipients: [me@example.com]
interval: 1h
armor: true
servers:
  - name: s
    type: local
    path: /mnt/backups
targets: [{server: s, bucket: shared}]

commands:
  - id: shared
    cmd: "echo default"
  - id: job-b
    cmd: "echo b"

jobs:
  - name: a
  - name: b
    files: [{command: job-b}]
    gpg-homedir: /keys
    recipients: [other@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if len(rc.FileJobs) != 2 {
		t.Fatalf("rc.FileJobs = %d, want 2", len(rc.FileJobs))
	}

	a, b := rc.FileJobs[0], rc.FileJobs[1]

	if !slices.Equal(a.Files, []FileJobFile{{Command: "shared"}}) || a.Interval != "1h" || !a.Armor || len(a.Targets) != 1 || a.Recipients[0] != "me@example.com" {
		t.Errorf("job a = %+v, want the top-level defaults layered in", a)
	}

	if a.GPGBin != "" {
		t.Errorf("job a GPGBin = %q, want \"\" (top-level gpg-bin stays global)", a.GPGBin)
	}

	if !slices.Equal(b.Files, []FileJobFile{{Command: "job-b"}}) || b.GPGHomedir != "/keys" || b.Recipients[0] != "other@example.com" {
		t.Errorf("job b = %+v, want its own fields kept", b)
	}

	if rc.GPG.Bin != "/usr/bin/gpg2" {
		t.Errorf("rc.GPG.Bin = %q, want /usr/bin/gpg2", rc.GPG.Bin)
	}
}

func TestResolveJob(t *testing.T) {
	t.Parallel()

	local, err := ResolveServer(FileServer{Name: "nas", Type: "local", Path: "/mnt", Retention: "7d"})
	if err != nil {
		t.Fatalf("ResolveServer() error: %v", err)
	}

	cmd, err := ResolveCommand(FileCommand{ID: "page", Cmd: "echo page"})
	if err != nil {
		t.Fatalf("ResolveCommand() error: %v", err)
	}

	servers := map[string]ResolvedServer{"nas": local}
	commands := map[string]Command{"page": cmd}
	gpg := notify.GPGSettings{Bin: "gpg", Homedir: "/keys"}

	job, err := ResolveJob(FileJob{
		Name: " db ", Files: []FileJobFile{{Command: "page"}}, Recipients: []string{"me@example.com"}, Interval: "1h",
		Targets: []FileJobTarget{{Server: "nas", Bucket: "b", OnError: &FileTargetOnError{Command: "page", After: 2}}},
	}, gpg, servers, commands, "primary")
	if err != nil {
		t.Fatalf("ResolveJob() error: %v", err)
	}

	if job.Name != "db" || job.GPGHomedir != "/keys" || job.ServerName != "primary" || job.Interval != time.Hour {
		t.Errorf("ResolveJob() = %+v", job)
	}

	if len(job.Targets) != 1 || job.Targets[0].Retention != 7*24*time.Hour || job.Targets[0].OnErrorCommand == nil {
		t.Errorf("ResolveJob() targets = %+v", job.Targets)
	}
}

func TestResolveJobRemoteTLS(t *testing.T) {
	t.Parallel()

	remote, err := ResolveServer(FileServer{Name: "offsite", Type: "remote", Endpoint: "https://offsite.example.com", TLSInsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("ResolveServer() error: %v", err)
	}

	cmd, err := ResolveCommand(FileCommand{ID: "page", Cmd: "echo page"})
	if err != nil {
		t.Fatalf("ResolveCommand() error: %v", err)
	}

	job, err := ResolveJob(FileJob{
		Name: "db", Files: []FileJobFile{{Command: "page"}}, Recipients: []string{"me@example.com"},
		Targets: []FileJobTarget{{Server: "offsite", Bucket: "b"}},
	}, notify.GPGSettings{Bin: "gpg"}, map[string]ResolvedServer{"offsite": remote}, map[string]Command{"page": cmd}, "primary")
	if err != nil {
		t.Fatalf("ResolveJob() error: %v", err)
	}

	if len(job.Targets) != 1 || job.Targets[0].TLS != (TargetTLS{InsecureSkipVerify: true}) {
		t.Errorf("ResolveJob() targets = %+v, want the server's TLS settings carried over", job.Targets)
	}
}

func TestResolveJobErrors(t *testing.T) {
	t.Parallel()

	local, err := ResolveServer(FileServer{Name: "nas", Type: "local", Path: "/mnt"})
	if err != nil {
		t.Fatalf("ResolveServer() error: %v", err)
	}

	servers := map[string]ResolvedServer{"nas": local}
	gpg := notify.GPGSettings{Bin: "gpg"}

	for _, tc := range []struct {
		name string
		fj   FileJob
		want string
	}{
		{"no name", FileJob{Files: []FileJobFile{{Command: "x"}}}, "name is required"},
		{"no command", FileJob{Name: "x", Recipients: []string{"r"}, Targets: []FileJobTarget{{Server: "nas", Bucket: "b"}}}, "at least one file"},
		{"unknown job command", FileJob{Name: "x", Files: []FileJobFile{{Command: "gone"}}, Recipients: []string{"r"}, Targets: []FileJobTarget{{Server: "nas", Bucket: "b"}}}, `no command "gone" defined`},
		{"unknown server", FileJob{Name: "x", Files: []FileJobFile{{Command: "x"}}, Recipients: []string{"r"}, Targets: []FileJobTarget{{Server: "gone", Bucket: "b"}}}, "no server named"},
		{"unknown command", FileJob{Name: "x", Files: []FileJobFile{{Command: "x"}}, Recipients: []string{"r"}, Targets: []FileJobTarget{{Server: "nas", Bucket: "b", OnRecover: &FileTargetOnRecover{Command: "gone"}}}}, "no command named"},
	} {
		if _, err := ResolveJob(tc.fj, gpg, servers, nil, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ResolveJob() error = %v, want substring %q", tc.name, err, tc.want)
		}
	}
}

func TestParseFlagsMultiJobValidationErrorNamesJob(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: broken
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `job "broken"`) {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, `job "broken"`)
	}
}

func TestParseFlagsInterval(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    interval: 24h
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.Interval != 24*time.Hour {
		t.Errorf("cfg.interval = %v, want 24h", cfg.Interval)
	}
}

func TestParseFlagsIntervalDefaultsToZero(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if cfg := singleJob(t, rc); cfg.Interval != 0 {
		t.Errorf("cfg.interval = %v, want 0", cfg.Interval)
	}
}

func TestParseFlagsIntervalNegativeRejected(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    interval: -1h
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "interval must not be negative") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "interval must not be negative")
	}
}

func TestParseFlagsMultiJobPerJobInterval(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
interval: 1h

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-hourly
    cmd: "echo hi"
  - id: job-daily
    cmd: "echo bye"
  - id: job-once
    cmd: "echo once"

jobs:
  - name: hourly
    files: [{command: job-hourly}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
  - name: daily
    files: [{command: job-daily}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    interval: 24h
  - name: once
    files: [{command: job-once}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    interval: 0
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	want := map[string]time.Duration{"hourly": time.Hour, "daily": 24 * time.Hour, "once": 0}
	for _, j := range rc.Jobs {
		if j.Interval != want[j.Name] {
			t.Errorf("job %q interval = %v, want %v", j.Name, j.Interval, want[j.Name])
		}
	}
}

func TestParseFlagsIntervalBadFileValue(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    interval: "not-a-duration"
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("ParseFlags() expected error for invalid config file interval, got nil")
	}
}

func TestParseFlagsStartTime(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    interval: 1h
    start-time: "2026-01-01T03:00:00Z"
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if !cfg.StartTime.Equal(want) {
		t.Errorf("cfg.startTime = %v, want %v", cfg.StartTime, want)
	}
}

func TestParseFlagsStartTimeNormalizedToUTC(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    interval: 1h
    start-time: "2026-01-01T05:00:00+02:00"
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if !cfg.StartTime.Equal(want) || cfg.StartTime.Location() != time.UTC {
		t.Errorf("cfg.startTime = %v, want %v in UTC", cfg.StartTime, want)
	}
}

func TestParseFlagsStartTimeDefaultsToZero(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if cfg := singleJob(t, rc); !cfg.StartTime.IsZero() {
		t.Errorf("cfg.startTime = %v, want zero", cfg.StartTime)
	}
}

func TestParseFlagsStartTimeBadFileValue(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    interval: 1h
    start-time: "not-a-timestamp"
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("ParseFlags() expected error for invalid config file start-time, got nil")
	}
}

func TestParseFlagsStartTimeRequiresInterval(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    start-time: "2026-01-01T03:00:00Z"
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "start-time requires interval") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "start-time requires interval")
	}
}

func TestParseFlagsRemoteServer(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: sibling
    type: remote
    endpoint: "https://backup2.example.com:8443"

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: sibling, bucket: from-primary}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := Target{
		ServerName: "sibling",
		Kind:       ServerKindRemote,
		Bucket:     "from-primary",
		Endpoint:   "https://backup2.example.com:8443",
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.Targets, want)
	}
}

func TestParseFlagsRemoteServerUUID(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: sibling
    type: remote
    endpoint: https://backup2.example.com:8443
    server-uuid: " 6f1c2a9e-3b4d-4e5f-8a7b-1c2d3e4f5a6b "

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: sibling, bucket: from-primary}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() error: %v", err)
	}

	cfg := singleJob(t, rc)
	if got, want := cfg.Targets[0].ServerUUID, "6f1c2a9e-3b4d-4e5f-8a7b-1c2d3e4f5a6b"; got != want {
		t.Errorf("cfg.Targets[0].ServerUUID = %q, want %q", got, want)
	}
}

func TestResolveServerUUIDValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fs      FileServer
		wantErr string
	}{
		{"invalid uuid", FileServer{Name: "s", Type: "remote", Endpoint: "https://x", ServerUUID: "not-a-uuid"}, "is not a valid UUID"},
		{"local rejects uuid", FileServer{Name: "s", Type: "local", Path: "/mnt", ServerUUID: "6f1c2a9e-3b4d-4e5f-8a7b-1c2d3e4f5a6b"}, "server-uuid is not valid for type: local"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := ResolveServer(tt.fs); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ResolveServer() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseFlagsRemoteServerRequiresEndpoint(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: sibling
    type: remote

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: sibling, bucket: from-primary}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "endpoint is required for type: remote") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "endpoint is required for type: remote")
	}
}

func TestParseFlagsRemoteServerRejectsOtherFields(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: sibling
    type: remote
    endpoint: "https://backup2.example.com:8443"
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: sibling, bucket: from-primary}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "are not valid for type: remote") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "are not valid for type: remote")
	}
}

func TestParseFlagsStagingDirDefaultsEmpty(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.StagingDir != "" {
		t.Errorf("cfg.stagingDir = %q, want empty (OS default temp dir)", cfg.StagingDir)
	}
}

func TestParseFlagsStagingDir(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: echo hi

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    staging-dir: /var/lib/go-backup-tool/staging
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.StagingDir != "/var/lib/go-backup-tool/staging" {
		t.Errorf("cfg.stagingDir = %q, want %q", cfg.StagingDir, "/var/lib/go-backup-tool/staging")
	}
}

func TestParseFlagsGlobalGPGBinAndHomedir(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
gpg-bin: /usr/local/bin/gpg2
gpg-homedir: /etc/go-backup-tool/gnupg

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-default-gpg
    cmd: echo hi
  - id: job-custom-gpg
    cmd: echo hi

jobs:
  - name: default-gpg
    files: [{command: job-default-gpg}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
  - name: custom-gpg
    files: [{command: job-custom-gpg}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    gpg-bin: /opt/gpg/bin/gpg
    gpg-homedir: /opt/gpg/home
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	wantBin := map[string]string{"default-gpg": "/usr/local/bin/gpg2", "custom-gpg": "/opt/gpg/bin/gpg"}
	wantHomedir := map[string]string{"default-gpg": "/etc/go-backup-tool/gnupg", "custom-gpg": "/opt/gpg/home"}

	for _, j := range rc.Jobs {
		if j.GPGBin != wantBin[j.Name] {
			t.Errorf("job %q gpgBin = %q, want %q", j.Name, j.GPGBin, wantBin[j.Name])
		}

		if j.GPGHomedir != wantHomedir[j.Name] {
			t.Errorf("job %q gpgHomedir = %q, want %q", j.Name, j.GPGHomedir, wantHomedir[j.Name])
		}
	}
}

func TestParseFlagsOIDCSettings(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  oidc:
    enabled: true
    issuer: "https://idp.example.com"
    client-id: "my-client"

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	want := OIDCSettings{
		Enabled:            true,
		Issuer:             "https://idp.example.com",
		ClientID:           "my-client",
		Scopes:             []string{"openid", "profile", "email"},
		ButtonLabel:        "Sign in with SSO",
		GroupsClaim:        "groups",
		DefaultPermissions: permission.PermissionView | permission.PermissionDownload,
		GroupPermissions:   map[string]permission.Permission{},
		ReauthMaxAge:       DefaultOIDCReauthMaxAge,
	}

	if !reflect.DeepEqual(rc.OIDC, want) {
		t.Errorf("rc.OIDC = %+v, want %+v", rc.OIDC, want)
	}
}

func TestParseFlagsOIDCDefaultPermissions(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  oidc:
    enabled: true
    issuer: "https://idp.example.com"
    client-id: "my-client"
    default-permissions: ["view"]

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.OIDC.DefaultPermissions != permission.PermissionView {
		t.Errorf("rc.OIDC.DefaultPermissions = %v, want %v", rc.OIDC.DefaultPermissions, permission.PermissionView)
	}
}

func TestParseFlagsOIDCDefaultPermissionsRejectsUnknown(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  oidc:
    enabled: true
    issuer: "https://idp.example.com"
    client-id: "my-client"
    default-permissions: ["delete"]

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	if _, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("ParseFlags() with an unknown webui.oidc.default-permissions entry = nil error, want one")
	}
}

func TestParseFlagsOIDCCustomScopes(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  oidc:
    enabled: true
    issuer: "https://idp.example.com"
    client-id: "my-client"
    scopes: ["groups"]

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if !reflect.DeepEqual(rc.OIDC.Scopes, []string{"groups"}) {
		t.Errorf("rc.OIDC.Scopes = %v, want [groups]", rc.OIDC.Scopes)
	}
}

func TestParseFlagsOIDCEnabledRequiresWebUIEnabled(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  oidc:
    enabled: true
    issuer: "https://idp.example.com"
    client-id: "my-client"

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "webui.enabled is not") {
		t.Fatalf("ParseFlags() error = %v, want it to mention webui.enabled is not", err)
	}
}

func TestParseFlagsOIDCEnabledRequiresEveryField(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		oidcYAML   string
		wantErrHas string
	}{
		{
			name: "missing issuer",
			oidcYAML: `
    enabled: true
    client-id: "my-client"
`,
			wantErrHas: "webui.oidc.issuer",
		},
		{
			name: "missing client-id",
			oidcYAML: `
    enabled: true
    issuer: "https://idp.example.com"
`,
			wantErrHas: "webui.oidc.client-id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  oidc:`+tc.oidcYAML+`

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

			_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Fatalf("ParseFlags() error = %v, want substring %q", err, tc.wantErrHas)
			}
		})
	}
}

func TestParseFlagsOIDCDisabledByDefault(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.OIDC.Enabled {
		t.Error("rc.OIDC.Enabled = true, want false (oidc: not configured)")
	}
}

// TestParseFlagsServerName exercises the top-level server-name: option:
// it lands on RunConfig.ServerName and is copied onto every receiver (see
// ResolvedReceiver.ServerName), ready for renderStaleWebhookPayload/
// renderDownloadWebhookPayload/pipeline.renderReportSubject to substitute
// into a notification's {server_name} placeholder.
func TestParseFlagsServerName(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
server-name: primary-backup-host

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.ServerName != "primary-backup-host" {
		t.Errorf("rc.ServerName = %q, want %q", rc.ServerName, "primary-backup-host")
	}
}

func TestParseFlagsServerNameDefaultsEmpty(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.ServerName != "" {
		t.Errorf("rc.ServerName = %q, want empty when server-name: is unset", rc.ServerName)
	}
}

func TestParseFlagsJobFailureNotifications(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
server-name: primary-backup-host

servers:
  - name: s
    type: local
    path: /mnt/backups


commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    failure-notifications: [ops-email]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	job := singleJob(t, rc)

	want := []string{"ops-email"}
	if !reflect.DeepEqual(job.FailureNotifications, want) {
		t.Errorf("job.FailureNotifications = %+v, want %+v", job.FailureNotifications, want)
	}

	if job.ServerName != "primary-backup-host" {
		t.Errorf("job.ServerName = %q, want %q", job.ServerName, "primary-backup-host")
	}
}

func TestParseFlagsJobFailureNotificationsSharedDefaultOverridden(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
failure-notifications: [shared]

servers:
  - name: s
    type: local
    path: /mnt/backups


commands:
  - id: job-inherits
    cmd: "echo hi"
  - id: job-overrides
    cmd: "echo hi"

jobs:
  - name: inherits
    files: [{command: job-inherits}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
  - name: overrides
    files: [{command: job-overrides}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    failure-notifications: [override]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if len(rc.Jobs) != 2 {
		t.Fatalf("ParseFlags() jobs = %d, want 2", len(rc.Jobs))
	}

	inherits, overrides := rc.Jobs[0], rc.Jobs[1]

	if len(inherits.FailureNotifications) != 1 || inherits.FailureNotifications[0] != "shared" {
		t.Errorf("inherits.FailureNotifications = %+v, want the shared default [shared]", inherits.FailureNotifications)
	}

	if len(overrides.FailureNotifications) != 1 || overrides.FailureNotifications[0] != "override" {
		t.Errorf("overrides.FailureNotifications = %+v, want the per-job override [override]", overrides.FailureNotifications)
	}
}

// TestParseFlagsJobUnknownFailureNotificationID checks an unknown
// failure-notifications id isn't a parse error: notifications live in the
// state db, so ids are checked at startup (see settings.Manager.Load) and
// skipped when firing.
func TestParseFlagsJobUnknownFailureNotificationID(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    failure-notifications: [nope]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if job := singleJob(t, rc); !reflect.DeepEqual(job.FailureNotifications, []string{"nope"}) {
		t.Errorf("job.FailureNotifications = %+v, want [nope]", job.FailureNotifications)
	}
}

func TestParseFlagsCommandsDuplicateID(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: job-test
    cmd: "echo hi"
  - id: dup
    cmd: "echo one"
  - id: dup
    cmd: "echo two"

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "duplicate command id") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "duplicate command id")
	}
}

func TestParseFlagsCommandsMissingCmd(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: job-test
    cmd: "echo hi"
  - id: empty

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "cmd is required") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "cmd is required")
	}
}

func TestParseFlagsCommandsTimeoutDefault(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: job-test
    cmd: "echo hi"
  - id: alert
    cmd: "echo hi"

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    files: [{command: job-test}]
    targets:
      - server: s
        bucket: b
        on-error: {command: alert, after: 3}
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	job := singleJob(t, rc)

	cmd := job.Targets[0].OnErrorCommand
	if cmd == nil {
		t.Fatalf("Targets[0].OnErrorCommand = nil, want resolved command")
	}

	if cmd.Timeout != defaultOnErrorCommandTimeout {
		t.Errorf("cmd.Timeout = %v, want default %v", cmd.Timeout, defaultOnErrorCommandTimeout)
	}
}

func TestParseFlagsCommandsTimeoutRejectsNonPositive(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: job-test
    cmd: "echo hi"
  - id: alert
    cmd: "echo hi"
    timeout: "0s"

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "timeout must be positive")
	}
}

func TestParseFlagsTargetOnErrorResolves(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: job-test
    cmd: "echo hi"
  - id: restart-nas
    cmd: "systemctl restart nas"
    timeout: 10s

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    files: [{command: job-test}]
    targets:
      - server: s
        bucket: b
        on-error:
          command: restart-nas
          after: 3
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	job := singleJob(t, rc)

	want := &Command{ID: "restart-nas", Kind: CommandKindShell, Cmd: "systemctl restart nas", Timeout: 10 * time.Second, TimeoutSet: true}
	if !reflect.DeepEqual(job.Targets[0].OnErrorCommand, want) {
		t.Errorf("Targets[0].OnErrorCommand = %+v, want %+v", job.Targets[0].OnErrorCommand, want)
	}

	if job.Targets[0].OnErrorAfter != 3 {
		t.Errorf("Targets[0].OnErrorAfter = %d, want 3", job.Targets[0].OnErrorAfter)
	}

	if job.Targets[0].OnErrorOnce {
		t.Errorf("Targets[0].OnErrorOnce = true, want false (repeat defaults to true)")
	}
}

func TestParseFlagsTargetOnErrorRepeatFalse(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: job-test
    cmd: "echo hi"
  - id: alert
    cmd: "echo hi"

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    files: [{command: job-test}]
    targets:
      - server: s
        bucket: b
        on-error: {command: alert, after: 3, repeat: false}
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if !singleJob(t, rc).Targets[0].OnErrorOnce {
		t.Errorf("Targets[0].OnErrorOnce = false, want true for repeat: false")
	}
}

func TestParseFlagsTargetOnErrorUnknownCommand(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets:
      - server: s
        bucket: b
        on-error: {command: nope, after: 3}
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no command named") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "no command named")
	}
}

func TestParseFlagsTargetOnErrorAfterMustBePositive(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: job-test
    cmd: "echo hi"
  - id: alert
    cmd: "echo hi"

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    files: [{command: job-test}]
    targets:
      - server: s
        bucket: b
        on-error: {command: alert, after: 0}
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "on-error.after must be a positive integer") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "on-error.after must be a positive integer")
	}
}

func TestParseFlagsTargetOnErrorCommandRequired(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets:
      - server: s
        bucket: b
        on-error: {after: 3}
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "on-error.command is required") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "on-error.command is required")
	}
}

func TestParseFlagsTargetOnRecoverResolves(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: job-test
    cmd: "echo hi"
  - id: all-clear
    cmd: "echo recovered"
    timeout: 10s

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    files: [{command: job-test}]
    targets:
      - server: s
        bucket: b
        on-recover:
          command: all-clear
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	job := singleJob(t, rc)

	want := &Command{ID: "all-clear", Kind: CommandKindShell, Cmd: "echo recovered", Timeout: 10 * time.Second, TimeoutSet: true}
	if !reflect.DeepEqual(job.Targets[0].OnRecoverCommand, want) {
		t.Errorf("Targets[0].OnRecoverCommand = %+v, want %+v", job.Targets[0].OnRecoverCommand, want)
	}

	if job.Targets[0].OnErrorCommand != nil {
		t.Errorf("Targets[0].OnErrorCommand = %+v, want nil (on-recover alone doesn't imply on-error)", job.Targets[0].OnErrorCommand)
	}
}

func TestParseFlagsTargetOnRecoverUnknownCommand(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets:
      - server: s
        bucket: b
        on-recover: {command: nope}
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no command named") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "no command named")
	}
}

func TestParseFlagsTargetOnRecoverCommandRequired(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets:
      - server: s
        bucket: b
        on-recover: {command: "  "}
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "on-recover.command is required") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "on-recover.command is required")
	}
}

func TestParseFlagsConfigFileLogFile(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
log-file: " /var/log/go-backup-tool.log "

servers:
  - name: s
    type: local
    path: /mnt/backups

commands:
  - id: job-test
    cmd: "echo hi"

jobs:
  - name: test
    files: [{command: job-test}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if want := "/var/log/go-backup-tool.log"; rc.LogFile != want {
		t.Errorf("rc.LogFile = %q, want %q", rc.LogFile, want)
	}
}

func TestParseFlagsRejectsUnknownKeys(t *testing.T) {
	t.Parallel()

	tests := []parseErrorCase{
		{
			name:    "top level",
			yaml:    "gpg-binary: gpg\n",
			wantErr: "gpg-binary",
		},
		{
			name: "nested in job",
			yaml: `
servers:
  - name: s
    type: local
    path: /mnt/backups
commands:
  - id: job-j
    cmd: echo hi

jobs:
  - name: j
    files: [{command: job-j}]
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    recipent: typo@example.com
`,
			wantErr: "recipent",
		},
	}

	checkParseErrors(t, tests)
}

func TestLoadFileConfigEmptyFile(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, "# only a comment\n")

	fc, err := loadFileConfig(path, true)
	if err != nil {
		t.Fatalf("loadFileConfig() unexpected error: %v", err)
	}

	if fc == nil {
		t.Fatal("loadFileConfig() = nil, want an empty config")
	}
}

// parseErrorCase is a config file that ParseFlags must reject with an error
// containing wantErr.
type parseErrorCase struct {
	name    string
	yaml    string
	wantErr string
}

// checkParseErrors runs each of tests as a parallel subtest.
func checkParseErrors(t *testing.T, tests []parseErrorCase) {
	t.Helper()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseFlags([]string{"-config", writeConfigFile(t, tt.yaml)}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseFlags() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveReauthMaxAge(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]time.Duration{"": DefaultOIDCReauthMaxAge, "0": 0, "5m": 5 * time.Minute} {
		if got, err := resolveReauthMaxAge(in); err != nil || got != want {
			t.Errorf("resolveReauthMaxAge(%q) = %v, %v; want %v", in, got, err, want)
		}
	}

	for _, in := range []string{"-1m", "soon"} {
		if _, err := resolveReauthMaxAge(in); err == nil {
			t.Errorf("resolveReauthMaxAge(%q) error = nil, want an error", in)
		}
	}
}

func TestResolveEventLogRetention(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]time.Duration{"": DefaultEventLogRetention, "0": 0, "365d": 365 * 24 * time.Hour} {
		if got, err := resolveEventLogRetention(in); err != nil || got != want {
			t.Errorf("resolveEventLogRetention(%q) = %v, %v; want %v", in, got, err, want)
		}
	}

	for _, in := range []string{"-1d", "forever"} {
		if _, err := resolveEventLogRetention(in); err == nil {
			t.Errorf("resolveEventLogRetention(%q) error = nil, want an error", in)
		}
	}
}
