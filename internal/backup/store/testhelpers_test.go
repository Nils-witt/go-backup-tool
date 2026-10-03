package store

import (
	"context"
	"path/filepath"
	"testing"
)

// openTestStore opens a fresh state db under t.TempDir(), closed
// automatically when the test ends.
func openTestStore(t *testing.T) *Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.db")

	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}
