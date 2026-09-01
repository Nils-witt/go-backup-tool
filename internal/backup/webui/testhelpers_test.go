package webui

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// discardLogger is a *slog.Logger that writes nowhere, for tests that need
// to pass one but don't assert on its output.
var discardLogger = slog.New(slog.DiscardHandler)

// openTestStateDB opens a fresh state db under t.TempDir(), closed
// automatically when the test ends.
func openTestStateDB(t *testing.T) *store.Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// newTestStore builds a *backup.StatusStore with a single job, for tests
// that need one but don't care about its exact configuration.
func newTestStore() (*backup.StatusStore, *config.Config) {
	cfg := &config.Config{
		Name:     "test",
		Interval: time.Minute,
		Targets: []config.Target{
			{ServerName: "sibling", Bucket: "b1", Kind: config.ServerKindRemote},
			{ServerName: "nas", Bucket: "b2", Kind: config.ServerKindLocal},
		},
	}

	return backup.NewStatusStore([]*config.Config{cfg}), cfg
}
