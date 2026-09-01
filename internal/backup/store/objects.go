package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// objectsSchema is objects: records every file go-backup-tool has written to
// a local target or receiver with retention: set, so a later sweep knows
// what's eligible for automatic deletion. retention_seconds records the
// retention duration in effect when each object was written, so a later
// config change to a server's retention: doesn't retroactively change how
// long already-written objects are kept (see ExpiredObjectPaths). It
// defaults to 0 ("unknown") so rows from before this column existed keep
// sweeping under the caller's current retention, exactly as they did before
// it was added.
const objectsSchema = `CREATE TABLE IF NOT EXISTS objects (
	server            TEXT NOT NULL,
	bucket            TEXT NOT NULL,
	path              TEXT NOT NULL PRIMARY KEY,
	written_at        TIMESTAMP NOT NULL,
	retention_seconds INTEGER NOT NULL DEFAULT 0
)`

// SaveObjectWrite records that a file was written to path on server/bucket
// at writtenAt, tracked for retentionSeconds — an upsert, so a later write
// to the same path (e.g. a repeating job overwriting its own object)
// refreshes both the write time and the retention window in effect for it.
func (s *Store) SaveObjectWrite(ctx context.Context, server, bucket, path string, writtenAt time.Time, retentionSeconds int64) error {
	const upsert = `INSERT INTO objects (server, bucket, path, written_at, retention_seconds) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET written_at = excluded.written_at, retention_seconds = excluded.retention_seconds`

	if _, err := s.db.ExecContext(ctx, upsert, server, bucket, path, writtenAt.UTC(), retentionSeconds); err != nil {
		return fmt.Errorf("recording write to state db: %w", err)
	}

	return nil
}

// DeleteObjectWrite removes any tracked record for path, used both when a
// swept file is removed from disk and when a write is rolled back, so the
// database doesn't go on tracking a file that no longer exists.
func (s *Store) DeleteObjectWrite(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM objects WHERE path = ?`, path); err != nil {
		return fmt.Errorf("removing retention record: %w", err)
	}

	return nil
}

// ExpiredObjectPaths returns the tracked paths for server that are past
// their retention window as of now. Each row's own retention_seconds is
// used when set (> 0); rows recorded before that column existed have it as
// 0 and fall back to fallbackRetention (the caller's current retention for
// server).
func (s *Store) ExpiredObjectPaths(ctx context.Context, server string, now time.Time, fallbackRetention time.Duration) ([]string, error) {
	type objectRow struct {
		path             string
		writtenAt        time.Time
		retentionSeconds int64
	}

	query := `SELECT path, written_at, retention_seconds FROM objects WHERE server = ?`

	rows, err := queryRows(ctx, s.db, "reading retention rows", query, []any{server}, func(rows *sql.Rows) (objectRow, error) {
		var r objectRow
		if err := rows.Scan(&r.path, &r.writtenAt, &r.retentionSeconds); err != nil {
			return objectRow{}, err
		}

		return r, nil
	})
	if err != nil {
		return nil, err
	}

	var paths []string

	for _, r := range rows {
		retention := time.Duration(r.retentionSeconds) * time.Second
		if retention <= 0 {
			retention = fallbackRetention
		}

		if r.writtenAt.Add(retention).Before(now) {
			paths = append(paths, r.path)
		}
	}

	return paths, nil
}
