// Package receiver implements go-backup-tool's receiver API: the HTTP
// endpoints another go-backup-tool instance's type: remote target uploads
// to and deletes from (see HandleReceiveObject/HandleDeleteObject), plus the
// background work that keeps a receiver's state current: seeding its live
// status from persisted history at startup, sweeping expired objects, and
// notifying a configured webhook once a receiver goes stale.
package receiver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
)

// SeedReceiverStatusFromState initializes store's receivers from each one's
// most recently persisted receiver_events row (see
// backup.ReadLastReceiverEvent), so a restart's web UI can still show a
// receiver's last activity instead of every receiver reverting to idle
// until it next serves a request. Called once at startup, before the
// receiver API's handlers can serve any request.
func SeedReceiverStatusFromState(ctx context.Context, db *sql.DB, receivers map[string]backup.ResolvedReceiver, store *backup.ReceiverStatusStore, log *slog.Logger) {
	for id := range receivers {
		ev, ok, err := backup.ReadLastReceiverEvent(ctx, db, id)
		if err != nil {
			log.Warn("reading last receiver event from state db", "id", id, "err", err)
			continue
		}

		if !ok {
			continue
		}

		store.SeedLastEvent(id, ev)
	}
}

// recordReceiverEventBestEffort appends a receiver_events row for one
// HandleReceiveObject/HandleDeleteObject request, win or lose, so the daily
// report (see pipeline.RunDailyReportLoop) can summarize receiver activity
// later. A nil db (the state db couldn't be opened at startup) is a no-op,
// matching every other state-db write; a write failure is logged rather
// than returned, since it shouldn't affect the response the request already
// committed to.
func recordReceiverEventBestEffort(ctx context.Context, db *sql.DB, log *slog.Logger, receiverID, kind, key string, size int64, recvErr error) {
	if db == nil {
		return
	}

	ev := backup.ReceiverEvent{At: time.Now(), ReceiverID: receiverID, Kind: kind, Key: key, Size: size, Success: recvErr == nil}
	if recvErr != nil {
		ev.Error = recvErr.Error()
	}

	if err := backup.RecordReceiverEvent(ctx, db, ev); err != nil {
		log.Warn("receiver: recording event failed", "id", receiverID, "err", err)
	}
}

// SweepStartupReceiverRetention runs one retention sweep, before the web UI
// starts serving, for every receiver with retention: set — mirroring
// pipeline.SweepStartupRetention's reasoning for local server targets:
// without this, a receiver would only get swept whenever it next happens to
// receive a write, potentially long after files there actually expired. A
// nil db (retention tracking unavailable this run) is a no-op.
func SweepStartupReceiverRetention(ctx context.Context, db *sql.DB, receivers map[string]backup.ResolvedReceiver, log *slog.Logger) {
	if db == nil {
		return
	}

	for _, recv := range receivers {
		if recv.Retention <= 0 {
			continue
		}

		t := backup.ReceiverTarget(recv)

		log.Debug("startup receiver retention sweep", "id", recv.ID, "path", recv.Path, "retention", recv.Retention)

		if err := backup.SweepRetentionForTarget(ctx, db, t, log); err != nil {
			log.Warn("startup receiver retention sweep failed", "id", recv.ID, "err", err)
		}
	}
}

// staleReceiverCheckInterval is how often MonitorStaleReceivers re-checks
// every receiver with stale-after: set.
const staleReceiverCheckInterval = time.Minute

// staleWebhookTimeout bounds a single stale-receiver webhook POST, since
// notifyStaleReceiverWebhook runs on its own background schedule rather than
// under a run's -timeout.
const staleWebhookTimeout = 10 * time.Second

// staleReceiverMonitor tracks, per receiver id, whether its stale webhook has
// already fired for the receiver's current gap in incoming files, so
// MonitorStaleReceivers fires it once per gap instead of on every check —
// the gap clears (and the webhook can fire again) once a fresh file arrives
// and brings the receiver back under its stale-after: threshold.
type staleReceiverMonitor struct {
	mu       sync.Mutex
	notified map[string]bool
}

// newStaleReceiverMonitor returns a staleReceiverMonitor with no receiver yet
// marked as notified.
func newStaleReceiverMonitor() *staleReceiverMonitor {
	return &staleReceiverMonitor{notified: make(map[string]bool)}
}

// check evaluates recv's current staleness — nothing received within
// recv.StaleAfter — and fires its webhook exactly once per gap. It never
// fires for a receiver that has never received anything at all: with no
// file on disk there's nothing to be stale, so that's left to whatever
// alerting already watches for a receiver that should have received its
// first file by now. A recv.StaleAfter <= 0 (the monitor disabled for this
// receiver) is also a no-op.
func (m *staleReceiverMonitor) check(recv backup.ResolvedReceiver, log *slog.Logger) {
	if recv.StaleAfter <= 0 {
		return
	}

	lastSeen, ok, err := backup.LastReceivedAt(recv)
	if err != nil {
		log.Warn("stale receiver check: listing files failed", "id", recv.ID, "err", err)
		return
	}

	stale := ok && time.Since(lastSeen) > recv.StaleAfter

	m.mu.Lock()
	alreadyNotified := m.notified[recv.ID]
	m.notified[recv.ID] = stale
	m.mu.Unlock()

	if !stale || alreadyNotified {
		return
	}

	notifyStaleReceiverWebhook(recv, lastSeen, log)
}

