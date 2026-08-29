package webui

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
)

// discardLogger is a *slog.Logger that writes nowhere, for tests that need
// to pass one but don't assert on its output.
var discardLogger = slog.New(slog.DiscardHandler)

// openTestStateDB opens a fresh state db under t.TempDir(), closed
// automatically when the test ends.
func openTestStateDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.db")

	db, err := backup.OpenScheduleStateDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// newTestStore builds a *backup.StatusStore with a single job, for tests
// that need one but don't care about its exact configuration.
func newTestStore() (*backup.StatusStore, *backup.Config) {
	cfg := &backup.Config{
		Name:     "test",
		Interval: time.Minute,
		Targets: []backup.Target{
			{ServerName: "primary", Bucket: "b1", Kind: backup.ServerKindS3},
			{ServerName: "nas", Bucket: "b2", Kind: backup.ServerKindLocal},
		},
	}

	return backup.NewStatusStore([]*backup.Config{cfg}), cfg
}
