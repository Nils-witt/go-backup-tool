package config

import (
	"bytes"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/version"
)

func TestJobCommandReference(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: /mnt/backups

commands:
  - id: dump
    cmd: "pg_dump app"
    container: db
    container-user: postgres
    timeout: 2h

jobs:
  - name: db
    files: [{command: dump}]
    targets: [{server: nas, bucket: db}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() error: %v", err)
	}

	src := singleJob(t, rc).Sources[0]
	if src.ID != "dump" || src.Cmd != "pg_dump app" || src.Container != "db" || src.ContainerUser != "postgres" ||
		src.Timeout != 2*time.Hour || !src.TimeoutSet {
		t.Errorf("Source = %+v", src)
	}

	if len(rc.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", rc.Warnings)
	}
}

func TestInlineJobCmdIsConvertedToACommand(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
cmd: "echo default"

servers:
  - name: nas
    type: local
    path: /mnt/backups

commands:
  - id: job-db
    cmd: "already taken"

jobs:
  - name: db
    cmd: "pg_dump app"
    targets: [{server: nas, bucket: db}]
    recipients: [me@example.com]
  - name: files
    targets: [{server: nas, bucket: files}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() error: %v", err)
	}

	if len(rc.Jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(rc.Jobs))
	}

	if s := rc.Jobs[0].Sources[0]; s.ID != "job-db-2" || s.Cmd != "pg_dump app" || s.TimeoutSet {
		t.Errorf("db Source = %+v, want generated command job-db-2 without a timeout", s)
	}

	if s := rc.Jobs[1].Sources[0]; s.ID != "job-files" || s.Cmd != "echo default" {
		t.Errorf("files Source = %+v, want the inherited cmd as job-files", s)
	}

	checkConvertedFileDefs(t, rc)

	if len(rc.Warnings) != 2 {
		t.Errorf("Warnings = %v, want one per converted job", rc.Warnings)
	}
}

// checkConvertedFileDefs checks the raw definitions jobs.Manager imports:
// jobs name a command and carry no cmd, and the generated commands follow
// the config file's own.
func checkConvertedFileDefs(t *testing.T, rc *RunConfig) {
	t.Helper()

	for _, fj := range rc.FileJobs {
		if fj.Cmd != "" || fj.Command != "" || len(fj.Files) != 1 {
			t.Errorf("FileJob %q = cmd %q, command %q, files %+v; want only one file", fj.Name, fj.Cmd, fj.Command, fj.Files)
		}
	}

	ids := make([]string, len(rc.FileCommands))
	for i, fc := range rc.FileCommands {
		ids[i] = fc.ID
	}

	if got := strings.Join(ids, ","); got != "job-db,job-db-2,job-files" {
		t.Errorf("FileCommands = %s", got)
	}
}

