package objectstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// Every access to a local target's or receiver's objects goes through an
// *os.Root opened at its base directory, never a plain path joined under
// it: object keys are validated lexically (see SanitizeObjectKey), but a
// symlink inside the directory (planted by another process, or a
// misconfigured volume) would otherwise lead a write, read, or delete out
// of it. The base directory itself comes from the config and may be a
// symlink.

// openRoot opens dir as an *os.Root, creating it (and its parents) first
// when create is set. Without create, a missing dir fails with an error
// matching fs.ErrNotExist.
func openRoot(dir string, create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating directory %q: %w", dir, err)
		}
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", dir, err)
	}

	return root, nil
}

// targetBase is the directory local target t's objects live under:
// t.LocalPath, with t.Bucket as a subdirectory.
func targetBase(t *config.Target) string {
	return filepath.Join(t.LocalPath, t.Bucket)
}

// objectRel is cfg.Key's cfg.File (or cfg.Key itself, a flat object)
// relative to targetBase.
func objectRel(cfg *config.Config) string {
	return filepath.Join(filepath.FromSlash(cfg.Key), cfg.File)
}

// OpenObject opens the object stored as key (slash-separated, already
// validated, see SanitizeObjectKey) under base for reading, without
// following any symlink below base. A missing object, or a missing base,
// fails with an error matching fs.ErrNotExist.
func OpenObject(base, key string) (*os.File, error) {
	root, err := openRoot(base, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	// The file stays usable once its root is closed.
	return root.Open(filepath.FromSlash(key))
}

// StatObject is OpenObject's file info for key under base, without opening
// it.
func StatObject(base, key string) (fs.FileInfo, error) {
	root, err := openRoot(base, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	return root.Stat(filepath.FromSlash(key))
}

// tempFileName returns a fresh name for a temp file in dir, matching the
// ".*.tmp" pattern listings skip.
func tempFileName(dir string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating temp file name: %w", err)
	}

	return filepath.Join(dir, "."+hex.EncodeToString(b)+".tmp"), nil
}

// isNotExist reports whether err means a file (or its root) is missing.
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
