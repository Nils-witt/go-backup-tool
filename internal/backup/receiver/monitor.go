// Package receiver implements go-backup-tool's receiver API: the HTTP
// endpoints another go-backup-tool instance's type: remote target uploads
// to and deletes from (see HandleReceiveObject/HandleDeleteObject), plus the
// background work that keeps a receiver's state current: seeding its live
// status from persisted history at startup, sweeping expired objects, and
// notifying a configured webhook once a receiver goes stale.
package receiver

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// SeedReceiverStatusFromState initializes statusStore's receivers from each
// one's most recently persisted receiver_events row (see the store
// package's GetLastReceiverEvent), so a restart's web UI can still show a
// receiver's last activity instead of every receiver reverting to idle
// until it next serves a request. Called once at startup, before the
// receiver API's handlers can serve any request.
func SeedReceiverStatusFromState(ctx context.Context, db *store.Store, receivers map[string]config.ResolvedReceiver, statusStore *backup.ReceiverStatusStore, log *slog.Logger) {
	for id := range receivers {
		seedReceiverStatus(ctx, db, id, statusStore, log)
	}
}

// seedReceiverStatus is SeedReceiverStatusFromState for a single receiver
// id, also used when a receiver is created in the web UI (see
// Manager.Create), since an id that existed before may have history.
func seedReceiverStatus(ctx context.Context, db *store.Store, id string, statusStore *backup.ReceiverStatusStore, log *slog.Logger) {
	ev, ok, err := db.GetLastReceiverEvent(ctx, id)
	if err != nil {
		log.Warn("reading last receiver event from state db", "id", id, "err", err)
		return
	}

	if !ok {
		return
	}

	statusStore.SeedLastEvent(id, ev.Key, ev.At, ev.Success, ev.Error)
}

// recordReceiverEventBestEffort appends a receiver_events row for one
// HandleReceiveObject/HandleDeleteObject request, win or lose, so the daily
// report (see pipeline.RunDailyReportLoop) can summarize receiver activity
// later. A nil db (the state db couldn't be opened at startup) is a no-op,
// matching every other state-db write; a write failure is logged rather
// than returned, since it shouldn't affect the response the request already
// committed to.
func recordReceiverEventBestEffort(ctx context.Context, db *store.Store, log *slog.Logger, receiverID, kind, key string, size int64, recvErr error) {
	if db == nil {
		return
	}

	ev := store.ReceiverEvent{At: time.Now(), ReceiverID: receiverID, Kind: kind, Key: key, Size: size, Success: recvErr == nil}
	if recvErr != nil {
		ev.Error = recvErr.Error()
	}

	if err := db.SaveReceiverEvent(ctx, ev); err != nil {
		log.Warn("receiver: recording event failed", "id", receiverID, "err", err)
	}
}

// receiverRetentionSweepInterval is how often MonitorReceiverRetention
// re-sweeps every receiver with retention: set.
const receiverRetentionSweepInterval = time.Minute

// MonitorReceiverRetention periodically sweeps every receiver with
// retention: set for objects now past their retention window (see
// backup.SweepRetentionForTarget), replacing the old approach of sweeping a
// receiver only when it next happened to receive a write: that tied a
// receiver's retention sweep to its incoming traffic (a receiver that
// stopped hearing from its sender would never get swept again) and made
// every write pay for a sweep it usually didn't need. It sweeps once
// immediately, then every receiverRetentionSweepInterval, until ctx is
// done. Receivers are read from the registry on every sweep, so ones added
// or edited in the web UI are picked up. A nil db (retention tracking
// unavailable this run) is a no-op.
func MonitorReceiverRetention(ctx context.Context, db *store.Store, receivers *backup.ReceiverRegistry, log *slog.Logger) {
	if db == nil {
		return
	}

	backup.RunPeriodically(ctx, receiverRetentionSweepInterval, true, func() {
		sweepAllReceivers(ctx, db, receivers.Snapshot(), log)
	})
}

