// Package webui implements go-backup-tool's web UI: the live status
// dashboard, its login/bearer-token auth (including optional OIDC SSO, see
// oidc.go), and the read-only views of receiver state and files it shows
// alongside a job's own status. It shares one HTTP server/mux with the
// receiver API (internal/backup/receiver) via StartWebUI's
// registerExtraRoutes hook, so the composition root can mount both on the
// same listen address without this package importing that one.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
	"nilswitt.dev/go-backup-tool/internal/version"
)

// Server wraps the HTTP server behind the -listen web UI, letting
// callers shut it down cleanly (see StartWebUI/shutdown).
type Server struct {
	http *http.Server
	done chan struct{}
	addr string // the listener's actual bound address, e.g. resolved from ":0"
}

// StartWebUI binds addr and starts an HTTP server serving a live dashboard
// of store's job/target statuses (see dashboardHTML and handleStatus), plus
// the receiver API (see HandleReceiveObject/HandleDeleteObject) for any
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
// RecordLocalWrite/recordLoginEvent/recordDownloadEvent). logs backs the dashboard's
// log viewer (served over /api/logs, see handleLogs); nil starts an empty
// one, so passing the caller's own buffer only matters if the caller also
// arranged for it to be written to — which newRunLogger in app.go only does
// when the config file's enable-log-viewer: is true, so the viewer stays
// effectively empty (and its "Logs" section hidden) unless an operator opts
// in. webUIUsername/webUIPassword, when webUIUsername is non-empty, gate
// the dashboard's /api/... endpoints (including minting a per-receiver file
// download ticket) behind a login page and a bearer token the dashboard's
// own JavaScript attaches as "Authorization: Bearer <token>" on every call
// (see requireWebUISession/handleWebUILogin) — the dashboard shell itself
// (GET /) is always served, since a bearer token can't ride along on a
// plain page navigation the way a cookie could; the receiver API
// (HandleReceiveObject/HandleDeleteObject) is unaffected, since it
// authenticates each request on its own via each receiver's own
// public-key-verified JWT (see authorizeReceiver). An empty webUIUsername
// leaves the web UI open, as before this was added.
// oidcAuth, when non-nil (see newOIDCAuth in oidc.go), additionally lets a
// browser log in via that provider's own "Log in with SSO" link on the
// login page (see handleOIDCLogin/handleOIDCCallback), alongside the
// username/password form if one is also configured; either kind of login
// starts the same kind of bearer token. Login is required whenever
// webUIUsername or oidcAuth is set — either alone is enough to gate the
// dashboard's data.
// identity, when non-nil (see loadServerIdentityAtStartup in app.go), is
// served over /api/identity (see handleIdentity) for the dashboard's "Server
// identity" section, so an operator can read off this instance's UUID and
// public key without digging through its keys-dir: on disk; a nil identity
// (loadServerIdentityAtStartup failed at startup) hides that section.
func StartWebUI(addr string, store *backup.StatusStore, receivers map[string]backup.ResolvedReceiver, receiverStore *backup.ReceiverStatusStore, log *slog.Logger, db *sql.DB, logs *LogRingBuffer, webUIUsername, webUIPassword string, oidcAuth *OIDCAuth, identity *identity.ServerIdentity, trustProxyHeaders bool, registerExtraRoutes func(*http.ServeMux)) *Server {
	uiSessions, err := newSessionStore()
	if err != nil {
		log.Error("web UI: starting session store", "err", err)
		return nil
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

	downloadTickets := newDownloadTicketStore()

	// authEnabled mirrors requireWebUISession's own gating condition:
	// either a username/password or an SSO provider is enough on its own to
	// require a login before the dashboard's data is served.
	authEnabled := webUIUsername != "" || oidcAuth != nil

	// api gates a JSON endpoint the dashboard's own JavaScript calls via
	// fetch(): a missing/invalid/expired bearer token reports 401 rather
	// than redirecting, since fetch() (unlike a browser navigation) can't
	// follow a redirect into a login page and do anything useful with it —
	// see requireWebUISession. The dashboard shell (GET /) and file
	// downloads (GET /api/receivers/{id}/download/{key...}) aren't wrapped
	// in this: a plain browser navigation can never carry a bearer token,
	// so the shell is always public and downloads are authorized by a
	// one-time ticket instead (see downloadTicketStore).
	api := func(h http.HandlerFunc) http.HandlerFunc {
		return requireWebUISession(authEnabled, uiSessions, h)
	}

	dashboardPage := strings.Replace(dashboardHTML, "{{LOGOUT_HIDDEN}}", logoutLinkAttr(authEnabled), 1)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleDashboard(dashboardPage))
	mux.HandleFunc("GET /api/status", api(handleStatus(store)))
	mux.HandleFunc("GET /api/logs", api(handleLogs(logs)))
	mux.HandleFunc("GET /api/identity", api(handleIdentity(identity)))
	mux.HandleFunc("GET /api/receivers", api(handleReceiverStatus(receivers, receiverStore, log)))
	mux.HandleFunc("GET /api/receivers/{id}/files", api(handleReceiverFiles(receivers, log)))
	mux.HandleFunc("POST /api/receivers/{id}/download/{key...}", api(handleMintDownloadTicket(receivers, downloadTickets, uiSessions)))
	mux.HandleFunc("GET /api/receivers/{id}/download/{key...}", handleDownloadFile(receivers, log, db, downloadTickets, trustProxyHeaders))
	mux.HandleFunc("GET /login", handleWebUILogin(webUIUsername, webUIPassword, oidcAuth != nil, uiSessions, db, log, trustProxyHeaders))
	mux.HandleFunc("POST /login", handleWebUILogin(webUIUsername, webUIPassword, oidcAuth != nil, uiSessions, db, log, trustProxyHeaders))
	mux.HandleFunc("POST /api/logout", handleAPILogout(uiSessions))
	mux.HandleFunc("GET /api/login-events", api(handleLoginEvents(db, log)))
	mux.HandleFunc("GET /api/download-events", api(handleDownloadEvents(db, log)))

	if registerExtraRoutes != nil {
		registerExtraRoutes(mux)
	}

	if oidcAuth != nil {
		pending := newOIDCPendingStore()
		mux.HandleFunc("GET /login/oidc", handleOIDCLogin(oidcAuth, pending))
		mux.HandleFunc("GET /login/oidc/callback", handleOIDCCallback(oidcAuth, pending, uiSessions, log, db, trustProxyHeaders))
	}

	srv := &Server{
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
func handleReceiverStatus(receivers map[string]backup.ResolvedReceiver, store *backup.ReceiverStatusStore, log *slog.Logger) http.HandlerFunc {
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

// annotateReceiverStaleness fills snap's StaleAfter/Stale fields from recv's
// current state on disk, for a receiver with stale-after: set; a no-op
// otherwise. Stale mirrors staleReceiverMonitor.check's own condition — at
// least one file received, and the most recent one older than
// recv.StaleAfter — so the dashboard never disagrees with what actually
// fires the webhook. A lastReceivedAt failure is logged and leaves Stale
// false rather than failing the whole /api/receivers response over one
// receiver's directory listing.
func annotateReceiverStaleness(snap *backup.ReceiverSnapshot, recv backup.ResolvedReceiver, log *slog.Logger) {
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
func handleReceiverFiles(receivers map[string]backup.ResolvedReceiver, log *slog.Logger) http.HandlerFunc {
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
func lookupReceiver(w http.ResponseWriter, r *http.Request, receivers map[string]backup.ResolvedReceiver) (backup.ResolvedReceiver, bool) {
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

// bearerToken extracts the token from r's "Authorization: Bearer <token>"
// header, reporting false if it's missing or malformed. Mirrors the
// receiver API's own helper of the same name
// (internal/backup/receiver/receiver.go); duplicated here rather than
// shared, since it's five lines and pulling in a dependency between these
// packages for it isn't worth it.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}

	return strings.TrimPrefix(auth, prefix), true
}

// sessionTTL is how long a dashboard login remains valid — i.e. how long its
// bearer token (see sessionStore) is honored — before the browser has to log
// in again.
const sessionTTL = 12 * time.Hour

// sessionSigningKeySize is the size, in bytes, of the random HMAC key each
// sessionStore generates at startup (see newSessionStore) to sign and
// verify its own dashboard bearer tokens. 256 bits, matching the entropy of
// the opaque session ids this replaced.
const sessionSigningKeySize = 32

// sessionStore mints and verifies the dashboard's bearer tokens: signed
// JWTs (HS256, see create) whose claims — Subject (the logged-in username)
// and ID (a per-token jti) — are trusted once the signature checks out,
// without needing a server-side record of every currently valid session.
// Logout (see revoke) still needs some server-side state, since a valid
// JWT's signature alone can't be un-signed: revoke blocklists the token's
// jti instead of deleting a whole session record, and a jti past its own
// token's expiry is pruned lazily the next time isRevoked looks it up
// (never, if it's never presented again) — bounded by the logout rate over
// sessionTTL, far smaller than tracking every active session the way the
// previous opaque-token store did. key is generated fresh per process, so a
// restart invalidates every session, same as before. Safe for concurrent
// use, since login and other requests can arrive concurrently.
type sessionStore struct {
	key []byte

	mu      sync.Mutex
	revoked map[string]time.Time // jti -> that token's own expiry
}

// newSessionStore returns an empty sessionStore with a freshly generated
// signing key.
func newSessionStore() (*sessionStore, error) {
	key := make([]byte, sessionSigningKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating session signing key: %w", err)
	}

	return &sessionStore{key: key, revoked: make(map[string]time.Time)}, nil
}

// create mints a new bearer token for username, valid for sessionTTL.
func (s *sessionStore) create(username string) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("building signer: %w", err)
	}

	jti, err := randomSessionID()
	if err != nil {
		return "", err
	}

	now := time.Now()
	claims := jwt.Claims{
		Subject:  username,
		ID:       jti,
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(sessionTTL)),
	}

	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("serializing token: %w", err)
	}

	return token, nil
}

// parse verifies raw's HS256 signature against s.key and that it's
// currently unexpired, reporting its claims and ok=true only if both hold.
// It does not check revocation — see valid/usernameFor, which do.
func (s *sessionStore) parse(raw string) (jwt.Claims, bool) {
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return jwt.Claims{}, false
	}

	var claims jwt.Claims
	if err := token.Claims(s.key, &claims); err != nil {
		return jwt.Claims{}, false
	}

	if err := claims.Validate(jwt.Expected{Time: time.Now()}); err != nil {
		return jwt.Claims{}, false
	}

	return claims, true
}

