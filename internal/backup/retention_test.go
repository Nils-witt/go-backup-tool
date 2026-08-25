package backup

import (
	"os"
	"strings"
	"testing"
	"time"
)

// countRetentionRows returns how many rows the retention db at
// retentionDBPath(localRoot) has for path, for assertions below.
func countRetentionRows(t *testing.T, localRoot, path string) int {
	t.Helper()

	db, err := openRetentionDB(t.Context(), retentionDBPath(localRoot))
	if err != nil {
		t.Fatalf("openRetentionDB() unexpected error: %v", err)
	}
	defer func() { _ = db.Close() }()

	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM objects WHERE path = ?`, path).Scan(&n); err != nil {
		t.Fatalf("counting retention rows: %v", err)
	}

	return n
}

// setRetentionWrittenAt backdates path's written_at in localRoot's retention
// db, so a sweep can be tested without waiting out a real retention window.
func setRetentionWrittenAt(t *testing.T, localRoot, path string, when time.Time) {
	t.Helper()

	db, err := openRetentionDB(t.Context(), retentionDBPath(localRoot))
	if err != nil {
		t.Fatalf("openRetentionDB() unexpected error: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(t.Context(), `UPDATE objects SET written_at = ? WHERE path = ?`, when.UTC(), path); err != nil {
		t.Fatalf("backdating written_at: %v", err)
	}
}

func TestRecordLocalWriteNoRetentionIsNoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config{key: "backup.gpg"}
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir}

	if err := recordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	if _, err := os.Stat(retentionDBPath(dir)); !os.IsNotExist(err) {
		t.Errorf("retention db exists = %v, want no db file when retention is unset", err)
	}
}

func TestRecordLocalWriteTracksObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config{key: "backup.gpg"}
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir, retention: time.Hour}

	if err := writeLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	path := localObjectPath(cfg, tgt)
	if got := countRetentionRows(t, dir, path); got != 1 {
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
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir, retention: time.Hour}

	oldCfg := &config{key: "old.gpg"}
	if err := writeLocalObject(oldCfg, tgt, strings.NewReader("stale")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), oldCfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	oldPath := localObjectPath(oldCfg, tgt)
	setRetentionWrittenAt(t, dir, oldPath, time.Now().Add(-2*time.Hour))

	// A fresh write triggers recordLocalWrite's sweep, which should now
	// catch the backdated object above.
	newCfg := &config{key: "new.gpg"}
	if err := writeLocalObject(newCfg, tgt, strings.NewReader("fresh")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), newCfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expired object still present after sweep: err = %v", err)
	}

	if got := countRetentionRows(t, dir, oldPath); got != 0 {
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
	cfg := &config{key: "gone.gpg"}
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir, retention: time.Hour}

	if err := writeLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	path := localObjectPath(cfg, tgt)
	setRetentionWrittenAt(t, dir, path, time.Now().Add(-2*time.Hour))

	// The file is gone from disk (e.g. removed by hand) but its db record
	// isn't; the sweep should still clean up the record without erroring.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing test file: %v", err)
	}

	if err := sweepRetentionForTarget(t.Context(), tgt, discardLogger); err != nil {
		t.Fatalf("sweepRetentionForTarget() unexpected error: %v", err)
	}

	if got := countRetentionRows(t, dir, path); got != 0 {
		t.Errorf("retention rows for %q = %d, want 0", path, got)
	}
}

func TestRemoveRetentionRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config{key: "backup.gpg"}
	tgt := &target{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir, retention: time.Hour}

	if err := writeLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := recordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("recordLocalWrite() unexpected error: %v", err)
	}

	path := localObjectPath(cfg, tgt)
	if got := countRetentionRows(t, dir, path); got != 1 {
		t.Fatalf("retention rows for %q = %d, want 1 before removal", path, got)
	}

	if err := removeRetentionRecord(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("removeRetentionRecord() unexpected error: %v", err)
	}

	if got := countRetentionRows(t, dir, path); got != 0 {
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
