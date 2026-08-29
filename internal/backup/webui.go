package backup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/version"
)

// webUIServer wraps the HTTP server behind the -listen web UI, letting
// callers shut it down cleanly (see startWebUI/shutdown).
type webUIServer struct {
	http *http.Server
	done chan struct{}
	addr string // the listener's actual bound address, e.g. resolved from ":0"
}

// startWebUI binds addr and starts an HTTP server serving a live dashboard
// of store's job/target statuses (see dashboardHTML and handleStatus), plus
// the receiver API (see handleReceiveObject/handleDeleteObject) for any
// entries in receivers — whose own live status is tracked in a
// receiverStatusStore, seeded from each receiver's last persisted
// receiver_events row so a restart doesn't revert every receiver to idle
// (see seedReceiverStatusFromState in receiver.go), and served over
// /api/receivers (see handleReceiverStatus) — reporting it (and any later
// failure) to log.
// Binding happens synchronously so a bad address is reported immediately and
// callers can rely on the server being reachable as soon as this returns; it
// returns nil if addr couldn't be bound, leaving the web UI disabled for
// this run rather than failing the whole process over a dashboard. db is the
// shared state/retention db (see schedule_state.go and retention.go), used
// by the receiver API's handlers to track retention on incoming writes, and
// by the login handlers (handleWebUILogin/handleOIDCCallback) to append
// every login attempt to the login log served over /api/login-events (see
// handleLoginEvents) and shown in the dashboard's "Login log" section, and
// by handleDownloadFile to append every file download to the download log
// served over /api/download-events (see handleDownloadEvents) and shown in
// the dashboard's "Download log" section; nil disables all three (see
// recordLocalWrite/recordLoginEvent/recordDownloadEvent). logs backs the dashboard's
// log viewer (served over /api/logs, see handleLogs); nil starts an empty
// one, so passing the caller's own buffer only matters if the caller also
// arranged for it to be written to — which newRunLogger in app.go only does
// when the config file's enable-log-viewer: is true, so the viewer stays
// effectively empty (and its "Logs" section hidden) unless an operator opts
// in. webUIUsername/webUIPassword, when webUIUsername is non-empty, gate
// the dashboard and its /api/... endpoints (including per-receiver file
// downloads) behind a login page and session cookie (see
// requireWebUISession/handleWebUILogin); the receiver API
// (handleReceiveObject/handleDeleteObject) is unaffected, since it
// authenticates each request on its own via each receiver's own
// public-key-verified JWT (see authorizeReceiver). An empty webUIUsername
// leaves the web UI open, as before this was added.
// oidcAuth, when non-nil (see newOIDCAuth in oidc.go), additionally lets a
// browser log in via that provider's own "Log in with SSO" link on the
// login page (see handleOIDCLogin/handleOIDCCallback), alongside the
// username/password form if one is also configured; either kind of login
// starts the same dashboard session. Login is required whenever
// webUIUsername or oidcAuth is set — either alone is enough to gate the
// dashboard.
// identity, when non-nil (see loadServerIdentityAtStartup in app.go), is
// served over /api/identity (see handleIdentity) for the dashboard's "Server
// identity" section, so an operator can read off this instance's UUID and
// public key without digging through its keys-dir: on disk; a nil identity
// (loadServerIdentityAtStartup failed at startup) hides that section.
func startWebUI(addr string, store *statusStore, receivers map[string]resolvedReceiver, log *slog.Logger, db *sql.DB, logs *logRingBuffer, webUIUsername, webUIPassword string, oidcAuth *oidcAuth, identity *serverIdentity, trustProxyHeaders bool) *webUIServer {
	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		log.Error("web UI: listening", "addr", addr, "err", err)
		return nil
	}

	if logs == nil {
		logs = newLogRingBuffer(logBufferCapacity)
	}

	receiverStore := newReceiverStatusStore(receivers)
	if db != nil {
		seedReceiverStatusFromState(context.Background(), db, receivers, receiverStore, log)
	}

	uiSessions := newSessionStore()

	// authEnabled mirrors requireWebUISession's own gating condition:
	// either a username/password or an SSO provider is enough on its own to
	// require a login before the dashboard serves anything.
	authEnabled := webUIUsername != "" || oidcAuth != nil

	// page gates a full-page navigation (the dashboard itself, or a link a
	// person clicks, like a file download): a missing/invalid session
	// redirects the browser to the login page. api gates a JSON endpoint
	// polled by the dashboard's own JavaScript (fetch()): a redirect there
	// would hand the poller an HTML login page as its "JSON" response, so
	// it reports 401 instead — see requireWebUISession.
	page := func(h http.HandlerFunc) http.HandlerFunc {
		return requireWebUISession(authEnabled, uiSessions, true, h)
	}
	api := func(h http.HandlerFunc) http.HandlerFunc {
		return requireWebUISession(authEnabled, uiSessions, false, h)
	}

	dashboardPage := strings.Replace(dashboardHTML, "{{LOGOUT_HIDDEN}}", logoutLinkAttr(authEnabled), 1)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", page(handleDashboard(dashboardPage)))
	mux.HandleFunc("GET /api/status", api(handleStatus(store)))
	mux.HandleFunc("GET /api/logs", api(handleLogs(logs)))
	mux.HandleFunc("GET /api/identity", api(handleIdentity(identity)))
	mux.HandleFunc("GET /api/receivers", api(handleReceiverStatus(receivers, receiverStore, log)))
	mux.HandleFunc("GET /api/receivers/{id}/files", api(handleReceiverFiles(receivers, log)))
	mux.HandleFunc("GET /api/receivers/{id}/download/{key...}", page(handleDownloadFile(receivers, log, db, uiSessions, trustProxyHeaders)))
	mux.HandleFunc("GET /login", handleWebUILogin(webUIUsername, webUIPassword, oidcAuth != nil, uiSessions, db, log, trustProxyHeaders))
	mux.HandleFunc("POST /login", handleWebUILogin(webUIUsername, webUIPassword, oidcAuth != nil, uiSessions, db, log, trustProxyHeaders))
	mux.HandleFunc("GET /logout", handleWebUILogout(uiSessions))
	mux.HandleFunc("GET /api/login-events", api(handleLoginEvents(db, log)))
	mux.HandleFunc("GET /api/download-events", api(handleDownloadEvents(db, log)))
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", handleReceiveObject(receivers, receiverStore, log, db))
	mux.HandleFunc("DELETE /api/v1/objects/{id}/{key...}", handleDeleteObject(receivers, receiverStore, log, db))

	if oidcAuth != nil {
		pending := newOIDCPendingStore()
		mux.HandleFunc("GET /login/oidc", handleOIDCLogin(oidcAuth, pending))
		mux.HandleFunc("GET /login/oidc/callback", handleOIDCCallback(oidcAuth, pending, uiSessions, log, db, trustProxyHeaders))
	}

	srv := &webUIServer{
		http: &http.Server{Handler: logRequests(log, mux, trustProxyHeaders), ReadHeaderTimeout: 10 * time.Second},
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

// shutdown gracefully stops the web UI server, waiting for it to finish.
func (s *webUIServer) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = s.http.Shutdown(ctx)

	<-s.done
}

