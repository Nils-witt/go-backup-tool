package backup

import (
	"bytes"
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
func singleJob(t *testing.T, rc *runConfig) *config {
	t.Helper()

	if len(rc.jobs) != 1 {
		t.Fatalf("parseFlags() jobs = %d, want 1", len(rc.jobs))
	}

	return rc.jobs[0]
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

			rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("parseFlags() unexpected error: %v", err)
				}

				if rc == nil {
					t.Fatal("parseFlags() returned nil config with no error")
				}

				return
			}

			if err == nil {
				t.Fatalf("parseFlags() expected error containing %q, got nil", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseFlags() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestParseFlagsHelp(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	_, err := parseFlags([]string{"-h"}, &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseFlags(-h) error = %v, want flag.ErrHelp", err)
	}

	if out.Len() == 0 {
		t.Error("parseFlags(-h) wrote no usage output")
	}
}

//nolint:paralleltest // t.Chdir changes the process's working directory, so this test can't have parallel ancestors
func TestParseFlagsNoConfigFile(t *testing.T) {
	t.Chdir(t.TempDir())

	_, err := parseFlags(nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no config file found") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "no config file found")
	}
}

func TestParseFlagsKeyTimeNotYetSubstituted(t *testing.T) {
	t.Parallel()

	// parseFlags leaves {time} in the key unresolved: substituteKeyTime
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	if cfg.key != "prefix-{time}-suffix.gpg" {
		t.Errorf("parseFlags() key = %q, want unresolved template %q", cfg.key, "prefix-{time}-suffix.gpg")
	}
}

func TestSubstituteKeyTime(t *testing.T) {
	t.Parallel()

	got := substituteKeyTime("prefix-{time}-suffix.gpg")

	if strings.Contains(got, "{time}") {
		t.Errorf("substituteKeyTime() = %q, want {time} placeholder substituted", got)
	}

	if !strings.HasPrefix(got, "prefix-") || !strings.HasSuffix(got, "-suffix.gpg") {
		t.Errorf("substituteKeyTime() = %q, want prefix-<timestamp>-suffix.gpg", got)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := []string{"a@example.com", "b@example.com"}
	if len(cfg.recipients) != len(want) {
		t.Fatalf("parseFlags() recipients = %v, want %v", cfg.recipients, want)
	}

	for i, r := range want {
		if cfg.recipients[i] != r {
			t.Errorf("parseFlags() recipients[%d] = %q, want %q", i, cfg.recipients[i], r)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	if cfg.cmd != "echo from-file" {
		t.Errorf("cfg.cmd = %q, want %q", cfg.cmd, "echo from-file")
	}

	want := target{serverName: "primary", bucket: "file-bucket", region: "eu-central-1"}
	if len(cfg.targets) != 1 || cfg.targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.targets, want)
	}

	if len(cfg.recipients) != 1 || cfg.recipients[0] != "file@example.com" {
		t.Errorf("cfg.recipients = %v, want [file@example.com]", cfg.recipients)
	}

	if rc.timeout != 5*time.Minute {
		t.Errorf("rc.timeout = %v, want 5m", rc.timeout)
	}

	if rc.listen != ":8080" {
		t.Errorf("rc.listen = %q, want %q", rc.listen, ":8080")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if rc.listen != "" {
		t.Errorf("rc.listen = %q, want empty (web UI disabled by default)", rc.listen)
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "webui.listen is not set") {
		t.Fatalf("parseFlags() error = %v, want it to mention webui.listen is not set", err)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if rc.listen != "" {
		t.Errorf("rc.listen = %q, want empty (webui.enabled unset/false)", rc.listen)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if rc.webUIUsername != "admin" {
		t.Errorf("rc.webUIUsername = %q, want %q", rc.webUIUsername, "admin")
	}

	if rc.webUIPassword != "secret" {
		t.Errorf("rc.webUIPassword = %q, want %q", rc.webUIPassword, "secret")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if rc.logViewer {
		t.Error("rc.logViewer = true, want false (log viewer disabled by default)")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if !rc.logViewer {
		t.Error("rc.logViewer = false, want true (enable-log-viewer: true set in config file)")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if rc.logLevel != slog.LevelDebug {
		t.Errorf("rc.logLevel = %v, want %v", rc.logLevel, slog.LevelDebug)
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

	rc, err := parseFlags([]string{"-config", path, "-log-level", "error"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if rc.logLevel != slog.LevelError {
		t.Errorf("rc.logLevel = %v, want %v (explicit -log-level should win)", rc.logLevel, slog.LevelError)
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseFlags() expected error for invalid config file log-level, got nil")
	}
}

func TestParseFlagsConfigFileMissingExplicit(t *testing.T) {
	t.Parallel()

	_, err := parseFlags([]string{
		"-config", filepath.Join(t.TempDir(), "does-not-exist.yaml"),
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseFlags() expected error for missing explicit -config file, got nil")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseFlags() expected error for invalid config file timeout, got nil")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if len(rc.jobs) != 2 {
		t.Fatalf("parseFlags() jobs = %d, want 2", len(rc.jobs))
	}

	db, files := rc.jobs[0], rc.jobs[1]

	if db.name != "database" || db.cmd != "mysqldump db" {
		t.Errorf("db job = %+v", db)
	}

	wantDBTarget := target{serverName: "primary", bucket: "db-bucket", region: "eu-central-1"}
	if len(db.targets) != 1 || db.targets[0] != wantDBTarget {
		t.Errorf("db.targets = %+v, want [%+v]", db.targets, wantDBTarget)
	}

	// db job inherits the shared recipients default.
	if len(db.recipients) != 1 || db.recipients[0] != "default@example.com" {
		t.Errorf("db.recipients = %v, want inherited [default@example.com]", db.recipients)
	}

	// files job targets a different server and overrides recipients.
	wantFilesTarget := target{serverName: "secondary", bucket: "files-bucket", region: "us-west-2"}
	if len(files.targets) != 1 || files.targets[0] != wantFilesTarget {
		t.Errorf("files.targets = %+v, want [%+v]", files.targets, wantFilesTarget)
	}

	if len(files.recipients) != 1 || files.recipients[0] != "files-only@example.com" {
		t.Errorf("files.recipients = %v, want override [files-only@example.com]", files.recipients)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := []target{
		{serverName: "primary", bucket: "primary-bucket", region: "eu-central-1", endpoint: "https://s3.example.com"},
		{serverName: "offsite", bucket: "secondary-bucket", region: "us-west-2", endpoint: "https://minio.example.com", pathStyle: true},
	}

	if len(cfg.targets) != len(want) {
		t.Fatalf("cfg.targets = %+v, want %+v", cfg.targets, want)
	}

	for i := range want {
		if cfg.targets[i] != want[i] {
			t.Errorf("cfg.targets[%d] = %+v, want %+v", i, cfg.targets[i], want[i])
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "targets[0]: server is required") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "targets[0]: server is required")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "targets[0]: bucket is required") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "targets[0]: bucket is required")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "servers[0]: name is required") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "servers[0]: name is required")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `duplicate server name "dup"`) {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, `duplicate server name "dup"`)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if len(cfg.targets) != 1 || cfg.targets[0].region != defaultRegion {
		t.Errorf("cfg.targets = %+v, want region %q", cfg.targets, defaultRegion)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := target{serverName: "nas", kind: serverKindLocal, bucket: "b", localPath: dir}
	if len(cfg.targets) != 1 || cfg.targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.targets, want)
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "path is required for type: local") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "path is required for type: local")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := target{serverName: "nas", kind: serverKindLocal, bucket: "b", localPath: dir, retention: 168 * time.Hour}
	if len(cfg.targets) != 1 || cfg.targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.targets, want)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := target{serverName: "nas", kind: serverKindLocal, bucket: "b", localPath: dir, retention: 7 * 24 * time.Hour}
	if len(cfg.targets) != 1 || cfg.targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.targets, want)
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "retention must not be negative") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "retention must not be negative")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "retention is not valid for type: s3") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "retention is not valid for type: s3")
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

			rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
			if err != nil {
				t.Fatalf("parseFlags() unexpected error: %v", err)
			}

			cfg := singleJob(t, rc)

			want := target{serverName: "nas", kind: serverKindLocal, bucket: "b", localPath: dir, retention: tt.wantRetention}
			if len(cfg.targets) != 1 || cfg.targets[0] != want {
				t.Errorf("cfg.targets = %+v, want [%+v]", cfg.targets, want)
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
    token: shared-secret

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

			_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseFlags() error = %v, want substring %q", err, tt.wantErr)
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "are not valid for type: local") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "are not valid for type: local")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `unknown type "ftp"`) {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, `unknown type "ftp"`)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if len(cfg.targets) != 1 {
		t.Fatalf("cfg.targets = %+v, want 1 entry", cfg.targets)
	}

	got := cfg.targets[0]
	if got.accessKey != "AKIATEST" || got.secretKey != "shh-secret" {
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "must be set together")
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

	rc, err := parseFlags([]string{"-config", path, "-job", "database"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.name != "database" {
		t.Errorf("cfg.name = %q, want %q", cfg.name, "database")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `"DOES_NOT_EXIST_ACCESS"`) {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, `"DOES_NOT_EXIST_ACCESS"`)
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "name is required")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "duplicate job name") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "duplicate job name")
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

	rc, err := parseFlags([]string{"-config", path, "-job", "files"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.name != "files" {
		t.Errorf("cfg.name = %q, want %q", cfg.name, "files")
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

	_, err := parseFlags([]string{"-config", path, "-job", "nope"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no such job") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "no such job")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	for _, j := range rc.jobs {
		if j.passphrase != "secret" {
			t.Errorf("job %q passphrase = %q, want %q", j.name, j.passphrase, "secret")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `job "broken"`) {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, `job "broken"`)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.interval != 24*time.Hour {
		t.Errorf("cfg.interval = %v, want 24h", cfg.interval)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if cfg := singleJob(t, rc); cfg.interval != 0 {
		t.Errorf("cfg.interval = %v, want 0", cfg.interval)
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "interval must not be negative") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "interval must not be negative")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	want := map[string]time.Duration{"hourly": time.Hour, "daily": 24 * time.Hour, "once": 0}
	for _, j := range rc.jobs {
		if j.interval != want[j.name] {
			t.Errorf("job %q interval = %v, want %v", j.name, j.interval, want[j.name])
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseFlags() expected error for invalid config file interval, got nil")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if !cfg.startTime.Equal(want) {
		t.Errorf("cfg.startTime = %v, want %v", cfg.startTime, want)
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	if cfg := singleJob(t, rc); !cfg.startTime.IsZero() {
		t.Errorf("cfg.startTime = %v, want zero", cfg.startTime)
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseFlags() expected error for invalid config file start-time, got nil")
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "start-time requires interval") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "start-time requires interval")
	}
}

func TestParseFlagsRemoteServer(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: sibling
    type: remote
    endpoint: "https://backup2.example.com:8443"
    token: "shared-secret"

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: sibling, bucket: from-primary}]
    recipients: [me@example.com]
`)

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)

	want := target{
		serverName: "sibling",
		kind:       serverKindRemote,
		bucket:     "from-primary",
		endpoint:   "https://backup2.example.com:8443",
		token:      "shared-secret",
	}
	if len(cfg.targets) != 1 || cfg.targets[0] != want {
		t.Errorf("cfg.targets = %+v, want [%+v]", cfg.targets, want)
	}
}

func TestParseFlagsRemoteServerRequiresEndpoint(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: sibling
    type: remote
    token: "shared-secret"

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: sibling, bucket: from-primary}]
    recipients: [me@example.com]
`)

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "endpoint is required for type: remote") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "endpoint is required for type: remote")
	}
}

