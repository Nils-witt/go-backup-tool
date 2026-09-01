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
	"crypto/rsa"
	"crypto/subtle"
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
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
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
func StartWebUI(addr string, statusStore *backup.StatusStore, receivers map[string]config.ResolvedReceiver, receiverStore *backup.ReceiverStatusStore, log *slog.Logger, db *store.Store, logs *LogRingBuffer, webUIUsername, webUIPassword string, oidcAuth *OIDCAuth, identity *identity.ServerIdentity, trustProxyHeaders bool, registerExtraRoutes func(*http.ServeMux)) *Server {
	uiSessions, err := newSessionStore(identity, db)
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

	authEnabled := webUIUsername != "" || oidcAuth != nil

	// authOnly gates a JSON endpoint the dashboard's own JavaScript calls
	// via fetch() behind nothing more than a currently valid session: a
	// missing/invalid/expired bearer token reports 401 rather than
	// redirecting, since fetch() (unlike a browser navigation) can't follow
	// a redirect into a login page and do anything useful with it — see
	// requireWebUISession. The dashboard shell (GET /) and file downloads
	// (GET /api/receivers/{id}/download/{key...}) aren't wrapped in this: a
	// plain browser navigation can never carry a bearer token, so the shell
	// is always public and downloads are authorized by a one-time ticket
	// instead (see downloadTicketStore).
	authOnly := func(h http.HandlerFunc) http.HandlerFunc {
		return requireWebUISession(authEnabled, uiSessions, h)
	}

	// api additionally requires the session hold permission.PermissionView (see
	// requirePermission) — every endpoint below except /api/session (any
	// authenticated session, regardless of its permissions, needs to be
	// able to read its own), the login/download history endpoints (see
	// apiLoginLog/apiDownloadLog below, their own dedicated permissions
	// instead), and the "Users" admin endpoints (see admin below,
	// requireAdmin's own gate instead).
	api := func(h http.HandlerFunc) http.HandlerFunc {
		return authOnly(requirePermission(authEnabled, uiSessions, permission.PermissionView, h))
	}

	// apiDownload requires permission.PermissionDownload instead of View,
	// gating handleMintDownloadTicket — the one step in the download flow a
	// bearer token actually authorizes (see downloadTicketStore's own doc
	// comment for why the second, actual download request can't be gated
	// the same way).
	apiDownload := func(h http.HandlerFunc) http.HandlerFunc {
		return authOnly(requirePermission(authEnabled, uiSessions, permission.PermissionDownload, h))
	}

	// apiLoginLog and apiDownloadLog gate the login/download history
	// endpoints on their own dedicated permissions (see
	// permission.PermissionViewLoginLog/PermissionViewDownloadLog) rather than
	// api's permission.PermissionView — a session can see the rest of the
	// dashboard without being able to see either history, and vice versa.
	apiLoginLog := func(h http.HandlerFunc) http.HandlerFunc {
		return authOnly(requirePermission(authEnabled, uiSessions, permission.PermissionViewLoginLog, h))
	}
	apiDownloadLog := func(h http.HandlerFunc) http.HandlerFunc {
		return authOnly(requirePermission(authEnabled, uiSessions, permission.PermissionViewDownloadLog, h))
	}

	// admin requires the session belong to the config-file admin (see
	// requireAdmin), gating the "Users" admin section's own endpoints.
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return authOnly(requireAdmin(authEnabled, uiSessions, webUIUsername, h))
	}

	dashboardPage := strings.Replace(dashboardHTML, "{{LOGOUT_HIDDEN}}", logoutLinkAttr(authEnabled), 1)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleDashboard(dashboardPage))
	mux.HandleFunc("GET /api/session", authOnly(handleSessionInfo(uiSessions, authEnabled, webUIUsername, oidcAuth != nil)))
	mux.HandleFunc("GET /api/status", api(handleStatus(statusStore)))
	mux.HandleFunc("GET /api/logs", api(handleLogs(logs)))
	mux.HandleFunc("GET /api/identity", api(handleIdentity(identity)))
	mux.HandleFunc("GET /api/receivers", api(handleReceiverStatus(receivers, receiverStore, log)))
	mux.HandleFunc("GET /api/job-runs", api(handleJobRunEvents(db, log)))
	mux.HandleFunc("GET /api/target-runs", api(handleTargetRunEvents(db, log)))
	mux.HandleFunc("GET /api/receivers/{id}/files", api(handleReceiverFiles(receivers, log)))
	mux.HandleFunc("POST /api/receivers/{id}/download/{key...}", apiDownload(handleMintDownloadTicket(receivers, downloadTickets, uiSessions)))
	mux.HandleFunc("GET /api/receivers/{id}/download/{key...}", handleDownloadFile(receivers, log, db, downloadTickets, trustProxyHeaders))
	mux.HandleFunc("GET /login", handleWebUILogin(webUIUsername, webUIPassword, oidcAuth != nil, uiSessions, db, log, trustProxyHeaders))
	mux.HandleFunc("POST /login", handleWebUILogin(webUIUsername, webUIPassword, oidcAuth != nil, uiSessions, db, log, trustProxyHeaders))
	mux.HandleFunc("POST /api/logout", handleAPILogout(uiSessions, db, log))
	mux.HandleFunc("GET /api/login-events", apiLoginLog(handleLoginEvents(db, log)))
	mux.HandleFunc("GET /api/download-events", apiDownloadLog(handleDownloadEvents(db, log)))
	mux.HandleFunc("GET /api/users", admin(handleListWebUIUsers(db, log)))
	mux.HandleFunc("POST /api/users", admin(handleCreateWebUIUser(db, webUIUsername, log)))
	mux.HandleFunc("PUT /api/users/{username}", admin(handleUpdateWebUIUser(db, log)))
	mux.HandleFunc("DELETE /api/users/{username}", admin(handleDeleteWebUIUser(db, log)))
	mux.HandleFunc("POST /api/users/{username}/tokens", admin(handleIssueWebUIUserToken(uiSessions, db, log)))
	mux.HandleFunc("GET /api/users/{username}/tokens", admin(handleListWebUIUserTokens(db, log)))
	mux.HandleFunc("DELETE /api/users/{username}/tokens/{jti}", admin(handleRevokeWebUIUserToken(uiSessions, db, log)))
	mux.HandleFunc("GET /api/oidc-users", admin(handleListOIDCUserPermissions(db, log)))
	mux.HandleFunc("PUT /api/oidc-users/{identity...}", admin(handleSetOIDCUserPermissions(db, log)))
	mux.HandleFunc("DELETE /api/oidc-users/{identity...}", admin(handleDeleteOIDCUserPermissions(db, log)))

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

