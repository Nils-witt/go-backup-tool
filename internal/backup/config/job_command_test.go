package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
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

	if len(rc.Warnings) != 1 || !strings.Contains(rc.Warnings[0], "command: is deprecated") {
		t.Errorf("Warnings = %v, want one deprecation warning", rc.Warnings)
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
