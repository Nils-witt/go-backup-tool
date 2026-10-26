package status

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"
)

// peerMinOfflineAfter is the shortest silence after which a pushing
// instance is shown offline, however short its push interval: a brief
// network hiccup or a restart of either side shouldn't flag it.
const peerMinOfflineAfter = 2 * time.Minute

// peerMissedPushes is how many of an instance's push intervals may pass
// without a push before it's shown offline.
const peerMissedPushes = 3

// PeerReport is the status another instance last pushed to this one (see
// the peerstatus package): its jobs and receivers as it serves them on its
// own /api/status and /api/receivers.
type PeerReport struct {
	// ID is the pushing instance's server UUID: the trusted server it
	// authenticated as.
	ID string

	// InstanceName is the pushing instance's own server-name:, "" if unset.
	InstanceName string

	// Interval is how often the instance said it pushes at the latest, which
	// sets when it's considered offline (see PeerSnapshot.Offline).
	Interval time.Duration

	// ReceivedAt is when this instance received the push.
	ReceivedAt time.Time

	Jobs      []JobSnapshot
	Receivers []ReceiverSnapshot
}

// PeerSnapshot is one pushing instance's status, as reported over
// /api/peers and /api/live.
type PeerSnapshot struct {
	ID string `json:"id"`

	// Name is the trusted server's name on this instance.
	Name         string    `json:"name"`
	InstanceName string    `json:"instance_name,omitempty"`
	LastSeen     time.Time `json:"last_seen"`
	Interval     string    `json:"interval"`

	// Offline is true once no push arrived for peerMissedPushes intervals
	// (at least peerMinOfflineAfter): Jobs and Receivers are then only the
	// last status it pushed.
	Offline bool `json:"offline,omitempty"`

	Jobs      []JobSnapshot      `json:"jobs"`
	Receivers []ReceiverSnapshot `json:"receivers"`
}

// PeerStore holds the last status every other instance pushed to this one,
// keyed by its server UUID, updated by the status push endpoint (see the
// peerstatus package) and read by the web UI. Safe for concurrent use.
type PeerStore struct {
	mu   sync.Mutex
	byID map[string]PeerReport
	changeNotifier
}

// NewPeerStore returns an empty PeerStore.
func NewPeerStore() *PeerStore {
	return &PeerStore{byID: make(map[string]PeerReport), changeNotifier: newChangeNotifier()}
}

// Upsert records r as instance r.ID's current status, replacing what it
// pushed before.
func (s *PeerStore) Upsert(r PeerReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.notifyLocked()

	r.Jobs = slices.Clone(r.Jobs)
	r.Receivers = slices.Clone(r.Receivers)
	s.byID[r.ID] = r
}

// Remove forgets instance id, reporting whether it was present.
func (s *PeerStore) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.byID[id]; !ok {
		return false
	}

	delete(s.byID, id)
	s.notifyLocked()

	return true
}

// Changed returns a channel that is closed on the store's next change (see
// changeNotifier).
func (s *PeerStore) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.changed
}

// Snapshot returns every instance's status as of now, sorted by name, then
// id. trusted names each instance's trusted server: one it reports false
// for is no longer trusted and is left out, even before its stored status
// is deleted. A nil store holds no instances.
func (s *PeerStore) Snapshot(now time.Time, trusted func(id string) (name string, ok bool)) []PeerSnapshot {
	if s == nil {
		return []PeerSnapshot{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]PeerSnapshot, 0, len(s.byID))

	for _, id := range slices.Sorted(maps.Keys(s.byID)) {
		name, ok := trusted(id)
		if !ok {
			continue
		}

		r := s.byID[id]
		out = append(out, PeerSnapshot{
			ID:           id,
			Name:         name,
			InstanceName: r.InstanceName,
			LastSeen:     r.ReceivedAt,
			Interval:     intervalString(r.Interval),
			Offline:      now.Sub(r.ReceivedAt) > peerOfflineAfter(r.Interval),
			Jobs:         cloneJobs(r.Jobs),
			Receivers:    slices.Clone(r.Receivers),
		})
	}

	slices.SortStableFunc(out, func(a, b PeerSnapshot) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

// peerOfflineAfter is how long an instance pushing every interval may stay
// silent before it's shown offline.
func peerOfflineAfter(interval time.Duration) time.Duration {
	return max(peerMissedPushes*interval, peerMinOfflineAfter)
}

// cloneJobs deep-copies jobs, so a snapshot can be serialized without
// holding the store's lock.
func cloneJobs(jobs []JobSnapshot) []JobSnapshot {
	out := slices.Clone(jobs)
	for i := range out {
		out[i].Targets = slices.Clone(out[i].Targets)
		out[i].Files = slices.Clone(out[i].Files)
	}

	return out
}
