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
    command: dump
    targets: [{server: nas, bucket: db}]
    recipients: [me@example.com]
`)

	rc, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseFlags() error: %v", err)
	}

	src := singleJob(t, rc).Source
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

	if s := rc.Jobs[0].Source; s.ID != "job-db-2" || s.Cmd != "pg_dump app" || s.TimeoutSet {
		t.Errorf("db Source = %+v, want generated command job-db-2 without a timeout", s)
	}

	if s := rc.Jobs[1].Source; s.ID != "job-files" || s.Cmd != "echo default" {
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
		if fj.Cmd != "" || fj.Command == "" {
			t.Errorf("FileJob %q = cmd %q, command %q; want only command", fj.Name, fj.Cmd, fj.Command)
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
