package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// retentionDBName is the sqlite database go-backup-tool keeps at the root of
// every local server with retention: set, tracking what it has written there
// so a later sweep knows what's eligible for automatic deletion. It lives
// alongside the buckets it tracks rather than e.g. under the job's own
// config, since a local server (and so its retention policy) may be shared
// by several jobs.
const retentionDBName = ".go-backup-tool-retention.db"

// retentionDBPath returns the sqlite database path for a local server whose
// root directory is localRoot.
func retentionDBPath(localRoot string) string {
	return filepath.Join(localRoot, retentionDBName)
}

// openRetentionDB opens (creating if needed) the retention-tracking sqlite
// database at path, ensuring its schema exists. The caller must Close it.
//
// SetMaxOpenConns(1) serializes every access through a single connection:
// sqlite handles one writer at a time regardless, and go-backup-tool's own
// jobs (which may share a local server, and so this same database file) run
// concurrently, so this avoids "database is locked" errors from this
// process's own concurrent use rather than relying on busy_timeout alone.
func openRetentionDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening retention db %q: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	// retention_seconds records the retention duration in effect when each
	// object was written, so a later config change to a server's retention:
	// doesn't retroactively change how long already-written objects are
	// kept (see sweepRetention). It defaults to 0 ("unknown") so rows from
	// before this column existed keep sweeping under the target's current
	// retention, exactly as they did before it was added.
	const schema = `CREATE TABLE IF NOT EXISTS objects (
		server            TEXT NOT NULL,
		bucket            TEXT NOT NULL,
		path              TEXT NOT NULL PRIMARY KEY,
		written_at        TIMESTAMP NOT NULL,
		retention_seconds INTEGER NOT NULL DEFAULT 0
	)`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing retention db %q: %w", path, err)
	}

	if err := ensureRetentionSecondsColumn(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating retention db %q: %w", path, err)
	}

	return db, nil
}

// ensureRetentionSecondsColumn adds the retention_seconds column to objects
// if it's missing, for a retention db created before that column existed.
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

// recordLocalWrite records, in t's server's retention database, that cfg's
// job just wrote the object at localObjectPath(cfg, t) — then sweeps that
// server for anything now past its retention window. Only called after a
// successful write to a local target whose server has retention: set;
// retention == 0 means no tracking (and so nothing to sweep).
func recordLocalWrite(ctx context.Context, cfg *config, t *target, log *slog.Logger) error {
	if t.retention <= 0 {
		return nil
	}

	db, err := openRetentionDB(ctx, retentionDBPath(t.localPath))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	path := localObjectPath(cfg, t)

	const upsert = `INSERT INTO objects (server, bucket, path, written_at, retention_seconds) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET written_at = excluded.written_at, retention_seconds = excluded.retention_seconds`

	retentionSeconds := int64(t.retention / time.Second)

	if _, err := db.ExecContext(ctx, upsert, t.serverName, t.bucket, path, time.Now().UTC(), retentionSeconds); err != nil {
		return fmt.Errorf("recording write to retention db %q: %w", retentionDBPath(t.localPath), err)
	}

	log.Debug("recorded write to retention db", "path", path, "server", t.serverName, "retention", t.retention)

	return sweepRetention(ctx, db, t, log)
}

// removeRetentionRecord removes any retention-db record for the object at
// localObjectPath(cfg, t), used when that object's write is rolled back
// (see handleDeleteObject in webui.go) so the database doesn't go on
// tracking a file that no longer exists.
func removeRetentionRecord(ctx context.Context, cfg *config, t *target) error {
	if t.retention <= 0 {
		return nil
	}

	db, err := openRetentionDB(ctx, retentionDBPath(t.localPath))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(ctx, `DELETE FROM objects WHERE path = ?`, localObjectPath(cfg, t)); err != nil {
		return fmt.Errorf("removing retention db record: %w", err)
	}

	return nil
}

// sweepRetentionForTarget opens t's server's retention database and sweeps
// it, for callers (see sweepStartupRetention) that don't already have a
// *sql.DB open for it.
func sweepRetentionForTarget(ctx context.Context, t *target, log *slog.Logger) error {
	if t.retention <= 0 {
		return nil
	}

	db, err := openRetentionDB(ctx, retentionDBPath(t.localPath))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	return sweepRetention(ctx, db, t, log)
}

// sweepRetention deletes every file tracked in db for t's server that's past
// its retention window, along with its db record. Each row expires against
// the retention_seconds recorded for it at write time (see recordLocalWrite)
// rather than t.retention, so a later change to a server's retention: only
// affects objects written after the change; a row from before that column
// existed has retention_seconds == 0 ("unknown") and falls back to
// t.retention, matching its pre-migration behavior. A file already missing
// from disk is not an error: its record is still removed. Errors are
// collected per file and returned joined, so one bad entry doesn't stop the
// rest of the sweep.
func sweepRetention(ctx context.Context, db *sql.DB, t *target, log *slog.Logger) error {
	if t.retention <= 0 {
		return nil
	}

	now := time.Now().UTC()

	expired, err := expiredRetentionPaths(ctx, db, t.serverName, now, t.retention)
	if err != nil {
		return err
	}

	log.Debug("retention sweep", "server", t.serverName, "now", now, "expired", len(expired))

	var errs []error

	for _, p := range expired {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing expired backup %q: %w", p, err))
			continue
		}

		if _, err := db.ExecContext(ctx, `DELETE FROM objects WHERE path = ?`, p); err != nil {
			errs = append(errs, fmt.Errorf("removing retention db record for %q: %w", p, err))
			continue
		}

		log.Info("removed expired backup", "path", p, "server", t.serverName, "retention", t.retention)
	}

	return errors.Join(errs...)
}

// expiredRetentionPaths returns the tracked paths for server that are past
// their retention window as of now. Each row's own retention_seconds is
// used when set (> 0); rows recorded before that column existed have it as
// 0 and fall back to fallbackRetention (the calling target's current
// retention).
func expiredRetentionPaths(ctx context.Context, db *sql.DB, server string, now time.Time, fallbackRetention time.Duration) (paths []string, err error) {
	rows, err := db.QueryContext(ctx, `SELECT path, written_at, retention_seconds FROM objects WHERE server = ?`, server)
	if err != nil {
		return nil, fmt.Errorf("querying retention db: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			p                string
			writtenAt        time.Time
			retentionSeconds int64
		)

		if err := rows.Scan(&p, &writtenAt, &retentionSeconds); err != nil {
			return nil, fmt.Errorf("reading retention db: %w", err)
		}

		retention := time.Duration(retentionSeconds) * time.Second
		if retention <= 0 {
			retention = fallbackRetention
		}

		if writtenAt.Add(retention).Before(now) {
			paths = append(paths, p)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading retention db: %w", err)
	}

	return paths, nil
}
