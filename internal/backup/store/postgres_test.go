package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// requirePostgres skips t unless TEST_POSTGRES_DSN is set, returning it.
func requirePostgres(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(postgresDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set", postgresDSNEnv)
	}

	return dsn
}

func TestOpenPostgresCreatesSchema(t *testing.T) {
	t.Parallel()

	requirePostgres(t)

	db := openTestStore(t)

	m := db.db.Migrator()
	for _, table := range []any{&jobRunModel{}, &objectModel{}, &jobModel{}, &notificationModel{}} {
		if !m.HasTable(table) {
			t.Errorf("table for %T missing", table)
		}
	}

	if !m.HasIndex(&objectModel{}, "idx_objects_expiry") {
		t.Error("index idx_objects_expiry missing")
	}
}

func TestOpenPostgresReopenKeepsData(t *testing.T) {
	t.Parallel()

	requirePostgres(t)

	ctx := t.Context()
	db := openTestStore(t)

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := db.SaveJobRun(ctx, "nightly", "success", true, at.Add(-time.Minute), at, 7, ""); err != nil {
		t.Fatalf("SaveJobRun() error: %v", err)
	}

	// Opening the same schema again must migrate cleanly over the existing
	// tables and see the row written above.
	again, err := openPostgres(ctx, searchPathDSN(t, db))
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}

	t.Cleanup(func() { _ = again.Close() })

	got, ok, err := again.GetLastJobSuccess(ctx, "nightly")
	if err != nil || !ok || !got.Equal(at) {
		t.Errorf("GetLastJobSuccess() = %v, %v, %v, want %v, true, nil", got, ok, err, at)
	}
}

// searchPathDSN returns a DSN for the schema db was opened in, so a test
// can open the same schema a second time.
func searchPathDSN(t *testing.T, db *gormStore) string {
	t.Helper()

	var schema string
	if err := db.read.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatalf("reading current schema: %v", err)
	}

	return withSearchPath(t, os.Getenv(postgresDSNEnv), schema)
}

func TestOpenPostgresUnreachableFails(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Port 1 on loopback refuses connections, so this needs no server.
	s, err := OpenPostgres(ctx, "postgres://u:hunter2@127.0.0.1:1/x?sslmode=disable&connect_timeout=2")
	if err == nil {
		_ = s.Close()

		t.Fatal("OpenPostgres() error = nil, want a connection error")
	}

	if s != nil {
		t.Errorf("OpenPostgres() store = %v, want a nil interface", s)
	}

	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("OpenPostgres() error = %q, which reveals the password", err)
	}
}
