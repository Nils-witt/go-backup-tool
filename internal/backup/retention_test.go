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

	db, err := OpenScheduleStateDB(t.Context(), filepath.Join(t.TempDir(), "state.db"))
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
	cfg := &Config{Key: "backup.gpg", StateDB: db}
	tgt := &Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "sub", LocalPath: dir}

	if err := RecordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	if got := countRetentionRows(t, db, LocalObjectPath(cfg, tgt)); got != 0 {
		t.Errorf("retention rows = %d, want 0 when retention is unset", got)
	}
}

func TestRecordLocalWriteNilStateDBIsNoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{Key: "backup.gpg"} // stateDB left nil
	tgt := &Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

	if err := WriteLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	if err := RecordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}
}

func TestRecordLocalWriteTracksObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	cfg := &Config{Key: "backup.gpg", StateDB: db}
	tgt := &Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

	if err := WriteLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	if err := RecordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	path := LocalObjectPath(cfg, tgt)
	if got := countRetentionRows(t, db, path); got != 1 {
		t.Errorf("retention rows for %q = %d, want 1", path, got)
	}

	// A well within-retention object survives the sweep RecordLocalWrite
	// triggers.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("object removed by sweep despite being within retention: %v", err)
	}
}

func TestRecordLocalWriteSweepsExpiredObjects(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	tgt := &Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

	oldCfg := &Config{Key: "old.gpg", StateDB: db}
	if err := WriteLocalObject(oldCfg, tgt, strings.NewReader("stale")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	if err := RecordLocalWrite(t.Context(), oldCfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	oldPath := LocalObjectPath(oldCfg, tgt)
	setRetentionWrittenAt(t, db, oldPath, time.Now().Add(-2*time.Hour))

	// A fresh write triggers RecordLocalWrite's sweep, which should now
	// catch the backdated object above.
	newCfg := &Config{Key: "new.gpg", StateDB: db}
	if err := WriteLocalObject(newCfg, tgt, strings.NewReader("fresh")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	if err := RecordLocalWrite(t.Context(), newCfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expired object still present after sweep: err = %v", err)
	}

	if got := countRetentionRows(t, db, oldPath); got != 0 {
		t.Errorf("retention rows for expired %q = %d, want 0", oldPath, got)
	}

	newPath := LocalObjectPath(newCfg, tgt)
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("fresh object removed by sweep: %v", err)
	}
}

func TestSweepRetentionForTargetIgnoresMissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	cfg := &Config{Key: "gone.gpg", StateDB: db}
	tgt := &Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

	if err := WriteLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	if err := RecordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	path := LocalObjectPath(cfg, tgt)
	setRetentionWrittenAt(t, db, path, time.Now().Add(-2*time.Hour))

	// The file is gone from disk (e.g. removed by hand) but its db record
	// isn't; the sweep should still clean up the record without erroring.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing test file: %v", err)
	}

	if err := SweepRetentionForTarget(t.Context(), db, tgt, discardLogger); err != nil {
		t.Fatalf("SweepRetentionForTarget() unexpected error: %v", err)
	}

	if got := countRetentionRows(t, db, path); got != 0 {
		t.Errorf("retention rows for %q = %d, want 0", path, got)
	}
}

func TestSweepRetentionForTargetNilDBIsNoop(t *testing.T) {
	t.Parallel()

	tgt := &Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "sub", LocalPath: t.TempDir(), Retention: time.Hour}

	if err := SweepRetentionForTarget(t.Context(), nil, tgt, discardLogger); err != nil {
		t.Fatalf("SweepRetentionForTarget() unexpected error: %v", err)
	}
}

func TestRemoveRetentionRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	cfg := &Config{Key: "backup.gpg", StateDB: db}
	tgt := &Target{ServerName: "nas", Kind: ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

	if err := WriteLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	if err := RecordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	path := LocalObjectPath(cfg, tgt)
	if got := countRetentionRows(t, db, path); got != 1 {
		t.Fatalf("retention rows for %q = %d, want 1 before removal", path, got)
	}

	if err := RemoveRetentionRecord(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("RemoveRetentionRecord() unexpected error: %v", err)
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
