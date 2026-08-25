package backup

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// fileReceiver is one top-level receivers: entry, defining a path this
// instance accepts incoming objects into over its receiver API (see
// webui.go's handleReceiveObject/handleDeleteObject), for pairing with
// another go-backup-tool instance's type: remote target. Only reachable
// when listen: is set, since the receiver API is served by the same HTTP
// server as the web UI dashboard.
type fileReceiver struct {
	ID        string `yaml:"id"`
	Token     string `yaml:"token"`     // bearer token a sender must present; matches the sender's servers: entry token
	Path      string `yaml:"path"`      // root directory incoming objects for this id are written under
	Retention string `yaml:"retention"` // optional, same syntax as a local server's retention: e.g. "30d"
}

// resolvedReceiver is one fileReceiver after validation, ready to be used by
// the receiver API's handlers.
type resolvedReceiver struct {
	id        string
	token     string
	path      string
	retention time.Duration
}

// buildReceivers validates fileReceivers and builds an id -> resolvedReceiver
// map, requiring every entry to have a unique, non-empty id, a non-empty
// token, and a non-empty path.
func buildReceivers(fileReceivers []fileReceiver) (map[string]resolvedReceiver, error) {
	receivers := make(map[string]resolvedReceiver, len(fileReceivers))

	for i, fr := range fileReceivers {
		id := strings.TrimSpace(fr.ID)
		if id == "" {
			return nil, fmt.Errorf("receivers[%d]: id is required", i)
		}

		if _, exists := receivers[id]; exists {
			return nil, fmt.Errorf("receivers[%d]: duplicate receiver id %q", i, id)
		}

		if strings.TrimSpace(fr.Token) == "" {
			return nil, fmt.Errorf("receiver %q: token is required", id)
		}

		if strings.TrimSpace(fr.Path) == "" {
			return nil, fmt.Errorf("receiver %q: path is required", id)
		}

		retention, err := parseRetention(fr.Retention)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		receivers[id] = resolvedReceiver{id: id, token: fr.Token, path: fr.Path, retention: retention}
	}

	return receivers, nil
}

// sanitizeObjectKey validates key (the {key...} wildcard segment of a
// receiver API request path) before it's joined into a filesystem path.
// Although the request is already authenticated, key still comes from the
// network and must not be trusted to stay within its receiver's root: this
// rejects an empty/absolute key or one containing a "." or ".." path
// segment.
func sanitizeObjectKey(key string) (string, error) {
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

// receiverTarget builds a synthetic local target for recv, so the receiver
// API's handlers can reuse writeLocalObject/recordLocalWrite/
// deleteLocalObject/removeRetentionRecord (see pipeline.go and retention.go)
// exactly as a type: local target does — a receiver is a local target
// written to over HTTP with token auth, not a distinct storage
// implementation.
func receiverTarget(recv resolvedReceiver) *target {
	return &target{
		serverName: "receiver:" + recv.id,
		kind:       serverKindLocal,
		localPath:  recv.path,
		retention:  recv.retention,
	}
}

// receiverSnapshot is one configured receiver's current status, as reported
// over /api/receivers, for display in the web UI dashboard (see webui.go).
type receiverSnapshot struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Retention string    `json:"retention,omitempty"`
	State     runState  `json:"state"` // idle until the first object is received
	LastKey   string    `json:"last_key,omitempty"`
	LastSeen  time.Time `json:"last_seen"` // zero if nothing has been received yet
	Error     string    `json:"error,omitempty"`
}

// receiverStatusStore tracks the live state of every configured receiver,
// updated as the receiver API's handlers (see handleReceiveObject/
// handleDeleteObject in webui.go) serve incoming requests, and read by the
// web UI's HTTP handlers. Safe for concurrent use, since receiver requests
// can arrive concurrently with each other and with status requests.
type receiverStatusStore struct {
	mu    sync.Mutex
	byID  map[string]*receiverSnapshot
	order []string // receiver ids, sorted, for stable UI listing
}

// newReceiverStatusStore builds a receiverStatusStore with one idle entry
// per entry in receivers, listed in id order (receivers is keyed by id, so
// no other ordering survives buildReceivers).
func newReceiverStatusStore(receivers map[string]resolvedReceiver) *receiverStatusStore {
	ids := make([]string, 0, len(receivers))
	for id := range receivers {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	s := &receiverStatusStore{byID: make(map[string]*receiverSnapshot, len(receivers)), order: ids}

	for _, id := range ids {
		recv := receivers[id]
		s.byID[id] = &receiverSnapshot{ID: id, Path: recv.path, Retention: intervalString(recv.retention), State: stateIdle}
	}

	return s
}

// record updates receiver id's status after it just handled a request for
// key: err nil marks it ok, non-nil marks it failed with err's message,
// mirroring statusStore.targetDone's success/failure recording for job
// targets.
func (s *receiverStatusStore) record(id, key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.byID[id]
	if !ok {
		return
	}

	r.LastKey = key
	r.LastSeen = time.Now()

	if err != nil {
		r.State = stateFailed
		r.Error = err.Error()

		return
	}

	r.State = stateOK
	r.Error = ""
}

// snapshot returns every receiver's current status, in id order, each a copy
// safe to serialize without holding s.mu.
func (s *receiverStatusStore) snapshot() []receiverSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]receiverSnapshot, 0, len(s.order))

	for _, id := range s.order {
		out = append(out, *s.byID[id])
	}

	return out
}

// sweepStartupReceiverRetention runs one retention sweep, before the web UI
// starts serving, for every receiver with retention: set — mirroring
// sweepStartupRetention's reasoning for local server targets: without this,
// a receiver would only get swept whenever it next happens to receive a
// write, potentially long after files there actually expired. A nil db
// (retention tracking unavailable this run) is a no-op.
func sweepStartupReceiverRetention(ctx context.Context, db *sql.DB, receivers map[string]resolvedReceiver, log *slog.Logger) {
	if db == nil {
		return
	}

	for _, recv := range receivers {
		if recv.retention <= 0 {
			continue
		}

		t := receiverTarget(recv)

		log.Debug("startup receiver retention sweep", "id", recv.id, "path", recv.path, "retention", recv.retention)

		if err := sweepRetentionForTarget(ctx, db, t, log); err != nil {
			log.Warn("startup receiver retention sweep failed", "id", recv.id, "err", err)
		}
	}
}
