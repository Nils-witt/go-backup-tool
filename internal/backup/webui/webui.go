// Package webui implements go-backup-tool's web UI: the live status
// dashboard, its SSO bearer-token auth (see auth.go), and the read-only views of receiver state and files it shows
// alongside a job's own status. It shares one HTTP server/mux with the
// receiver API (internal/backup/receiver) via StartWebUI's
// registerExtraRoutes hook, so the composition root can mount both on the
// same listen address without this package importing that one.
package webui

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/version"
)

// Server wraps the HTTP server behind the -listen web UI, letting
// callers shut it down cleanly (see StartWebUI/shutdown).
type Server struct {
	http *http.Server
	done chan struct{}
	addr string // the listener's actual bound address, e.g. resolved from ":0"
}

// StartWebUI starts the -listen web UI dashboard and returns a Server the
// caller can shut down with Server.Shutdown. Returns nil if the server
// fails to start.
func StartWebUI(addr string, statusStore *backup.StatusStore, jobs []*config.Config, runner *pipeline.Runner, receivers map[string]config.ResolvedReceiver, receiverStore *backup.ReceiverStatusStore, log *slog.Logger, db *store.Store, logs *LogRingBuffer, oidcSettings config.OIDCSettings, identity *identity.ServerIdentity, trustProxyHeaders, devMode bool, registerExtraRoutes func(*http.ServeMux), queue *notify.Queue) *Server {
	jobsByName := make(map[string]*config.Config, len(jobs))
	for _, j := range jobs {
		jobsByName[j.Name] = j
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		log.Error("web UI: listening", "addr", addr, "err", err)
		return nil
	}

	if logs == nil {
		logs = NewLogRingBuffer(LogBufferCapacity)
	}

	if !oidcSettings.Enabled {
		log.Warn("web UI: webui.oidc is not enabled, so nobody can sign in to the dashboard (the receiver API is unaffected)")
	}

	downloadTickets := newDownloadTicketStore()

	auth := newAuthenticator(oidcSettings, db, log, trustProxyHeaders)

	// perm gates a JSON endpoint the dashboard's own JavaScript calls via
	// fetch() behind a valid SSO access token (see requireUser) whose
	// permissions pass check: a missing/invalid token reports 401, a
	// signed-in user lacking the permission 403. The dashboard shell (GET
	// /) and file downloads (GET /api/receivers/{id}/download/{key...})
	// aren't wrapped in this: a plain browser navigation can never carry a
	// bearer token, so the shell is always public and downloads are
	// authorized by a one-time ticket instead (see downloadTicketStore).
	perm := func(check func(permission.Permission) bool) func(http.HandlerFunc) http.HandlerFunc {
		return func(h http.HandlerFunc) http.HandlerFunc { return requirePermission(auth, check, h) }
	}

	// api requires view access — every dashboard data endpoint except the
	// download/log/admin ones below, which each name their own permission.
	api := perm(permission.Permission.CanView)
	apiDownload := perm(permission.Permission.CanDownload)
	apiLoginLog := perm(permission.Permission.CanViewLoginLog)
	apiDownloadLog := perm(permission.Permission.CanViewDownloadLog)
	apiJobRunLog := perm(permission.Permission.CanViewJobRunLog)
	apiTargetRunLog := perm(permission.Permission.CanViewTargetRunLog)
	apiReceiverLog := perm(permission.Permission.CanViewReceiverLog)
	admin := perm(permission.Permission.CanAdmin)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleDashboard(dashboardIndexHTML))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", cacheForever(http.FileServerFS(dashboardAssetsFS))))
	mux.HandleFunc("GET /api/meta", handleMeta())
	mux.HandleFunc("GET /api/sso/status", handleSSOStatus(oidcSettings))
	mux.HandleFunc("GET /api/me", requireUser(auth, handleMe()))
	mux.HandleFunc("POST /api/sso/login", requireUser(auth, handleSSOLogin(auth)))
	mux.HandleFunc("GET /api/status", api(handleStatus(statusStore)))
	mux.HandleFunc("GET /api/logs", api(handleLogs(logs)))
	mux.HandleFunc("GET /api/identity", api(handleIdentity(identity)))
	mux.HandleFunc("GET /api/receivers", api(handleReceiverStatus(receivers, receiverStore, log)))
	mux.HandleFunc("GET /api/job-runs", apiJobRunLog(handleJobRunEvents(db, log)))
	mux.HandleFunc("GET /api/target-runs", apiTargetRunLog(handleTargetRunEvents(db, log)))
	mux.HandleFunc("POST /api/jobs/{name}/retry", admin(handleRetryFailedTargets(jobsByName, statusStore, runner, log)))
	mux.HandleFunc("GET /api/receivers/{id}/files", api(handleReceiverFiles(receivers, log)))
	mux.HandleFunc("POST /api/receivers/{id}/download/{key...}", apiDownload(handleMintDownloadTicket(receivers, downloadTickets)))
	mux.HandleFunc("GET /api/receivers/{id}/download/{key...}", handleDownloadFile(receivers, log, db, downloadTickets, trustProxyHeaders, queue))
	mux.HandleFunc("GET /api/login-events", apiLoginLog(handleLoginEvents(db, log)))
	mux.HandleFunc("GET /api/download-events", apiDownloadLog(handleDownloadEvents(db, log)))
	mux.HandleFunc("GET /api/receiver-events", apiReceiverLog(handleReceiverEvents(db, log)))

	if registerExtraRoutes != nil {
		registerExtraRoutes(mux)
	}

	var handler http.Handler = mux
	if devMode {
		handler = corsMiddleware(handler)
	}

	srv := &Server{
		http: &http.Server{Handler: logRequests(log, handler, trustProxyHeaders), ReadHeaderTimeout: 10 * time.Second},
		done: make(chan struct{}),
		addr: ln.Addr().String(),
	}

	go func() {
		defer close(srv.done)

		if err := srv.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web UI", "err", err)
		}
	}()

	log.Info("web UI listening", "addr", srv.addr)

	return srv
}