// handleStatus serves store's current job/target statuses as JSON.
func handleStatus(store *statusStore) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if err := json.NewEncoder(w).Encode(store.snapshot()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// handleReceiverStatus serves store's current receiver statuses as JSON,
// annotated with each receiver's live staleness (see
// annotateReceiverStaleness) for any entry in receivers with stale-after:
// set.
func handleReceiverStatus(receivers map[string]resolvedReceiver, store *receiverStatusStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		snapshots := store.snapshot()

		for i := range snapshots {
			annotateReceiverStaleness(&snapshots[i], receivers[snapshots[i].ID], log)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if err := json.NewEncoder(w).Encode(snapshots); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
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
func handleIdentity(identity *serverIdentity) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var out identityJSON

		if identity != nil {
			out = identityJSON{UUID: identity.uuid, PublicKey: identity.publicKeyPEM}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if err := json.NewEncoder(w).Encode(out); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// annotateReceiverStaleness fills snap's StaleAfter/Stale fields from recv's
// current state on disk, for a receiver with stale-after: set; a no-op
// otherwise. Stale mirrors staleReceiverMonitor.check's own condition — at
// least one file received, and the most recent one older than
// recv.staleAfter — so the dashboard never disagrees with what actually
// fires the webhook. A lastReceivedAt failure is logged and leaves Stale
// false rather than failing the whole /api/receivers response over one
// receiver's directory listing.
func annotateReceiverStaleness(snap *receiverSnapshot, recv resolvedReceiver, log *slog.Logger) {
	if recv.staleAfter <= 0 {
		return
	}

	snap.StaleAfter = recv.staleAfter.String()

	lastSeen, ok, err := lastReceivedAt(recv)
	if err != nil {
		log.Warn("receiver: checking staleness failed", "id", recv.id, "err", err)
		return
	}

	snap.Stale = ok && time.Since(lastSeen) > recv.staleAfter
}

// handleReceiverFiles serves GET /api/receivers/{id}/files: the objects
// currently stored under receiver {id}'s path (see listReceiverFiles), for
// the web UI dashboard's per-receiver file listing. Unlike the receiver API
// (handleReceiveObject/handleDeleteObject), this is dashboard-only and isn't
// JWT-authenticated, matching /api/receivers.
func handleReceiverFiles(receivers map[string]resolvedReceiver, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := receivers[r.PathValue("id")]
		if !ok {
			http.Error(w, "unknown receiver id", http.StatusNotFound)
			return
		}

		files, err := listReceiverFiles(recv)
		if err != nil {
			log.Warn("receiver: listing files failed", "id", recv.id, "err", err)
			http.Error(w, "listing files failed", http.StatusInternalServerError)

			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if err := json.NewEncoder(w).Encode(files); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// logBufferCapacity is how many of the most recent log lines startWebUI's
// logRingBuffer keeps for the dashboard's log viewer (see handleLogs).
const logBufferCapacity = 1000

// logRingBuffer is a bounded, concurrency-safe in-memory tail of the most
// recent log lines written to it, for the web UI's log viewer (see
// handleLogs). It's an io.Writer meant to sit alongside the process's real
// log output (see runWithContext in app.go, which fans writes out to both),
// treating each Write call as one line — matching how a slog handler calls
// Write exactly once per record. Lines live only in memory: a restart clears
// it, same as the receiver status store.
type logRingBuffer struct {
	mu    sync.Mutex
	lines []string
	cap   int
	start int // index of the oldest entry in lines, once lines is full
}

// newLogRingBuffer returns an empty logRingBuffer holding at most capacity
// lines.
func newLogRingBuffer(capacity int) *logRingBuffer {
	return &logRingBuffer{cap: capacity, lines: make([]string, 0, capacity)}
}

// Write records p, trimmed of its trailing newline, as the newest line,
// evicting the oldest one once the buffer is at capacity. Always succeeds,
// so a logger writing through this never fails on that account.
func (b *logRingBuffer) Write(p []byte) (int, error) {
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
func (b *logRingBuffer) snapshot() []string {
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
func handleLogs(buf *logRingBuffer) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if err := json.NewEncoder(w).Encode(buf.snapshot()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// webUISessionCookie is the cookie a successful dashboard login (see
// handleWebUILogin) sets, gating the dashboard and its /api/... endpoints —
// including per-receiver file downloads — behind one login (see
// requireWebUISession). Its value is an opaque, randomly generated session
// id rather than the underlying username/password, so the credential never
// needs to be re-sent (or stored client-side) once logged in.
const webUISessionCookie = "gbt_webui_session"

// sessionTTL is how long a dashboard login (see webUISessionCookie) remains
// valid before its session expires and the browser has to log in again.
const sessionTTL = 12 * time.Hour

// sessionEntry is one currently valid dashboard login (see sessionStore):
// its expiry, plus the identity that logged in, so handlers downstream of
// requireWebUISession (e.g. handleDownloadFile) can attribute what they do
// to a username without re-deriving it from the request.
type sessionEntry struct {
	expires  time.Time
	username string // best-effort identity recorded at login (see handleWebUILogin/handleOIDCCallback); may be empty
}

// sessionStore tracks currently valid dashboard logins (see
// webUISessionCookie/handleWebUILogin), mapping each session id to its
// sessionEntry. Sessions live only in this process's memory: a restart
// invalidates every session, same as it does the receiver status store.
// Safe for concurrent use, since login and other requests can arrive
// concurrently.
type sessionStore struct {
	mu   sync.Mutex
	byID map[string]sessionEntry
}

// newSessionStore returns an empty sessionStore.
func newSessionStore() *sessionStore {
	return &sessionStore{byID: make(map[string]sessionEntry)}
}

// create starts a new session for username, valid for sessionTTL, and
// returns its id.
func (s *sessionStore) create(username string) (string, error) {
	id, err := randomSessionID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.byID[id] = sessionEntry{expires: time.Now().Add(sessionTTL), username: username}
	s.mu.Unlock()

	return id, nil
}

// valid reports whether id names a currently unexpired session, evicting it
// first if it has expired.
func (s *sessionStore) valid(id string) bool {
	if id == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.byID[id]
	if !ok {
		return false
	}

	if time.Now().After(e.expires) {
		delete(s.byID, id)
		return false
	}

	return true
}

// usernameFor returns the username recorded (see create) for r's session
// cookie, for handlers that want to attribute an action to whoever is
// currently logged in (e.g. handleDownloadFile's download log). It returns
// "" whenever there's no currently valid session — including when the web
// UI has no login configured at all, in which case every download is
// logged with an empty username rather than failing to log it.
func (s *sessionStore) usernameFor(r *http.Request) string {
	c, err := r.Cookie(webUISessionCookie)
	if err != nil {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.byID[c.Value]
	if !ok || time.Now().After(e.expires) {
		return ""
	}

	return e.username
}

// revoke ends session id (a no-op if it doesn't exist), used by a logout
// handler.
func (s *sessionStore) revoke(id string) {
	s.mu.Lock()
	delete(s.byID, id)
	s.mu.Unlock()
}

// authenticated reports whether r carries a currently valid session cookie
// for s.
func (s *sessionStore) authenticated(r *http.Request) bool {
	c, err := r.Cookie(webUISessionCookie)
	if err != nil {
		return false
	}

	return s.valid(c.Value)
}

// cookie builds s's session cookie for value, or clears it when value is ""
// (used by handleWebUILogout). Secure is only set when the request itself
// arrived over TLS (r.TLS != nil): this process never terminates TLS on its
// own listen: address (see startWebUI), but a reverse proxy in front of it
// might, in which case Go's net/http sets r.TLS for the connection it
// accepted from that proxy. HttpOnly and SameSite=Lax are always set, so
// the session id is never readable from JavaScript and is only ever sent on
// same-site navigations.
func (s *sessionStore) cookie(value string, secure bool) *http.Cookie {
	c := &http.Cookie{ //nolint:gosec // Secure is intentionally conditional (see doc comment above), not a literal true, so gosec can't verify it statically
		Name:     webUISessionCookie,
		Value:    value,
		Path:     "/",
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}

	if value == "" {
		c.MaxAge = -1
	} else {
		c.Expires = time.Now().Add(sessionTTL)
	}

	return c
}

// randomSessionID returns a 256-bit random value hex-encoded, unguessable
// enough to serve as a bearer session id.
func randomSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

// safeNextPath validates a login redirect target (the next= query/form
// value the login handlers and handleDownloadFile pass around): it must be
// a same-site path, never an absolute URL or protocol-relative "//host/..."
// one, so a crafted link can't use this instance's own login page to
// redirect a browser off-site after a successful login. Anything else falls
// back to "/".
func safeNextPath(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}

	return next
}

// handleWebUILogin serves the dashboard's own login form (GET /login) and
// its submission (POST /login), checking username/password (webui.username/
// webui.password in the config file) with subtle.ConstantTimeCompare rather
// than ==, so a mismatch can't be timed to learn how many leading bytes
// were guessed correctly (authorizeReceiver's own auth is a JWT signature
// check, not a raw comparison, so it needs no such care). A
// successful submission starts a session (see sessionStore) and sets
// webUISessionCookie so requireWebUISession lets the browser back in
// without asking for credentials on every request; it then redirects to
// next (see safeNextPath), typically the page that sent the browser here in
// the first place. showSSO adds a "Log in with SSO" link to the page (see
// renderLoginPage), pointing at /login/oidc, whenever oidc.enabled is set
// (see startWebUI) — independently of whether a username/password is also
// configured. An empty username with showSSO false (neither kind of login
// configured) redirects straight to next rather than showing a form there's
// no way to satisfy; an empty username with showSSO true shows the page
// with only the SSO link, and POST /login (which only the password form
// submits) 404s in that case, since there's no username/password to check.
// db, when non-nil, gets every submitted attempt appended to the login log
// (see recordLoginEvent), win or lose, for the dashboard's login log view
// (see handleLoginEvents); a write failure there is only logged, not
// surfaced to the browser, since it must never block an otherwise-successful
// login.
func handleWebUILogin(username, password string, showSSO bool, sessions *sessionStore, db *sql.DB, log *slog.Logger, trustProxyHeaders bool) http.HandlerFunc {
	showPassword := username != ""

	return func(w http.ResponseWriter, r *http.Request) {
		next := safeNextPath(r.FormValue("next"))

		if !showPassword && !showSSO {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}

		if r.Method == http.MethodGet {
			writeLoginPage(w, renderLoginPage("", next, showPassword, showSSO))
			return
		}

		if !showPassword {
			http.NotFound(w, r)
			return
		}

		submittedUser := r.FormValue("username")
		userMatch := subtle.ConstantTimeCompare([]byte(submittedUser), []byte(username)) == 1
		passMatch := subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(password)) == 1
		success := userMatch && passMatch

		if db != nil {
			detail := ""
			if !success {
				detail = "incorrect username or password"
			}

			ev := loginEvent{At: time.Now(), Username: submittedUser, Method: "password", Success: success, RemoteAddr: clientAddr(r, trustProxyHeaders), Detail: detail}
			if err := recordLoginEvent(r.Context(), db, ev); err != nil {
				log.Warn("web UI: recording login event failed", "err", err)
			}
		}

		if !success {
			writeLoginPage(w, renderLoginPage("incorrect username or password", next, showPassword, showSSO))
			return
		}

		id, err := sessions.create(submittedUser)
		if err != nil {
			http.Error(w, "starting session failed", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, sessions.cookie(id, r.TLS != nil))
		http.Redirect(w, r, next, http.StatusSeeOther)
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
func handleLoginEvents(db *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, loginEventsLimit, readLoginEvents,
			func(ev loginEvent) loginEventJSON { return loginEventJSON(ev) },
			"reading login events failed")
	}
}

// serveEventLog writes a JSON array of the most recently recorded audit-log
// events (see readLoginEvents/readDownloadEvents), newest first, converting
// each raw event E to its wire shape J via toJSON. db nil (state tracking
// unavailable) serves an empty list rather than failing the request, since
// the state db is optional (see startWebUI). Shared by handleLoginEvents
// and handleDownloadEvents, whose bodies would otherwise be identical but
// for the event/read/limit types involved.
func serveEventLog[E, J any](w http.ResponseWriter, r *http.Request, log *slog.Logger, db *sql.DB, limit int, read func(context.Context, *sql.DB, int) ([]E, error), toJSON func(E) J, errMsg string) {
	var events []E

	if db != nil {
		var err error

		events, err = read(r.Context(), db, limit)
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

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if err := json.NewEncoder(w).Encode(out); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
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
func handleDownloadEvents(db *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, downloadEventsLimit, readDownloadEvents,
			func(ev downloadEvent) downloadEventJSON { return downloadEventJSON(ev) },
			"reading download events failed")
	}
}

// handleWebUILogout serves GET /logout: it revokes the requesting browser's
// dashboard session, if any, clears its cookie, and sends it back to the
// login page.
func handleWebUILogout(sessions *sessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(webUISessionCookie); err == nil {
			sessions.revoke(c.Value)
		}

		http.SetCookie(w, sessions.cookie("", r.TLS != nil))
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// writeLoginPage writes a login page (built by renderLoginPage) to w.
func writeLoginPage(w http.ResponseWriter, page string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, page)
}

// loginPageTemplateSrc is the dashboard's login page (see renderLoginPage),
// kept in its own file so the markup lives alongside dashboardHTMLSrc rather
// than as a Go string literal.
//
//go:embed webui/login.html
var loginPageTemplateSrc string

// loginPageTemplate is loginPageTemplateSrc parsed once at package init.
// html/template's contextual autoescaping handles ErrMsg and Next itself
// (HTML-escaping ErrMsg, and applying the right escaping to Next in both the
// hidden-input attribute and the /login/oidc?next= query value), so
// renderLoginPage no longer has to escape either by hand.
var loginPageTemplate = template.Must(template.New("login.html").Parse(loginPageTemplateSrc))

// loginPageData is loginPageTemplate's input.
type loginPageData struct {
	ErrMsg       string
	Next         string
	ShowPassword bool
	ShowSSO      bool
	Version      string
	Commit       string
	Year         int
}

// renderLoginPage builds the dashboard's login page's HTML: the
// username/password form (showPassword), a "Log in with SSO" link to
// /login/oidc (showSSO), or both, stacked with a divider between them.
// errMsg can echo back a failed login attempt and next comes directly from
// the request; both are safe to embed as-is since loginPageTemplate escapes
// them contextually. next is also used for /login/oidc's own next= so SSO
// redirects to the same place the password form would.
func renderLoginPage(errMsg, next string, showPassword, showSSO bool) string {
	var buf strings.Builder

	data := loginPageData{
		ErrMsg:       errMsg,
		Next:         next,
		ShowPassword: showPassword,
		ShowSSO:      showSSO,
		Version:      version.Version,
		Commit:       version.Commit,
		Year:         time.Now().Year(),
	}
	if err := loginPageTemplate.Execute(&buf, data); err != nil {
		// loginPageTemplate is a fixed, compile-time-checked template
		// executed against a plain struct of strings/bools, so this can't
		// fail in practice; panicking here would be worse than a broken
		// page for a login attempt.
		return ""
	}

	return buf.String()
}

// handleDownloadFile serves GET /api/receivers/{id}/download/{key...}: the
// actual content of one object currently stored under receiver {id}'s path,
// for a person to save from the dashboard's file listing (see
// listReceiverFiles/handleReceiverFiles for the metadata-only listing this
// complements). Unlike the receiver API's own per-receiver JWT auth (see
// authorizeReceiver), this relies entirely on the dashboard's own login (see
// requireWebUISession, which wraps this handler in startWebUI) rather than
// any auth of its own, since the audience here is a person clicking a link
// in a browser rather than another go-backup-tool instance. db, when
// non-nil, gets every attempt appended to the download log (see
// recordDownloadEvent), win or lose, for the dashboard's "Download log"
// section (see handleDownloadEvents); a write failure there is only logged,
// not surfaced to the browser, mirroring handleWebUILogin's own tolerance
// for a login log write failure. sessions supplies the username to record
// (see sessionStore.usernameFor), best-effort: it's empty whenever the web
// UI has no login configured.
func handleDownloadFile(receivers map[string]resolvedReceiver, log *slog.Logger, db *sql.DB, sessions *sessionStore, trustProxyHeaders bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := receivers[r.PathValue("id")]
		if !ok {
			http.Error(w, "unknown receiver id", http.StatusNotFound)
			return
		}

		key, err := sanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		record := func(success bool, detail string) {
			if db == nil {
				return
			}

			ev := downloadEvent{At: time.Now(), Username: sessions.usernameFor(r), ReceiverID: recv.id, Key: key, Success: success, RemoteAddr: clientAddr(r, trustProxyHeaders), Detail: detail}
			if err := recordDownloadEvent(r.Context(), db, ev); err != nil {
				log.Warn("download: recording download event failed", "err", err)
			}
		}

		path := filepath.Join(recv.path, filepath.FromSlash(key))

		f, err := os.Open(path) //nolint:gosec // key is sanitized by sanitizeObjectKey and joined under recv.path, not attacker-controlled beyond that
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "not found", http.StatusNotFound)
				record(false, "not found")
			} else {
				log.Warn("download: opening file failed", "id", recv.id, "key", key, "err", err)
				http.Error(w, "opening file failed", http.StatusInternalServerError)
				record(false, "opening file failed")
			}

			return
		}
		defer func() { _ = f.Close() }()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(key)+`"`)

		record(true, "")

		if _, err := io.Copy(w, f); err != nil {
			log.Warn("download: streaming file failed", "id", recv.id, "key", key, "err", err)
		}
	}
}

// handleReceiveObject serves PUT /api/v1/objects/{id}/{key...}: after
// authorizing the request against receivers (see authorizeReceiver), it
// writes the request body to disk exactly as a type: local target would
// (see receiverTarget in receiver.go), so a remote target's PUT and this
// instance's own local-target writes share the same on-disk behavior
// (atomic temp-file-then-rename) and retention tracking. Every attempt is
// recorded to status, win or lose, so /api/receivers reflects it.
func handleReceiveObject(receivers map[string]resolvedReceiver, status *receiverStatusStore, log *slog.Logger, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := authorizeReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := sanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		cfg := &config{key: key, stateDB: db}
		t := receiverTarget(recv)

		if err := writeLocalObject(cfg, t, r.Body); err != nil {
			log.Warn("receiver: writing object failed", "id", recv.id, "key", key, "err", err)
			status.record(recv.id, key, err)
			recordReceiverEventBestEffort(r.Context(), db, log, recv.id, receiverEventReceive, key, 0, err)
			http.Error(w, "writing object failed", http.StatusInternalServerError)

			return
		}

		if err := recordLocalWrite(r.Context(), cfg, t, log); err != nil {
			log.Warn("receiver: retention tracking failed", "id", recv.id, "key", key, "err", err)
		}

		status.record(recv.id, key, nil)

		var size int64
		if info, statErr := os.Stat(localObjectPath(cfg, t)); statErr == nil {
			size = info.Size()
		}

		recordReceiverEventBestEffort(r.Context(), db, log, recv.id, receiverEventReceive, key, size, nil)

		log.Info("receiver: object written", "id", recv.id, "key", key, "path", localObjectPath(cfg, t))
		w.WriteHeader(http.StatusCreated)
	}
}

// handleDeleteObject serves DELETE /api/v1/objects/{id}/{key...}, the
// receiver API's client-facing counterpart to deleteRemoteObject in
// pipeline.go. Every attempt is recorded to status, win or lose, so
// /api/receivers reflects it.
func handleDeleteObject(receivers map[string]resolvedReceiver, status *receiverStatusStore, log *slog.Logger, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := authorizeReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := sanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		cfg := &config{key: key, stateDB: db}
		t := receiverTarget(recv)

		if err := deleteLocalObject(cfg, t); err != nil {
			log.Warn("receiver: deleting object failed", "id", recv.id, "key", key, "err", err)
			status.record(recv.id, key, err)
			recordReceiverEventBestEffort(r.Context(), db, log, recv.id, receiverEventDelete, key, 0, err)
			http.Error(w, "deleting object failed", http.StatusInternalServerError)

			return
		}

		if err := removeRetentionRecord(r.Context(), cfg, t); err != nil {
			log.Warn("receiver: removing retention record failed", "id", recv.id, "key", key, "err", err)
		}

		status.record(recv.id, key, nil)
		recordReceiverEventBestEffort(r.Context(), db, log, recv.id, receiverEventDelete, key, 0, nil)
		log.Info("receiver: object deleted", "id", recv.id, "key", key, "path", localObjectPath(cfg, t))
		w.WriteHeader(http.StatusNoContent)
	}
}

// requireWebUISession wraps next, requiring a currently valid dashboard
// session cookie (see sessionStore/handleWebUILogin/handleOIDCCallback)
// before running it. A missing/invalid session either redirects the browser
// to the login page (redirectOnFail true — for a full-page navigation, like
// the dashboard itself or a file download link) or reports 401
// (redirectOnFail false — for a JSON endpoint the dashboard's own
// JavaScript polls via fetch(), which would otherwise silently receive an
// HTML login page as its "JSON" response). authEnabled false (neither a
// username/password nor an OIDC provider configured) disables the check
// entirely, leaving the web UI open — this gates the dashboard and its
// /api/... endpoints (see startWebUI), not the receiver API, which
// authenticates separately via each receiver's own public-key-verified JWT.
func requireWebUISession(authEnabled bool, sessions *sessionStore, redirectOnFail bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authEnabled || sessions.authenticated(r) {
			next(w, r)
			return
		}

		if !redirectOnFail {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	}
}

// authorizeReceiver looks up the receiver named by the request's {id} path
// value and verifies its Authorization: Bearer <token> header as a JWT
// signed by that receiver's configured public-key: (see
// verifyRemoteAuthToken/signRemoteAuthToken in remoteauth.go), writing an
// error response and returning ok=false if either the id is unknown or the
// token doesn't verify.
func authorizeReceiver(w http.ResponseWriter, r *http.Request, receivers map[string]resolvedReceiver) (recv resolvedReceiver, ok bool) {
	recv, exists := receivers[r.PathValue("id")]
	if !exists {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
		return resolvedReceiver{}, false
	}

	token, hasToken := bearerToken(r)
	if !hasToken || verifyRemoteAuthToken(token, recv.publicKey, recv.id) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return resolvedReceiver{}, false
	}

	return recv, true
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// request header, reporting false if the header is missing or malformed.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}

	return strings.TrimPrefix(auth, prefix), true
}

// handleDashboard serves the static dashboard page, which polls
// /api/status itself; the page has no server-rendered state beyond whether
// its "Log out" link is shown, which is baked into html once at startup
// (see startWebUI) based on authEnabled, since a login-less deployment has
// no session for that link to end.
func handleDashboard(html string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, html)
	}
}

