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
	return writeLocalObject(cfg, t, r, true)
}

// ErrObjectExists is returned by WriteNewLocalObject when an object already
// exists at its path.
var ErrObjectExists = errors.New("object already exists")

// WriteNewLocalObject is WriteLocalObject for a write-once destination (an
// immutable: receiver): it never replaces an existing object, returning
// ErrObjectExists instead — even when another write lands at the same path
// while r is still streaming, since the finished temp file is hard-linked
// into place rather than renamed over it.
func WriteNewLocalObject(cfg *config.Config, t *config.Target, r io.Reader) error {
	return writeLocalObject(cfg, t, r, false)
}

func writeLocalObject(cfg *config.Config, t *config.Target, r io.Reader, replace bool) error {
	root, err := openRoot(targetBase(t), true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	dst := objectRel(cfg)
	dir := filepath.Dir(dst)

	if err := root.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating directory %q: %w", filepath.Join(root.Name(), dir), err)
	}

	if cfg.File != "" {
		if err := writeMultiFileMarker(root, dir); err != nil {
			return err
		}
	}

	return writeFileAtomically(root, dir, dst, r, replace)
}

// writeFileAtomically streams r into a temp file in dir (which must exist)
// under root, syncs it, and renames it to dst, so a reader never observes
// a partially written file and a failure leaves nothing at dst. Without
// replace, the temp file is hard-linked to dst instead, which fails with
// ErrObjectExists if dst already exists. dir and dst are relative to root.
func writeFileAtomically(root *os.Root, dir, dst string, r io.Reader, replace bool) error {
	name, err := tempFileName(dir)
	if err != nil {
		return err
	}

	tmp, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", filepath.Join(root.Name(), dir), err)
	}

	// A successful rename below moves name to dst, so this Remove then
	// finds nothing and is a harmless no-op (after a link, it drops the
	// temp name and leaves dst); on any earlier error path it cleans up
	// the leftover temp file.
	defer func() { _ = root.Remove(name) }()

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

	full := filepath.Join(root.Name(), dst)

	if !replace {
		if err := root.Link(name, dst); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("%w: %q", ErrObjectExists, full)
			}

			return fmt.Errorf("linking %q to %q: %w", tmp.Name(), full, err)
		}

		return nil
	}

	if err := root.Rename(name, dst); err != nil {
		return fmt.Errorf("renaming %q to %q: %w", tmp.Name(), full, err)
	}

	return nil
}

// writeMultiFileMarker creates dir's MultiFileMarker under root unless it
// exists.
func writeMultiFileMarker(root *os.Root, dir string) error {
	path := filepath.Join(dir, MultiFileMarker)

	f, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("creating backup marker %q: %w", filepath.Join(root.Name(), path), err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing backup marker %q: %w", filepath.Join(root.Name(), path), err)
	}

	return nil
}

// removeEmptyMultiFileDir removes dir (relative to root) — a key folder
// that held one of a multi-file backup's files, which was just deleted —
// once nothing but its MultiFileMarker is left in it. A dir without the
// marker (a flat object's own parent folder) is left alone, as is one that
// still holds any file.
func removeEmptyMultiFileDir(root *os.Root, dir string) error {
	entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("reading %q: %w", filepath.Join(root.Name(), dir), err)
	}

	hasMarker := false

	for _, e := range entries {
		if e.Name() != MultiFileMarker {
			return nil
		}

		hasMarker = true
	}

	if !hasMarker || dir == "." {
		return nil
	}

	if err := root.Remove(filepath.Join(dir, MultiFileMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing backup marker in %q: %w", filepath.Join(root.Name(), dir), err)
	}

	if err := root.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing empty backup folder %q: %w", filepath.Join(root.Name(), dir), err)
	}

	return nil
}

// SetObjectModTime backdates the file at LocalObjectPath(cfg, t)'s mtime
// (and atime) to at, used by the receiver API when a PUT's creationTime
// query parameter was given, so the dashboard's mtime-derived ExpiresAt
// (see ListReceiverFiles) stays consistent with the retention db record
// RecordObjectWrite made for the same write.
func SetObjectModTime(cfg *config.Config, t *config.Target, at time.Time) error {
	root, err := openRoot(targetBase(t), false)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	return root.Chtimes(objectRel(cfg), at, at)
}
