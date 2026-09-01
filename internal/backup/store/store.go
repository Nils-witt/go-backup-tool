// Package store is go-backup-tool's only home for direct SQL: every raw
// query and schema statement against the shared state/retention sqlite
// database lives here, behind a *Store whose exported methods are Get*/List*
// lookups and Save*/Delete* writes (plus a handful of clearly-named one-off
// operations — VerifyWebUIUser, RevokeAPIToken, ExpiredObjectPaths — whose
// behavior a plain Get/Save name would obscure). Every other package reaches
// the database only through a *Store, never through a *sql.DB of its own.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Store wraps the shared state/retention sqlite database go-backup-tool
// keeps alongside the config file: each start-time-anchored job's last
// successful run (so a restart can tell a genuinely missed run apart from an
// ordinary restart of a job that already ran on time), every file written to
// a local target or receiver with retention: set (so a later sweep knows
// what's eligible for automatic deletion), and the web UI's own users, API
// tokens, OIDC permission overrides, and audit logs.
type Store struct {
	db *sql.DB
}

// stateDBName is the sqlite database file Open opens, a sibling of the
// config file (see StateDBPath).
const stateDBName = ".go-backup-tool-state.db"

// StateDBPath returns the state db path for the config file at configPath: a
// sibling file in the same directory.
func StateDBPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), stateDBName)
}

// Open opens (creating if needed) the state-tracking sqlite database at
// path, ensuring its schema exists. The caller must Close it.
//
// SetMaxOpenConns(1) serializes every access through a single connection:
// sqlite handles one writer at a time regardless, and the returned *Store is
// shared across every job's goroutine for the life of the run.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening job state db %q: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	preMigration := []string{jobRunsSchema, targetRunsSchema, objectsSchema}
	postMigration := []string{
		loginEventsSchema,
		downloadEventsSchema,
		receiverEventsSchema,
		webUIUsersSchema,
		oidcUserPermissionsSchema,
		apiTokensSchema,
		targetErrorsSchema,
	}

	for _, schema := range preMigration {
		if err := execSchemaOrClose(ctx, db, path, schema); err != nil {
			return nil, err
		}
	}

	if err := ensureRetentionSecondsColumn(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	for _, schema := range postMigration {
		if err := execSchemaOrClose(ctx, db, path, schema); err != nil {
			return nil, err
		}
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// execSchemaOrClose runs schema against db, closing db and wrapping the
// error with path if it fails, so Open doesn't repeat that close-and-wrap
// boilerplate for each of its CREATE TABLE statements.
func execSchemaOrClose(ctx context.Context, db *sql.DB, path, schema string) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	return nil
}

// ensureRetentionSecondsColumn adds the retention_seconds column to objects
// if it's missing, for a state db created before that column existed.
// CREATE TABLE IF NOT EXISTS above is a no-op against such a db, so the
// column has to be added explicitly.
func ensureRetentionSecondsColumn(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(objects)`)
	if err != nil {
		return fmt.Errorf("reading objects table schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var found bool

	for rows.Next() {
		var (
			cid           int
			name, colType string
			notNull, pk   int
			defaultValue  sql.NullString
		)

		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("reading objects table schema: %w", err)
		}

		if name == "retention_seconds" {
			found = true
			break
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading objects table schema: %w", err)
	}

	if found {
		return nil
	}

	if _, err := db.ExecContext(ctx, `ALTER TABLE objects ADD COLUMN retention_seconds INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("adding retention_seconds column: %w", err)
	}

	return nil
}

// queryRows runs query against db and collects one T per row via scan,
// wrapping any error (the query itself, a per-row scan failure, or a final
// rows.Err()) with errMsg. Shared by every "list of rows" reader in this
// package, which otherwise repeat the same query/scan/append/rows.Err
// boilerplate with only the query, args, and scan function differing.
func queryRows[T any](ctx context.Context, db *sql.DB, errMsg, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", errMsg, err)
	}
	defer func() { _ = rows.Close() }()

	var out []T

	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", errMsg, err)
		}

		out = append(out, v)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", errMsg, err)
	}

	return out, nil
}

// queryRowOptional runs query against db and scans a single row via scan,
// reporting (zero, false, nil) if no row matched rather than an error.
// Shared by every "row may legitimately not exist yet" reader in this
// package.
func queryRowOptional[T any](ctx context.Context, db *sql.DB, errMsg, query string, args []any, scan func(*sql.Row) (T, error)) (T, bool, error) {
	var zero T

	v, err := scan(db.QueryRowContext(ctx, query, args...))

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return zero, false, nil
	case err != nil:
		return zero, false, fmt.Errorf("%s: %w", errMsg, err)
	default:
		return v, true, nil
	}
}