// isRevoked reports whether jti was blocklisted by revoke and hasn't yet
// reached its own token's expiry, evicting it first if it has — at that
// point the token would fail parse's own expiry check anyway, so there's no
// need to keep tracking it as revoked.
func (s *sessionStore) isRevoked(jti string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	expires, ok := s.revoked[jti]
	if !ok {
		return false
	}

	if time.Now().After(expires) {
		delete(s.revoked, jti)
		return false
	}

	return true
}

// valid reports whether raw is a currently valid, non-revoked bearer token
// for s.
func (s *sessionStore) valid(raw string) bool {
	if raw == "" {
		return false
	}

	claims, ok := s.parse(raw)

	return ok && !s.isRevoked(claims.ID)
}

// usernameFor returns the username claimed by r's bearer token, for
// handlers that want to attribute an action to whoever is currently logged
// in (e.g. handleMintDownloadTicket's download ticket). It returns ""
// whenever there's no currently valid, non-revoked token — including when
// the web UI has no login configured at all, in which case every download
// is logged with an empty username rather than failing to log it.
func (s *sessionStore) usernameFor(r *http.Request) string {
	token, ok := bearerToken(r)
	if !ok {
		return ""
	}

	claims, ok := s.parse(token)
	if !ok || s.isRevoked(claims.ID) {
		return ""
	}

	return claims.Subject
}