// staleReceiverPayload is the default JSON body POSTed to a receiver's
// webhook:, used unless the receiver's webhook-payload: overrides it (see
// renderStaleWebhookPayload).
type staleReceiverPayload struct {
	ReceiverID   string    `json:"receiver_id"`
	Path         string    `json:"path"`
	StaleAfter   string    `json:"stale_after"`
	LastReceived time.Time `json:"last_received"`
}

// defaultStaleWebhookContentType is the Content-Type sent with a stale
// receiver webhook request whose receiver doesn't set a Content-Type among
// webhook.headers:.
const defaultStaleWebhookContentType = "application/json"

// staleWebhookBody builds the request body sent to recv.Webhook: recv's
// webhook.body: template (see renderStaleWebhookPayload), if set, otherwise
// the default JSON staleReceiverPayload.
func staleWebhookBody(recv backup.ResolvedReceiver, lastSeen time.Time) ([]byte, error) {
	if recv.Webhook.Body != "" {
		return []byte(renderStaleWebhookPayload(recv.Webhook.Body, recv, lastSeen)), nil
	}

	payload := staleReceiverPayload{
		ReceiverID: recv.ID, Path: recv.Path, StaleAfter: recv.StaleAfter.String(), LastReceived: lastSeen,
	}

	return json.Marshal(payload)
}

// renderStaleWebhookPayload substitutes a receiver's webhook.body: template's
// placeholders with recv's current staleness, mirroring how a job's key:
// substitutes {time}: {receiver_id}, {path}, and {stale_after} are recv's
// own fields; {last_received} is lastSeen formatted as RFC 3339. This lets
// an operator's webhook receiver (Slack, PagerDuty, a custom endpoint
// expecting its own JSON/form shape, ...) get a body it already understands
// instead of go-backup-tool's own default shape.
func renderStaleWebhookPayload(tmpl string, recv backup.ResolvedReceiver, lastSeen time.Time) string {
	replacer := strings.NewReplacer(
		"{receiver_id}", recv.ID,
		"{path}", recv.Path,
		"{stale_after}", recv.StaleAfter.String(),
		"{last_received}", lastSeen.UTC().Format(time.RFC3339),
	)

	return replacer.Replace(tmpl)
}

// notifyStaleReceiverWebhook sends recv's current staleness to recv.Webhook,
// logging (rather than returning) any failure: a webhook delivery problem
// shouldn't affect anything else this process is doing, and there's no
// caller to report it to — MonitorStaleReceivers already marked this gap as
// notified before calling this, so a failed delivery isn't retried until the
// gap clears and reopens.
func notifyStaleReceiverWebhook(recv backup.ResolvedReceiver, lastSeen time.Time, log *slog.Logger) {
	body, err := staleWebhookBody(recv, lastSeen)
	if err != nil {
		log.Warn("stale receiver webhook: encoding payload failed", "id", recv.ID, "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), staleWebhookTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, recv.Webhook.Method, recv.Webhook.URL, bytes.NewReader(body))
	if err != nil {
		log.Warn("stale receiver webhook: building request failed", "id", recv.ID, "err", err)
		return
	}

	for k, v := range recv.Webhook.Headers {
		req.Header.Set(k, v)
	}

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", defaultStaleWebhookContentType)
	}

	resp, err := staleWebhookHTTPClient.Do(req)
	if err != nil {
		log.Warn("stale receiver webhook: request failed", "id", recv.ID, "webhook", recv.Webhook.URL, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("stale receiver webhook: non-2xx response", "id", recv.ID, "webhook", recv.Webhook.URL, "status", resp.StatusCode)
		return
	}

	log.Info("stale receiver webhook fired", "id", recv.ID, "webhook", recv.Webhook.URL, "stale_after", recv.StaleAfter, "last_received", lastSeen)
}

// staleWebhookHTTPClient is shared by every stale-receiver webhook POST;
// staleWebhookTimeout bounds each request since these run on a background
// schedule rather than under a run's -timeout.
var staleWebhookHTTPClient = &http.Client{Timeout: staleWebhookTimeout}

// MonitorStaleReceivers periodically checks every receiver with stale-after:
// set, POSTing to its webhook: whenever the most recent file under its path
// (see backup.LastReceivedAt) is older than stale-after. A receiver that has
// never received anything at all never fires — there's no file to be stale
// — so this only alerts on a sender that stopped showing up, not one that
// never started. It checks once immediately, then every
// staleReceiverCheckInterval, until ctx is done; a receiver's webhook fires
// once per gap (see staleReceiverMonitor), not on every check, so a sender
// that stays down doesn't spam the webhook indefinitely. A no-op if no
// receiver has stale-after: set.
func MonitorStaleReceivers(ctx context.Context, receivers map[string]backup.ResolvedReceiver, log *slog.Logger) {
	if !anyReceiverHasStaleAfter(receivers) {
		return
	}

	monitor := newStaleReceiverMonitor()

	checkAll := func() {
		for _, recv := range receivers {
			monitor.check(recv, log)
		}
	}

	checkAll()

	ticker := time.NewTicker(staleReceiverCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkAll()
		}
	}
}

// anyReceiverHasStaleAfter reports whether any entry in receivers has
// stale-after: set, i.e. whether MonitorStaleReceivers has anything to do.
func anyReceiverHasStaleAfter(receivers map[string]backup.ResolvedReceiver) bool {
	for _, recv := range receivers {
		if recv.StaleAfter > 0 {
			return true
		}
	}

	return false
}
