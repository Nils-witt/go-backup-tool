package pipeline

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
)

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

// discardLogger is a *slog.Logger that writes nowhere, for tests that need
// to pass one but don't assert on its output.
var discardLogger = slog.New(slog.DiscardHandler)

// testServerIdentity builds a *backup.ServerIdentity backed by a freshly
// generated RSA key, for tests that need one to exercise signing without
// touching disk.
func testServerIdentity(t *testing.T) *identity.ServerIdentity {
	t.Helper()

	id, _ := testServerIdentityAndKey(t)

	return id
}

// testServerIdentityAndKey is testServerIdentity, additionally returning the
// generated private key directly, for tests that need to verify a signature
// against its public half (ServerIdentity itself keeps the key private).
func testServerIdentityAndKey(t *testing.T) (*identity.ServerIdentity, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	return identity.NewTestServerIdentity("test-sender-uuid", key), key
}