// revoke ends the session named by bearer token raw (a no-op if it doesn't
// parse as a currently valid token — there's then nothing to blocklist),
// used by a logout handler.
func (s *sessionStore) revoke(raw string) {
	claims, ok := s.parse(raw)
	if !ok {
		return
	}

	s.mu.Lock()
	s.revoked[claims.ID] = claims.Expiry.Time()
	s.mu.Unlock()
}

// authenticated reports whether r carries a currently valid bearer token for
// s.
func (s *sessionStore) authenticated(r *http.Request) bool {
	token, ok := bearerToken(r)
	if !ok {
		return false
	}

	return s.valid(token)
}

// randomSessionID returns a 256-bit random value hex-encoded, unguessable
// enough to serve as a bearer token's jti (see sessionStore.create) or an
// OIDC state/nonce (see oidc.go).
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

// loginResponseJSON is a successful POST /login's response body: the bearer
// token the dashboard's own JavaScript should attach to every subsequent
// request (see dashboard.js) as "Authorization: Bearer <token>", and when it
// stops being valid.
type loginResponseJSON struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// loginErrorJSON is a failed POST /login's response body.
type loginErrorJSON struct {
	Error string `json:"error"`
}

// handleWebUILogin serves the dashboard's own login form (GET /login) and
// its submission (POST /login), checking username/password (webui.username/
// webui.password in the config file) with subtle.ConstantTimeCompare rather
// than ==, so a mismatch can't be timed to learn how many leading bytes
// were guessed correctly (authorizeReceiver's own auth is a JWT signature
// check, not a raw comparison, so it needs no such care). A successful
// submission starts a session (see sessionStore) and reports its token as
// JSON (loginResponseJSON) rather than a redirect: login.html's own inline
// script stores that token (see requireWebUISession) and navigates the
// browser to next (see safeNextPath) itself, since there's no cookie left
// for a server-side redirect to rely on. A failed submission likewise
// reports loginErrorJSON rather than re-rendering the page. showSSO adds a
// "Log in with SSO" link to the page (see renderLoginPage), pointing at
// /login/oidc, whenever oidc.enabled is set (see StartWebUI) —
// independently of whether a username/password is also configured. An empty
// username with showSSO false (neither kind of login configured) redirects
// straight to next rather than showing a form there's no way to satisfy; an
// empty username with showSSO true shows the page with only the SSO link,
// and POST /login (which only the password form submits) 404s in that case,
// since there's no username/password to check. db, when non-nil, gets every
// submitted attempt appended to the login log (see recordLoginEvent), win or
// lose, for the dashboard's login log view (see handleLoginEvents); a write
// failure there is only logged, not surfaced to the browser, since it must
// never block an otherwise-successful login.
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

		detail := ""
		if !success {
			detail = "incorrect username or password"
		}

		recordLogin(r.Context(), db, log, r, trustProxyHeaders, "password", "web UI", submittedUser, detail, success)

		if !success {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(loginErrorJSON{Error: "incorrect username or password"})

			return
		}

		id, err := sessions.create(submittedUser)
		if err != nil {
			http.Error(w, "starting session failed", http.StatusInternalServerError)
			return
		}

		writeJSON(w, loginResponseJSON{Token: id, ExpiresAt: time.Now().Add(sessionTTL)})
	}
}

