// Package backup holds go-backup-tool's shared run-time state and storage
// primitives, used by both a job's own uploads and the receiver API: local
// object read/write (this file), retention tracking and sweeping
// (retention.go), receiver status/listing (receiver.go), job/target status
// for the web UI (status.go), and the periodic-loop helper (periodic.go)
// they're all built on.
package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// LocalObjectPath returns the filesystem path a local target (t.Kind ==
// ServerKindLocal) writes cfg.Key to: t.LocalPath, with t.Bucket as a
// subdirectory, so a local target shares the same targets: schema
// (server/bucket) as a remote one.
func LocalObjectPath(cfg *config.Config, t *config.Target) string {
	return filepath.Join(t.LocalPath, t.Bucket, cfg.Key)
}

// WriteLocalObject streams r to the local filesystem at
// LocalObjectPath(cfg, t), for a target whose server has type: local.
//
// It writes to a temporary file in the destination directory first and
// renames it into place once fully written, so a reader never observes a
// partially written object and a mid-stream failure leaves nothing at the
// final path.
func WriteLocalObject(cfg *config.Config, t *config.Target, r io.Reader) error {
	dst := LocalObjectPath(cfg, t)
	dir := filepath.Dir(dst)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating directory %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}

	// A successful rename below moves tmp.Name() to dst, so this Remove
	// then finds nothing and is a harmless no-op; on any earlier error path
	// it cleans up the leftover temp file.
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting permissions on %q: %w", tmp.Name(), err)
	}

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %q: %w", tmp.Name(), err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", tmp.Name(), err)
	}

	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("renaming %q to %q: %w", tmp.Name(), dst, err)
	}

	return nil
}

// SetObjectModTime backdates the file at LocalObjectPath(cfg, t)'s mtime
// (and atime) to at, used by the receiver API when a PUT's creationTime
// query parameter was given, so the dashboard's mtime-derived ExpiresAt
// (see ListReceiverFiles) stays consistent with the retention db record
// RecordObjectWrite made for the same write.
func SetObjectModTime(cfg *config.Config, t *config.Target, at time.Time) error {
	return os.Chtimes(LocalObjectPath(cfg, t), at, at)
}

// DeleteLocalObject removes the file at LocalObjectPath(cfg, t), used by the
// receiver API's DELETE endpoint (see handleDeleteObject in webui.go). A
// file that's already gone is not an error.
func DeleteLocalObject(cfg *config.Config, t *config.Target) error {
	err := os.Remove(LocalObjectPath(cfg, t))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}
