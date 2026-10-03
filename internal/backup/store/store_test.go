package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestStateDBPath(t *testing.T) {
	t.Parallel()

	got := StateDBPath("/etc/go-backup-tool/config.yaml")
	want := filepath.Join("/etc/go-backup-tool", stateDBName)

	if got != want {
		t.Errorf("StateDBPath() = %q, want %q", got, want)
	}
}

func TestOpenDropsObsoleteTables(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}

	for _, table := range obsoleteTables {
		if _, err := rawDB.ExecContext(ctx, `CREATE TABLE "`+table+`" (id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatalf("creating %s: %v", table, err)
		}
	}

	_ = rawDB.Close()

	// Twice: the second Open (nothing left to drop) must be a no-op.
	for range 2 {
		db, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open() error: %v", err)
		}

		for _, table := range obsoleteTables {
			if db.db.Migrator().HasTable(table) {
				t.Errorf("table %q still exists after Open", table)
			}
		}

		if err := db.Close(); err != nil {
			t.Fatalf("Close() error: %v", err)
		}
	}
}
