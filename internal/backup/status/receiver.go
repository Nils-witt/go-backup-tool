package status

import (
	"cmp"
	"slices"
	"sort"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// ReceiverSnapshot is one configured receiver's current status, as reported
// over /api/receivers, for display in the web UI dashboard (see webui.go).
// StaleAfter and Stale are filled in by receiver.Snapshots, not
// NewReceiverStore/record below, since they reflect what's actually on disk
// (like LastReceivedAt) rather than live state this process tracks as
// requests come in.
type ReceiverSnapshot struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"` // the receiver's type: "backup" or "directory"
	Path         string    `json:"path"`
	Retention    string    `json:"retention,omitempty"`
	KeepVersions int       `json:"keep_versions,omitempty"` // a directory receiver's keep-versions:
	State        RunState  `json:"state"`                   // idle until the first object is received
	LastKey      string    `json:"last_key,omitempty"`
	LastSeen     time.Time `json:"last_seen"` // zero if nothing has been received yet
	Error        string    `json:"error,omitempty"`
	StaleAfter   string    `json:"stale_after,omitempty"` // set only when this receiver has stale-after: configured
	Stale        bool      `json:"stale,omitempty"`       // true once the most recent file is older than StaleAfter; always false when StaleAfter is unset
}

// ReceiverStore tracks the live state of every configured receiver,
// updated as the receiver API's handler (see receiver.HandleReceiveObject)
// serves incoming requests, and read by the
// web UI's HTTP handlers. Safe for concurrent use, since receiver requests
// can arrive concurrently with each other and with status requests.
type ReceiverStore struct {
	mu    sync.Mutex
	byID  map[string]*ReceiverSnapshot
	order []string // receiver ids, sorted, for stable UI listing
	changeNotifier
}

// NewReceiverStore builds a ReceiverStore with one idle entry
// per entry in receivers, listed in id order (receivers is keyed by id, so
// no other ordering survives buildReceivers).
func NewReceiverStore(receivers map[string]config.ResolvedReceiver) *ReceiverStore {
	ids := make([]string, 0, len(receivers))
	for id := range receivers {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	s := &ReceiverStore{byID: make(map[string]*ReceiverSnapshot, len(receivers)), order: ids, changeNotifier: newChangeNotifier()}

	for _, id := range ids {
		recv := receivers[id]
		s.byID[id] = &ReceiverSnapshot{ID: id, State: StateIdle}
		s.byID[id].setConfig(recv)
	}

	return s
}

// Record updates receiver id's status after it just handled a request for
// key: err nil marks it ok, non-nil marks it failed with err's message,
// mirroring statusStore.targetDone's success/failure recording for job
// targets.
func (s *ReceiverStore) Record(id, key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.notifyLocked()

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
func (s *ReceiverStore) SeedLastEvent(id, key string, at time.Time, success bool, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.notifyLocked()

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

// Changed returns a channel that is closed on the store's next change (see
// changeNotifier).
func (s *ReceiverStore) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.changed
}

// Snapshot returns every receiver's current status, in id order, each a copy
// safe to serialize without holding s.mu.
func (s *ReceiverStore) Snapshot() []ReceiverSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ReceiverSnapshot, 0, len(s.order))

	for _, id := range s.order {
		out = append(out, *s.byID[id])
	}

	return out
}

// Upsert adds recv to the store as an idle entry, or — for an id already
// present — updates its configured Path/Retention while keeping its live
// state, so a receiver created or edited in the web UI shows up on the
// dashboard immediately.
func (s *ReceiverStore) Upsert(recv config.ResolvedReceiver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.notifyLocked()

	if r, ok := s.byID[recv.ID]; ok {
		r.setConfig(recv)

		return
	}

	s.byID[recv.ID] = &ReceiverSnapshot{ID: recv.ID, State: StateIdle}
	s.byID[recv.ID].setConfig(recv)
	s.order = append(s.order, recv.ID)
	sort.Strings(s.order)
}

// setConfig copies recv's configured settings onto r.
func (r *ReceiverSnapshot) setConfig(recv config.ResolvedReceiver) {
	r.Kind = string(cmp.Or(recv.Kind, config.ReceiverKindBackup))
	r.Path = recv.Path
	r.Retention = intervalString(recv.Retention)
	r.KeepVersions = recv.KeepVersions
}

// Remove drops receiver id from the store, a no-op if it isn't present.
func (s *ReceiverStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.notifyLocked()

	if _, ok := s.byID[id]; !ok {
		return
	}

	delete(s.byID, id)
	s.order = slices.DeleteFunc(s.order, func(o string) bool { return o == id })
}