func TestParseFlagsRemoteServerRequiresToken(t *testing.T) {
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "token is required for type: remote") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "token is required for type: remote")
	}
}

func TestParseFlagsRemoteServerRejectsOtherFields(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: sibling
    type: remote
    endpoint: "https://backup2.example.com:8443"
    token: "shared-secret"
    path: /mnt/backups

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: sibling, bucket: from-primary}]
    recipients: [me@example.com]
`)

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "are not valid for type: remote") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "are not valid for type: remote")
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
    token: "shared-secret"
    path: `+dir+`
    retention: 30d

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	recv, ok := rc.receivers["from-primary"]
	if !ok {
		t.Fatalf("rc.receivers = %+v, want an entry for %q", rc.receivers, "from-primary")
	}

	want := resolvedReceiver{id: "from-primary", token: "shared-secret", path: dir, retention: 30 * 24 * time.Hour}
	if !reflect.DeepEqual(recv, want) {
		t.Errorf("rc.receivers[%q] = %+v, want %+v", "from-primary", recv, want)
	}
}

func TestParseFlagsReceiverRequiresToken(t *testing.T) {
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

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `receiver "from-primary": token is required`) {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, `receiver "from-primary": token is required`)
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
    token: "a"
    path: /mnt/a
  - id: dup
    token: "b"
    path: /mnt/b

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `duplicate receiver id "dup"`) {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, `duplicate receiver id "dup"`)
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
    token: "shared-secret"
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	recv, ok := rc.receivers["from-primary"]
	if !ok {
		t.Fatalf("rc.receivers = %+v, want an entry for %q", rc.receivers, "from-primary")
	}

	want := resolvedReceiver{
		id: "from-primary", token: "shared-secret", path: dir,
		staleAfter: 6 * time.Hour,
		webhook: resolvedWebhook{
			url:     "https://alerts.example.com/hook",
			method:  http.MethodPut,
			headers: map[string]string{"Authorization": "Bearer webhook-token"},
			body:    `{"text":"{receiver_id} is stale"}`,
		},
	}
	if !reflect.DeepEqual(recv, want) {
		t.Errorf("rc.receivers[%q] = %+v, want %+v", "from-primary", recv, want)
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
    token: "shared-secret"
    path: /mnt/backups
    stale-after: 6h

jobs:
  - name: test
    cmd: echo hi
    targets: [{server: s, bucket: b}]
    recipients: [me@example.com]
`)

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "stale-after and webhook.url must be set together") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "stale-after and webhook.url must be set together")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.retries != defaultRetries {
		t.Errorf("cfg.retries = %v, want %v", cfg.retries, defaultRetries)
	}

	if cfg.retryDelay != defaultRetryDelay {
		t.Errorf("cfg.retryDelay = %v, want %v", cfg.retryDelay, defaultRetryDelay)
	}

	if cfg.stagingDir != "" {
		t.Errorf("cfg.stagingDir = %q, want empty (OS default temp dir)", cfg.stagingDir)
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
    retry-delay: 30s
    staging-dir: /var/lib/go-backup-tool/staging
`)

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	cfg := singleJob(t, rc)
	if cfg.retries != 5 {
		t.Errorf("cfg.retries = %v, want 5", cfg.retries)
	}

	if cfg.retryDelay != 30*time.Second {
		t.Errorf("cfg.retryDelay = %v, want 30s", cfg.retryDelay)
	}

	if cfg.stagingDir != "/var/lib/go-backup-tool/staging" {
		t.Errorf("cfg.stagingDir = %q, want %q", cfg.stagingDir, "/var/lib/go-backup-tool/staging")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	wantBin := map[string]string{"default-gpg": "/usr/local/bin/gpg2", "custom-gpg": "/opt/gpg/bin/gpg"}
	wantHomedir := map[string]string{"default-gpg": "/etc/go-backup-tool/gnupg", "custom-gpg": "/opt/gpg/home"}

	for _, j := range rc.jobs {
		if j.gpgBin != wantBin[j.name] {
			t.Errorf("job %q gpgBin = %q, want %q", j.name, j.gpgBin, wantBin[j.name])
		}

		if j.gpgHomedir != wantHomedir[j.name] {
			t.Errorf("job %q gpgHomedir = %q, want %q", j.name, j.gpgHomedir, wantHomedir[j.name])
		}
	}
}

func TestParseFlagsRetryDelayBadFileValue(t *testing.T) {
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
    retry-delay: "not-a-duration"
`)

	_, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "parsing retry-delay") {
		t.Fatalf("parseFlags() error = %v, want substring %q", err, "parsing retry-delay")
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

	rc, err := parseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags() unexpected error: %v", err)
	}

	want := map[string]int{"default-retries": 2, "custom-retries": 7}

	for _, j := range rc.jobs {
		if j.retries != want[j.name] {
			t.Errorf("job %q retries = %v, want %v", j.name, j.retries, want[j.name])
		}
	}
}
