package backup

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// ParseCreationTime parses raw (the receiver API's creationTime query
// parameter, RFC3339) against now (the time the upload request is being
// handled), returning the zero Time and no error when raw is empty —
// meaning "not provided, use upload time" (see RecordObjectWrite). A
// non-empty raw that fails to parse, or that names a time not strictly
// before now, is an error.
func ParseCreationTime(raw string, now time.Time) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid creationTime %q: %w", raw, err)
	}

	if !t.Before(now) {
		return time.Time{}, fmt.Errorf("creationTime %q must be before the upload time", raw)
	}

	return t, nil
}

// SanitizeObjectKey validates key (the {key...} wildcard segment of a
// receiver API request path) before it's joined into a filesystem path.
// Although the request is already authenticated, key still comes from the
// network and must not be trusted to stay within its receiver's root: this
// rejects an empty/absolute key or one containing a "." or ".." path
// segment.
func SanitizeObjectKey(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("invalid key %q", key)
	}

	for seg := range strings.SplitSeq(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("invalid key %q", key)
		}
	}

	return key, nil
}

// ReceiverTarget builds a synthetic local target for recv, so the receiver
// API's handlers can reuse WriteLocalObject/RecordLocalWrite/
// DeleteLocalObject/RemoveRetentionRecord (see pipeline.go and retention.go)
// exactly as a type: local target does — a receiver is a local target
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
// dashboard (see webui.go). ExpiresAt is the zero time.Time (see hasTime in
// the frontend's format.ts) when the receiver has no retention: configured,
// since there's then nothing to expire.
type ReceiverFile struct {
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ListReceiverFiles walks recv.Path and returns every object currently
// stored there, keyed the same way handleReceiveObject/handleDeleteObject
// address them (the path relative to recv.Path, using "/" separators
// regardless of OS), sorted by creation time ascending (oldest first). A root
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

		files = append(files, ReceiverFile{
			Key: filepath.ToSlash(rel), Size: info.Size(), ModTime: info.ModTime(), ExpiresAt: expiresAt,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].ModTime.Before(files[j].ModTime) })

	return files, nil
}

// LastReceivedAt returns the most recent modification time among every
// object currently stored under recv.Path, skipping in-progress temp files
// the same way ListReceiverFiles does — used by the stale-receiver webhook
// monitor (see MonitorStaleReceivers) as the "last received" signal, since it
// reflects what's actually on disk regardless of process restarts (unlike
// receiverStatusStore's in-memory LastSeen). ok is false if recv.Path
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

// ReceiverSnapshot is one configured receiver's current status, as reported
// over /api/receivers, for display in the web UI dashboard (see webui.go).
// StaleAfter and Stale are filled in by handleReceiverStatus (see
// annotateReceiverStaleness), not NewReceiverStatusStore/record below, since
// they reflect what's actually on disk (like LastReceivedAt) rather than
// live state this process tracks as requests come in.
type ReceiverSnapshot struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	Retention  string    `json:"retention,omitempty"`
	State      RunState  `json:"state"` // idle until the first object is received
	LastKey    string    `json:"last_key,omitempty"`
	LastSeen   time.Time `json:"last_seen"` // zero if nothing has been received yet
	Error      string    `json:"error,omitempty"`
	StaleAfter string    `json:"stale_after,omitempty"` // set only when this receiver has stale-after: configured
	Stale      bool      `json:"stale,omitempty"`       // true once the most recent file is older than StaleAfter; always false when StaleAfter is unset
}

// ReceiverStatusStore tracks the live state of every configured receiver,
// updated as the receiver API's handlers (see handleReceiveObject/
// handleDeleteObject in webui.go) serve incoming requests, and read by the
// web UI's HTTP handlers. Safe for concurrent use, since receiver requests
// can arrive concurrently with each other and with status requests.
type ReceiverStatusStore struct {
	mu    sync.Mutex
	byID  map[string]*ReceiverSnapshot
	order []string // receiver ids, sorted, for stable UI listing
}

// NewReceiverStatusStore builds a receiverStatusStore with one idle entry
// per entry in receivers, listed in id order (receivers is keyed by id, so
// no other ordering survives buildReceivers).
func NewReceiverStatusStore(receivers map[string]config.ResolvedReceiver) *ReceiverStatusStore {
	ids := make([]string, 0, len(receivers))
	for id := range receivers {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	s := &ReceiverStatusStore{byID: make(map[string]*ReceiverSnapshot, len(receivers)), order: ids}

	for _, id := range ids {
		recv := receivers[id]
		s.byID[id] = &ReceiverSnapshot{ID: id, Path: recv.Path, Retention: intervalString(recv.Retention), State: StateIdle}
	}

	return s
}

// Record updates receiver id's status after it just handled a request for
// key: err nil marks it ok, non-nil marks it failed with err's message,
// mirroring statusStore.targetDone's success/failure recording for job
// targets.
func (s *ReceiverStatusStore) Record(id, key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.byID[id]
	if !ok {
		return
	}

	r.LastKey = key
	r.LastSeen = time.Now()

	SetOutcome(&r.State, &r.Error, err)
}

// SeedLastEvent initializes receiver id's snapshot from its most recently
// persisted receiver_events row (see the store package's
// GetLastReceiverEvent), mirroring statusStore.seedLastRun's reasoning for
// job status: a restart's web UI can still show a receiver's last activity
// instead of it reverting to idle until it next serves a request. Called
// once at startup, before the receiver API can serve any request, so it
// never races an actual record call.
func (s *ReceiverStatusStore) SeedLastEvent(id, key string, at time.Time, success bool, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.byID[id]
	if !ok {
		return
	}

	r.LastKey = key
	r.LastSeen = at

	if success {
		r.State = StateOK
		r.Error = ""

		return
	}

	r.State = StateFailed
	r.Error = errText
}

// Snapshot returns every receiver's current status, in id order, each a copy
// safe to serialize without holding s.mu.
func (s *ReceiverStatusStore) Snapshot() []ReceiverSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ReceiverSnapshot, 0, len(s.order))

	for _, id := range s.order {
		out = append(out, *s.byID[id])
	}

	return out
}
