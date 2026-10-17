package webui

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

// handleReceiverStatus serves store's current receiver statuses as JSON,
// annotated with each receiver's live staleness (see
// annotateReceiverStaleness) for any entry in receivers with stale-after:
// set.
func handleReceiverStatus(receivers *receiver.Registry, store *status.ReceiverStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, receiverSnapshots(receivers, store, log))
	}
}

// receiverSnapshots returns store's current receiver statuses, each
// annotated with its live staleness (see annotateReceiverStaleness). Shared
// by handleReceiverStatus and the live status WebSocket (see handleLive).
func receiverSnapshots(receivers *receiver.Registry, store *status.ReceiverStore, log *slog.Logger) []status.ReceiverSnapshot {
	snapshots := store.Snapshot()

	for i := range snapshots {
		recv, _ := receivers.Get(snapshots[i].ID)
		annotateReceiverStaleness(&snapshots[i], recv, log)
	}

	return snapshots
}

// annotateReceiverStaleness fills snap's StaleAfter/Stale fields from recv's
// current state on disk, for a receiver with stale-after: set; a no-op
// otherwise. Stale mirrors staleReceiverMonitor.check's own condition — at
// least one file received, and the most recent one older than
// recv.StaleAfter — so the dashboard never disagrees with what actually
// fires the webhook. A lastReceivedAt failure is logged and leaves Stale
// false rather than failing the whole /api/receivers response over one
// receiver's directory listing.
func annotateReceiverStaleness(snap *status.ReceiverSnapshot, recv config.ResolvedReceiver, log *slog.Logger) {
	if recv.StaleAfter <= 0 {
		return
	}

	snap.StaleAfter = recv.StaleAfter.String()

	lastSeen, ok, err := lastReceived.get(recv, snap.LastSeen)
	if err != nil {
		log.Warn("receiver: checking staleness failed", "id", recv.ID, "err", err)
		return
	}

	snap.Stale = ok && time.Since(lastSeen) > recv.StaleAfter
}

// lastReceivedTTL is how long lastReceived reuses one objectstore.LastReceivedAt
// walk of a receiver's directory. Staleness is measured against stale-after
// windows of minutes to days, so a few seconds' lag is invisible, while
// without it every live-status client walked every stale-after receiver's
// directory on every push and ping.
const lastReceivedTTL = 15 * time.Second

// lastReceived is the process-wide lastReceivedCache annotateReceiverStaleness
// reads through.
var lastReceived = &lastReceivedCache{byPath: map[string]lastReceivedEntry{}}

// lastReceivedEntry is one cached objectstore.LastReceivedAt result.
type lastReceivedEntry struct {
	at       time.Time
	ok       bool
	seen     time.Time // the receiver's LastSeen when this was computed
	computed time.Time
}

// lastReceivedCache memoizes objectstore.LastReceivedAt per receiver path for
// lastReceivedTTL. An entry is also invalidated as soon as the receiver's
// in-memory LastSeen moves on, so a fresh receive clears a stale flag on the
// very next push. Safe for concurrent use.
type lastReceivedCache struct {
	mu     sync.Mutex
	byPath map[string]lastReceivedEntry
}

// get returns recv's last-received time, from cache when that is younger
// than lastReceivedTTL and was computed at the same lastSeen.
func (c *lastReceivedCache) get(recv config.ResolvedReceiver, lastSeen time.Time) (time.Time, bool, error) {
	now := time.Now()

	c.mu.Lock()
	e, hit := c.byPath[recv.Path]
	c.mu.Unlock()

	if hit && e.seen.Equal(lastSeen) && now.Sub(e.computed) < lastReceivedTTL {
		return e.at, e.ok, nil
	}

	at, ok, err := objectstore.LastReceivedAt(recv)
	if err != nil {
		return time.Time{}, false, err
	}

	c.mu.Lock()
	c.byPath[recv.Path] = lastReceivedEntry{at: at, ok: ok, seen: lastSeen, computed: now}
	c.mu.Unlock()

	return at, ok, nil
}

// handleReceiverFiles serves GET /api/receivers/{id}/files: the objects
// currently stored under receiver {id}'s path (see listReceiverFiles), for
// the web UI dashboard's per-receiver file listing. Unlike the receiver API
// (receiver.HandleReceiveObject), this is dashboard-only and isn't
// JWT-authenticated, matching /api/receivers.
func handleReceiverFiles(receivers *receiver.Registry, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := lookupReceiver(w, r, receivers)
		if !ok {
			return
		}

		files, err := objectstore.ListReceiverFiles(recv)
		if err != nil {
			log.Warn("receiver: listing files failed", "id", recv.ID, "err", err)
			http.Error(w, "listing files failed", http.StatusInternalServerError)

			return
		}

		writeJSON(w, files)
	}
}

// lookupReceiver returns the receiver named by r's {id} path value, writing
// a 404 and reporting ok as false if it's unknown. Shared by
// handleReceiverFiles and handleDownloadFile.
func lookupReceiver(w http.ResponseWriter, r *http.Request, receivers *receiver.Registry) (config.ResolvedReceiver, bool) {
	recv, ok := receivers.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
	}

	return recv, ok
}