// logoutLinkAttr returns the dashboard's "{{LOGOUT_HIDDEN}}" substitution
// (see dashboardHTML/handleDashboard): the "Log out" link is hidden unless
// authEnabled, mirroring requireWebUISession's own gating condition.
func logoutLinkAttr(authEnabled bool) string {
	if authEnabled {
		return ""
	}

	return " hidden"
}

// dashboardHTMLSrc is the dashboard page's markup and CSS, kept in its own
// file so the web UI's HTML doesn't live as a Go string literal; the JS is
// kept separately in dashboard.js and spliced into the "{{DASHBOARD_JS}}"
// placeholder below rather than fetched by the browser as its own request,
// preserving dashboardHTML's single-self-contained-page behavior (see
// handleDashboard).
//
//go:embed webui/dashboard.html
var dashboardHTMLSrc string

//go:embed webui/dashboard.js
var dashboardJS string

// dashboardHTML is the entire web UI: a single self-contained page (no
// external assets) that polls /api/status every couple of seconds and
// re-renders the job/target table.
var dashboardHTML = strings.NewReplacer(
	"{{DASHBOARD_JS}}", dashboardJS,
	"{{VERSION}}", version.Version,
	"{{COMMIT}}", version.Commit,
	"{{COPYRIGHT_YEAR}}", strconv.Itoa(time.Now().Year()),
).Replace(dashboardHTMLSrc)