// logRequests wraps next, logging every request it handles at debug level
// once it completes: method, path, remote address, response status, and
// how long it took — the same per-request detail an operator would reach
// for a proper HTTP access log, without pulling in a logging middleware
// dependency for a handful of routes.
func logRequests(log *slog.Logger, next http.Handler, trustProxyHeaders bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, req)

		log.Debug("web UI request",
			"method", req.Method,
			"path", req.URL.Path,
			"remote", clientAddr(req, trustProxyHeaders),
			"status", sw.status,
			"duration", time.Since(start),
		)
	})
}

// corsMiddleware adds permissive CORS response headers to every request
// next handles, and answers a CORS preflight (an OPTIONS request) itself
// rather than passing it on — mux has no route registered for OPTIONS on
// any path, so without this a preflight would otherwise 404/405 before next
// ever saw it. Only wired in when webui.dev-mode: is set (see StartWebUI):
// it lets a frontend dev server running on its own origin (e.g. `npm run
// dev`) call this instance's API directly, at the cost of letting any
// website's JavaScript read API responses from a browser that also holds a
// valid bearer token for this instance — never enable it against a
// production instance.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// clientAddr returns the address to record as a request's origin: req's raw
// TCP peer (req.RemoteAddr) normally, or — when trustProxyHeaders is set,
// because this instance sits behind a reverse proxy — the original client's
// address as reported by that proxy, so logs reflect the real client rather
// than the proxy's own address. Only enable trustProxyHeaders when a proxy
// that itself sets these headers (and strips any client-supplied copies
// first) is guaranteed to be in front of every request; otherwise a client
// can spoof its own logged address by sending these headers itself.
//
// The standard Forwarded header (RFC 7239) is preferred, since its for=
// parameter can carry the client's port alongside its IP; the de facto
// X-Forwarded-For and X-Real-Ip headers only ever carry the IP.
func clientAddr(req *http.Request, trustProxyHeaders bool) string {
	if !trustProxyHeaders {
		return req.RemoteAddr
	}

	if fwd := req.Header.Get("Forwarded"); fwd != "" {
		if addr, ok := forwardedFor(fwd); ok {
			return addr
		}
	}

	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); ip != "" {
			return ip
		}
	}

	if ip := strings.TrimSpace(req.Header.Get("X-Real-Ip")); ip != "" {
		return ip
	}

	return req.RemoteAddr
}

