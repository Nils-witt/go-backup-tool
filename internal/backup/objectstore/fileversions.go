package objectstore

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// A directory receiver (config.ReceiverKindDirectory) keeps every version
// of a file it's sent under the path the sender gave, mirrored below its
// root: the versions of "docs/a.txt" live in the folder
// "docs/a.txt.gbtv" (see config.VersionDirSuffix), one file per version
// named "<version>-<plaintext sha256>.gpg", the version zero-padded to
// versionDigits. The layout alone describes every version — no state db
// row is needed to find, deduplicate, or restore them — and the reserved
// suffix keeps a version folder from ever clashing with a source folder.

// versionDigits is how wide a version number is zero-padded, so versions
// sort by name; larger numbers still parse.
const versionDigits = 6

// versionFileRE matches a version file's name: its version number and the
// hex SHA-256 of its plaintext.
var versionFileRE = regexp.MustCompile(`^([0-9]+)-([0-9a-f]{64})\.gpg$`)

// plainSHA256RE matches a well-formed hex SHA-256, as sent by an
// on-change job.
var plainSHA256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// versionLocks serializes writes (and sweeps) of one version folder, keyed
// by its path, so two uploads of one file can't pick the same version
// number. Entries are never removed: one small mutex per path received
// since startup.
var versionLocks sync.Map

// FileVersion is one stored version of a file on a directory receiver.
type FileVersion struct {
	// Path is the file's path as its sender gave it, slash-separated.
	Path string `json:"path"`

	// Version numbers a path's versions from 1, newest highest.
	Version int `json:"version"`

	// SHA256 is the hex SHA-256 of the file's plaintext.
	SHA256 string `json:"sha256"`

	// Key is the version's own path below the receiver's root,
	// slash-separated: what the dashboard downloads it by.
	Key string `json:"key"`

	Size       int64     `json:"size"`
	ReceivedAt time.Time `json:"received_at"`
}

// VersionRetention is a directory receiver's retention rules. A version
// other than its path's newest is deleted only once every rule set no
// longer keeps it; a zero rule keeps nothing, and with both zero nothing is
// ever deleted.
type VersionRetention struct {
	// KeepVersions keeps a path's newest KeepVersions versions.
	KeepVersions int

	// MaxAge keeps every version received less than MaxAge ago.
	MaxAge time.Duration
}

// ValidateFilePath checks rel, the path an on-change job uploads a file as
// (slash-separated, relative to its source), for use under a directory
// receiver's root: a valid object key (see SanitizeObjectKey) none of
// whose segments ends in config.VersionDirSuffix.
func ValidateFilePath(rel string) error {
	if _, err := SanitizeObjectKey(rel); err != nil {
		return err
	}

	for seg := range strings.SplitSeq(rel, "/") {
		if strings.HasSuffix(seg, config.VersionDirSuffix) {
			return fmt.Errorf("invalid path %q: %q is a reserved suffix", rel, config.VersionDirSuffix)
		}
	}

	return nil
}

// WriteFileVersion stores r, one encrypted file whose plaintext hashes to
// plainSHA256, as the newest version of rel (see ValidateFilePath) under a
// directory receiver's root. When rel's newest version already has
// plainSHA256, r is read to its end (so a caller verifying its digest
// still can) and discarded, and stored is false. version is rel's newest
// version number either way.
func WriteFileVersion(base, rel, plainSHA256 string, r io.Reader) (version int, stored bool, err error) {
	if err := ValidateFilePath(rel); err != nil {
		return 0, false, err
	}

	if !plainSHA256RE.MatchString(plainSHA256) {
		return 0, false, fmt.Errorf("invalid content hash %q", plainSHA256)
	}

	root, err := openRoot(base, true)
	if err != nil {
		return 0, false, err
	}

	defer func() { _ = root.Close() }()

	dir := versionDir(rel)

	unlock := lockVersionDir(base, dir)
	defer unlock()

	versions, err := readVersionDir(root, dir)
	if err != nil {
		return 0, false, err
	}

	if n := len(versions); n > 0 && versions[n-1].sha256 == plainSHA256 {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return 0, false, fmt.Errorf("reading upload: %w", err)
		}

		return versions[n-1].version, false, nil
	}

	next := 1
	if n := len(versions); n > 0 {
		next = versions[n-1].version + 1
	}

	if err := root.MkdirAll(dir, 0o700); err != nil {
		return 0, false, fmt.Errorf("creating directory %q: %w", filepath.Join(base, dir), err)
	}

	if err := writeFileAtomically(root, dir, filepath.Join(dir, versionFileName(next, plainSHA256)), r, true); err != nil {
		return 0, false, err
	}

	return next, true, nil
}

