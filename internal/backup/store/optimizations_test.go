package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOpenEnablesWAL(t *testing.T) {
	t.Parallel()

	db := openSQLiteTestStore(t)

	var mode string
	if err := db.db.Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil {
		t.Fatalf("PRAGMA journal_mode error: %v", err)
	}

	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestReadPoolIsQueryOnly(t *testing.T) {
	t.Parallel()

	db := openSQLiteTestStore(t)

	err := db.read.Exec("INSERT INTO login_events (at, username, method, success, remote_addr) VALUES (?, 'u', 'oidc', 1, '')", time.Now()).Error
	if err == nil {
		t.Fatal("write through the read pool succeeded, want it rejected by query_only")
	}
}

func TestReadPoolSeesCommittedWrites(t *testing.T) {
	t.Parallel()

	db := openSQLiteTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

	// Warm a read connection first, so the check below can't pass just
	// because the read pool opened its first connection after the write.
	if _, _, err := db.GetLastRun(ctx, "job"); err != nil {
		t.Fatalf("GetLastRun() error: %v", err)
	}

	if err := db.SaveJobRun(ctx, "job", "ok", true, start, start.Add(time.Minute), 42, ""); err != nil {
		t.Fatalf("SaveJobRun() error: %v", err)
	}

	run, ok, err := db.GetLastRun(ctx, "job")
	if err != nil || !ok {
		t.Fatalf("GetLastRun() = %v, %v, %v; want the run just saved", run, ok, err)
	}

	if run.Size != 42 {
		t.Errorf("GetLastRun().Size = %d, want 42", run.Size)
	}
}

func TestOpenDropsObsoleteObjectsServerIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := openSQLite(ctx, path)
	if err != nil {
		t.Fatalf("openSQLite() error: %v", err)
	}

	// Recreate the index an older version declared on objects.server.
	if err := db.db.Exec("CREATE INDEX idx_objects_server ON objects(server)").Error; err != nil {
		t.Fatalf("creating legacy index: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	db, err = openSQLite(ctx, path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	m := db.db.Migrator()

	if m.HasIndex(&objectModel{}, "idx_objects_server") {
		t.Error("idx_objects_server still exists after reopening")
	}

	for _, idx := range []struct {
		model any
		name  string
	}{
		{&objectModel{}, "idx_objects_expiry"},
		{&outstandingTargetUploadModel{}, "idx_outstanding_target_uploads_run_at"},
		{&outstandingTargetUploadModel{}, "idx_outstanding_target_uploads_file"},
	} {
		if !m.HasIndex(idx.model, idx.name) {
			t.Errorf("index %s missing", idx.name)
		}
	}
}

func TestExpiredObjectPathsUsesExpiryIndex(t *testing.T) {
	t.Parallel()

	db := openSQLiteTestStore(t)

	var plan []struct {
		Detail string `gorm:"column:detail"`
	}

	err := db.read.Raw("EXPLAIN QUERY PLAN SELECT path FROM objects WHERE server = ? AND retention_seconds = ? AND written_at < ? ORDER BY written_at",
		"a", 0, time.Now()).Scan(&plan).Error
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN error: %v", err)
	}

	var details []string
	for _, row := range plan {
		details = append(details, row.Detail)
	}

	got := strings.Join(details, "; ")
	if !strings.Contains(got, "idx_objects_expiry") || strings.Contains(got, "TEMP B-TREE") {
		t.Errorf("query plan = %q, want an idx_objects_expiry search with no separate sort", got)
	}
}

func TestExpiredObjectPathsPerRowRetention(t *testing.T) {
	t.Parallel()

	db := openSQLiteTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	day := int64(24 * 60 * 60)

	writes := []struct {
		server, path string
		age          time.Duration
		retention    int64
	}{
		{"a", "/a/old-1d", 48 * time.Hour, day},     // expired under its own 1d
		{"a", "/a/new-1d", 12 * time.Hour, day},     // still within 1d
		{"a", "/a/old-7d", 48 * time.Hour, 7 * day}, // within its own 7d
		{"a", "/a/legacy-old", 72 * time.Hour, 0},   // falls back to 2d: expired
		{"a", "/a/legacy-new", 24 * time.Hour, 0},   // falls back to 2d: kept
		{"b", "/b/old-1d", 48 * time.Hour, day},     // other server
	}

	for _, w := range writes {
		if err := db.SaveObjectWrite(ctx, w.server, "bucket", w.path, now.Add(-w.age), w.retention); err != nil {
			t.Fatalf("SaveObjectWrite(%q) error: %v", w.path, err)
		}
	}

	got, err := db.ExpiredObjectPaths(ctx, "a", now, 48*time.Hour-time.Minute)
	if err != nil {
		t.Fatalf("ExpiredObjectPaths() error: %v", err)
	}

	slices.Sort(got)

	want := []string{"/a/legacy-old", "/a/old-1d"}
	if !slices.Equal(got, want) {
		t.Errorf("ExpiredObjectPaths() = %v, want %v", got, want)
	}
}

func TestPruneEventsRemovesOnlyOlderRows(t *testing.T) {
	t.Parallel()

	db := openSQLiteTestStore(t)
	ctx := context.Background()
	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	before, after := cutoff.Add(-time.Hour), cutoff.Add(time.Hour)

	for _, at := range []time.Time{before, after} {
		if err := db.SaveLoginEvent(ctx, LoginEvent{At: at, Username: "u", Method: "oidc"}); err != nil {
			t.Fatal(err)
		}

		if err := db.SaveDownloadEvent(ctx, DownloadEvent{At: at, ReceiverID: "r", Key: "k"}); err != nil {
			t.Fatal(err)
		}

		if err := db.SaveReceiverEvent(ctx, ReceiverEvent{At: at, ReceiverID: "r", Kind: ReceiverEventReceive, Key: "k"}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.PruneEvents(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneEvents() error: %v", err)
	}

	if n != 3 {
		t.Errorf("PruneEvents() removed %d rows, want 3", n)
	}

	logins, _ := db.ListLoginEvents(ctx, 10)
	downloads, _ := db.ListDownloadEvents(ctx, 10)
	receives, _ := db.ListReceiverEvents(ctx, 10)

	if len(logins) != 1 || len(downloads) != 1 || len(receives) != 1 {
		t.Fatalf("after prune: %d logins, %d downloads, %d receiver events, want 1 each", len(logins), len(downloads), len(receives))
	}

	if !logins[0].At.Equal(after) {
		t.Errorf("kept login event at %v, want %v", logins[0].At, after)
	}
}