// sessionStore mints and verifies the dashboard's bearer tokens: signed
// JWTs (RS256, see create) whose claims — Subject (the logged-in username)
// and ID (a per-token jti) — are trusted once the signature checks out,
// without needing a server-side record of every currently valid session.
// Logout (see revoke) still needs some server-side state, since a valid
// JWT's signature alone can't be un-signed: revoke blocklists the token's
// jti instead of deleting a whole session record, and a jti past its own
// token's expiry is pruned lazily the next time isRevoked looks it up
// (never, if it's never presented again) — bounded by the logout rate over
// sessionTTL, far smaller than tracking every active session the way the
// previous opaque-token store did. privateKey is this instance's own
// persistent RSA key (see newSessionStore) rather than a random key
// generated fresh per process, so unlike the HS256 scheme this replaced, a
// restart no longer invalidates every outstanding session. db, when
// non-nil, backs a long-lived API token's revocation (see revokeJTI and
// db.RevokeAPIToken) so that, unlike an interactive session's, it
// survives a restart: newSessionStore preloads the in-memory revoked map
// below from it, since a long-lived token (up to maxAPITokenDays) can
// easily outlive the process that revoked it. Safe for concurrent use,
// since login and other requests can arrive concurrently.
type sessionStore struct {
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	db         *store.Store

	mu      sync.Mutex
	revoked map[string]time.Time // jti -> that token's own expiry
}

// newSessionStore returns a sessionStore that signs/verifies dashboard
// bearer tokens with id's persistent RSA key pair (the same
// key — ensured to exist on disk at startup, see
// identity.LoadServerIdentityAtStartup — that SignRequest uses for
// outgoing remote-target requests), so sessions survive a process restart.
// If id is nil (e.g. a caller that doesn't wire up a server identity, such
// as some tests), a fresh key pair is generated instead, matching this
// store's previous per-process-random-key behavior. db, when non-nil, is
// used to preload the in-memory revocation blocklist with every
// currently-revoked, not-yet-expired long-lived API token (see
// db.ListRevokedAPITokens) — otherwise a revocation made before a
// restart would be forgotten, since a JWT's signature alone can't be
// un-signed and the blocklist itself only lives in memory once loaded.
func newSessionStore(id *identity.ServerIdentity, db *store.Store) (*sessionStore, error) {
	var key *rsa.PrivateKey

	if id != nil {
		key = id.PrivateKey()
	} else {
		generated, err := rsa.GenerateKey(rand.Reader, identity.ServerKeyBits)
		if err != nil {
			return nil, fmt.Errorf("generating session signing key: %w", err)
		}

		key = generated
	}

	revoked := make(map[string]time.Time)

	if db != nil {
		tokens, err := db.ListRevokedAPITokens(context.Background(), time.Now())
		if err != nil {
			return nil, fmt.Errorf("loading revoked API tokens: %w", err)
		}

		for _, t := range tokens {
			revoked[t.JTI] = t.ExpiresAt
		}
	}

	return &sessionStore{privateKey: key, publicKey: &key.PublicKey, db: db, revoked: revoked}, nil
}

// sessionClaims is the private claim a bearer token carries alongside the
// standard jwt.Claims (see create/parse): the permissions granted at login
// time (see permission.Permission), resolved once from whichever login path
// authenticated the session (the config-file admin, an OIDC provider's
// default, or a web UI "Users" admin-managed account — see
// handleWebUILogin/handleOIDCCallback) and then trusted for the token's
// whole lifetime, the same way Subject/ID are. A later change to that
// account's stored permissions (see UpdateWebUIUserPermissions) therefore
// only takes effect on that account's next login, not retroactively —
// matching how a password change doesn't invalidate already-issued
// sessions either.
type sessionClaims struct {
	Perm permission.Permission `json:"perm"`
}

