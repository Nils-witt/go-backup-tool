package objectstore

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// SanitizeObjectKey validates key (the {key...} wildcard segment of a
// receiver API request path) before it's joined into a filesystem path.
// Although the request is already authenticated, key still comes from the
// network and must not be trusted to stay within its receiver's root: this
// rejects an empty/absolute key or one containing a "." or ".." path
// segment.
//
// Keys are '/'-separated on every platform, so '\' and ':' are rejected
// outright: on Windows filepath.Join treats '\' as a separator and ':' as a
// volume marker, which would let "..\..\x" or "C:x" escape the root even
// though no '/'-separated segment is "..". filepath.IsLocal then applies
// the host OS's own rules (e.g. Windows reserved names like NUL) as a last
// check on the exact path the callers join.
func SanitizeObjectKey(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, `\:`+"\x00") {
		return "", fmt.Errorf("invalid key %q", key)
	}

	for seg := range strings.SplitSeq(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("invalid key %q", key)
		}
	}

	if !filepath.IsLocal(filepath.FromSlash(key)) {
		return "", fmt.Errorf("invalid key %q", key)
	}

	return key, nil
}

// ReceiverTarget builds a synthetic local target for recv, so the receiver
// API's handler can reuse WriteLocalObject/RecordLocalWrite (see
// localobject.go and retention.go) exactly as a type: local target does — a receiver is a local target
// written to over HTTP with JWT auth, not a distinct storage
// implementation.
func ReceiverTarget(recv config.ResolvedReceiver) *config.Target {
	return &config.Target{
		ServerName: "receiver:" + recv.ID,
		Kind:       config.ServerKindLocal,
		LocalPath:  recv.Path,
		Retention:  recv.Retention,
	}
}

// ReceiverFile is one object currently stored under a receiver's path, as
// reported over /api/receivers/{id}/files, for display in the web UI
// dashboard (see webui.go). File is set for one file of a multi-file backup
// (see MultiFileMarker): Key is then the backup's key and File the file's
// name under it; a flat object has only Key. ExpiresAt is the zero
// time.Time (see hasTime in the frontend's format.ts) when the receiver has
// no retention: configured, since there's then nothing to expire.
type ReceiverFile struct {
	Key       string    `json:"key"`
	File      string    `json:"file,omitempty"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ListReceiverFiles walks recv.Path and returns every object currently
// stored there, keyed the same way the receiver API addresses them (the path
// relative to recv.Path, using "/" separators regardless of OS — split into
// Key and File for a multi-file backup's files, see ReceiverFile), sorted by
// creation time ascending (oldest first). A root
// directory that doesn't exist yet (nothing has been received) is not an
// error — it just yields no files.
// Temp files left behind by an interrupted write (see WriteLocalObject's
// ".*.tmp" pattern) are skipped since they're not yet complete objects. Each
// file's ExpiresAt is derived from recv's current retention: setting rather
// than the retention_seconds recorded at write time (see RecordObjectWrite),
// so — like the rest of this listing, which just reflects what's on disk —
// it can disagree with the state db's own sweep if retention: changed after
// the file was written.
func ListReceiverFiles(recv config.ResolvedReceiver) ([]ReceiverFile, error) {
	if _, err := os.Stat(recv.Path); err != nil {
		if os.IsNotExist(err) {
			return []ReceiverFile{}, nil
		}

		return nil, err
	}

	files := []ReceiverFile{}
	isMultiFile := multiFileDirCache(recv.Path)

	err := filepath.WalkDir(recv.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(recv.Path, path)
		if err != nil {
			return err
		}

		var expiresAt time.Time
		if recv.Retention > 0 {
			expiresAt = info.ModTime().Add(recv.Retention)
		}

		f := ReceiverFile{Key: filepath.ToSlash(rel), Size: info.Size(), ModTime: info.ModTime(), ExpiresAt: expiresAt}
		if isMultiFile(filepath.Dir(path)) {
			f.Key, f.File = filepath.ToSlash(filepath.Dir(rel)), d.Name()
		}

		files = append(files, f)

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].ModTime.Before(files[j].ModTime) })

	return files, nil
}

// multiFileDirCache returns a func reporting whether dir — a folder under
// root, root itself excluded — holds a MultiFileMarker, checking each dir on
// disk only once.
func multiFileDirCache(root string) func(dir string) bool {
	seen := map[string]bool{}

	return func(dir string) bool {
		if dir == filepath.Clean(root) {
			return false
		}

		is, ok := seen[dir]
		if !ok {
			_, err := os.Stat(filepath.Join(dir, MultiFileMarker))
			is = err == nil
			seen[dir] = is
		}

		return is
	}
}

// ObjectPath joins key, and file when non-empty, under root: where a
// receiver stores (and the dashboard reads) one object. Both must already be
// validated (see SanitizeObjectKey and config.ValidateFileName).
func ObjectPath(root, key, file string) string {
	return filepath.Join(root, filepath.FromSlash(key), file)
}

// LastReceivedAt returns the most recent modification time among every
// object currently stored under recv.Path, skipping in-progress temp files
// the same way ListReceiverFiles does — used by the stale-receiver webhook
// monitor (see MonitorStaleReceivers) as the "last received" signal, since it
// reflects what's actually on disk regardless of process restarts (unlike
// status.ReceiverStore's in-memory LastSeen). ok is false if recv.Path
// doesn't exist yet or holds no objects, meaning nothing has ever been
// received.
func LastReceivedAt(recv config.ResolvedReceiver) (t time.Time, ok bool, err error) {
	if _, statErr := os.Stat(recv.Path); statErr != nil {
		if os.IsNotExist(statErr) {
			return time.Time{}, false, nil
		}

		return time.Time{}, false, statErr
	}

	walkErr := filepath.WalkDir(recv.Path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.ModTime().After(t) {
			t = info.ModTime()
			ok = true
		}

		return nil
	})
	if walkErr != nil {
		return time.Time{}, false, walkErr
	}

	return t, ok, nil
}
