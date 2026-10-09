package config

import (
	"bytes"
	"crypto/rsa"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
)

// testConfigRSAPublicKeyPEM is a fixed RSA public key, PEM-encoded the same
// way ensureServerKeyPair writes server.pub, for embedding as a receivers:
// entry's public-key: in raw YAML config fixtures below (see
// indentYAMLBlock) — a fixed value keeps these fixtures readable, since the
// tests that need it don't care whose key it is, only that it's a valid
// one. testConfigRSAPublicKey parses it back, for building the
// resolvedReceiver a test expects ParseFlags to produce.
const testConfigRSAPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA0tEcLZdvrCAVooY+qTwb
0Er/KU65mc7jhrs6OV5yhDjzpdLD8/oN1stMyp47XAUbIwL7Sm0EaFmqbTPkgE+E
Q+3czxCDSTnRLMLBk7qK/QdTQ03zNTmq/ZGatISUl+OWeJP+EdC4vMTHrMKtBquM
rHOPc29Qc+KTTrRyqGJlFsfpFx6RuSphXDqC0rEuxcdxXf6/Nesux1r6yA1lJqcX
Tik8xq6oBBbbnF7CK4oUPMgSKlrOs2+TrYEv1jG4zmv6XFWu70z2mYbll5LvguIT
wnccZSbEZ0rr3WTuW3NGjGYJFXx1f1IzoCbt4LxjT3sLvqyWlmCXSnhAZkVvN5YQ
MQIDAQAB
-----END PUBLIC KEY-----
`

func testConfigRSAPublicKey(t *testing.T) *rsa.PublicKey {
	t.Helper()

	pub, err := parseReceiverPublicKey(testConfigRSAPublicKeyPEM)
	if err != nil {
		t.Fatalf("parsing testConfigRSAPublicKeyPEM: %v", err)
	}

	return pub
}

// indentYAMLBlock indents every line of text by indent, for embedding a
// multi-line PEM value under a YAML public-key: |  block scalar in the raw
// config fixtures below.
func indentYAMLBlock(text, indent string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}

	return strings.Join(lines, "\n")
}

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
			name:    "missing jobs list",
			yaml:    "servers:\n  - name: s\n    type: local\n    path: /mnt/backups\nrecipients: [me@example.com]\n",
			wantErr: "must define at least one job",
		},
		{
			name: "missing jobs list allowed with listen set",
			yaml: "webui:\n  enabled: true\n  listen: :8080\nservers:\n  - name: s\n    type: local\n    path: /mnt/backups\nrecipients: [me@example.com]\n",
		},
		{
			name:    "missing cmd",
			yaml:    "servers:\n  - name: s\n    type: local\n    path: /mnt/backups\njobs:\n  - name: test\n    targets: [{server: s, bucket: b}]\n    recipients: [me@example.com]\n",
			wantErr: "cmd is required",
		},
		{
			name:    "missing targets",
			yaml:    "jobs:\n  - name: test\n    cmd: echo hi\n    recipients: [me@example.com]\n",
			wantErr: "at least one target is required",
		},
		{
			name:    "target references unknown server",
			yaml:    "jobs:\n  - name: test\n    cmd: echo hi\n    recipients: [me@example.com]\n    targets: [{server: nope, bucket: b}]\n",
			wantErr: `no server named "nope"`,
		},
		{
			name:    "no recipients",
			yaml:    "servers:\n  - name: s\n    type: local\n    path: /mnt/backups\njobs:\n  - name: test\n    cmd: echo hi\n    targets: [{server: s, bucket: b}]\n",
			wantErr: "specify at least one recipient",
		},
		{
			name: "valid recipient config",
			yaml: "servers:\n  - name: s\n    type: local\n    path: /mnt/backups\njobs:\n  - name: test\n    cmd: echo hi\n    targets: [{server: s, bucket: b}]\n    recipients: [me@example.com]\n",
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

//nolint:paralleltest // t.Chdir changes the process's working directory, so this test can't have parallel ancestors
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: "echo from-file"
    targets: [{server: primary, bucket: file-bucket}]
    recipients:
      - file@example.com
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	if cfg.Cmd != "echo from-file" {
		t.Errorf("cfg.cmd = %q, want %q", cfg.Cmd, "echo from-file")
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

func TestParseFlagsConfigFileLogLevel(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
log-level: debug

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: database
    cmd: "mysqldump db"
    targets: [{server: primary, bucket: db-bucket}]
  - name: files
    cmd: "tar czf - /data"
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

	if db.Name != "database" || db.Cmd != "mysqldump db" {
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

jobs:
  - name: test
    cmd: echo hi
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
jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "negative duration",
			yaml: `
servers:
  - name: nas
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: sib, bucket: b, retention: 168h}]
    recipients: [me@example.com]
