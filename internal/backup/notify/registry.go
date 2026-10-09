package notify

import (
	"log/slog"
	"maps"
	"slices"
	"sync"
)

// Registry holds every notification currently defined, keyed by id. Jobs'
// failure-notifications, receivers' stale/download notifications, and the
// report all reference notifications by id and look them up here each time
// they fire (see Resolve), so a notification created, edited, or deleted in
// the web UI takes effect without a restart. Safe for concurrent use; a nil
// *Registry holds no notifications.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Notification
}

// NewRegistry builds a registry holding notifications (copied).
func NewRegistry(notifications map[string]Notification) *Registry {
	return &Registry{byID: maps.Clone(notifications)}
}

// Get returns notification id, reporting false if there is none.
func (r *Registry) Get(id string) (Notification, bool) {
	if r == nil {
		return Notification{}, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	n, ok := r.byID[id]

	return n, ok
}

// IDs returns every notification id, sorted.
func (r *Registry) IDs() []string {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return slices.Sorted(maps.Keys(r.byID))
}

// Put adds n, replacing any existing notification with the same id.
func (r *Registry) Put(n Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.byID == nil {
		r.byID = make(map[string]Notification)
	}

	r.byID[n.ID] = n
}

// Delete removes notification id, a no-op if there is none.
func (r *Registry) Delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.byID, id)
}

// Resolve looks up every id in ids, in order, for a trigger about to fire.
// An id with no notification (deleted, or invalid and so inactive) is
// logged with attrs and skipped rather than failing the whole trigger: the
// remaining notifications still fire.
func (r *Registry) Resolve(ids []string, log *slog.Logger, attrs ...any) []Notification {
	if len(ids) == 0 {
		return nil
	}

	out := make([]Notification, 0, len(ids))

	for _, id := range ids {
		n, ok := r.Get(id)
		if !ok {
			log.Warn("notification not found, skipping it", append([]any{"notification", id}, attrs...)...)
			continue
		}

		out = append(out, n)
	}

	return out
}
