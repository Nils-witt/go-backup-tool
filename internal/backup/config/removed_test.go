package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestRemovedJobKeysAreRejected(t *testing.T) {
	t.Parallel()

	const servers = `
servers:
  - name: nas
    type: local
    path: /mnt/backups
`

	tests := []struct {
		name     string
		yaml     string
		expected string
	}{
		{
			name:     "inline cmd in a job",
			yaml:     "jobs:\n  - name: db\n    cmd: pg_dump app\n",
			expected: `job "db": cmd: was removed in v8.0.0: add the command to commands:`,
		},
		{
			name:     "single command in a job",
			yaml:     "jobs:\n  - name: db\n    command: dump\n",
			expected: `job "db": command: was removed in v8.0.0: list the command under the job's files:`,
		},
		{
			name:     "job without a name is named by index",
			yaml:     "jobs:\n  - name: ok\n    files: [{command: x}]\n  - cmd: echo hi\n",
			expected: "jobs[1]: cmd: was removed in v8.0.0",
		},
		{
			name:     "top-level default cmd",
			yaml:     "cmd: echo default\n",
			expected: "top level: cmd: was removed in v8.0.0",
		},
		{
			name:     "top-level default command",
			yaml:     "command: dump\n",
			expected: "top level: command: was removed in v8.0.0",
		},
		{
			name:     "receivers section",
			yaml:     "receivers:\n  - id: r\n    path: /mnt/r\n",
			expected: "top level: receivers: was removed in v8.0.0: receivers are managed in the web UI; to keep these, move the section into a settings file and run go-backup-tool -import-settings <file>",
		},
		{
			name:     "notifications section",
			yaml:     "notifications:\n  - id: ops\n    webhook: {url: https://x}\n",
			expected: "top level: notifications: was removed in v8.0.0",
		},
		{
			name:     "report section",
			yaml:     "report:\n  enabled: false\n",
			expected: "top level: report: was removed in v8.0.0",
		},
		{
			name:     "jobs under job-editing",
			yaml:     "webui:\n  enabled: true\n  listen: \":0\"\n  job-editing: true\ncommands:\n  - id: c\n    cmd: echo hi\n",
			expected: "jobs:, servers: and commands: are managed in the web UI while webui.job-editing is on",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseFlags([]string{"-config", writeConfigFile(t, servers+tt.yaml)}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.expected) {
				t.Fatalf("ParseFlags() error = %v, want it to contain %q", err, tt.expected)
			}
		})
	}
}

// TestRemovedKeyCheckIgnoresValidUses makes sure the removed-key check only
// looks at job level: commands: entries have a cmd:, and files:/on-error:
// entries a command:, all of which stay valid.
func TestRemovedKeyCheckIgnoresValidUses(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
commands:
  - id: dump
    cmd: "pg_dump app"
  - id: page
    cmd: "echo page"

servers:
  - name: nas
    type: local
    path: /mnt/backups

jobs:
  - name: db
    files: [{command: dump}]
    targets: [{server: nas, bucket: db, on-error: {command: page, after: 1}}]
    recipients: [me@example.com]
`)

	if _, err := ParseFlags([]string{"-config", path}, &bytes.Buffer{}); err != nil {
		t.Fatalf("ParseFlags() error: %v", err)
	}
}