// create mints a new bearer token for username granting perm, valid for
// sessionTTL.
func (s *sessionStore) create(username string, perm permission.Permission) (string, error) {
	token, _, err := s.createWithTTL(username, perm, sessionTTL)
	return token, err
}

// createWithTTL is create, generalized to a caller-chosen validity period —
// sessionTTL for a normal interactive login (see create), or an
// admin-chosen, much longer one for a long-lived API token (see
// handleIssueWebUIUserToken, which also records the returned jti via
// db.SaveAPIToken so it can later be looked up and revoked without
// the raw token itself).
func (s *sessionStore) createWithTTL(username string, perm permission.Permission, ttl time.Duration) (token, jti string, err error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: s.privateKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", "", fmt.Errorf("building signer: %w", err)
	}

	jti, err = randomSessionID()
	if err != nil {
		return "", "", err
	}

	now := time.Now()
	claims := jwt.Claims{
		Subject:  username,
		ID:       jti,
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(ttl)),
	}

	token, err = jwt.Signed(signer).Claims(claims).Claims(sessionClaims{Perm: perm}).Serialize()
	if err != nil {
		return "", "", fmt.Errorf("serializing token: %w", err)
	}

	return token, jti, nil
}

// parse verifies raw's RS256 signature against s.publicKey and that it's
// currently unexpired, reporting its standard and private claims (see
// sessionClaims) and ok=true only if both hold. It does not check
// revocation — see valid/usernameFor/permissionsFor, which do.
func (s *sessionStore) parse(raw string) (jwt.Claims, sessionClaims, bool) {
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return jwt.Claims{}, sessionClaims{}, false
	}

	var (
		claims jwt.Claims
		sc     sessionClaims
	)

	if err := token.Claims(s.publicKey, &claims, &sc); err != nil {
		return jwt.Claims{}, sessionClaims{}, false
	}

	if err := claims.Validate(jwt.Expected{Time: time.Now()}); err != nil {
		return jwt.Claims{}, sessionClaims{}, false
	}

	return claims, sc, true
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

	claims, _, ok := s.parse(raw)

	return ok && !s.isRevoked(claims.ID)
}

// current looks up r's bearer token and returns its claims, reporting
// ok=true only if it's present, currently valid (see parse), and not
// revoked. Shared by every method below that needs a request's currently
// authenticated session.
func (s *sessionStore) current(r *http.Request) (jwt.Claims, sessionClaims, bool) {
	token, ok := bearerToken(r)
	if !ok {
		return jwt.Claims{}, sessionClaims{}, false
	}

	claims, sc, ok := s.parse(token)
	if !ok || s.isRevoked(claims.ID) {
		return jwt.Claims{}, sessionClaims{}, false
	}

	return claims, sc, true
}

// usernameFor returns the username claimed by r's bearer token, for
// handlers that want to attribute an action to whoever is currently logged
// in (e.g. handleMintDownloadTicket's download ticket). It returns ""
// whenever there's no currently valid, non-revoked token — including when
// the web UI has no login configured at all, in which case every download
// is logged with an empty username rather than failing to log it.
func (s *sessionStore) usernameFor(r *http.Request) string {
	claims, _, ok := s.current(r)
	if !ok {
		return ""
	}

	return claims.Subject
}

// permissionsFor returns the permissions granted to r's bearer token at the
// time it was minted (see sessionClaims), or 0 (no permissions) whenever
// there's no currently valid, non-revoked token.
func (s *sessionStore) permissionsFor(r *http.Request) permission.Permission {
	_, sc, ok := s.current(r)
	if !ok {
		return 0
	}

	return sc.Perm
}

// revoke ends the session named by bearer token raw, reporting its jti and
// ok=true — or ok=false, a no-op, if raw doesn't parse as a currently valid
// token, since there's then nothing to blocklist. Used by handleAPILogout,
// which — when raw names a recorded long-lived API token (see
// db.RevokeAPIToken) rather than an ordinary interactive session, which
// is never recorded — also persists the revocation so it survives a
// restart (see newSessionStore's own preload of s.revoked).
func (s *sessionStore) revoke(raw string) (jti string, ok bool) {
	claims, _, ok := s.parse(raw)
	if !ok {
		return "", false
	}

	s.revokeJTI(claims.ID, claims.Expiry.Time())

	return claims.ID, true
}

// revokeJTI blocklists jti in s's in-memory revocation map until expires,
// the token's own expiry — shared by revoke (which parses expires out of a
// raw token) and handleRevokeWebUIUserToken (which already has it from the
// recorded store.APIToken row, with no raw token in hand to parse).
func (s *sessionStore) revokeJTI(jti string, expires time.Time) {
	s.mu.Lock()
	s.revoked[jti] = expires
	s.mu.Unlock()
}