`,
			wantErr: `retention is not valid for server "sib" (type remote; local only)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfigFile(t, tt.yaml)

			_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseFlags() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseFlagsLocalServerRejectsRemoteFields(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: /mnt/backups
    endpoint: "https://example.com"

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - cmd: "echo hi"
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

jobs:
  - name: dup
    cmd: "echo hi"
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
  - name: dup
    cmd: "echo bye"
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

jobs:
  - name: database
    cmd: "mysqldump db"
    targets: [{server: s, bucket: db-bucket}]
    recipients: [me@example.com]
  - name: files
    cmd: "tar czf - /data"
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

jobs:
  - name: database
    cmd: "mysqldump db"
    targets: [{server: s, bucket: db-bucket}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path, "-job", "nope"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no such job") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "no such job")
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: hourly
    cmd: "echo hi"
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
  - name: daily
    cmd: "echo bye"
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    interval: 24h
  - name: once
    cmd: "echo once"
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

func TestParseFlagsRemoteServerRequiresEndpoint(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: sibling
    type: remote

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: sibling, bucket: from-primary}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "are not valid for type: remote") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "are not valid for type: remote")
	}
}

func TestParseFlagsReceivers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

receivers:
  - id: from-primary
    public-key: |
`+indentYAMLBlock(testConfigRSAPublicKeyPEM, "      ")+`
    path: `+dir+`
    retention: 30d

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	recv, ok := rc.Receivers["from-primary"]
	if !ok {
		t.Fatalf("rc.Receivers = %+v, want an entry for %q", rc.Receivers, "from-primary")
	}

	want := ResolvedReceiver{ID: "from-primary", PublicKey: testConfigRSAPublicKey(t), Path: dir, Retention: 30 * 24 * time.Hour}
	if !reflect.DeepEqual(recv, want) {
		t.Errorf("rc.Receivers[%q] = %+v, want %+v", "from-primary", recv, want)
	}
}

func TestParseFlagsReceiverRequiresPublicKey(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

receivers:
  - id: from-primary
    path: /mnt/backups

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `receiver "from-primary": public-key is required`) {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, `receiver "from-primary": public-key is required`)
	}
}

func TestParseFlagsReceiverDuplicateID(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

receivers:
  - id: dup
    public-key: |
`+indentYAMLBlock(testConfigRSAPublicKeyPEM, "      ")+`
    path: /mnt/a
  - id: dup
    public-key: |
`+indentYAMLBlock(testConfigRSAPublicKeyPEM, "      ")+`
    path: /mnt/b

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `duplicate receiver id "dup"`) {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, `duplicate receiver id "dup"`)
	}
}

func TestParseFlagsReceiverStaleAfterAndNotifications(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

notifications:
  - id: alerts
    webhook:
      url: "https://alerts.example.com/hook"
      method: put
      headers:
        Authorization: "Bearer webhook-token"
      body: '{"text":"{receiver_id} is stale"}'

receivers:
  - id: from-primary
    public-key: |
`+indentYAMLBlock(testConfigRSAPublicKeyPEM, "      ")+`
    path: `+dir+`
    stale-after: 6h
    stale-notifications: [alerts]

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	recv, ok := rc.Receivers["from-primary"]
	if !ok {
		t.Fatalf("rc.Receivers = %+v, want an entry for %q", rc.Receivers, "from-primary")
	}

	want := ResolvedReceiver{
		ID: "from-primary", PublicKey: testConfigRSAPublicKey(t), Path: dir,
		StaleAfter:         6 * time.Hour,
		StaleNotifications: []string{"alerts"},
	}
	if !reflect.DeepEqual(recv, want) {
		t.Errorf("rc.Receivers[%q] = %+v, want %+v", "from-primary", recv, want)
	}
}

func TestParseFlagsReceiverStaleAfterRequiresNotifications(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

receivers:
  - id: from-primary
    public-key: |
`+indentYAMLBlock(testConfigRSAPublicKeyPEM, "      ")+`
    path: /mnt/backups
    stale-after: 6h

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "stale-after and stale-notifications must be set together") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "stale-after and stale-notifications must be set together")
	}
}