// recordLogin appends one dashboard login attempt to db's login log (see
// backup.RecordLoginEvent) with method/username/detail/success, warning via
// log (tagged with source, e.g. "web UI" or "oidc") rather than failing the
// caller's request if the write itself fails — a login must never be
// blocked by an audit-log hiccup. A nil db is a no-op, matching
// StartWebUI's optional db. Shared by handleWebUILogin and oidc.go's
// handleOIDCCallback, which otherwise duplicate this event-building/
// recording/warn-on-failure sequence.
func recordLogin(ctx context.Context, db *sql.DB, log *slog.Logger, r *http.Request, trustProxyHeaders bool, method, source, username, detail string, success bool) {
	if db == nil {
		return
	}

	ev := backup.LoginEvent{At: time.Now(), Username: username, Method: method, Success: success, RemoteAddr: clientAddr(r, trustProxyHeaders), Detail: detail}
	if err := backup.RecordLoginEvent(ctx, db, ev); err != nil {
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
func handleLoginEvents(db *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, loginEventsLimit, backup.ReadLoginEvents,
			func(ev backup.LoginEvent) loginEventJSON { return loginEventJSON(ev) },
			"reading login events failed")
	}
}

// serveEventLog writes a JSON array of the most recently recorded audit-log
// events (see readLoginEvents/readDownloadEvents), newest first, converting
// each raw event E to its wire shape J via toJSON. db nil (state tracking
// unavailable) serves an empty list rather than failing the request, since
// the state db is optional (see StartWebUI). Shared by handleLoginEvents
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
func handleDownloadEvents(db *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, downloadEventsLimit, backup.ReadDownloadEvents,
			func(ev backup.DownloadEvent) downloadEventJSON { return downloadEventJSON(ev) },
			"reading download events failed")
	}
}

