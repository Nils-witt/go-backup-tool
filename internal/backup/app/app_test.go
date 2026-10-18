package app

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// TestNewRunLoggerLogViewerGate covers newRunLogger's *webui.LogRingBuffer
// decision: it must only wire one up (so /api/logs has anything to serve)
// when both the web UI is enabled (listen:) and the operator opted into the
// log viewer (enable-log-viewer:) — never from listen: alone, since unlike
// file downloads the dashboard has no login guarding the viewer.
func TestNewRunLoggerLogViewerGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		listen    string
		logViewer bool
		wantLogs  bool
	}{
		{name: "no web UI, viewer unset", listen: "", logViewer: false, wantLogs: false},
		{name: "no web UI, viewer enabled", listen: "", logViewer: true, wantLogs: false},
		{name: "web UI, viewer unset", listen: ":0", logViewer: false, wantLogs: false},
		{name: "web UI, viewer enabled", listen: ":0", logViewer: true, wantLogs: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rc := &config.RunConfig{Listen: tt.listen, LogViewer: tt.logViewer}

			_, logs := newRunLogger(io.Discard, rc)
			if got := logs != nil; got != tt.wantLogs {
				t.Errorf("newRunLogger() logs != nil = %v, want %v", got, tt.wantLogs)
			}
		})
	}
}

// TestOpenLogFileAppends covers openLogFile creating a missing parent
// directory and appending to (not truncating) an existing file.
func TestOpenLogFileAppends(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "app.log")

	for _, line := range []string{"first\n", "second\n"} {
		f, err := openLogFile(path)
		if err != nil {
			t.Fatalf("openLogFile() unexpected error: %v", err)
		}

		if _, err := f.WriteString(line); err != nil {
			t.Fatalf("WriteString() unexpected error: %v", err)
		}

		if err := f.Close(); err != nil {
			t.Fatalf("Close() unexpected error: %v", err)
		}
	}

	got, err := os.ReadFile(path) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile() unexpected error: %v", err)
	}

	if want := "first\nsecond\n"; string(got) != want {
		t.Errorf("log file contents = %q, want %q", got, want)
	}
}

func TestCheckPublicKeyReceivers(t *testing.T) {
	t.Parallel()

	if err := checkPublicKeyReceivers(t.Context(), nil); err != nil {
		t.Errorf("checkPublicKeyReceivers(nil db) = %v, want nil", err)
	}

	path := filepath.Join(t.TempDir(), "state.db")

	db, err := store.OpenSQLite(t.Context(), path)
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if err := checkPublicKeyReceivers(t.Context(), db); err != nil {
		t.Errorf("checkPublicKeyReceivers(empty db) = %v, want nil", err)
	}

	// A receiver as v7.x stored it: authorized only by a public key.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}

	defer func() { _ = raw.Close() }()

	now := time.Now().UTC()
	if _, err := raw.ExecContext(t.Context(),
		"INSERT INTO receivers (id, public_key, path, created_at, created_by, updated_at, updated_by) VALUES ('legacy', 'pem', '/srv/legacy', ?, '', ?, '')", now, now); err != nil {
		t.Fatalf("inserting a legacy receiver: %v", err)
	}

	err = checkPublicKeyReceivers(t.Context(), db)
	if err == nil || !strings.Contains(err.Error(), "receivers legacy still authenticate senders only by public key") || !strings.Contains(err.Error(), "-import-settings") {
		t.Fatalf("checkPublicKeyReceivers() = %v, want it to name receiver legacy and the fix", err)
	}
}