// authenticated reports whether r carries a currently valid, non-revoked
// bearer token for s.
func (s *sessionStore) authenticated(r *http.Request) bool {
	_, _, ok := s.current(r)
	return ok
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
// its submission (POST /login). A submission is checked two ways, in order:
// first against the single config-file admin (webui.username/webui.password),
// using subtle.ConstantTimeCompare rather than == so a mismatch can't be
// timed to learn how many leading bytes were guessed correctly
// (authorizeReceiver's own auth is a JWT signature check, not a raw
// comparison, so it needs no such care) — a match grants full access, every
// permission.Permission bit set explicitly rather than just PermissionAdmin
// (see the perm assignment below for why); then, if that didn't match and
// db is non-nil,
// against the web UI's "Users" admin-managed accounts (see
// db.VerifyWebUIUser/webusers.go), whose own granted permissions are
// used instead. Note that in practice a "Users" admin-managed account can
// only exist once an operator has used the config-file admin to create one
// (see handleCreateWebUIUser) — so this second check only ever matters
// when the config-file admin is also configured. A successful submission
// starts a session (see sessionStore) carrying whichever permissions
// matched and reports its token as JSON (loginResponseJSON) rather than a
// redirect: login.html's own inline script stores that token (see
// requireWebUISession) and navigates the browser to next (see
// safeNextPath) itself, since there's no cookie left for a server-side
// redirect to rely on. A failed submission likewise reports loginErrorJSON
// rather than re-rendering the page. showSSO adds a "Log in with SSO" link
// to the page (see renderLoginPage), pointing at /login/oidc, whenever
// oidc.enabled is set (see StartWebUI) — independently of whether a
// username/password is also configured. An empty username with showSSO
// false (neither kind of login configured) redirects straight to next
// rather than showing a form there's no way to satisfy; an empty username
// with showSSO true shows the page with only the SSO link, and POST /login
// (which only the password form submits) 404s in that case, since there's
// no username/password to check. db, when non-nil, gets every submitted
// attempt appended to the login log (see recordLoginEvent), win or lose,
// for the dashboard's login log view (see handleLoginEvents); a write
// failure there is only logged, not surfaced to the browser, since it must
// never block an otherwise-successful login.
func handleWebUILogin(username, password string, showSSO bool, sessions *sessionStore, db *store.Store, log *slog.Logger, trustProxyHeaders bool) http.HandlerFunc {
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
		submittedPass := r.FormValue("password")
		userMatch := subtle.ConstantTimeCompare([]byte(submittedUser), []byte(username)) == 1
		passMatch := subtle.ConstantTimeCompare([]byte(submittedPass), []byte(password)) == 1

		success := userMatch && passMatch
		// Every bit, not just PermissionAdmin (which alone would already
		// imply the rest via Can*) — sessionInfoJSON.Permissions only lists
		// directly-granted names (see Permission.Names), and the
		// dashboard's own JavaScript checks that list literally (e.g.
		// canDownload), so a session meant to look and behave like full
		// access needs every bit set explicitly, matching handleSessionInfo's
		// own authEnabled-false case below.
		perm := permission.PermissionView | permission.PermissionDownload | permission.PermissionAdmin | permission.PermissionViewLoginLog | permission.PermissionViewDownloadLog

		if !success && db != nil {
			dbPerm, ok, err := db.VerifyWebUIUser(r.Context(), submittedUser, submittedPass)
			if err != nil {
				log.Warn("web UI: verifying db user failed", "err", err)
			} else if ok {
				success, perm = true, dbPerm
			}
		}

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

		id, err := sessions.create(submittedUser, perm)
		if err != nil {
			http.Error(w, "starting session failed", http.StatusInternalServerError)
			return
		}

		writeJSON(w, loginResponseJSON{Token: id, ExpiresAt: time.Now().Add(sessionTTL)})
	}
}

// recordLogin appends one dashboard login attempt to db's login log (see
// db.SaveLoginEvent) with method/username/detail/success, warning via
// log (tagged with source, e.g. "web UI" or "oidc") rather than failing the
// caller's request if the write itself fails — a login must never be
// blocked by an audit-log hiccup. A nil db is a no-op, matching
// StartWebUI's optional db. Shared by handleWebUILogin and oidc.go's
// handleOIDCCallback, which otherwise duplicate this event-building/
// recording/warn-on-failure sequence.
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

