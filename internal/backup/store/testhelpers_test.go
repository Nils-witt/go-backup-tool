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

// mustGetUser fetches username via db.GetUser, failing the test if it
// errors or doesn't exist — the repeated "GetUser, then assert ok" shape
// several group/membership tests need after every mutation.
func mustGetUser(ctx context.Context, t *testing.T, db *Store, username string) User {
	t.Helper()

	user, ok, err := db.GetUser(ctx, username)
	if err != nil || !ok {
		t.Fatalf("GetUser(%q) = (ok=%v, err=%v), want (true, nil)", username, ok, err)
	}

	return user
}
