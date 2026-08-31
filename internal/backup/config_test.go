package backup

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

	"nilswitt.dev/go-backup-tool/internal/backup/app/config"
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
			yaml:    "servers:\n  - name: s\n    region: us-east-1\nrecipients: [me@example.com]\n",
			wantErr: "must define at least one job",
		},
		{
			name: "missing jobs list allowed with listen set",
			yaml: "webui:\n  enabled: true\n  listen: :8080\nservers:\n  - name: s\n    region: us-east-1\nrecipients: [me@example.com]\n",
		},
		{
			name:    "missing cmd",
			yaml:    "servers:\n  - name: s\n    region: us-east-1\njobs:\n  - name: test\n    targets: [{server: s, bucket: b}]\n    recipients: [me@example.com]\n",
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
			name:    "symmetric and recipient conflict",
			yaml:    "servers:\n  - name: s\n    region: us-east-1\njobs:\n  - name: test\n    cmd: echo hi\n    targets: [{server: s, bucket: b}]\n    symmetric: true\n    recipients: [me@example.com]\n",
			wantErr: "cannot be combined",
		},
		{
			name:    "neither recipient nor symmetric",
			yaml:    "servers:\n  - name: s\n    region: us-east-1\njobs:\n  - name: test\n    cmd: echo hi\n    targets: [{server: s, bucket: b}]\n",
			wantErr: "specify at least one recipient",
		},
		{
			name:    "symmetric without passphrase env",
			yaml:    "servers:\n  - name: s\n    region: us-east-1\njobs:\n  - name: test\n    cmd: echo hi\n    targets: [{server: s, bucket: b}]\n    symmetric: true\n",
			wantErr: "GPG_PASSPHRASE",
		},
		{
			name: "valid recipient config",
			yaml: "servers:\n  - name: s\n    region: us-east-1\njobs:\n  - name: test\n    cmd: echo hi\n    targets: [{server: s, bucket: b}]\n    recipients: [me@example.com]\n",
		},
		{
			name: "valid symmetric config",
			yaml: "servers:\n  - name: s\n    region: us-east-1\njobs:\n  - name: test\n    cmd: echo hi\n    targets: [{server: s, bucket: b}]\n    symmetric: true\n",
			env:  map[string]string{"GPG_PASSPHRASE": "secret"},
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
    region: us-east-1

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
    region: us-east-1

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
    region: eu-central-1

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

	want := Target{ServerName: "primary", Bucket: "file-bucket", Region: "eu-central-1"}
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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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

func TestParseFlagsWebUIUsernamePassword(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
webui:
  enabled: true
  listen: ":0"
  username: "admin"
  password: "secret"

servers:
  - name: s
    region: us-east-1

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

	if rc.WebUIUsername != "admin" {
		t.Errorf("rc.WebUIUsername = %q, want %q", rc.WebUIUsername, "admin")
	}

	if rc.WebUIPassword != "secret" {
		t.Errorf("rc.WebUIPassword = %q, want %q", rc.WebUIPassword, "secret")
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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: eu-central-1
  - name: secondary
    region: us-west-2

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

	wantDBTarget := Target{ServerName: "primary", Bucket: "db-bucket", Region: "eu-central-1"}
	if len(db.Targets) != 1 || db.Targets[0] != wantDBTarget {
		t.Errorf("db.targets = %+v, want [%+v]", db.Targets, wantDBTarget)
	}

	// db job inherits the shared recipients default.
	if len(db.Recipients) != 1 || db.Recipients[0] != "default@example.com" {
		t.Errorf("db.recipients = %v, want inherited [default@example.com]", db.Recipients)
	}

	// files job targets a different server and overrides recipients.
	wantFilesTarget := Target{ServerName: "secondary", Bucket: "files-bucket", Region: "us-west-2"}
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
    region: eu-central-1
    endpoint: "https://s3.example.com"
  - name: offsite
    region: us-west-2
    endpoint: "https://minio.example.com"
    path-style: true

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
		{ServerName: "primary", Bucket: "primary-bucket", Region: "eu-central-1", Endpoint: "https://s3.example.com"},
		{ServerName: "offsite", Bucket: "secondary-bucket", Region: "us-west-2", Endpoint: "https://minio.example.com", PathStyle: true},
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
    region: us-west-2

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
  - region: us-east-1

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
    region: us-east-1
  - name: dup
    region: us-west-2

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

func TestParseFlagsServerDefaultRegion(t *testing.T) {
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

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if len(cfg.Targets) != 1 || cfg.Targets[0].Region != config.DefaultRegion {
		t.Errorf("cfg.targets = %+v, want region %q", cfg.Targets, config.DefaultRegion)
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

func TestParseFlagsS3ServerRejectsRetention(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    retention: 168h

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "retention is not valid for type: s3") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "retention is not valid for type: s3")
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
			name: "s3 server",
			yaml: `
servers:
  - name: s

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b, retention: 168h}]
    recipients: [me@example.com]
`,
			wantErr: `retention is not valid for server "s" (type s3; local only)`,
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

func TestParseFlagsLocalServerRejectsS3Fields(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "are not valid for type: local") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "are not valid for type: local")
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

func TestParseFlagsServerCredentialsFromEnv(t *testing.T) {
	t.Setenv("TEST_ACCESS_KEY", "AKIATEST")
	t.Setenv("TEST_SECRET_KEY", "shh-secret")

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1
    access-key-env: TEST_ACCESS_KEY
    secret-key-env: TEST_SECRET_KEY

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
	if len(cfg.Targets) != 1 {
		t.Fatalf("cfg.targets = %+v, want 1 entry", cfg.Targets)
	}

	got := cfg.Targets[0]
	if got.AccessKey != "AKIATEST" || got.SecretKey != "shh-secret" {
		t.Errorf("cfg.targets[0] = %+v, want accessKey %q secretKey %q", got, "AKIATEST", "shh-secret")
	}
}

func TestParseFlagsServerCredentialsRequireBothEnvVars(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1
    access-key-env: TEST_ACCESS_KEY_ONLY

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "must be set together")
	}
}

func TestParseFlagsJobFilterSkipsUnusedServerCredentials(t *testing.T) {
	t.Parallel()

	// A server's access-key-env/secret-key-env should only be required
	// when a job that actually targets it survives -job filtering.
	path := writeConfigFile(t, `
servers:
  - name: primary
    region: us-east-1
  - name: offsite
    region: us-east-1
    access-key-env: UNSET_OFFSITE_ACCESS_KEY
    secret-key-env: UNSET_OFFSITE_SECRET_KEY

jobs:
  - name: database
    cmd: echo hi
    targets: [{server: primary, bucket: b}]
    recipients: [me@example.com]
  - name: uploads
    cmd: echo hi
    targets: [{server: offsite, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path, "-job", "database"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.Name != "database" {
		t.Errorf("cfg.name = %q, want %q", cfg.Name, "database")
	}
}

func TestParseFlagsServerCredentialsEnvVarUnset(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1
    access-key-env: DOES_NOT_EXIST_ACCESS
    secret-key-env: DOES_NOT_EXIST_SECRET

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `"DOES_NOT_EXIST_ACCESS"`) {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, `"DOES_NOT_EXIST_ACCESS"`)
	}
}

func TestParseFlagsMultiJobRequiresName(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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

func TestParseFlagsMultiJobSharedPassphrase(t *testing.T) {
	t.Setenv("GPG_PASSPHRASE", "secret")

	path := writeConfigFile(t, `
symmetric: true

servers:
  - name: s
    region: us-east-1

jobs:
  - name: a
    cmd: "echo hi"
    targets: [{server: s, bucket: b1}]
  - name: b
    cmd: "echo bye"
    targets: [{server: s, bucket: b2}]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	for _, j := range rc.Jobs {
		if j.Passphrase != "secret" {
			t.Errorf("job %q passphrase = %q, want %q", j.Name, j.Passphrase, "secret")
		}
	}

	if os.Getenv("GPG_PASSPHRASE") != "" {
		t.Error("GPG_PASSPHRASE should have been cleared from the environment")
	}
}

func TestParseFlagsMultiJobValidationErrorNamesJob(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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

func TestParseFlagsStartTimeDefaultsToZero(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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
    region: us-east-1

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

func TestParseFlagsReceiverStaleAfterAndWebhook(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1

receivers:
  - id: from-primary
    public-key: |
`+indentYAMLBlock(testConfigRSAPublicKeyPEM, "      ")+`
    path: `+dir+`
    stale-after: 6h
    webhook:
      url: "https://alerts.example.com/hook"
      method: put
      headers:
        Authorization: "Bearer webhook-token"
      body: '{"text":"{receiver_id} is stale"}'

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
		StaleAfter: 6 * time.Hour,
		Webhook: ResolvedWebhook{
			URL:     "https://alerts.example.com/hook",
			Method:  http.MethodPut,
			Headers: map[string]string{"Authorization": "Bearer webhook-token"},
			Body:    `{"text":"{receiver_id} is stale"}`,
		},
	}
	if !reflect.DeepEqual(recv, want) {
		t.Errorf("rc.Receivers[%q] = %+v, want %+v", "from-primary", recv, want)
	}
}

func TestParseFlagsReceiverStaleAfterRequiresWebhook(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1

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
	if err == nil || !strings.Contains(err.Error(), "stale-after and webhook.url must be set together") {
		t.Fatalf("ParseFlags() error = %v, want substring %q", err, "stale-after and webhook.url must be set together")
	}
}

func TestParseFlagsRetriesDefaults(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1

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
	if cfg.Retries != config.DefaultRetries {
		t.Errorf("cfg.retries = %v, want %v", cfg.Retries, config.DefaultRetries)
	}

	if cfg.StagingDir != "" {
		t.Errorf("cfg.stagingDir = %q, want empty (OS default temp dir)", cfg.StagingDir)
	}
}

func TestParseFlagsRetriesAndStagingDir(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    retries: 5
    staging-dir: /var/lib/go-backup-tool/staging
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.Retries != 5 {
		t.Errorf("cfg.retries = %v, want 5", cfg.Retries)
	}

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
    region: us-east-1

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

func TestParseFlagsMultiJobPerJobRetries(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
retries: 2

servers:
  - name: s
    region: us-east-1

jobs:
  - name: default-retries
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
  - name: custom-retries
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
    retries: 7
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() unexpected error: %v", err)
	}

	want := map[string]int{"default-retries": 2, "custom-retries": 7}

	for _, j := range rc.Jobs {
		if j.Retries != want[j.Name] {
			t.Errorf("job %q retries = %v, want %v", j.Name, j.Retries, want[j.Name])
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
    client-secret: "s3cr3t"
    redirect-url: "https://backups.example.com/login/oidc/callback"

servers:
  - name: s
    region: us-east-1

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
		ClientSecret:       "s3cr3t",
		RedirectURL:        "https://backups.example.com/login/oidc/callback",
		Scopes:             []string{"profile", "email"},
		DefaultPermissions: PermissionView | PermissionDownload,
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
    client-secret: "s3cr3t"
    redirect-url: "https://backups.example.com/login/oidc/callback"
    default-permissions: ["view"]

servers:
  - name: s
    region: us-east-1

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

	if rc.OIDC.DefaultPermissions != PermissionView {
		t.Errorf("rc.OIDC.DefaultPermissions = %v, want %v", rc.OIDC.DefaultPermissions, PermissionView)
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
    client-secret: "s3cr3t"
    redirect-url: "https://backups.example.com/login/oidc/callback"
    default-permissions: ["delete"]

servers:
  - name: s
    region: us-east-1

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
    client-secret: "s3cr3t"
    redirect-url: "https://backups.example.com/login/oidc/callback"
    scopes: ["groups"]

servers:
  - name: s
    region: us-east-1

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
    client-secret: "s3cr3t"
    redirect-url: "https://backups.example.com/login/oidc/callback"

servers:
  - name: s
    region: us-east-1

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
    client-secret: "s3cr3t"
    redirect-url: "https://backups.example.com/login/oidc/callback"`,
			wantErrHas: "webui.oidc.issuer",
		},
		{
			name: "missing client-id",
			oidcYAML: `
    enabled: true
    issuer: "https://idp.example.com"
    client-secret: "s3cr3t"
    redirect-url: "https://backups.example.com/login/oidc/callback"`,
			wantErrHas: "webui.oidc.client-id",
		},
		{
			name: "missing client-secret",
			oidcYAML: `
    enabled: true
    issuer: "https://idp.example.com"
    client-id: "my-client"
    redirect-url: "https://backups.example.com/login/oidc/callback"`,
			wantErrHas: "webui.oidc.client-secret",
		},
		{
			name: "missing redirect-url",
			oidcYAML: `
    enabled: true
    issuer: "https://idp.example.com"
    client-id: "my-client"
    client-secret: "s3cr3t"`,
			wantErrHas: "webui.oidc.redirect-url",
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
    region: us-east-1

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
    region: us-east-1

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
report:
  enabled: true
  to: ["ops@example.com"]
  time: "06:30"
  smtp:
    host: smtp.example.com
    port: 2525
    username: backups@example.com
    password-env: TEST_SMTP_PASSWORD
    security: none

servers:
  - name: s
    region: us-east-1

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

	want := ReportSettings{
		Enabled:    true,
		To:         []string{"ops@example.com"},
		From:       "backups@example.com",
		SendHour:   6,
		SendMinute: 30,
		SMTP: SMTPSettings{
			Host:     "smtp.example.com",
			Port:     2525,
			Username: "backups@example.com",
			Password: "s3cr3t",
			Security: SMTPSecurityNone,
		},
	}

	if !reflect.DeepEqual(rc.Report, want) {
		t.Errorf("rc.Report = %+v, want %+v", rc.Report, want)
	}
}

func TestParseFlagsReportDisabledByDefault(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: s
    region: us-east-1

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

	if rc.Report.Enabled {
		t.Error("rc.Report.Enabled = true, want false (report: not configured)")
	}
}

func TestParseFlagsReportEnabledRequiresTo(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
report:
  enabled: true
  smtp:
    host: smtp.example.com

servers:
  - name: s
    region: us-east-1

jobs:
  - name: test
    cmd: "echo hi"
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "report.to") {
		t.Fatalf("ParseFlags() error = %v, want it to mention report.to", err)
	}
}