// handleAPILogout serves POST /api/logout: it revokes the bearer token
// carried in the request's Authorization header, if any — a missing or
// already-invalid one is a no-op, since there's nothing to revoke — and
// always reports success. Unlike the old cookie-based logout, this doesn't
// redirect anywhere: the dashboard's own JavaScript (see dashboard.js) calls
// this, then clears its locally stored token and navigates to /login itself.
func handleAPILogout(sessions *sessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r); ok {
			sessions.revoke(token)
		}

		w.WriteHeader(http.StatusNoContent)
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
//go:embed login.html
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
	id, err := randomSessionID()
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

// downloadTicketJSON is a freshly minted download ticket's wire shape (see
// handleMintDownloadTicket).
type downloadTicketJSON struct {
	Ticket string `json:"ticket"`
}

// handleMintDownloadTicket serves POST /api/receivers/{id}/download/{key...}:
// it mints a short-lived, single-use download ticket (see
// downloadTicketStore) for the receiver/key named by the path, attributed to
// whoever is currently logged in (see sessionStore.usernameFor). The
// dashboard's JS calls this — with its Authorization: Bearer header — right
// before navigating the browser to the matching GET, which can't carry that
// header itself (see handleDownloadFile).
func handleMintDownloadTicket(receivers map[string]backup.ResolvedReceiver, tickets *downloadTicketStore, sessions *sessionStore) http.HandlerFunc {
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

		ticket, err := tickets.create(recv.ID, key, sessions.usernameFor(r))
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
// requireWebUISession), this is authorized by a one-time download ticket
// (see downloadTicketStore) rather than a bearer token: the request behind
// this is a plain browser navigation, which can't carry an Authorization
// header the way the dashboard's own fetch() calls can (see
// handleMintDownloadTicket, which the dashboard's JS calls first to obtain
// one). db, when non-nil, gets every attempt appended to the download log
// (see recordDownloadEvent), win or lose, for the dashboard's "Download log"
// section (see handleDownloadEvents); a write failure there is only logged,
// not surfaced to the browser, mirroring handleWebUILogin's own tolerance
// for a login log write failure.
func handleDownloadFile(receivers map[string]backup.ResolvedReceiver, log *slog.Logger, db *sql.DB, tickets *downloadTicketStore, trustProxyHeaders bool) http.HandlerFunc {
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

			ev := backup.DownloadEvent{At: time.Now(), Username: username, ReceiverID: recv.ID, Key: key, Success: success, RemoteAddr: clientAddr(r, trustProxyHeaders), Detail: detail}
			if err := backup.RecordDownloadEvent(r.Context(), db, ev); err != nil {
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

		if _, err := io.Copy(w, f); err != nil {
			log.Warn("download: streaming file failed", "id", recv.ID, "key", key, "err", err)
		}
	}
}

// requireWebUISession wraps next, requiring a currently valid bearer token
// (see sessionStore/handleWebUILogin/handleOIDCCallback, and bearerToken for
// how it's read off the request) before running it. A missing, invalid, or
// expired token reports 401 — there's no server-side redirect to a login
// page any more, since every request this gates is a fetch() call from the
// dashboard's own JavaScript (see dashboard.js), which reads that response
// itself and sends the browser to /login client-side. authEnabled false
// (neither a username/password nor an OIDC provider configured) disables
// the check entirely, leaving the web UI open — this gates the dashboard's
// /api/... endpoints (see StartWebUI), not the receiver API, which
// authenticates separately via each receiver's own public-key-verified JWT,
// nor file downloads, which are authorized by a one-time download ticket
// instead (see downloadTicketStore) since that request is a plain browser
// navigation rather than a fetch() call.
func requireWebUISession(authEnabled bool, sessions *sessionStore, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authEnabled || sessions.authenticated(r) {
			next(w, r)
			return
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// handleDashboard serves the static dashboard page, which polls
// /api/status itself; the page has no server-rendered state beyond whether
// its "Log out" link is shown, which is baked into html once at startup
// (see StartWebUI) based on authEnabled, since a login-less deployment has
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
//go:embed dashboard.html
var dashboardHTMLSrc string

//go:embed dashboard.js
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