// forwardedFor extracts the for= parameter naming the originating client
// from the first (nearest-client) element of a Forwarded header value, e.g.
// "for=192.0.2.60:4711;proto=http" or, quoted as RFC 7239 requires whenever
// the value itself contains reserved characters like an IPv6 literal's
// brackets and colons, `for="[2001:db8:cafe::17]:4711"`. Further elements
// after a comma, if any, were each prepended by the proxy in front of it, so
// the first is the one closest to the original client.
func forwardedFor(header string) (string, bool) {
	first, _, _ := strings.Cut(header, ",")

	for part := range strings.SplitSeq(first, ";") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "for") {
			continue
		}

		value = strings.Trim(strings.TrimSpace(value), `"`)
		if value == "" {
			return "", false
		}

		return value, true
	}

	return "", false
}

// statusWriter wraps an http.ResponseWriter to capture the status code
// written to it, since http.ResponseWriter doesn't expose that itself once
// WriteHeader has been called.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Shutdown gracefully stops the web UI server, waiting for it to finish.
func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = s.http.Shutdown(ctx)

	<-s.done
}

// handleStatus serves store's current job/target statuses as JSON.
func handleStatus(store *backup.StatusStore) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, store.Snapshot())
	}
}

// handleRetryFailedTargets serves POST /api/jobs/{name}/retry (see the
// dashboard's "Retry failed targets" button, dashboard.js): it kicks off a
// re-run of the named job's pipeline for whichever of its targets
// /api/status currently reports as failed (see
// pipeline.Runner.RetryFailedTargets's doc comment for why that means
// re-running the whole pipeline, not just re-uploading). Gated on admin
// rather than plain view/download, since it
// re-executes the job's configured backup command — a broader capability
// than anything else those two permissions grant.
//
// The retry runs in the background: this handler only starts it and
// returns immediately, since a backup can take far longer than an HTTP
// client wants to wait for a response. The dashboard's existing 2s
// /api/status poll picks up the retry's progress instead.
// context.WithoutCancel detaches the retry from this request's own
// context, so it isn't cut short the moment the response is written.
func handleRetryFailedTargets(jobs map[string]*config.Config, statusStore *backup.StatusStore, runner *pipeline.Runner, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")

		job, ok := jobs[name]
		if !ok {
			http.Error(w, "unknown job", http.StatusNotFound)
			return
		}

		targets := statusStore.FailedTargets(name)
		if len(targets) == 0 {
			http.Error(w, "no failed targets to retry", http.StatusConflict)
			return
		}

		retryCtx := context.WithoutCancel(r.Context())

		go func() {
			if err := runner.RetryFailedTargets(retryCtx, job, targets); err != nil {
				log.Warn("web UI: retrying failed targets", "job", name, "targets", targets, "err", err)
			}
		}()

		w.WriteHeader(http.StatusAccepted)
	}
}

// writeJSON encodes v as the response body with a JSON content type,
// writing a 500 if encoding fails. Shared by every simple "serve the
// current snapshot as JSON" handler in this file.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleReceiverStatus serves store's current receiver statuses as JSON,
// annotated with each receiver's live staleness (see
// annotateReceiverStaleness) for any entry in receivers with stale-after:
// set.
func handleReceiverStatus(receivers map[string]config.ResolvedReceiver, store *backup.ReceiverStatusStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		snapshots := store.Snapshot()

		for i := range snapshots {
			annotateReceiverStaleness(&snapshots[i], receivers[snapshots[i].ID], log)
		}

		writeJSON(w, snapshots)
	}
}

// identityJSON is serverIdentity's wire shape for handleIdentity, matching
// the dashboard's own field naming (snake_case, as every other /api/...
// endpoint here uses).
type identityJSON struct {
	UUID      string `json:"uuid"`
	PublicKey string `json:"public_key"`
}

