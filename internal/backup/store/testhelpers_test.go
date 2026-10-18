package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// postgresDSNEnv names the environment variable that, when set to a
// PostgreSQL connection URL (postgres://...), makes openTestStore run against PostgreSQL
// instead of SQLite (see openPostgresTestStore).
const postgresDSNEnv = "TEST_POSTGRES_DSN"

// openTestStore opens a fresh state db, closed automatically when the test
// ends: a PostgreSQL schema of its own when TEST_POSTGRES_DSN is set,
// otherwise a SQLite file under t.TempDir().
func openTestStore(t *testing.T) *gormStore {
	t.Helper()

	if dsn := os.Getenv(postgresDSNEnv); dsn != "" {
		return openPostgresTestStore(t, dsn)
	}

	return openSQLiteTestStore(t)
}

// openSQLiteTestStore opens a fresh SQLite state db under t.TempDir(),
// closed automatically when the test ends, for tests of SQLite-only
// behavior (pragmas, query plans) and for openTestStore's default.
func openSQLiteTestStore(t *testing.T) *gormStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.db")

	db, err := openSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("openSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// openPostgresTestStore opens a fresh state db in a new, uniquely named
// schema of the PostgreSQL database dsn names, so parallel and shuffled
// tests never see each other's rows. The schema is dropped when the test
// ends.
func openPostgresTestStore(t *testing.T, dsn string) *gormStore {
	t.Helper()

	schema := createTestSchema(t, dsn)

	db, err := openPostgres(context.Background(), withSearchPath(t, dsn, schema))
	if err != nil {
		t.Fatalf("openPostgres() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// withSearchPath returns the postgres:// URL dsn with its search_path set to
// schema.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing %s: %v", postgresDSNEnv, err)
	}

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	return u.String()
}

// createTestSchema creates a schema with a random name in the database dsn
// names and registers its removal with t.Cleanup.
func createTestSchema(t *testing.T, dsn string) string {
	t.Helper()

	// Lowercase: PostgreSQL folds the unquoted search_path entry the DSN
	// sets to lowercase.
	schema := "test_" + strings.ToLower(rand.Text())

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("connecting to %s: %v", postgresDSNEnv, err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	// schema is generated above from lowercase base32 characters, never user input,
	// so quoting it as an identifier is safe.
	if _, err := admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("creating schema %s: %v", schema, err)
	}

	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema)); err != nil {
			t.Errorf("dropping schema %s: %v", schema, err)
		}
	})

	return schema
}
