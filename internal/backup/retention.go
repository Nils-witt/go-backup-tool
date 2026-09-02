package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// RecordLocalWrite records, in the shared state db, that cfg's job just
// wrote the object at LocalObjectPath(cfg, t) — then sweeps t's server for
// anything now past its retention window. Only called after a successful
// write to a local target whose server has retention: set; retention == 0
// means no tracking (and so nothing to sweep). cfg.StateDB == nil (the state
// db couldn't be opened at startup) also disables tracking rather than
// erroring, since the backup write itself already succeeded and this is
// auxiliary bookkeeping.
func RecordLocalWrite(ctx context.Context, cfg *config.Config, t *config.Target, log *slog.Logger) error {
	if t.Retention <= 0 || cfg.StateDB == nil {
		return nil
	}

	if err := RecordObjectWrite(ctx, cfg, t, log); err != nil {
		return err
	}

	return sweepRetention(ctx, cfg.StateDB, t, log)
}

// RecordObjectWrite records, in the shared state db, that cfg's job just
// wrote the object at LocalObjectPath(cfg, t), without sweeping for expired
// objects — unlike RecordLocalWrite, whose immediate per-write sweep this
// factors out so a caller with its own sweep schedule (see the receiver
// package's MonitorReceiverRetention, which sweeps every receiver on a
// one-minute timer instead) can record a write without also paying for a
// sweep on every single request. retention == 0 means no tracking, and
// cfg.StateDB == nil (the state db couldn't be opened at startup) also
// disables tracking rather than erroring, since the backup write itself
// already succeeded and this is auxiliary bookkeeping.
func RecordObjectWrite(ctx context.Context, cfg *config.Config, t *config.Target, log *slog.Logger) error {
	if t.Retention <= 0 || cfg.StateDB == nil {
		return nil
	}

	path := LocalObjectPath(cfg, t)
	retentionSeconds := int64(t.Retention / time.Second)

	if err := cfg.StateDB.SaveObjectWrite(ctx, t.ServerName, t.Bucket, path, time.Now().UTC(), retentionSeconds); err != nil {
		return err
	}

	log.Debug("recorded write to state db", "path", path, "server", t.ServerName, "retention", t.Retention)

	return nil
}

// RemoveRetentionRecord removes any retention record for the object at
// LocalObjectPath(cfg, t), used when that object's write is rolled back
// (see handleDeleteObject in webui.go) so the database doesn't go on
// tracking a file that no longer exists.
func RemoveRetentionRecord(ctx context.Context, cfg *config.Config, t *config.Target) error {
	if t.Retention <= 0 || cfg.StateDB == nil {
		return nil
	}

	return cfg.StateDB.DeleteObjectWrite(ctx, LocalObjectPath(cfg, t))
}

// SweepRetentionForTarget sweeps db for t's server, for callers (see
// sweepStartupRetention/sweepStartupReceiverRetention) that aren't already
// in the middle of a write. A nil db (the state db couldn't be opened at
// startup) is a no-op.
func SweepRetentionForTarget(ctx context.Context, db *store.Store, t *config.Target, log *slog.Logger) error {
	if t.Retention <= 0 || db == nil {
		return nil
	}

	return sweepRetention(ctx, db, t, log)
}

// sweepRetention deletes every file tracked in db for t's server that's past
// its retention window, along with its db record. Each row expires against
// the retention_seconds recorded for it at write time (see RecordLocalWrite)
// rather than t.Retention, so a later change to a server's retention: only
// affects objects written after the change; a row from before that column
// existed has retention_seconds == 0 ("unknown") and falls back to
// t.Retention, matching its pre-migration behavior. A file already missing
// from disk is not an error: its record is still removed. Errors are
// collected per file and returned joined, so one bad entry doesn't stop the
// rest of the sweep.
func sweepRetention(ctx context.Context, db *store.Store, t *config.Target, log *slog.Logger) error {
	if t.Retention <= 0 {
		return nil
	}

	now := time.Now().UTC()

	expired, err := db.ExpiredObjectPaths(ctx, t.ServerName, now, t.Retention)
	if err != nil {
		return err
	}

	log.Debug("retention sweep", "server", t.ServerName, "now", now, "expired", len(expired))

	var errs []error

	for _, p := range expired {
		log.Debug("removing expired backup", "path", p, "server", t.ServerName, "retention", t.Retention)

		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing expired backup %q: %w", p, err))
			continue
		}

		if err := db.DeleteObjectWrite(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("removing retention db record for %q: %w", p, err))
			continue
		}

		log.Info("removed expired backup", "path", p, "server", t.ServerName, "retention", t.Retention)
	}

	return errors.Join(errs...)
}