// handleIdentity serves GET /api/identity: this instance's persistent UUID
// and PEM-encoded public key (see serverIdentity), for the dashboard's
// "Server identity" section — an operator reads them off there to fill in a
// receiving instance's matching receivers: entry (id: and public-key:)
// rather than digging through this instance's keys-dir: on disk. identity
// nil (loadServerIdentityAtStartup failed at startup, or the receiver API
// isn't used by any type: remote target) serves a zero-value identityJSON,
// which the dashboard's JS treats as "no identity to show".
func handleIdentity(identity *identity.ServerIdentity) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var out identityJSON

		if identity != nil {
			out = identityJSON{UUID: identity.UUID(), PublicKey: identity.PublicKeyPEM()}
		}

		writeJSON(w, out)
	}
}

// metaJSON is this instance's build metadata, as served by GET /api/meta,
// for the dashboard's footer — the SPA's index.html is a static build artifact with no server-side
// templating, so this replaces what dashboardHTML's now-removed
// {{VERSION}}/{{COMMIT}} placeholder substitutions did.
type metaJSON struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// handleMeta serves GET /api/meta: always public/unauthenticated, since the
// footer it feeds is shown on the login page too. Leaks nothing the
// binary's own --version doesn't already report.
func handleMeta() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, metaJSON{Version: version.Version, Commit: version.Commit})
	}
}

// annotateReceiverStaleness fills snap's StaleAfter/Stale fields from recv's
// current state on disk, for a receiver with stale-after: set; a no-op
// otherwise. Stale mirrors staleReceiverMonitor.check's own condition — at
// least one file received, and the most recent one older than
// recv.StaleAfter — so the dashboard never disagrees with what actually
// fires the webhook. A lastReceivedAt failure is logged and leaves Stale
// false rather than failing the whole /api/receivers response over one
// receiver's directory listing.
func annotateReceiverStaleness(snap *backup.ReceiverSnapshot, recv config.ResolvedReceiver, log *slog.Logger) {
	if recv.StaleAfter <= 0 {
		return
	}

	snap.StaleAfter = recv.StaleAfter.String()

	lastSeen, ok, err := backup.LastReceivedAt(recv)
	if err != nil {
		log.Warn("receiver: checking staleness failed", "id", recv.ID, "err", err)
		return
	}

	snap.Stale = ok && time.Since(lastSeen) > recv.StaleAfter
}

// handleReceiverFiles serves GET /api/receivers/{id}/files: the objects
// currently stored under receiver {id}'s path (see listReceiverFiles), for
// the web UI dashboard's per-receiver file listing. Unlike the receiver API
// (HandleReceiveObject/HandleDeleteObject), this is dashboard-only and isn't
// JWT-authenticated, matching /api/receivers.
func handleReceiverFiles(receivers map[string]config.ResolvedReceiver, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := lookupReceiver(w, r, receivers)
		if !ok {
			return
		}

		files, err := backup.ListReceiverFiles(recv)
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
func lookupReceiver(w http.ResponseWriter, r *http.Request, receivers map[string]config.ResolvedReceiver) (config.ResolvedReceiver, bool) {
	recv, ok := receivers[r.PathValue("id")]
	if !ok {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
	}

	return recv, ok
}

// LogBufferCapacity is how many of the most recent log lines StartWebUI's
// LogRingBuffer keeps for the dashboard's log viewer (see handleLogs).
const LogBufferCapacity = 500

// LogRingBuffer is a bounded, concurrency-safe in-memory tail of the most
// recent log lines written to it, for the web UI's log viewer (see
// handleLogs). It's an io.Writer meant to sit alongside the process's real
// log output (see runWithContext in app.go, which fans writes out to both),
// treating each Write call as one line — matching how a slog handler calls
// Write exactly once per record. Lines live only in memory: a restart clears
// it, same as the receiver status store.
type LogRingBuffer struct {
	mu    sync.Mutex
	lines []string
	cap   int
	start int // index of the oldest entry in lines, once lines is full
}

// NewLogRingBuffer returns an empty LogRingBuffer holding at most capacity
// lines.
func NewLogRingBuffer(capacity int) *LogRingBuffer {
	return &LogRingBuffer{cap: capacity, lines: make([]string, 0, capacity)}
}

// Write records p, trimmed of its trailing newline, as the newest line,
// evicting the oldest one once the buffer is at capacity. Always succeeds,
// so a logger writing through this never fails on that account.
func (b *LogRingBuffer) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.lines) < b.cap {
		b.lines = append(b.lines, line)
	} else {
		b.lines[b.start] = line
		b.start = (b.start + 1) % b.cap
	}

	return len(p), nil
}

