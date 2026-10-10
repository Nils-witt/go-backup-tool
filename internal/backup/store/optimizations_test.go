package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestOpenEnablesWAL(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)

	var mode string
	if err := db.db.Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil {
		t.Fatalf("PRAGMA journal_mode error: %v", err)
	}

	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestExpiredObjectPathsPerRowRetention(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
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

	db := openTestStore(t)
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