func TestJobCmdAndCommandConflict(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: /mnt/backups

commands:
  - id: dump
    cmd: "pg_dump app"

jobs:
  - name: db
    cmd: "pg_dump app"
    command: dump
    targets: [{server: nas, bucket: db}]
    recipients: [me@example.com]
`)

	if _, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("ParseFlags() error = %v, want a cmd/command conflict", err)
	}
}

func TestJobFiles(t *testing.T) {
	t.Parallel()

	const header = `
servers:
  - name: nas
    type: local
    path: /mnt/backups

commands:
  - id: dump
    cmd: "pg_dump app"
  - id: tar
    cmd: "tar czf - /srv"

jobs:
  - name: db
    targets: [{server: nas, bucket: db}]
    recipients: [me@example.com]
`

	tests := []struct {
		name     string
		job      string
		expected []string // file names, in order
		wantErr  string
	}{
		{
			name:     "default names",
			job:      "    files: [{command: dump}, {command: tar}]\n",
			expected: []string{"dump.gpg", "tar.gpg"},
		},
		{
			name:     "armor default extension",
			job:      "    armor: true\n    files: [{command: dump}]\n",
			expected: []string{"dump.asc"},
		},
		{
			name:     "explicit names",
			job:      "    files: [{command: dump, name: db.sql.gpg}, {command: dump, name: db-again.sql.gpg}]\n",
			expected: []string{"db.sql.gpg", "db-again.sql.gpg"},
		},
		{
			name:    "duplicate default names",
			job:     "    files: [{command: dump}, {command: dump}]\n",
			wantErr: `duplicate file name "dump.gpg"`,
		},
		{
			name:    "name with a slash",
			job:     "    files: [{command: dump, name: a/b}]\n",
			wantErr: "must not contain",
		},
		{
			name:    "hidden name",
			job:     "    files: [{command: dump, name: .gbt-backup}]\n",
			wantErr: "must not start with",
		},
		{
			name:    "unknown command",
			job:     "    files: [{command: nope}]\n",
			wantErr: `no command "nope"`,
		},
		{
			name:    "files and deprecated command",
			job:     "    command: dump\n    files: [{command: tar}]\n",
			wantErr: "set either files or the deprecated command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rc, err := ParseFlags([]string{"-config", writeConfigFile(t, header+tt.job)}, &bytes.Buffer{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseFlags() error = %v, want substring %q", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseFlags() error: %v", err)
			}

			sources := singleJob(t, rc).Sources

			names := make([]string, len(sources))
			for i, s := range sources {
				names[i] = s.Name
			}

			if strings.Join(names, ",") != strings.Join(tt.expected, ",") {
				t.Errorf("file names = %v, want %v", names, tt.expected)
			}
		})
	}
}

func TestDeprecatedJobCommandBecomesFile(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: /mnt/backups

commands:
  - id: dump
    cmd: "pg_dump app"

jobs:
  - name: db
    command: dump
    targets: [{server: nas, bucket: db}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() error: %v", err)
	}

	if sources := singleJob(t, rc).Sources; len(sources) != 1 || sources[0].ID != "dump" || sources[0].Name != "dump.gpg" {
		t.Errorf("Sources = %+v, want the one file dump.gpg", sources)
	}

	if fj := rc.FileJobs[0]; fj.Command != "" || len(fj.Files) != 1 || fj.Files[0].Command != "dump" {
		t.Errorf("FileJob = %+v, want command moved into files", fj)
	}

	if len(rc.Warnings) != 1 || !strings.Contains(rc.Warnings[0], "command: is deprecated") || !strings.Contains(rc.Warnings[0], version.DeprecatedConfigRemoval) {
		t.Errorf("Warnings = %v, want one deprecation warning naming %s", rc.Warnings, version.DeprecatedConfigRemoval)
	}
}

func TestValidateFileName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		isValid bool
	}{
		{name: "plain", input: "db.sql.gpg", isValid: true},
		{name: "empty", input: ""},
		{name: "dot", input: "."},
		{name: "parent dir", input: ".."},
		{name: "hidden", input: ".x"},
		{name: "slash", input: "a/b"},
		{name: "backslash", input: `a\b`},
		{name: "colon", input: "c:x"},
		{name: "nul", input: "a\x00b"},
		{name: "too long", input: strings.Repeat("a", 256)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := ValidateFileName(tt.input); (err == nil) != tt.isValid {
				t.Errorf("ValidateFileName(%q) error = %v, want valid = %v", tt.input, err, tt.isValid)
			}
		})
	}
}

func TestTarCommand(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("uses Unix-style absolute tar paths")
	}

	path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: /mnt/backups

commands:
  - id: etc
    type: tar
    path: /etc/
    exclude: ["*.bak", " ", "ssl/private"]
  - id: www
    type: tar
    path: /srv/www
    compress: gzip

jobs:
  - name: host
    files: [{command: etc}, {command: www}, {command: www, name: www-copy.tgz.gpg}]
    targets: [{server: nas, bucket: host}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() error: %v", err)
	}

	sources := singleJob(t, rc).Sources
	if len(sources) != 3 {
		t.Fatalf("len(Sources) = %d, want 3", len(sources))
	}

	etc := sources[0]
	if etc.Kind != CommandKindTar || etc.Tar.Root != "/etc" || etc.Tar.Gzip ||
		!slices.Equal(etc.Tar.Exclude, []string{"*.bak", "ssl/private"}) || etc.Name != "etc.tar.gpg" {
		t.Errorf("Sources[0] = %+v", etc)
	}

	if www := sources[1]; www.Kind != CommandKindTar || !www.Tar.Gzip || www.Name != "www.tar.gz.gpg" {
		t.Errorf("Sources[1] = %+v", www)
	}

	if name := sources[2].Name; name != "www-copy.tgz.gpg" {
		t.Errorf("Sources[2].Name = %q, want the explicit name", name)
	}
}

func TestResolveCommandType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fc       FileCommand
		expected string // substring of the error; "" means valid
	}{
		{name: "unset type is shell", fc: FileCommand{ID: "c", Cmd: "true"}},
		{name: "explicit shell", fc: FileCommand{ID: "c", Type: "shell", Cmd: "true"}},
		{name: "unknown type", fc: FileCommand{ID: "c", Type: "zip", Cmd: "true"}, expected: `unknown type "zip"`},
		{name: "shell with path", fc: FileCommand{ID: "c", Cmd: "true", Path: "/etc"}, expected: "only apply to type: tar"},
		{name: "shell with compress", fc: FileCommand{ID: "c", Cmd: "true", Compress: "gzip"}, expected: "only apply to type: tar"},
		{name: "tar", fc: FileCommand{ID: "c", Type: "tar", Path: "/etc"}},
		{name: "tar missing path", fc: FileCommand{ID: "c", Type: "tar"}, expected: "path is required"},
		{name: "tar relative path", fc: FileCommand{ID: "c", Type: "tar", Path: "etc"}, expected: "must be absolute"},
		{name: "tar with cmd", fc: FileCommand{ID: "c", Type: "tar", Path: "/etc", Cmd: "true"}, expected: "don't apply to type: tar"},
		{name: "tar with container", fc: FileCommand{ID: "c", Type: "tar", Path: "/etc", Container: "db"}, expected: "don't apply to type: tar"},
		{name: "tar bad compress", fc: FileCommand{ID: "c", Type: "tar", Path: "/etc", Compress: "zstd"}, expected: `unknown compress "zstd"`},
		{name: "tar bad exclude", fc: FileCommand{ID: "c", Type: "tar", Path: "/etc", Exclude: []string{"[a-"}}, expected: "exclude pattern"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.fc.Type == "tar" && runtime.GOOS == "windows" {
				t.Skip("tar paths in this table are Unix-style absolute paths")
			}

			_, err := ResolveCommand(tt.fc)

			switch {
			case tt.expected == "" && err != nil:
				t.Errorf("ResolveCommand() error: %v", err)
			case tt.expected != "" && (err == nil || !strings.Contains(err.Error(), tt.expected)):
				t.Errorf("ResolveCommand() error = %v, want it to contain %q", err, tt.expected)
			}
		})
	}
}

func TestTarCommandRejectedAsHook(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("uses Unix-style absolute tar paths")
	}

	for _, hook := range []string{"on-error: {command: etc, after: 1}", "on-recover: {command: etc}"} {
		path := writeConfigFile(t, `
servers:
  - name: nas
    type: local
    path: /mnt/backups

commands:
  - id: dump
    cmd: "echo hi"
  - id: etc
    type: tar
    path: /etc

jobs:
  - name: host
    files: [{command: dump}]
    targets:
      - server: nas
        bucket: host
        `+hook+`
    recipients: [me@example.com]
`)

		_, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "can't be run as a hook") {
			t.Errorf("%s: ParseFlags() error = %v, want a hook rejection", hook, err)
		}
	}
}
