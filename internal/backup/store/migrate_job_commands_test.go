package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenMigratesInlineJobCommands(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	// A command already holding db's generated id forces a suffix.
	if err := db.CreateCommandConfig(ctx, CommandConfig{ID: "job-db", Cmd: "taken", CreatedAt: now, CreatedBy: "erin", UpdatedAt: now, UpdatedBy: "erin"}); err != nil {
		t.Fatal(err)
	}

	for _, j := range []JobConfig{
		{Name: "db", LegacyCmd: "pg_dump app", CreatedAt: now, CreatedBy: "erin", UpdatedAt: now, UpdatedBy: "erin"},
		{Name: "files", Command: "job-db", CreatedAt: now, CreatedBy: "erin", UpdatedAt: now, UpdatedBy: "erin"},
	} {
		if err := db.CreateJobConfig(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	_ = db.Close()

	// Opening twice must migrate once.
	for range 2 {
		db, err = Open(ctx, path)
		if err != nil {
			t.Fatalf("Open() error: %v", err)
		}

		checkMigratedJobCommands(t, db)

		_ = db.Close()
	}
}

// checkMigratedJobCommands checks TestOpenMigratesInlineJobCommands' db
// after Open: db's inline cmd moved into command job-db-2, files untouched.
func checkMigratedJobCommands(t *testing.T, db *Store) {
	t.Helper()

	ctx := context.Background()

	jobs, err := db.ListJobConfigs(ctx)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("ListJobConfigs() = %+v, %v", jobs, err)
	}

	if j := jobs[0]; j.Command != "job-db-2" || j.LegacyCmd != "" {
		t.Errorf("db = command %q, cmd %q; want job-db-2 and no inline cmd", j.Command, j.LegacyCmd)
	}

	if j := jobs[1]; j.Command != "job-db" {
		t.Errorf("files command = %q, want it untouched", j.Command)
	}

	commands, err := db.ListCommandConfigs(ctx)
	if err != nil || len(commands) != 2 || commands[1].ID != "job-db-2" || commands[1].Cmd != "pg_dump app" ||
		commands[1].CreatedBy != "erin" || commands[1].UpdatedBy != migrationUser {
		t.Errorf("ListCommandConfigs() = %+v, %v", commands, err)
	}
}