// sweepAllReceivers runs one retention sweep (see backup.SweepRetentionForTarget)
// across every entry in receivers with retention: set, factored out of
// MonitorReceiverRetention's periodic loop so a single sweep pass can be
// exercised directly in tests without also driving RunPeriodically's timing.
func sweepAllReceivers(ctx context.Context, db *store.Store, receivers map[string]config.ResolvedReceiver, log *slog.Logger) {
	for _, recv := range receivers {
		if recv.Retention <= 0 {
			continue
		}

		t := backup.ReceiverTarget(recv)

		log.Debug("receiver retention sweep", "id", recv.ID, "path", recv.Path, "retention", recv.Retention)

		if err := backup.SweepRetentionForTarget(ctx, db, t, log); err != nil {
			log.Warn("receiver retention sweep failed", "id", recv.ID, "err", err)
		}
	}
}

// staleReceiverCheckInterval is how often MonitorStaleReceivers re-checks
// every receiver with stale-after: set.
const staleReceiverCheckInterval = time.Minute

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

// forgetMissing drops the notified state of every receiver no longer in
// current (deleted in the web UI), so one re-created under the same id
// starts with a clean slate.
func (m *staleReceiverMonitor) forgetMissing(current map[string]config.ResolvedReceiver) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id := range m.notified {
		if _, ok := current[id]; !ok {
			delete(m.notified, id)
		}
	}
}

// check evaluates recv's current staleness — nothing received within
// recv.StaleAfter — and fires its webhook exactly once per gap. It never
// fires for a receiver that has never received anything at all: with no
// file on disk there's nothing to be stale, so that's left to whatever
// alerting already watches for a receiver that should have received its
// first file by now. A recv.StaleAfter <= 0 (the monitor disabled for this
// receiver) is also a no-op.
func (m *staleReceiverMonitor) check(recv config.ResolvedReceiver, queue *notify.Queue, log *slog.Logger) {
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

	notifyStaleReceiver(recv, lastSeen, queue, log)
}

// staleReceiverPayload is the default JSON body POSTed to a stale
// notification's webhook:, used unless its body: overrides it (see
// renderStaleWebhookPayload).
type staleReceiverPayload struct {
	ReceiverID   string    `json:"receiver_id"`
	Path         string    `json:"path"`
	StaleAfter   string    `json:"stale_after"`
	LastReceived time.Time `json:"last_received"`
	ServerName   string    `json:"server_name,omitempty"`
}

// defaultStaleWebhookContentType is the Content-Type sent with a stale
// notification's webhook request when it doesn't set a Content-Type among
// webhook.headers:.
const defaultStaleWebhookContentType = "application/json"

// defaultStaleSubject is a stale notification's email.subject default when
// left unset.
const defaultStaleSubject = "[{server_name}] receiver {receiver_id} is stale"

// defaultStaleBody is a stale notification's email.body default when left
// unset.
const defaultStaleBody = "Receiver {receiver_id} ({path}) has not received a file in over {stale_after}. Last received: {last_received}."

// staleWebhookBody builds the request body sent to wh: wh.Body: (see
// renderStaleWebhookPayload), if set, otherwise the default JSON
// staleReceiverPayload.
func staleWebhookBody(wh notify.Webhook, recv config.ResolvedReceiver, lastSeen time.Time) ([]byte, error) {
	if wh.Body != "" {
		return []byte(renderStaleWebhookPayload(wh.Body, recv, lastSeen)), nil
	}

	payload := staleReceiverPayload{
		ReceiverID: recv.ID, Path: recv.Path, StaleAfter: recv.StaleAfter.String(), LastReceived: lastSeen,
		ServerName: recv.ServerName,
	}

	return json.Marshal(payload)
}

// renderStaleWebhookPayload substitutes a stale notification's webhook.body:/
// email.subject:/email.body: template's placeholders with recv's current
// staleness, mirroring how a job's key: substitutes {time}: {receiver_id},
// {path}, and {stale_after} are recv's own fields; {last_received} is
// lastSeen formatted as RFC 3339; {server_name} is the config file's
// top-level server-name: (see config.ResolvedReceiver.ServerName), "" if
// unset. This lets an operator's webhook receiver (Slack, PagerDuty, a
// custom endpoint expecting its own JSON/form shape, ...) or email get a
// body it already understands instead of go-backup-tool's own default
// shape.
func renderStaleWebhookPayload(tmpl string, recv config.ResolvedReceiver, lastSeen time.Time) string {
	replacer := strings.NewReplacer(
		"{receiver_id}", recv.ID,
		"{path}", recv.Path,
		"{stale_after}", recv.StaleAfter.String(),
		"{last_received}", lastSeen.UTC().Format(time.RFC3339),
		"{server_name}", recv.ServerName,
	)

	return replacer.Replace(tmpl)
}

