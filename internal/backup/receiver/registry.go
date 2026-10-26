package receiver

import (
	"maps"
	"sync"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
)

// Registry holds every receiver this instance currently accepts
// objects into, keyed by id. It's shared by the receiver API, the
// dashboard, the stale/retention monitors, and the report, and changes at
// runtime as receivers are created, edited, or deleted in the web UI (see
// receiver.Manager), so every reader looks receivers up here per use rather
// than holding on to its own copy. Safe for concurrent use.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]config.ResolvedReceiver

	// usedTokens are the request tokens the receiver API has accepted,
	// shared across receivers (see remoteauth.ReplayCache).
	usedTokens remoteauth.ReplayCache
}

// NewRegistry builds a registry holding receivers (copied).
func NewRegistry(receivers map[string]config.ResolvedReceiver) *Registry {
	return &Registry{byID: maps.Clone(receivers)}
}

// Get returns receiver id, reporting false if there is none. A nil
// registry holds no receivers.
func (r *Registry) Get(id string) (config.ResolvedReceiver, bool) {
	if r == nil {
		return config.ResolvedReceiver{}, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	recv, ok := r.byID[id]

	return recv, ok
}

// Snapshot returns a copy of every receiver, keyed by id.
func (r *Registry) Snapshot() map[string]config.ResolvedReceiver {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return maps.Clone(r.byID)
}

// Put adds recv, replacing any existing receiver with the same id.
func (r *Registry) Put(recv config.ResolvedReceiver) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.byID == nil {
		r.byID = make(map[string]config.ResolvedReceiver)
	}

	r.byID[recv.ID] = recv
}

// Delete removes receiver id, a no-op if there is none.
func (r *Registry) Delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.byID, id)
}