// ListFileVersions returns every version stored under a directory
// receiver's root, ordered by path, then version. A root that doesn't
// exist yet holds no versions.
func ListFileVersions(base string) ([]FileVersion, error) {
	out := []FileVersion{}

	err := walkVersionDirs(base, func(_ *os.Root, rel string, versions []versionEntry) error {
		for _, v := range versions {
			out = append(out, FileVersion{
				Path:       rel,
				Version:    v.version,
				SHA256:     v.sha256,
				Key:        rel + config.VersionDirSuffix + "/" + v.name,
				Size:       v.size,
				ReceivedAt: v.modTime,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(out, func(a, b FileVersion) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), cmp.Compare(a.Version, b.Version))
	})

	return out, nil
}

// SweepFileVersions deletes, under a directory receiver's root, every
// version no rule of policy keeps any more as of now (see
// VersionRetention), and returns how many it deleted. A path's newest
// version is always kept: it's the file's current content. Failures to
// delete one version are logged and skipped.
func SweepFileVersions(base string, policy VersionRetention, now time.Time, log *slog.Logger) (int, error) {
	if policy.KeepVersions <= 0 && policy.MaxAge <= 0 {
		return 0, nil
	}

	removed := 0

	err := walkVersionDirs(base, func(root *os.Root, rel string, _ []versionEntry) error {
		dir := versionDir(rel)

		unlock := lockVersionDir(base, dir)
		defer unlock()

		// Re-read under the lock: a write may have added a version since
		// the walk listed this folder.
		versions, err := readVersionDir(root, dir)
		if err != nil {
			return err
		}

		for _, v := range expiredVersions(versions, policy, now) {
			if err := root.Remove(filepath.Join(dir, v.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Warn("deleting expired file version", "path", rel, "version", v.version, "error", err)
				continue
			}

			removed++

			log.Info("deleted expired file version", "path", rel, "version", v.version)
		}

		return nil
	})

	return removed, err
}

// expiredVersions returns the versions (sorted oldest first) that policy
// no longer keeps as of now. The newest is never among them.
func expiredVersions(versions []versionEntry, policy VersionRetention, now time.Time) []versionEntry {
	var out []versionEntry

	newest := len(versions) - 1

	for i, v := range versions[:max(newest, 0)] {
		keptByCount := policy.KeepVersions > 0 && newest-i < policy.KeepVersions
		keptByAge := policy.MaxAge > 0 && now.Sub(v.modTime) < policy.MaxAge

		if !keptByCount && !keptByAge {
			out = append(out, v)
		}
	}

	return out
}

// versionEntry is one version file read from a version folder.
type versionEntry struct {
	name    string
	version int
	sha256  string
	size    int64
	modTime time.Time
}

// versionDir is the folder rel's versions are kept in, relative to a
// directory receiver's base.
func versionDir(rel string) string {
	return filepath.FromSlash(rel) + config.VersionDirSuffix
}

// versionFileName names version of a file whose plaintext hashes to
// plainSHA256.
func versionFileName(version int, plainSHA256 string) string {
	return fmt.Sprintf("%0*d-%s.gpg", versionDigits, version, plainSHA256)
}

// lockVersionDir locks the entry in versionLocks for dir under base,
// returning its unlock.
func lockVersionDir(base, dir string) func() {
	mu, _ := versionLocks.LoadOrStore(filepath.Join(base, dir), &sync.Mutex{})

	m, _ := mu.(*sync.Mutex)
	m.Lock()

	return m.Unlock
}

// readVersionDir returns the versions in dir (relative to root), oldest
// first, ignoring any other file (such as an interrupted write's temp
// file). A dir that doesn't exist holds none.
func readVersionDir(root *os.Root, dir string) ([]versionEntry, error) {
	entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", filepath.Join(root.Name(), dir), err)
	}

	var out []versionEntry

	for _, e := range entries {
		m := versionFileRE.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}

		version, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}

		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", e.Name(), err)
		}

		out = append(out, versionEntry{name: e.Name(), version: version, sha256: m[2], size: info.Size(), modTime: info.ModTime()})
	}

	slices.SortFunc(out, func(a, b versionEntry) int { return cmp.Compare(a.version, b.version) })

	return out, nil
}

// walkVersionDirs calls fn with every version folder under base: base's
// root, the path its versions are of (slash-separated, relative to base),
// and its versions, oldest first. Symlinks are never followed. A base that
// doesn't exist yet holds none.
func walkVersionDirs(base string, fn func(root *os.Root, rel string, versions []versionEntry) error) error {
	root, err := openRoot(base, false)
	if isNotExist(err) {
		return nil
	}

	if err != nil {
		return err
	}

	defer func() { _ = root.Close() }()

	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() || !strings.HasSuffix(d.Name(), config.VersionDirSuffix) || p == "." {
			return nil
		}

		versions, err := readVersionDir(root, filepath.FromSlash(p))
		if err != nil {
			return err
		}

		if err := fn(root, strings.TrimSuffix(p, config.VersionDirSuffix), versions); err != nil {
			return err
		}

		return fs.SkipDir
	})
}
