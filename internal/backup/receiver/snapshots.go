package receiver

import (
	"log/slog"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

// Snapshots returns store's current receiver statuses, each annotated with
// its live staleness (see annotateReceiverStaleness). Shared by the web
// UI's /api/receivers and /api/live, and the status push to a central
// instance (see the peerstatus package), so all of them agree.
func Snapshots(receivers *Registry, store *status.ReceiverStore, log *slog.Logger) []status.ReceiverSnapshot {
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