// snapshot returns the currently buffered lines, oldest first.
func (b *LogRingBuffer) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, len(b.lines))
	for i := range out {
		out[i] = b.lines[(b.start+i)%len(b.lines)]
	}

	return out
}

// handleLogs serves buf's currently buffered log lines as JSON, for the
// dashboard's log viewer to poll.
func handleLogs(buf *LogRingBuffer) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, buf.snapshot())
	}
}

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

// loginEventsLimit caps how many of the most recent login events
// handleLoginEvents serves, for the dashboard's login log view.
const loginEventsLimit = 200

// handleLoginEvents serves GET /api/login-events: the most recently recorded
// dashboard login attempts (see recordLoginEvent), newest first, as JSON.
// db nil (state tracking unavailable) serves an empty list rather than
// failing the request, matching handleReceiverStatus's own tolerance for a
// missing dependency.
func handleLoginEvents(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, loginEventsLimit, db.ListLoginEvents,
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

// downloadEventsLimit caps how many of the most recent download events
// handleDownloadEvents serves, for the dashboard's download log view.
const downloadEventsLimit = 200

// handleDownloadEvents serves GET /api/download-events: the most recently
// recorded file download attempts (see recordDownloadEvent), newest first,
// as JSON. db nil (state tracking unavailable) serves an empty list rather
// than failing the request, matching handleLoginEvents's own tolerance for
// a missing dependency.
func handleDownloadEvents(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, downloadEventsLimit, db.ListDownloadEvents,
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

// jobRunEventsLimit caps how many of the most recent job runs
// handleJobRunEvents serves, for the dashboard's job run log view.
const jobRunEventsLimit = 200

// handleJobRunEvents serves GET /api/job-runs: the most recently recorded
// job runs (see Runner.recordJobRun), newest first, across every job, as
// JSON. db nil (state tracking unavailable) serves an empty list rather
// than failing the request, matching handleLoginEvents's own tolerance for
// a missing dependency.
func handleJobRunEvents(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, jobRunEventsLimit, db.ListJobRunEvents,
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

// targetRunEventsLimit caps how many of the most recent target runs
// handleTargetRunEvents serves, for the dashboard's target run log view.
const targetRunEventsLimit = 200

// handleTargetRunEvents serves GET /api/target-runs: the most recently
// recorded job target runs (see Runner.persistTargetRun), newest first,
// across every job, as JSON. db nil (state tracking unavailable) serves an
// empty list rather than failing the request, matching handleLoginEvents's
// own tolerance for a missing dependency.
func handleTargetRunEvents(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, targetRunEventsLimit, db.ListTargetRunEvents,
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

// receiverEventsLimit caps how many of the most recent receiver events
// handleReceiverEvents serves, for the dashboard's receiver log view.
const receiverEventsLimit = 200

// handleReceiverEvents serves GET /api/receiver-events: the most recently
// recorded receiver API requests (see recordReceiverEventBestEffort in
// internal/backup/receiver), newest first, across every receiver, as JSON.
// db nil (state tracking unavailable) serves an empty list rather than
// failing the request, matching handleLoginEvents's own tolerance for a
// missing dependency.
func handleReceiverEvents(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, receiverEventsLimit, db.ListReceiverEvents,
			func(ev store.ReceiverEvent) receiverEventJSON { return receiverEventJSON(ev) },
			"reading receiver events failed")
	}
}

// downloadTicketTTL is how long a minted download ticket (see
// downloadTicketStore) stays redeemable: long enough for the dashboard's JS
// to mint one and immediately navigate the browser to it, short enough that
// a ticket leaking into a server log or browser history is useless almost
// immediately.
const downloadTicketTTL = 60 * time.Second

// downloadTicketEntry is one currently valid download ticket (see
// downloadTicketStore): the receiver/key it authorizes one download of, the
// identity that minted it (for the download log, since the download request
// itself carries no Authorization header to attribute it from — see
// handleDownloadFile), and its expiry.
type downloadTicketEntry struct {
	receiverID string
	key        string
	username   string
	expires    time.Time
}

// downloadTicketStore tracks currently valid download tickets, mapping each
// ticket id to its downloadTicketEntry. Tickets exist so a file download can
// stay a plain browser navigation — letting the browser handle the save
// itself, rather than the dashboard's JavaScript buffering the whole file in
// memory as a Blob — even though bearer-token auth can't ride along on one:
// the dashboard's JS mints a ticket with an authenticated fetch() (see
// handleMintDownloadTicket) and only then navigates to the download URL with
// it attached as a query parameter (see handleDownloadFile). Safe for
// concurrent use.
type downloadTicketStore struct {
	mu   sync.Mutex
	byID map[string]downloadTicketEntry
}

// newDownloadTicketStore returns an empty downloadTicketStore.
func newDownloadTicketStore() *downloadTicketStore {
	return &downloadTicketStore{byID: make(map[string]downloadTicketEntry)}
}

// create mints a new ticket authorizing one download of receiverID/key,
// attributed to username (best-effort, may be empty), valid for
// downloadTicketTTL.
func (s *downloadTicketStore) create(receiverID, key, username string) (string, error) {
	id, err := randomTicketID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.byID[id] = downloadTicketEntry{receiverID: receiverID, key: key, username: username, expires: time.Now().Add(downloadTicketTTL)}
	s.mu.Unlock()

	return id, nil
}

// consume redeems ticket id for receiverID/key, reporting the username it
// was minted for and ok=true only if id names a currently unexpired ticket
// that was minted for exactly this receiver/key. Either way, id is no
// longer valid afterwards — a ticket authorizes exactly one download,
// successful or not, so it can't be replayed from a server log or browser
// history.
func (s *downloadTicketStore) consume(id, receiverID, key string) (username string, ok bool) {
	if id == "" {
		return "", false
	}

	s.mu.Lock()
	e, exists := s.byID[id]
	delete(s.byID, id)
	s.mu.Unlock()

	if !exists || time.Now().After(e.expires) || e.receiverID != receiverID || e.key != key {
		return "", false
	}

	return e.username, true
}

// randomTicketID returns a 256-bit random value hex-encoded, unguessable
// enough to serve as a download ticket id (see downloadTicketStore.create).
func randomTicketID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

// downloadTicketJSON is a freshly minted download ticket's wire shape (see
// handleMintDownloadTicket).
type downloadTicketJSON struct {
	Ticket string `json:"ticket"`
}

// handleMintDownloadTicket serves POST /api/receivers/{id}/download/{key...}:
// it mints a short-lived, single-use download ticket (see
// downloadTicketStore) for the receiver/key named by the path, attributed to
// whoever is currently signed in (see currentUser). The
// dashboard's JS calls this — with its Authorization: Bearer header — right
// before navigating the browser to the matching GET, which can't carry that
// header itself (see handleDownloadFile).
func handleMintDownloadTicket(receivers map[string]config.ResolvedReceiver, tickets *downloadTicketStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := lookupReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := backup.SanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var username string
		if user, ok := currentUser(r.Context()); ok {
			username = user.Username
		}

		ticket, err := tickets.create(recv.ID, key, username)
		if err != nil {
			http.Error(w, "minting download ticket failed", http.StatusInternalServerError)
			return
		}

		writeJSON(w, downloadTicketJSON{Ticket: ticket})
	}
}

// handleDownloadFile serves GET /api/receivers/{id}/download/{key...}: the
// actual content of one object currently stored under receiver {id}'s path,
// for a person to save from the dashboard's file listing (see
// listReceiverFiles/handleReceiverFiles for the metadata-only listing this
// complements). Unlike the receiver API's own per-receiver JWT auth (see
// authorizeReceiver), and unlike every other dashboard endpoint (see
// requireUser), this is authorized by a one-time download ticket
// (see downloadTicketStore) rather than a bearer token: the request behind
// this is a plain browser navigation, which can't carry an Authorization
// header the way the dashboard's own fetch() calls can (see
// handleMintDownloadTicket, which the dashboard's JS calls first to obtain
// one). db, when non-nil, gets every attempt appended to the download log
// (see recordDownloadEvent), win or lose, for the dashboard's "Download log"
// section (see handleDownloadEvents); a write failure there is only logged,
// not surfaced to the browser, mirroring recordLogin's own tolerance for a
// login log write failure.
func handleDownloadFile(receivers map[string]config.ResolvedReceiver, log *slog.Logger, db *store.Store, tickets *downloadTicketStore, trustProxyHeaders bool, queue *notify.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := lookupReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := backup.SanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		username, ok := tickets.consume(r.URL.Query().Get("ticket"), recv.ID, key)
		if !ok {
			http.Error(w, "missing or expired download ticket", http.StatusForbidden)
			return
		}

		record := func(success bool, detail string) {
			if db == nil {
				return
			}

			ev := store.DownloadEvent{At: time.Now(), Username: username, ReceiverID: recv.ID, Key: key, Success: success, RemoteAddr: clientAddr(r, trustProxyHeaders), Detail: detail}
			if err := db.SaveDownloadEvent(r.Context(), ev); err != nil {
				log.Warn("download: recording download event failed", "err", err)
			}
		}

		path := filepath.Join(recv.Path, filepath.FromSlash(key))

		f, err := os.Open(path) //nolint:gosec // key is sanitized by SanitizeObjectKey and joined under recv.Path, not attacker-controlled beyond that
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "not found", http.StatusNotFound)
				record(false, "not found")
			} else {
				log.Warn("download: opening file failed", "id", recv.ID, "key", key, "err", err)
				http.Error(w, "opening file failed", http.StatusInternalServerError)
				record(false, "opening file failed")
			}

			return
		}
		defer func() { _ = f.Close() }()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(key)+`"`)

		record(true, "")

		go receiver.NotifyDownload(recv, receiver.DownloadWebhookEvent{Username: username, Key: key, At: time.Now()}, queue, log)

		if _, err := io.Copy(w, f); err != nil {
			log.Warn("download: streaming file failed", "id", recv.ID, "key", key, "err", err)
		}
	}
}

