package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// RecordLocalWrite records, in the shared state db, that cfg's job just
// wrote the object at LocalObjectPath(cfg, t) — then sweeps t's server for
// anything now past its retention window. Only called after a successful
// write to a local target whose server has retention: set; retention == 0
// means no tracking (and so nothing to sweep). A nil db (the state db
// couldn't be opened at startup) also disables tracking rather than
// erroring, since the backup write itself already succeeded and this is
// auxiliary bookkeeping.
func RecordLocalWrite(ctx context.Context, db store.Store, cfg *config.Config, t *config.Target, log *slog.Logger) error {
	if t.Retention <= 0 || db == nil {
		return nil
	}

	if err := RecordObjectWrite(ctx, db, cfg, t, log); err != nil {
		return err
	}

	return sweepRetention(ctx, db, t, log)
}

// RecordObjectWrite records, in the shared state db, that cfg's job just
// wrote the object at LocalObjectPath(cfg, t), without sweeping for expired
// objects — unlike RecordLocalWrite, whose immediate per-write sweep this
// factors out so a caller with its own sweep schedule (see the receiver
// package's MonitorReceiverRetention, which sweeps every receiver on a
// one-minute timer instead) can record a write without also paying for a
// sweep on every single request. retention == 0 means no tracking, and a
// nil db (the state db couldn't be opened at startup) also disables
// tracking rather than erroring, since the backup write itself
// already succeeded and this is auxiliary bookkeeping. The retention basis
// is cfg.CreatedAt when set (the receiver API's creationTime, or a run's job
// start time — see remoteclient.Upload/receiver.parseCreationTime), or
// upload time otherwise.
func RecordObjectWrite(ctx context.Context, db store.Store, cfg *config.Config, t *config.Target, log *slog.Logger) error {
	if t.Retention <= 0 || db == nil {
		return nil
	}

	path := LocalObjectPath(cfg, t)
	retentionSeconds := int64(t.Retention / time.Second)

	writtenAt := cfg.CreatedAt
	if writtenAt.IsZero() {
		writtenAt = time.Now().UTC()
	}

	if err := db.SaveObjectWrite(ctx, t.ServerName, t.Bucket, path, writtenAt, retentionSeconds); err != nil {
		return err
	}

	log.Debug("recorded write to state db", "path", path, "server", t.ServerName, "retention", t.Retention)

	return nil
}

// SweepRetentionForTarget sweeps db for t's server, for callers (see
// sweepStartupRetention/sweepStartupReceiverRetention) that aren't already
// in the middle of a write. A nil db (the state db couldn't be opened at
// startup) is a no-op.
func SweepRetentionForTarget(ctx context.Context, db store.Store, t *config.Target, log *slog.Logger) error {
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
func sweepRetention(ctx context.Context, db store.Store, t *config.Target, log *slog.Logger) error {
	if t.Retention <= 0 {
		return nil
	}

	now := time.Now().UTC()

	expired, err := db.ExpiredObjectPaths(ctx, t.ServerName, now, t.Retention)
	if err != nil {
		return err
	}

	log.Debug("retention sweep", "server", t.ServerName, "now", now, "expired", len(expired))

	// The recorded paths are absolute, under t.LocalPath: each is removed
	// through a root opened there (see root.go), so a symlink can't lead
	// the delete outside it.
	root, err := openRoot(t.LocalPath, false)
	if err != nil && !isNotExist(err) {
		return err
	}

	if root != nil {
		defer func() { _ = root.Close() }()
	}

	var errs []error

	for _, p := range expired {
		if err := removeExpiredObject(ctx, db, root, t, p, log); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// removeExpiredObject deletes p, an expired object recorded for t's server,
// through root (opened at t.LocalPath; nil when that doesn't exist, so
// there's no file left to delete), then its db record and, once empty, its
// multi-file backup folder.
func removeExpiredObject(ctx context.Context, db store.Store, root *os.Root, t *config.Target, p string, log *slog.Logger) error {
	log.Debug("removing expired backup", "path", p, "server", t.ServerName, "retention", t.Retention)

	rel, err := filepath.Rel(t.LocalPath, p)
	if err != nil || !filepath.IsLocal(rel) {
		// Written before the server's path changed: no longer this
		// server's to delete. Forget it rather than retrying forever.
		log.Warn("expired backup is outside its server's path; leaving it in place", "path", p, "server", t.ServerName)

		if err := db.DeleteObjectWrite(ctx, p); err != nil {
			return fmt.Errorf("removing retention db record for %q: %w", p, err)
		}

		return nil
	}

	if root != nil {
		if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing expired backup %q: %w", p, err)
		}
	}

	if err := db.DeleteObjectWrite(ctx, p); err != nil {
		return fmt.Errorf("removing retention db record for %q: %w", p, err)
	}

	if root != nil {
		if err := removeEmptyMultiFileDir(root, filepath.Dir(rel)); err != nil {
			return err
		}
	}

	log.Info("removed expired backup", "path", p, "server", t.ServerName, "retention", t.Retention)

	return nil
}
