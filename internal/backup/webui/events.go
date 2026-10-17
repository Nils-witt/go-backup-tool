package webui

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// recordLogin appends one dashboard login attempt to db's login log (see
// db.SaveLoginEvent) with method/username/detail/success, warning via
// log (tagged with source, e.g. "web UI" or "oidc") rather than failing the
// caller's request if the write itself fails — a login must never be
// blocked by an audit-log hiccup. A nil db is a no-op, matching
// StartWebUI's optional db. Used by auth.go for SSO logins (see
// handleSSOLogin) and rejected bearer tokens (see rejectBearer).
func recordLogin(ctx context.Context, db *store.Store, log *slog.Logger, r *http.Request, trustProxyHeaders bool, method, source, username, detail string, success bool) {
	if db == nil {
		return
	}

	ev := store.LoginEvent{At: time.Now(), Username: username, Method: method, Success: success, RemoteAddr: clientAddr(r, trustProxyHeaders), Detail: detail}
	if err := db.SaveLoginEvent(ctx, ev); err != nil {
		log.Warn(source+": recording login event failed", "err", err)
	}
}

// loginEventJSON is loginEvent's wire shape for handleLoginEvents, matching
// the dashboard's own field naming (snake_case, as every other /api/...
// endpoint here uses).
type loginEventJSON struct {
	At         time.Time `json:"at"`
	Username   string    `json:"username"`
	Method     string    `json:"method"`
	Success    bool      `json:"success"`
	RemoteAddr string    `json:"remote_addr"`
	Detail     string    `json:"detail"`
}

// handleLoginEvents serves GET /api/login-events: the most recently recorded
// dashboard login attempts (see recordLoginEvent), newest first, as JSON.
// db nil (state tracking unavailable) serves an empty list rather than
// failing the request, matching handleReceiverStatus's own tolerance for a
// missing dependency.
func handleLoginEvents(db *store.Store, log *slog.Logger, limit int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, limit, db.ListLoginEvents,
			func(ev store.LoginEvent) loginEventJSON { return loginEventJSON(ev) },
			"reading login events failed")
	}
}

// serveEventLog writes a JSON array of the most recently recorded audit-log
// events (see store.Store.ListLoginEvents/ListDownloadEvents), newest first,
// converting each raw event E to its wire shape J via toJSON. read is a
// store method value (e.g. db.ListLoginEvents) so this doesn't need its own
// *store.Store parameter to call it with. db nil (state tracking
// unavailable) serves an empty list rather than failing the request, since
// the state db is optional (see StartWebUI). Shared by handleLoginEvents
// and handleDownloadEvents, whose bodies would otherwise be identical but
// for the event/read/limit types involved.
func serveEventLog[E, J any](w http.ResponseWriter, r *http.Request, log *slog.Logger, db *store.Store, limit int, read func(context.Context, int) ([]E, error), toJSON func(E) J, errMsg string) {
	// A limit below 1 (a caller that didn't resolve webui.event-log-limit:,
	// e.g. a test) falls back to the default rather than serving nothing.
	if limit < 1 {
		limit = config.DefaultEventLogLimit
	}

	var events []E

	if db != nil {
		var err error

		events, err = read(r.Context(), limit)
		if err != nil {
			log.Warn("web UI: "+errMsg, "err", err)
			http.Error(w, errMsg, http.StatusInternalServerError)

			return
		}
	}

	out := make([]J, len(events))
	for i, ev := range events {
		out[i] = toJSON(ev)
	}

	writeJSON(w, out)
}

// downloadEventJSON is downloadEvent's wire shape for handleDownloadEvents,
// matching the dashboard's own field naming (snake_case, as every other
// /api/... endpoint here uses).
type downloadEventJSON struct {
	At         time.Time `json:"at"`
	Username   string    `json:"username"`
	ReceiverID string    `json:"receiver_id"`
	Key        string    `json:"key"`
	Success    bool      `json:"success"`
	RemoteAddr string    `json:"remote_addr"`
	Detail     string    `json:"detail"`
}

// handleDownloadEvents serves GET /api/download-events: the most recently
// recorded file download attempts (see recordDownloadEvent), newest first,
// as JSON. db nil (state tracking unavailable) serves an empty list rather
// than failing the request, matching handleLoginEvents's own tolerance for
// a missing dependency.
func handleDownloadEvents(db *store.Store, log *slog.Logger, limit int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, limit, db.ListDownloadEvents,
			func(ev store.DownloadEvent) downloadEventJSON { return downloadEventJSON(ev) },
			"reading download events failed")
	}
}

// jobRunEventJSON is store.JobRunEvent's wire shape for handleJobRunEvents,
// matching the dashboard's own field naming (snake_case, as every other
// /api/... endpoint here uses).
type jobRunEventJSON struct {
	JobName string    `json:"job_name"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Success bool      `json:"success"`
	Size    int64     `json:"size"`
	Error   string    `json:"error"`
}

// handleJobRunEvents serves GET /api/job-runs: the most recently recorded
// job runs (see Runner.recordJobRun), newest first, across every job, as
// JSON. db nil (state tracking unavailable) serves an empty list rather
// than failing the request, matching handleLoginEvents's own tolerance for
// a missing dependency.
func handleJobRunEvents(db *store.Store, log *slog.Logger, limit int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, limit, db.ListJobRunEvents,
			func(ev store.JobRunEvent) jobRunEventJSON { return jobRunEventJSON(ev) },
			"reading job run events failed")
	}
}

// targetRunEventJSON is store.TargetRunEvent's wire shape for
// handleTargetRunEvents, matching the dashboard's own field naming
// (snake_case, as every other /api/... endpoint here uses).
type targetRunEventJSON struct {
	At      time.Time `json:"at"`
	JobName string    `json:"job_name"`
	Target  string    `json:"target"`
	Success bool      `json:"success"`
	State   string    `json:"state"`
	Error   string    `json:"error"`
}

// handleTargetRunEvents serves GET /api/target-runs: the most recently
// recorded job target runs (see Runner.persistTargetRun), newest first,
// across every job, as JSON. db nil (state tracking unavailable) serves an
// empty list rather than failing the request, matching handleLoginEvents's
// own tolerance for a missing dependency.
func handleTargetRunEvents(db *store.Store, log *slog.Logger, limit int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, limit, db.ListTargetRunEvents,
			func(ev store.TargetRunEvent) targetRunEventJSON { return targetRunEventJSON(ev) },
			"reading target run events failed")
	}
}

// receiverEventJSON is store.ReceiverEvent's wire shape for
// handleReceiverEvents, matching the dashboard's own field naming
// (snake_case, as every other /api/... endpoint here uses).
type receiverEventJSON struct {
	At         time.Time `json:"at"`
	ReceiverID string    `json:"receiver_id"`
	Kind       string    `json:"kind"`
	Key        string    `json:"key"`
	Size       int64     `json:"size"`
	Success    bool      `json:"success"`
	Error      string    `json:"error"`
}

// handleReceiverEvents serves GET /api/receiver-events: the most recently
// recorded receiver API requests (see recordReceiverEventBestEffort in
// internal/backup/receiver), newest first, across every receiver, as JSON.
// db nil (state tracking unavailable) serves an empty list rather than
// failing the request, matching handleLoginEvents's own tolerance for a
// missing dependency.
func handleReceiverEvents(db *store.Store, log *slog.Logger, limit int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, limit, db.ListReceiverEvents,
			func(ev store.ReceiverEvent) receiverEventJSON { return receiverEventJSON(ev) },
			"reading receiver events failed")
	}
}