// notifyStaleReceiver sends recv's current staleness to every one of
// recv.StaleNotifications (looked up in recv.Notifications), logging (rather than returning) any failure: a
// notification delivery problem shouldn't affect anything else this process
// is doing, and there's no caller to report it to — MonitorStaleReceivers
// already marked this gap as notified before calling this, so a failed
// delivery isn't retried until the gap clears and reopens.
func notifyStaleReceiver(recv config.ResolvedReceiver, lastSeen time.Time, queue *notify.Queue, log *slog.Logger) {
	for _, n := range recv.Notifications.Resolve(recv.StaleNotifications, log, "receiver", recv.ID) {
		if n.Webhook != nil {
			notifyStaleReceiverWebhook(recv, *n.Webhook, lastSeen, log)
		}

		if n.Email != nil {
			notifyStaleReceiverEmail(recv, *n.Email, lastSeen, queue, log)
		}
	}
}

// notifyStaleReceiverWebhook POSTs recv's current staleness to wh.
func notifyStaleReceiverWebhook(recv config.ResolvedReceiver, wh notify.Webhook, lastSeen time.Time, log *slog.Logger) {
	body, err := staleWebhookBody(wh, recv, lastSeen)
	if err != nil {
		log.Warn("stale receiver webhook: encoding payload failed", "id", recv.ID, "err", err)
		return
	}

	resp, err := notify.PostWebhook(context.Background(), wh, body, defaultStaleWebhookContentType)
	if err != nil {
		log.Warn("stale receiver webhook: request failed", "id", recv.ID, "webhook", wh.URL, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("stale receiver webhook: non-2xx response", "id", recv.ID, "webhook", wh.URL, "status", resp.StatusCode)
		return
	}

	log.Info("stale receiver webhook fired", "id", recv.ID, "webhook", wh.URL, "stale_after", recv.StaleAfter, "last_received", lastSeen)
}

// notifyStaleReceiverEmail emails recv's current staleness via email. queue,
// if non-nil, retries delivery later on failure instead of losing it.
func notifyStaleReceiverEmail(recv config.ResolvedReceiver, email notify.Email, lastSeen time.Time, queue *notify.Queue, log *slog.Logger) {
	subjectTmpl := email.Subject
	if subjectTmpl == "" {
		subjectTmpl = defaultStaleSubject
	}

	bodyTmpl := email.Body
	if bodyTmpl == "" {
		bodyTmpl = defaultStaleBody
	}

	subject := renderStaleWebhookPayload(subjectTmpl, recv, lastSeen)
	body := renderStaleWebhookPayload(bodyTmpl, recv, lastSeen)

	ctx, cancel := context.WithTimeout(context.Background(), notify.Timeout)
	defer cancel()

	if err := notify.SendMailQueued(ctx, queue, email.SMTP, email.From, email.To, subject, body, email.Encrypt); err != nil {
		log.Warn("stale receiver email: sending failed, queued for retry", "id", recv.ID, "to", email.To, "err", err)
		return
	}

	log.Info("stale receiver email sent", "id", recv.ID, "to", email.To, "stale_after", recv.StaleAfter, "last_received", lastSeen)
}

// MonitorStaleReceivers periodically checks every receiver with stale-after:
// set, notifying its stale-notifications: whenever the most recent file
// under its path (see backup.LastReceivedAt) is older than stale-after. A
// receiver that has never received anything at all never fires — there's no
// file to be stale — so this only alerts on a sender that stopped showing
// up, not one that never started. It checks once immediately, then every
// staleReceiverCheckInterval, until ctx is done; a receiver's notifications
// fire once per gap (see staleReceiverMonitor), not on every check, so a
// sender that stays down doesn't spam them indefinitely. Receivers are read
// from the registry on every check, so ones added, edited, or deleted in
// the web UI are picked up.
func MonitorStaleReceivers(ctx context.Context, receivers *backup.ReceiverRegistry, queue *notify.Queue, log *slog.Logger) {
	monitor := newStaleReceiverMonitor()

	checkAll := func() {
		current := receivers.Snapshot()
		monitor.forgetMissing(current)

		for _, recv := range current {
			monitor.check(recv, queue, log)
		}
	}

	backup.RunPeriodically(ctx, staleReceiverCheckInterval, true, checkAll)
}
