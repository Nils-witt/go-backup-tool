package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openTestRetentionDB opens a fresh shared state db (see schedule_state.go)
// for a retention test, in its own temp directory rather than sharing
// openTestStateDB's, since some assertions below want to point it at a
// distinct temp file.
func openTestRetentionDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := openScheduleStateDB(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("openScheduleStateDB() unexpected error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// countRetentionRows returns how many rows db has for path, for assertions
// below.
func countRetentionRows(t *testing.T, db *sql.DB, path string) int {
	t.Helper()

	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM objects WHERE path = ?`, path).Scan(&n); err != nil {
		t.Fatalf("counting retention rows: %v", err)
	}

	return n
}

// setRetentionWrittenAt backdates path's written_at in db, so a sweep can be
// tested without waiting out a real retention window.
func setRetentionWrittenAt(t *testing.T, db *sql.DB, path string, when time.Time) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), `UPDATE objects SET written_at = ? WHERE path = ?`, when.UTC(), path); err != nil {
		t.Fatalf("backdating written_at: %v", err)
	}
}

func TestRecordLocalWriteNoRetentionIsNoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	cfg := &config{key: "backup.gpg", stateDB: db}
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir}

	if err := recordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	if got := countRetentionRows(t, db, localObjectPath(cfg, tgt)); got != 0 {
		t.Errorf("retention rows = %d, want 0 when retention is unset", got)
	}
}

func TestRecordLocalWriteNilStateDBIsNoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config{key: "backup.gpg"} // stateDB left nil
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir, retention: time.Hour}

	if err := writeLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}
}

func TestRecordLocalWriteTracksObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	cfg := &config{key: "backup.gpg", stateDB: db}
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir, retention: time.Hour}

	if err := writeLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	path := localObjectPath(cfg, tgt)
	if got := countRetentionRows(t, db, path); got != 1 {
		t.Errorf("retention rows for %q = %d, want 1", path, got)
	}

	// A well within-retention object survives the sweep recordLocalWrite
	// triggers.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("object removed by sweep despite being within retention: %v", err)
	}
}

func TestRecordLocalWriteSweepsExpiredObjects(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir, retention: time.Hour}

	oldCfg := &config{key: "old.gpg", stateDB: db}
	if err := writeLocalObject(oldCfg, tgt, strings.NewReader("stale")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), oldCfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	oldPath := localObjectPath(oldCfg, tgt)
	setRetentionWrittenAt(t, db, oldPath, time.Now().Add(-2*time.Hour))

	// A fresh write triggers recordLocalWrite's sweep, which should now
	// catch the backdated object above.
	newCfg := &config{key: "new.gpg", stateDB: db}
	if err := writeLocalObject(newCfg, tgt, strings.NewReader("fresh")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), newCfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expired object still present after sweep: err = %v", err)
	}

	if got := countRetentionRows(t, db, oldPath); got != 0 {
		t.Errorf("retention rows for expired %q = %d, want 0", oldPath, got)
	}

	newPath := localObjectPath(newCfg, tgt)
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("fresh object removed by sweep: %v", err)
	}
}

func TestSweepRetentionForTargetIgnoresMissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	cfg := &config{key: "gone.gpg", stateDB: db}
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir, retention: time.Hour}

	if err := writeLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	path := localObjectPath(cfg, tgt)
	setRetentionWrittenAt(t, db, path, time.Now().Add(-2*time.Hour))

	// The file is gone from disk (e.g. removed by hand) but its db record
	// isn't; the sweep should still clean up the record without erroring.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing test file: %v", err)
	}

	if err := sweepRetentionForTarget(t.Context(), db, tgt, discardLogger); err != nil {
		t.Fatalf("sweepRetentionForTarget() unexpected error: %v", err)
	}

	if got := countRetentionRows(t, db, path); got != 0 {
		t.Errorf("retention rows for %q = %d, want 0", path, got)
	}
}

func TestSweepRetentionForTargetNilDBIsNoop(t *testing.T) {
	t.Parallel()

	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: t.TempDir(), retention: time.Hour}

	if err := sweepRetentionForTarget(t.Context(), nil, tgt, discardLogger); err != nil {
		t.Fatalf("sweepRetentionForTarget() unexpected error: %v", err)
	}
}

func TestRemoveRetentionRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	cfg := &config{key: "backup.gpg", stateDB: db}
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir, retention: time.Hour}

	if err := writeLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	path := localObjectPath(cfg, tgt)
	if got := countRetentionRows(t, db, path); got != 1 {
		t.Fatalf("retention rows for %q = %d, want 1 before removal", path, got)
	}

	if err := removeRetentionRecord(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("removeRetentionRecord() unexpected error: %v", err)
	}

	if got := countRetentionRows(t, db, path); got != 0 {
		t.Errorf("retention rows for %q = %d, want 0 after removal", path, got)
	}
}

func TestParseRetention(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty means no expiry", in: "", want: 0},
		{name: "hours", in: "168h", want: 168 * time.Hour},
		{name: "days", in: "7d", want: 7 * 24 * time.Hour},
		{name: "days plus hours", in: "1d12h", want: 36 * time.Hour},
		{name: "fractional days", in: "1.5d", want: 36 * time.Hour},
		{name: "minutes", in: "90m", want: 90 * time.Minute},
		{name: "negative hours rejected", in: "-1h", wantErr: true},
		{name: "negative days rejected", in: "-1d", wantErr: true},
		{name: "bare sign rejected", in: "-", wantErr: true},
		{name: "unit with no number rejected", in: "d", wantErr: true},
		{name: "garbage rejected", in: "not-a-duration", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseRetention(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseRetention(%q) error = nil, want error", tt.in)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Errorf("parseRetention(%q) = (%v, %v), want (%v, nil)", tt.in, got, err, tt.want)
			}
		})
	}
}
