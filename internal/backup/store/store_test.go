package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
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
		db, err := openSQLite(ctx, path)
		if err != nil {
			t.Fatalf("openSQLite() error: %v", err)
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

func TestOpenMigratesCommandsWithoutType(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.db")

	createLegacyCommandsDB(t, path)

	db, err := openSQLite(ctx, path)
	if err != nil {
		t.Fatalf("openSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	commands, err := db.ListCommandConfigs(ctx)
	if err != nil || len(commands) != 1 {
		t.Fatalf("ListCommandConfigs() = %+v, %v", commands, err)
	}

	got := commands[0]
	got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}

	want := CommandConfig{ID: "dump", Cmd: "pg_dump app", CreatedBy: "erin", UpdatedBy: "erin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("legacy command = %+v, want an unchanged shell command %+v", got, want)
	}
}

// createLegacyCommandsDB creates a state db at path with the schema from
// before commands had a type: today's, minus the columns that came with
// it, holding one shell command.
func createLegacyCommandsDB(t *testing.T, path string) {
	t.Helper()

	db, err := openSQLite(t.Context(), path)
	if err != nil {
		t.Fatalf("openSQLite() error: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}

	defer func() { _ = rawDB.Close() }()

	for _, stmt := range []string{
		`ALTER TABLE commands DROP COLUMN type`,
		`ALTER TABLE commands DROP COLUMN path`,
		`ALTER TABLE commands DROP COLUMN exclude`,
		`ALTER TABLE commands DROP COLUMN compress`,
		`INSERT INTO commands (id, cmd, created_at, created_by, updated_at, updated_by)
			VALUES ('dump', 'pg_dump app', '2026-01-01 00:00:00', 'erin', '2026-01-01 00:00:00', 'erin')`,
	} {
		if _, err := rawDB.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("preparing legacy db: %v", err)
		}
	}
}