// handleDashboard serves the SPA shell (the built dist/index.html), which
// fetches /api/meta and /api/status (among others) itself and polls from
// there; the page carries no server-rendered state at all any more (see
// metaJSON/handleMeta).
func handleDashboard(html string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, html)
	}
}

// cacheForever wraps next, marking every response immutable and
// long-lived — safe here because Vite content-hashes every file under
// dist/assets, so a given URL's content never changes; a new build simply
// produces new URLs (see dashboardIndexHTML, re-read at each startup, which
// is how a new build's asset URLs actually reach a client).
func cacheForever(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// dashboardDistFS holds the dashboard SPA's production build (see
// frontend/, built by `npm run build` into this directory — not committed,
// see .gitignore), embedded via go:embed rather than fetched at runtime.
// The "all:" prefix matters: Vite can emit dotfiles, which go:embed's
// default patterns skip.
//
//go:embed all:dist
var dashboardDistFS embed.FS

// dashboardAssetsFS serves dist/assets/* at GET /assets/* (see StartWebUI),
// stripped of its "dist/assets/" prefix.
var dashboardAssetsFS = mustSubFS(dashboardDistFS, "dist/assets")

// dashboardIndexHTML is the SPA shell's HTML, read once at package init
// (mirroring the pre-SPA dashboardHTML var's own "build it once, not per
// request" shape) rather than on every GET /.
var dashboardIndexHTML = mustReadFS(dashboardDistFS, "dist/index.html")

// mustSubFS/mustReadFS panic on error rather than returning one: both only
// ever fail if the frontend/ build didn't run before `go build` (see
// frontend/README.md), which is a build-time misconfiguration to fail fast
// and loudly on, not a runtime condition any caller could recover from.
func mustSubFS(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("webui: " + err.Error())
	}

	return sub
}

func mustReadFS(fsys embed.FS, name string) string {
	b, err := fsys.ReadFile(name)
	if err != nil {
		panic("webui: " + err.Error())
	}

	return string(b)
}