// handleAPILogout serves POST /api/logout: it revokes the bearer token
// carried in the request's Authorization header, if any — a missing or
// already-invalid one is a no-op, since there's nothing to revoke — and
// always reports success. Unlike the old cookie-based logout, this doesn't
// redirect anywhere: the dashboard's own JavaScript (see dashboard.js) calls
// this, then clears its locally stored token and navigates to /login itself.
// If the revoked token happens to be a recorded long-lived API token (see
// db.SaveAPIToken) rather than an ordinary interactive session — e.g.
// a script logging its own token out — its revocation is also persisted
// (see db.RevokeAPIToken) so the "Users" admin section's token listing
// reflects it and it survives a restart; db nil, or the token simply not
// being a recorded one (store.ErrAPITokenNotFound), are both silently
// ignored, since an ordinary session logging out is the common case.
func handleAPILogout(sessions *sessionStore, db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r); ok {
			if jti, ok := sessions.revoke(token); ok && db != nil {
				if _, err := db.RevokeAPIToken(r.Context(), jti, time.Now()); err != nil && !errors.Is(err, store.ErrAPITokenNotFound) {
					log.Warn("web UI: persisting token revocation failed", "err", err)
				}
			}
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// sessionInfoJSON is the currently authenticated session's wire shape (see
// handleSessionInfo), for the dashboard's own JavaScript to decide what to
// show: a download link/button only when Permissions includes "download",
// the login history only when Permissions includes "login-log", the
// download history only when Permissions includes "download-log", the
// "Users" admin section only when Admin, and its "OIDC users" listing
// (permission overrides for SSO logins) only when both Admin and
// OIDCEnabled.
type sessionInfoJSON struct {
	Username    string   `json:"username"`
	Permissions []string `json:"permissions"`
	Admin       bool     `json:"admin"`
	OIDCEnabled bool     `json:"oidc_enabled"`
}

// handleSessionInfo serves GET /api/session: the currently authenticated
// session's own username, granted permissions, whether it can reach the
// "Users" admin section — either as the config-file admin or by holding
// permission.PermissionAdmin (see requireAdmin) — and whether OIDC SSO is
// configured at all — everything the dashboard's own JavaScript needs to
// gate which sections it shows, since the server-side handlers behind those
// sections already enforce the same rules on every actual request.
// authEnabled false reports full access, matching every other endpoint's
// bypass in that case (see requireWebUISession).
func handleSessionInfo(sessions *sessionStore, authEnabled bool, adminUsername string, oidcEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authEnabled {
			writeJSON(w, sessionInfoJSON{Permissions: (permission.PermissionView | permission.PermissionDownload | permission.PermissionAdmin | permission.PermissionViewLoginLog | permission.PermissionViewDownloadLog).Names(), Admin: true, OIDCEnabled: oidcEnabled})
			return
		}

		username := sessions.usernameFor(r)
		perm := sessions.permissionsFor(r)

		writeJSON(w, sessionInfoJSON{
			Username:    username,
			Permissions: perm.Names(),
			Admin:       (username != "" && username == adminUsername) || perm.CanAdmin(),
			OIDCEnabled: oidcEnabled,
		})
	}
}

