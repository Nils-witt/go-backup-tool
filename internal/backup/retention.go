package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// recordLocalWrite records, in the shared state db, that cfg's job just
// wrote the object at localObjectPath(cfg, t) — then sweeps t's server for
// anything now past its retention window. Only called after a successful
// write to a local target whose server has retention: set; retention == 0
// means no tracking (and so nothing to sweep). cfg.stateDB == nil (the state
// db couldn't be opened at startup) also disables tracking rather than
// erroring, since the backup write itself already succeeded and this is
// auxiliary bookkeeping.
func recordLocalWrite(ctx context.Context, cfg *config, t *target, log *slog.Logger) error {
	if t.retention <= 0 || cfg.stateDB == nil {
		return nil
	}

	path := localObjectPath(cfg, t)

	const upsert = `INSERT INTO objects (server, bucket, path, written_at, retention_seconds) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET written_at = excluded.written_at, retention_seconds = excluded.retention_seconds`

	retentionSeconds := int64(t.retention / time.Second)

	if _, err := cfg.stateDB.ExecContext(ctx, upsert, t.serverName, t.bucket, path, time.Now().UTC(), retentionSeconds); err != nil {
		return fmt.Errorf("recording write to state db: %w", err)
	}

	log.Debug("recorded write to state db", "path", path, "server", t.serverName, "retention", t.retention)

	return sweepRetention(ctx, cfg.stateDB, t, log)
}

// removeRetentionRecord removes any retention record for the object at
// localObjectPath(cfg, t), used when that object's write is rolled back
// (see handleDeleteObject in webui.go) so the database doesn't go on
// tracking a file that no longer exists.
func removeRetentionRecord(ctx context.Context, cfg *config, t *target) error {
	if t.retention <= 0 || cfg.stateDB == nil {
		return nil
	}

	if _, err := cfg.stateDB.ExecContext(ctx, `DELETE FROM objects WHERE path = ?`, localObjectPath(cfg, t)); err != nil {
		return fmt.Errorf("removing retention record: %w", err)
	}

	return nil
}

// sweepRetentionForTarget sweeps db for t's server, for callers (see
// sweepStartupRetention/sweepStartupReceiverRetention) that aren't already
// in the middle of a write. A nil db (the state db couldn't be opened at
// startup) is a no-op.
func sweepRetentionForTarget(ctx context.Context, db *sql.DB, t *target, log *slog.Logger) error {
	if t.retention <= 0 || db == nil {
		return nil
	}

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
		log.Debug("removing expired backup", "path", p, "server", t.serverName, "retention", t.retention)

		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
		return nil, fmt.Errorf("querying retention rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			p                string
			writtenAt        time.Time
			retentionSeconds int64
		)

		if err := rows.Scan(&p, &writtenAt, &retentionSeconds); err != nil {
			return nil, fmt.Errorf("reading retention rows: %w", err)
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
		return nil, fmt.Errorf("reading retention rows: %w", err)
	}

	return paths, nil
}
