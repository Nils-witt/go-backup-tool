// Package objectstore is the on-disk object layout shared by a job's local
// targets and the receiver API: writing an object (one flat file, or one
// file of a multi-file backup under its marker directory), the paths and
// listings derived from that layout, and retention tracking and sweeping
// in the state db.
package objectstore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// MultiFileMarker is the hidden, empty file WriteLocalObject keeps in a
// backup's key folder once one of its files is written there: it tells
// ListReceiverFiles that folder is one backup made of several files, as
// opposed to a flat object whose key just happens to contain a "/".
const MultiFileMarker = ".gbt-backup"

// LocalObjectPath returns the filesystem path a local target (t.Kind ==
// ServerKindLocal) writes cfg.Key's cfg.File to: t.LocalPath, with t.Bucket
// as a subdirectory (so a local target shares the same targets: schema —
// server/bucket — as a remote one), then the key folder and the file. An
// empty cfg.File addresses cfg.Key itself, a flat object.
func LocalObjectPath(cfg *config.Config, t *config.Target) string {
	return filepath.Join(t.LocalPath, t.Bucket, cfg.Key, cfg.File)
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

	if cfg.File != "" {
		if err := writeMultiFileMarker(dir); err != nil {
			return err
		}
	}

	return writeFileAtomically(dir, dst, r)
}

// writeFileAtomically streams r into a temp file in dir (which must exist),
// syncs it, and renames it to dst, so a reader never observes a partially
// written file and a failure leaves nothing at dst.
func writeFileAtomically(dir, dst string, r io.Reader) error {
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

	// Flushed before the rename, so a crash right after it can't leave a
	// zero-length or truncated object at dst that retention then counts as
	// a good backup.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing %q: %w", tmp.Name(), err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", tmp.Name(), err)
	}

	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("renaming %q to %q: %w", tmp.Name(), dst, err)
	}

	return nil
}

// writeMultiFileMarker creates dir's MultiFileMarker unless it exists.
func writeMultiFileMarker(dir string) error {
	path := filepath.Join(dir, MultiFileMarker)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dir is a target's own key folder, not raw untrusted input
	if errors.Is(err, fs.ErrExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("creating backup marker %q: %w", path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing backup marker %q: %w", path, err)
	}

	return nil
}

// removeEmptyMultiFileDir removes dir — a key folder that held one of a
// multi-file backup's files, which was just deleted — once nothing but its
// MultiFileMarker is left in it. A dir without the marker (a flat object's
// own parent folder) is left alone, as is one that still holds any file.
func removeEmptyMultiFileDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("reading %q: %w", dir, err)
	}

	hasMarker := false

	for _, e := range entries {
		if e.Name() != MultiFileMarker {
			return nil
		}

		hasMarker = true
	}

	if !hasMarker {
		return nil
	}

	if err := os.Remove(filepath.Join(dir, MultiFileMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing backup marker in %q: %w", dir, err)
	}

	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing empty backup folder %q: %w", dir, err)
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