func TestParseFlagsStagingDirDefaultsEmpty(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: test
    cmd: echo hi
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

jobs:
  - name: default-gpg
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
  - name: custom-gpg
    cmd: echo hi
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

func TestParseFlagsReportSettings(t *testing.T) {
	t.Setenv("TEST_SMTP_PASSWORD", "s3cr3t")

	path := writeConfigFile(t, `
smtp:
  host: smtp.example.com
  port: 2525
  username: backups@example.com
  password-env: TEST_SMTP_PASSWORD
  security: none

notifications:
  - id: ops-email
    email:
      to: ["ops@example.com"]

report:
  enabled: true
  schedule: "30 6 * * *"
  notifications: [ops-email]

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	want := report.FileReport{Enabled: true, Schedule: "30 6 * * *", Notifications: []string{"ops-email"}}
	if !reflect.DeepEqual(rc.FileReport, want) {
		t.Errorf("rc.FileReport = %+v, want %+v", rc.FileReport, want)
	}

	wantSMTP := notify.SMTPSettings{Host: "smtp.example.com", Port: 2525, Username: "backups@example.com", Password: "s3cr3t", Security: notify.SMTPSecurityNone}
	if rc.SMTP != wantSMTP {
		t.Errorf("rc.SMTP = %+v, want %+v", rc.SMTP, wantSMTP)
	}
}

func TestParseFlagsReportDisabledByDefault(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	if rc.FileReport.Enabled {
		t.Error("rc.FileReport.Enabled = true, want false (report: not configured)")
	}
}

func TestParseFlagsReportEnabledRequiresNotifications(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
report:
  enabled: true

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "report.notifications") {
		t.Fatalf("ParseFlags() error = %v, want it to mention report.notifications", err)
	}
}

// TestParseFlagsNotificationSharedAcrossTriggers exercises the reusable
// notifications: design end to end: one notification combining both a
// webhook: and an email: channel, referenced by id from both a receiver's
// stale-notifications: and report.notifications:, confirming both keep
// the id (resolved against the live registry at fire time) and the
// notification itself resolves with both channels.
func TestParseFlagsNotificationSharedAcrossTriggers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writeConfigFile(t, `
smtp:
  host: smtp.example.com
  username: backups@example.com
  password: hunter2

notifications:
  - id: ops
    webhook:
      url: "https://example.com/hook"
    email:
      to: ["ops@example.com"]

servers:
  - name: s
    type: local
    path: /mnt/backups

receivers:
  - id: from-primary
    public-key: |
`+indentYAMLBlock(testConfigRSAPublicKeyPEM, "      ")+`
    path: `+dir+`
    stale-after: 6h
    stale-notifications: [ops]

report:
  enabled: true
  notifications: [ops]

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	recv, ok := rc.Receivers["from-primary"]
	if !ok {
		t.Fatalf("rc.Receivers = %+v, want an entry for %q", rc.Receivers, "from-primary")
	}

	want := notify.Notification{
		ID:      "ops",
		Webhook: &notify.Webhook{URL: "https://example.com/hook", Method: http.MethodPost},
		Email: &notify.Email{
			To:   []string{"ops@example.com"},
			From: "backups@example.com",
			SMTP: notify.SMTPSettings{Host: "smtp.example.com", Port: 587, Username: "backups@example.com", Password: "hunter2", Security: notify.SMTPSecurityStartTLS},
		},
	}

	if len(rc.FileNotifications) != 1 {
		t.Fatalf("rc.FileNotifications = %+v, want just ops", rc.FileNotifications)
	}

	got, err := notify.ResolveNotification(rc.FileNotifications[0], rc.SMTP, rc.GPG)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveNotification(ops) = %+v, %v, want %+v", got, err, want)
	}

	if !reflect.DeepEqual(recv.StaleNotifications, []string{"ops"}) {
		t.Errorf("recv.StaleNotifications = %+v, want [ops]", recv.StaleNotifications)
	}

	if !reflect.DeepEqual(rc.FileReport.Notifications, []string{"ops"}) {
		t.Errorf("rc.FileReport.Notifications = %+v, want [ops]", rc.FileReport.Notifications)
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

receivers:
  - id: from-primary
    public-key: |
`+indentYAMLBlock(testConfigRSAPublicKeyPEM, "      ")+`
    path: /mnt/remote

jobs:
  - name: test
    cmd: "echo hi"
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

	if got := rc.Receivers["from-primary"].ServerName; got != "primary-backup-host" {
		t.Errorf("rc.Receivers[from-primary].ServerName = %q, want %q", got, "primary-backup-host")
	}
}

func TestParseFlagsServerNameDefaultsEmpty(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
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

notifications:
  - id: ops-email
    webhook:
      url: "https://alerts.example.com/hook"

jobs:
  - name: test
    cmd: "echo hi"
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

notifications:
  - id: shared
    webhook:
      url: "https://shared.example.com/hook"
  - id: override
    webhook:
      url: "https://override.example.com/hook"

jobs:
  - name: inherits
    cmd: "echo hi"
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
  - name: overrides
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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
    cmd: "echo hi"
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
  - id: empty

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
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
  - id: alert
    cmd: "echo hi"

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
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
  - id: alert
    cmd: "echo hi"
    timeout: "0s"

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
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
  - id: restart-nas
    cmd: "systemctl restart nas"
    timeout: 10s

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
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

	want := &Command{ID: "restart-nas", Cmd: "systemctl restart nas", Timeout: 10 * time.Second}
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
  - id: alert
    cmd: "echo hi"

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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
  - id: alert
    cmd: "echo hi"

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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
  - id: all-clear
    cmd: "echo recovered"
    timeout: 10s

servers:
  - name: s
    type: local
    path: /mnt/backups

jobs:
  - name: test
    cmd: "echo hi"
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

	want := &Command{ID: "all-clear", Cmd: "echo recovered", Timeout: 10 * time.Second}
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

jobs:
  - name: test
    cmd: "echo hi"
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

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
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
jobs:
  - name: j
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    recipent: typo@example.com
`,
			wantErr: "recipent",
		},
		{
			name: "removed email.encrypt gpg-bin",
			yaml: `
smtp:
  host: mail.example.com
  username: backups@example.com
  password: secret
notifications:
  - id: n
    email:
      to: [ops@example.com]
      encrypt:
        recipients: [ops@example.com]
        gpg-bin: /usr/local/bin/gpg
`,
			wantErr: "gpg-bin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfigFile(t, tc.yaml)

			_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseFlags() error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
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
