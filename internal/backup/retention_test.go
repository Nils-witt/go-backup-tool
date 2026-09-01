package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// openTestRetentionDB opens a fresh shared state db for a retention test, in
// its own temp directory rather than sharing openTestStateDB's, since some
// assertions below want to point it at a distinct temp file.
func openTestRetentionDB(t *testing.T) *store.Store {
	t.Helper()

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open() unexpected error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// hasRetentionRow reports whether db still has a tracked row for path on
// server, regardless of whether it's currently within retention: it checks
// by asking for every path expired as of a point far enough in the future
// that any row on server — no matter its own retention_seconds — is bound
// to show up, which is as close to "SELECT COUNT(*)" as this test can get
// through store's own exported API.
func hasRetentionRow(t *testing.T, db *store.Store, server, path string) bool {
	t.Helper()

	farFuture := time.Now().Add(100 * 365 * 24 * time.Hour)

	paths, err := db.ExpiredObjectPaths(t.Context(), server, farFuture, 24*time.Hour)
	if err != nil {
		t.Fatalf("ExpiredObjectPaths(): %v", err)
	}

	return slices.Contains(paths, path)
}

func TestRecordLocalWriteNoRetentionIsNoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	cfg := &config.Config{Key: "backup.gpg", StateDB: db}
	tgt := &config.Target{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir}

	if err := RecordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	if hasRetentionRow(t, db, "nas", LocalObjectPath(cfg, tgt)) {
		t.Error("retention row exists, want none when retention is unset")
	}
}

func TestRecordLocalWriteNilStateDBIsNoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config.Config{Key: "backup.gpg"} // stateDB left nil
	tgt := &config.Target{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

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
	cfg := &config.Config{Key: "backup.gpg", StateDB: db}
	tgt := &config.Target{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

	if err := WriteLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	if err := RecordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	path := LocalObjectPath(cfg, tgt)
	if !hasRetentionRow(t, db, "nas", path) {
		t.Errorf("no retention row for %q, want one", path)
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
	tgt := &config.Target{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

	oldCfg := &config.Config{Key: "old.gpg", StateDB: db}
	if err := WriteLocalObject(oldCfg, tgt, strings.NewReader("stale")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	oldPath := LocalObjectPath(oldCfg, tgt)

	// Record the write backdated by two hours (past the target's one-hour
	// retention) directly through the store, rather than via
	// RecordLocalWrite (which always stamps "now"), so the sweep below has
	// something to actually catch.
	if err := db.SaveObjectWrite(t.Context(), tgt.ServerName, tgt.Bucket, oldPath, time.Now().Add(-2*time.Hour), int64(tgt.Retention/time.Second)); err != nil {
		t.Fatalf("SaveObjectWrite() unexpected error: %v", err)
	}

	// A fresh write triggers RecordLocalWrite's sweep, which should now
	// catch the backdated object above.
	newCfg := &config.Config{Key: "new.gpg", StateDB: db}
	if err := WriteLocalObject(newCfg, tgt, strings.NewReader("fresh")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	if err := RecordLocalWrite(t.Context(), newCfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expired object still present after sweep: err = %v", err)
	}

	if hasRetentionRow(t, db, "nas", oldPath) {
		t.Errorf("retention row for expired %q still present, want none", oldPath)
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
	cfg := &config.Config{Key: "gone.gpg", StateDB: db}
	tgt := &config.Target{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

	if err := WriteLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	path := LocalObjectPath(cfg, tgt)

	if err := db.SaveObjectWrite(t.Context(), tgt.ServerName, tgt.Bucket, path, time.Now().Add(-2*time.Hour), int64(tgt.Retention/time.Second)); err != nil {
		t.Fatalf("SaveObjectWrite() unexpected error: %v", err)
	}

	// The file is gone from disk (e.g. removed by hand) but its db record
	// isn't; the sweep should still clean up the record without erroring.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing test file: %v", err)
	}

	if err := SweepRetentionForTarget(t.Context(), db, tgt, discardLogger); err != nil {
		t.Fatalf("SweepRetentionForTarget() unexpected error: %v", err)
	}

	if hasRetentionRow(t, db, "nas", path) {
		t.Errorf("retention row for %q still present after sweep, want none", path)
	}
}

func TestSweepRetentionForTargetNilDBIsNoop(t *testing.T) {
	t.Parallel()

	tgt := &config.Target{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: t.TempDir(), Retention: time.Hour}

	if err := SweepRetentionForTarget(t.Context(), nil, tgt, discardLogger); err != nil {
		t.Fatalf("SweepRetentionForTarget() unexpected error: %v", err)
	}
}

func TestRemoveRetentionRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestRetentionDB(t)
	cfg := &config.Config{Key: "backup.gpg", StateDB: db}
	tgt := &config.Target{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir, Retention: time.Hour}

	if err := WriteLocalObject(cfg, tgt, strings.NewReader("data")); err != nil {
		t.Fatalf("WriteLocalObject() unexpected error: %v", err)
	}

	if err := RecordLocalWrite(t.Context(), cfg, tgt, discardLogger); err != nil {
		t.Fatalf("RecordLocalWrite() unexpected error: %v", err)
	}

	path := LocalObjectPath(cfg, tgt)
	if !hasRetentionRow(t, db, "nas", path) {
		t.Fatalf("no retention row for %q before removal, want one", path)
	}

	if err := RemoveRetentionRecord(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("RemoveRetentionRecord() unexpected error: %v", err)
	}

	if hasRetentionRow(t, db, "nas", path) {
		t.Errorf("retention row for %q still present after removal, want none", path)
	}
}