// webUIUserJSON is one store.WebUIUser's wire shape for the "Users" admin
// API (handleListWebUIUsers/handleCreateWebUIUser), matching the
// dashboard's own field naming (snake_case, as every other /api/...
// endpoint here uses). It never carries a password: handleListWebUIUsers
// doesn't have one to serve (see store.WebUIUser), and
// handleCreateWebUIUser/handleUpdateWebUIUser take one only in their own
// request body, write-only.
type webUIUserJSON struct {
	Username    string    `json:"username"`
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"created_at"`
}

// handleListJSON adapts a store.Store List* method value (e.g.
// db.ListWebUIUsers) into a GET handler: an empty JSON array when db is
// nil, a 500 on error (logged with errMsg), otherwise each item run through
// convert and written as JSON. Shared by handleListWebUIUsers and
// handleListOIDCUserPermissions, whose only difference is the row type and
// conversion.
func handleListJSON[T, S any](db *store.Store, log *slog.Logger, errMsg string, list func(context.Context) ([]S, error), convert func(S) T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeJSON(w, []T{})
			return
		}

		items, err := list(r.Context())
		if err != nil {
			log.Warn("web UI: "+errMsg, "err", err)
			http.Error(w, errMsg, http.StatusInternalServerError)

			return
		}

		out := make([]T, len(items))
		for i, it := range items {
			out[i] = convert(it)
		}

		writeJSON(w, out)
	}
}

// handleListWebUIUsers serves GET /api/users: every web UI "Users"
// admin-managed account, for the dashboard's "Users" admin section —
// requireAdmin (see StartWebUI) restricts this to the config-file admin.
func handleListWebUIUsers(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return handleListJSON(db, log, "listing users failed", db.ListWebUIUsers, func(u store.WebUIUser) webUIUserJSON {
		return webUIUserJSON{Username: u.Username, Permissions: u.Permissions.Names(), CreatedAt: u.CreatedAt}
	})
}

// webUIUserRequestJSON is handleCreateWebUIUser/handleUpdateWebUIUser's
// request body: Username is only used (and required) by
// handleCreateWebUIUser, which takes it from the body rather than the path
// the way handleUpdateWebUIUser's PUT /api/users/{username} does, since
// there's no username in a POST /api/users path to route on yet. Password
// is required for handleCreateWebUIUser but optional for
// handleUpdateWebUIUser, which leaves the stored password unchanged when
// it's omitted.
type webUIUserRequestJSON struct {
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	Permissions []string `json:"permissions"`
}

// handleCreateWebUIUser serves POST /api/users: it adds a new web UI
// "Users" admin-managed account from the request body (see
// webUIUserRequestJSON), rejecting a username that collides with the
// config-file admin's own (adminUsername) — that account's credentials
// live in the config file, not this table, so it must never be shadowed
// here — or one that's already taken (store.ErrWebUIUserExists).
func handleCreateWebUIUser(db *store.Store, adminUsername string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "user management requires the job state db", http.StatusServiceUnavailable)
			return
		}

		var req webUIUserRequestJSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if req.Username == "" || req.Password == "" {
			http.Error(w, "username and password are required", http.StatusBadRequest)
			return
		}

		if adminUsername != "" && req.Username == adminUsername {
			http.Error(w, "username is reserved for the configured admin account", http.StatusBadRequest)
			return
		}

		perm, err := permission.ParsePermissions(req.Permissions)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		switch err := db.SaveWebUIUser(r.Context(), req.Username, req.Password, perm); {
		case errors.Is(err, store.ErrWebUIUserExists):
			http.Error(w, "user already exists", http.StatusConflict)
		case err != nil:
			log.Warn("web UI: creating user failed", "err", err)
			http.Error(w, "creating user failed", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}
}

// handleUpdateWebUIUser serves PUT /api/users/{username}: it updates the
// named web UI "Users" admin-managed account's permissions from the
// request body (see webUIUserRequestJSON), and its password too if one was
// given (a blank Password leaves the stored one unchanged, so the
// dashboard's edit form doesn't have to re-submit it on every permission
// change).
func handleUpdateWebUIUser(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "user management requires the job state db", http.StatusServiceUnavailable)
			return
		}

		username := r.PathValue("username")

		var req webUIUserRequestJSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		perm, err := permission.ParsePermissions(req.Permissions)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := db.UpdateWebUIUserPermissions(r.Context(), username, perm); handleWebUIUserErr(w, log, "updating", err) {
			return
		}

		if req.Password != "" {
			if err := db.UpdateWebUIUserPassword(r.Context(), username, req.Password); handleWebUIUserErr(w, log, "updating", err) {
				return
			}
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// handleDeleteWebUIUser serves DELETE /api/users/{username}: it removes the
// named web UI "Users" admin-managed account.
func handleDeleteWebUIUser(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "user management requires the job state db", http.StatusServiceUnavailable)
			return
		}

		if err := db.DeleteWebUIUser(r.Context(), r.PathValue("username")); handleWebUIUserErr(w, log, "deleting", err) {
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// handleWebUIUserErr writes the right response for err, as returned by one
// of the backup.*WebUIUser* functions handleUpdateWebUIUser/
// handleDeleteWebUIUser call — store.ErrWebUIUserNotFound as 404, any
// other error as a logged 500 — and reports whether it wrote one at all
// (err == nil), so those handlers can `if handleWebUIUserErr(...) { return }`
// rather than repeating this switch themselves.
func handleWebUIUserErr(w http.ResponseWriter, log *slog.Logger, verb string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrWebUIUserNotFound):
		http.Error(w, "user not found", http.StatusNotFound)
	default:
		log.Warn("web UI: "+verb+" user failed", "err", err)
		http.Error(w, verb+" user failed", http.StatusInternalServerError)
	}

	return true
}

// minAPITokenDays/maxAPITokenDays bound how many days an admin can request
// handleIssueWebUIUserToken mint a long-lived API token for: at least a
// day, so it's meaningfully longer-lived than an interactive session
// (sessionTTL, 12 hours), and at most ten years, since a token revoked
// before its expiry still occupies sessionStore's in-memory revocation list
// for the rest of its claimed lifetime.
const (
	minAPITokenDays = 1
	maxAPITokenDays = 3650
)

// apiTokenRequestJSON is handleIssueWebUIUserToken's request body: how many
// days the minted token should stay valid for, clamped to
// [minAPITokenDays, maxAPITokenDays].
type apiTokenRequestJSON struct {
	Days int `json:"days"`
}

// handleIssueWebUIUserToken serves POST /api/users/{username}/tokens: it
// mints a long-lived bearer token (see sessionStore.createWithTTL) for the
// named "Users" admin-managed account, carrying that account's currently
// granted permissions (see db.GetWebUIUser) — the same kind of token a
// normal login produces, just valid for Days days instead of sessionTTL, for
// scripts/automation that can't sit through an interactive login. Returned
// as loginResponseJSON, the same shape POST /login uses, since it's the same
// kind of token; unlike an interactive session, the minted token's jti is
// also recorded (see db.SaveAPIToken) so an admin can later revoke it
// (see handleRevokeWebUIUserToken) without needing to hold the raw token
// itself — the whole point, since it's shown here once and never again.
func handleIssueWebUIUserToken(sessions *sessionStore, db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "user management requires the job state db", http.StatusServiceUnavailable)
			return
		}

		username := r.PathValue("username")

		var req apiTokenRequestJSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if req.Days < minAPITokenDays || req.Days > maxAPITokenDays {
			http.Error(w, fmt.Sprintf("days must be between %d and %d", minAPITokenDays, maxAPITokenDays), http.StatusBadRequest)
			return
		}

		user, ok, err := db.GetWebUIUser(r.Context(), username)
		if err != nil {
			log.Warn("web UI: looking up user failed", "err", err)
			http.Error(w, "issuing token failed", http.StatusInternalServerError)

			return
		}

		if !ok {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}

		ttl := time.Duration(req.Days) * 24 * time.Hour
		issuedAt := time.Now()
		expiresAt := issuedAt.Add(ttl)

		token, jti, err := sessions.createWithTTL(user.Username, user.Permissions, ttl)
		if err != nil {
			log.Warn("web UI: issuing token failed", "err", err)
			http.Error(w, "issuing token failed", http.StatusInternalServerError)

			return
		}

		if err := db.SaveAPIToken(r.Context(), jti, user.Username, user.Permissions, issuedAt, expiresAt); err != nil {
			log.Warn("web UI: recording issued token failed", "err", err)
			http.Error(w, "issuing token failed", http.StatusInternalServerError)

			return
		}

		writeJSON(w, loginResponseJSON{Token: token, ExpiresAt: expiresAt})
	}
}

// apiTokenJSON is one store.APIToken's wire shape for the "Users" admin
// section's per-user token listing (handleListWebUIUserTokens) and its
// revocation (handleRevokeWebUIUserToken), matching the dashboard's own
// field naming (snake_case, as every other /api/... endpoint here uses). It
// never carries the token's own signed value — only jti, the identifier
// needed to revoke it — since the raw token was already shown once, at
// issuance (see handleIssueWebUIUserToken), and is never stored.
type apiTokenJSON struct {
	JTI       string     `json:"jti"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	Revoked   bool       `json:"revoked"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// handleListWebUIUserTokens serves GET /api/users/{username}/tokens: every
// long-lived API token recorded for the named "Users" admin-managed account
// (see db.SaveAPIToken), most recently issued first, for the "Users"
// admin section's per-user token management dialog to show which of them
// are still outstanding and offer to revoke one.
func handleListWebUIUserTokens(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeJSON(w, []apiTokenJSON{})
			return
		}

		tokens, err := db.ListAPITokensForUser(r.Context(), r.PathValue("username"))
		if err != nil {
			log.Warn("web UI: listing API tokens failed", "err", err)
			http.Error(w, "listing tokens failed", http.StatusInternalServerError)

			return
		}

		out := make([]apiTokenJSON, len(tokens))
		for i, t := range tokens {
			out[i] = apiTokenJSON{JTI: t.JTI, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, Revoked: t.RevokedAt != nil, RevokedAt: t.RevokedAt}
		}

		writeJSON(w, out)
	}
}

// handleRevokeWebUIUserToken serves DELETE
// /api/users/{username}/tokens/{jti}: it revokes the named long-lived API
// token (see db.RevokeAPIToken) — a no-op, not an error, if it was
// already revoked — and immediately blocklists it in sessions' in-memory
// revocation check too (see sessionStore.revokeJTI), so it stops working on
// this instance right away rather than only after a restart reloads it from
// db (see newSessionStore). {username} is checked against the token's own
// recorded owner (store.ErrAPITokenNotFound if {jti} doesn't belong to
// {username}, matching a plain unknown jti) purely so a stale or
// copy-pasted URL can't revoke a different user's token out from under the
// "Users" admin section's per-user dialog.
func handleRevokeWebUIUserToken(sessions *sessionStore, db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "user management requires the job state db", http.StatusServiceUnavailable)
			return
		}

		username := r.PathValue("username")
		jti := r.PathValue("jti")

		// Checked before revoking, not after: db.RevokeAPIToken takes no
		// username of its own, so revoking first and only then comparing
		// owners would still permanently revoke a mismatched {username}'s
		// token while reporting 404, as if the request had no effect.
		existing, ok, err := db.GetAPIToken(r.Context(), jti)
		switch {
		case err != nil:
			log.Warn("web UI: looking up API token failed", "err", err)
			http.Error(w, "revoking token failed", http.StatusInternalServerError)

			return
		case !ok, existing.Username != username:
			http.Error(w, "token not found", http.StatusNotFound)
			return
		}

		t, err := db.RevokeAPIToken(r.Context(), jti, time.Now())
		if err != nil {
			log.Warn("web UI: revoking API token failed", "err", err)
			http.Error(w, "revoking token failed", http.StatusInternalServerError)

			return
		}

		sessions.revokeJTI(t.JTI, t.ExpiresAt)

		w.WriteHeader(http.StatusNoContent)
	}
}

// oidcUserPermissionJSON is one store.OIDCUserPermission's wire shape for
// the "Users" admin section's OIDC listing (handleListOIDCUserPermissions/
// handleSetOIDCUserPermissions), matching the dashboard's own field naming
// (snake_case, as every other /api/... endpoint here uses).
type oidcUserPermissionJSON struct {
	Identity    string    `json:"identity"`
	Permissions []string  `json:"permissions"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// handleListOIDCUserPermissions serves GET /api/oidc-users: every stored
// per-identity permission override for an SSO login (see
// store.OIDCUserPermission), for the dashboard's "Users"
// admin section's OIDC listing — requireAdmin (see StartWebUI) restricts
// this to the config-file admin, same as handleListWebUIUsers.
func handleListOIDCUserPermissions(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return handleListJSON(db, log, "listing oidc user permissions failed", db.ListOIDCUserPermissions, func(u store.OIDCUserPermission) oidcUserPermissionJSON {
		return oidcUserPermissionJSON{Identity: u.Identity, Permissions: u.Permissions.Names(), UpdatedAt: u.UpdatedAt}
	})
}

// oidcUserPermissionRequestJSON is handleSetOIDCUserPermissions's request
// body.
type oidcUserPermissionRequestJSON struct {
	Permissions []string `json:"permissions"`
}

// handleSetOIDCUserPermissions serves PUT /api/oidc-users/{identity...}: it
// stores (or replaces) the named identity's permission override (see
// db.SaveOIDCUserPermissions/oidcusers.go), taken effect on that
// identity's next SSO login (see handleOIDCCallback in oidc.go) — same as a
// web UI "Users" admin-managed account's permissions only taking effect on
// its own next login (see sessionClaims). Unlike handleCreateWebUIUser,
// there's no separate create step: an admin grants an identity's first
// override the same way they change an existing one.
func handleSetOIDCUserPermissions(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "user management requires the job state db", http.StatusServiceUnavailable)
			return
		}

		identity := r.PathValue("identity")
		if identity == "" {
			http.Error(w, "identity is required", http.StatusBadRequest)
			return
		}

		var req oidcUserPermissionRequestJSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		perm, err := permission.ParsePermissions(req.Permissions)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := db.SaveOIDCUserPermissions(r.Context(), identity, perm); err != nil {
			log.Warn("web UI: setting oidc user permissions failed", "err", err)
			http.Error(w, "setting oidc user permissions failed", http.StatusInternalServerError)

			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// handleDeleteOIDCUserPermissions serves DELETE /api/oidc-users/{identity...}:
// it removes the named identity's stored permission override (see
// db.DeleteOIDCUserPermissions/oidcusers.go), reverting its next SSO
// login to webui.oidc.default-permissions:.
func handleDeleteOIDCUserPermissions(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "user management requires the job state db", http.StatusServiceUnavailable)
			return
		}

		switch err := db.DeleteOIDCUserPermissions(r.Context(), r.PathValue("identity")); {
		case errors.Is(err, store.ErrOIDCUserPermissionsNotFound):
			http.Error(w, "no permission override for this identity", http.StatusNotFound)
		case err != nil:
			log.Warn("web UI: deleting oidc user permissions failed", "err", err)
			http.Error(w, "deleting oidc user permissions failed", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
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
func handleMintDownloadTicket(receivers map[string]config.ResolvedReceiver, tickets *downloadTicketStore, sessions *sessionStore) http.HandlerFunc {
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
func handleDownloadFile(receivers map[string]config.ResolvedReceiver, log *slog.Logger, db *store.Store, tickets *downloadTicketStore, trustProxyHeaders bool) http.HandlerFunc {
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

// requirePermission wraps next (itself already wrapped in
// requireWebUISession, so a request here is already known to carry a
// currently valid session whenever authEnabled), additionally requiring
// that session to hold required — reporting 403 rather than
// requireWebUISession's 401, since the request is authenticated, just not
// authorized for this endpoint. required must be one of
// permission.PermissionDownload, permission.PermissionViewLoginLog, or
// permission.PermissionViewDownloadLog (checked via the matching CanDownload/
// CanViewLoginLog/CanViewDownloadLog method); anything else, including
// permission.PermissionView, falls back to CanView. authEnabled false skips the
// check entirely, matching requireWebUISession's own bypass, since there's
// no session to hold a permission in that case.
func requirePermission(authEnabled bool, sessions *sessionStore, required permission.Permission, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authEnabled {
			perm := sessions.permissionsFor(r)

			var allowed bool

			switch required {
			case permission.PermissionDownload:
				allowed = perm.CanDownload()
			case permission.PermissionViewLoginLog:
				allowed = perm.CanViewLoginLog()
			case permission.PermissionViewDownloadLog:
				allowed = perm.CanViewDownloadLog()
			case permission.PermissionView, permission.PermissionAdmin:
				allowed = perm.CanView()
			default:
				allowed = perm.CanView()
			}

			if !allowed {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		next(w, r)
	}
}

// requireAdmin wraps next (itself already wrapped in requireWebUISession),
// additionally requiring the session either belong to the config-file admin
// (webui.username) or hold permission.PermissionAdmin — the two ways to reach
// the web UI's "Users" admin section (see handleListWebUIUsers and
// friends): the single config-file admin always could, and a "Users"
// admin-managed account or an OIDC login can too now, once granted
// PermissionAdmin (see permission.Permission). adminUsername empty (no
// config-file admin configured) just falls through to the permission
// check. authEnabled false skips the check entirely, matching
// requireWebUISession/requirePermission's own bypass.
func requireAdmin(authEnabled bool, sessions *sessionStore, adminUsername string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authEnabled {
			claims, sc, ok := sessions.current(r)
			isConfigAdmin := ok && adminUsername != "" && claims.Subject == adminUsername

			if !isConfigAdmin && !sc.Perm.CanAdmin() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		next(w, r)
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
