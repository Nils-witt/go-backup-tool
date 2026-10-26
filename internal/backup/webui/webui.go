// Package webui implements go-backup-tool's web UI: the live status
// dashboard, its SSO bearer-token auth (see auth.go), and the read-only views of receiver state and files it shows
// alongside a job's own status. It shares one HTTP server/mux with the
// receiver API (internal/backup/receiver) via Options.ExtraRoutes, so the
// composition root can mount both on the same listen address without this
// package mounting the receiver API's handlers itself.
package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/gpgkeys"
	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/jobs"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/reporter"
	"nilswitt.dev/go-backup-tool/internal/backup/settings"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// Server wraps the HTTP server behind the -listen web UI, letting
// callers shut it down cleanly (see StartWebUI/shutdown).
type Server struct {
	http *http.Server
	done chan struct{}
	addr string // the listener's actual bound address, e.g. resolved from ":0"

	// stopLive cancels the context every open live status WebSocket (see
	// handleLive) watches, since http.Server.Shutdown leaves hijacked
	// connections alone.
	stopLive context.CancelFunc
}

// Deps are the live objects the web UI reads and changes, built once by the
// composition root (see app.startWebUIIfConfigured). Log must be set. Any
// other field may be nil where the handlers using it allow that, which is
// how tests start a server with only what they exercise: a nil Runner
// drops GET /api/docker-containers, a nil Jobs drops the job-config
// routes, a nil Reporter drops POST /api/reports, and a nil Logs gets a
// fresh LogRingBuffer.
type Deps struct {
	Log             *slog.Logger
	DB              store.Store
	Status          *status.Store
	Jobs            *jobs.Manager
	Runner          *pipeline.Runner
	GPGKeyring      *gpgkeys.Keyring
	Receivers       *receiver.Registry
	ReceiverStatus  *status.ReceiverStore
	ReceiverManager *receiver.Manager
	Settings        *settings.Manager
	Trust           *trust.Manager
	Identity        *identity.ServerIdentity
	Logs            *LogRingBuffer

	// NotifyQueue retries a download notification email that fails to
	// send (see notify.Queue).
	NotifyQueue *notify.Queue

	// Reporter generates a report on demand; nil drops POST /api/reports.
	Reporter *reporter.Reporter
}

// Options are the web UI's plain settings, taken from the config file and
// flags.
type Options struct {
	// Addr is the listen address; ":0" picks a free port (see Server.addr).
	Addr string

	// InstanceName is the configured server-name:, shown by GET /api/meta
	// (see resolveInstanceName for the INSTANCE_NAME fallback).
	InstanceName string

	OIDC config.OIDCSettings

	// TrustProxyHeaders takes the client address from the Forwarded,
	// X-Forwarded-For or X-Real-Ip header for logs and audit events (see
	// clientAddr).
	TrustProxyHeaders bool

	// DevMode allows any CORS origin, for the Vite dev server.
	DevMode bool

	// EventLogLimit caps how many rows each event log endpoint returns;
	// below 1 means config.DefaultEventLogLimit (see serveEventLog).
	EventLogLimit int

	// CORSGetOrigins are the origins allowed to read GET endpoints from
	// another site, e.g. a status page (see getCORSMiddleware). Ignored in
	// DevMode.
	CORSGetOrigins []string

	// RateLimit limits each client's /api/... requests and failed
	// authentications (see rateLimit). The zero value is replaced by
	// config's defaults; set Disabled to turn it off.
	RateLimit config.RateLimitSettings

	// ExtraRoutes, if set, mounts more routes on the same mux, which is
	// how the receiver API shares the listen address without this package
	// importing receiver's HTTP handlers.
	ExtraRoutes func(*http.ServeMux)
}

// StartWebUI starts the -listen web UI dashboard and returns a Server the
// caller can shut down with Server.Shutdown. Returns nil if the server
// fails to start.
func StartWebUI(deps Deps, opts Options) *Server {
	log := deps.Log

	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", opts.Addr)
	if err != nil {
		log.Error("web UI: listening", "addr", opts.Addr, "err", err)
		return nil
	}

	if deps.Logs == nil {
		deps.Logs = NewLogRingBuffer(LogBufferCapacity)
	}

	if !opts.OIDC.Enabled {
		log.Warn("web UI: webui.oidc is not enabled, so nobody can sign in to the dashboard (the receiver API is unaffected)")
	}

	if !opts.TrustProxyHeaders && !loopbackOnly(ln.Addr()) {
		log.Warn("web UI: listening for plain HTTP beyond loopback without webui.trust-proxy-headers: access tokens, download tickets, and receiver uploads cross the network unencrypted unless a TLS-terminating reverse proxy sits in front (see README, \"Running behind a reverse proxy\")",
			"addr", ln.Addr().String())
	}

	liveCtx, stopLive := context.WithCancel(context.Background())

	mux := http.NewServeMux()
	registerRoutes(liveCtx, mux, deps, opts)

	var handler http.Handler = mux
	if opts.DevMode {
		handler = corsMiddleware(handler)
	} else if len(opts.CORSGetOrigins) > 0 {
		handler = getCORSMiddleware(opts.CORSGetOrigins, handler)
	}

	handler = rateLimit(opts.RateLimit, opts.TrustProxyHeaders, log, handler)
	handler = securityHeaders(opts.OIDC.Issuer, opts.TrustProxyHeaders, handler)

	srv := &Server{
		http:     &http.Server{Handler: logRequests(log, handler, opts.TrustProxyHeaders), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10},
		done:     make(chan struct{}),
		addr:     ln.Addr().String(),
		stopLive: stopLive,
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

// loopbackOnly reports whether addr, a bound listener's address, only
// accepts connections from this machine. A wildcard address (":8080",
// "0.0.0.0", "::") accepts them from anywhere.
func loopbackOnly(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// registerRoutes mounts every web UI route on mux: the single place to see
// which endpoint needs which permission. liveCtx is cancelled on
// Server.Shutdown to close open live status WebSockets (see handleLive).
func registerRoutes(liveCtx context.Context, mux *http.ServeMux, deps Deps, opts Options) {
	log, db := deps.Log, deps.DB

	lookupJob := pipeline.StaticJobs(nil)
	if deps.Jobs != nil {
		lookupJob = deps.Jobs.Get
	}

	downloadTickets := newDownloadTicketStore()
	liveTickets := newLiveTicketStore()

	tokens := newAPITokens(context.Background(), db, log)
	auth := newAuthenticator(opts.OIDC, tokens, db, log, opts.TrustProxyHeaders)

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
	apiAuditLog := perm(permission.Permission.CanViewAuditLog)
	admin := perm(permission.Permission.CanAdmin)

	// change gates an admin endpoint that changes this instance's stored
	// configuration (or acts on a job), recording each request in the audit
	// log, along with the fields it changed (see recordChange).
	auditRes := auditResources(db, deps.GPGKeyring)
	change := func(h http.HandlerFunc) http.HandlerFunc {
		return admin(recordChange(db, auditRes, log, opts.TrustProxyHeaders, h))
	}

	// codeChange is change for a change that could run code on this host or
	// redirect backups (jobs, servers, commands, GPG keys): it also needs a
	// recent sign-in at the provider (see requireRecentLogin).
	codeChange := func(h http.HandlerFunc) http.HandlerFunc {
		return admin(requireRecentLogin(auth, recordChange(db, auditRes, log, opts.TrustProxyHeaders, h)))
	}

	mux.HandleFunc("GET /", handleDashboard(dashboardIndexHTML))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", cacheForever(gzipStatic(dashboardAssetsFS, http.FileServerFS(dashboardAssetsFS)))))
	mux.HandleFunc("GET /api/meta", handleMeta(opts.InstanceName))
	mux.HandleFunc("GET /api/sso/status", handleSSOStatus(opts.OIDC))
	mux.HandleFunc("GET /api/me", requireUser(auth, handleMe()))
	mux.HandleFunc("POST /api/sso/login", requireUser(auth, handleSSOLogin(auth)))
	mux.HandleFunc("POST /api/sso/backchannel-logout", handleBackchannelLogout(auth))
	mux.HandleFunc("GET /api/status", api(handleStatus(deps.Status)))
	mux.HandleFunc("GET /api/logs", api(handleLogs(deps.Logs)))
	mux.HandleFunc("GET /api/identity", api(handleIdentity(deps.Identity)))
	mux.HandleFunc("GET /api/receivers", api(handleReceiverStatus(deps.Receivers, deps.ReceiverStatus, log)))
	mux.HandleFunc("POST /api/live/ticket", api(handleMintLiveTicket(liveTickets)))
	mux.HandleFunc("GET /api/live", handleLive(liveCtx, deps.Status, deps.Receivers, deps.ReceiverStatus, liveTickets, auth.logouts, opts.DevMode, opts.TrustProxyHeaders, log))
	mux.HandleFunc("GET /api/job-runs", apiJobRunLog(handleJobRunEvents(db, log, opts.EventLogLimit)))
	mux.HandleFunc("GET /api/target-runs", apiTargetRunLog(handleTargetRunEvents(db, log, opts.EventLogLimit)))
	mux.HandleFunc("POST /api/jobs/{name}/retry", change(handleRetryFailedTargets(lookupJob, deps.Status, deps.Runner, log)))
	mux.HandleFunc("POST /api/jobs/{name}/run", change(handleRunJob(lookupJob, deps.Runner)))
	mux.HandleFunc("GET /api/receivers/{id}/files", api(handleReceiverFiles(deps.Receivers, log)))
	mux.HandleFunc("POST /api/receivers/{id}/download/{key...}", apiDownload(handleMintDownloadTicket(deps.Receivers, downloadTickets)))
	mux.HandleFunc("GET /api/receivers/{id}/download/{key...}", handleDownloadFile(deps.Receivers, log, db, downloadTickets, opts.TrustProxyHeaders, deps.NotifyQueue))
	mux.HandleFunc("GET /api/login-events", apiLoginLog(handleLoginEvents(db, log, opts.EventLogLimit)))
	mux.HandleFunc("GET /api/download-events", apiDownloadLog(handleDownloadEvents(db, log, opts.EventLogLimit)))
	mux.HandleFunc("GET /api/receiver-events", apiReceiverLog(handleReceiverEvents(db, log, opts.EventLogLimit)))
	mux.HandleFunc("GET /api/audit-events", apiAuditLog(handleAuditEvents(db, log, opts.EventLogLimit)))
	mux.HandleFunc("GET /api/tokens", admin(handleListAPITokens(tokens, log)))
	mux.HandleFunc("POST /api/tokens", change(handleCreateAPIToken(tokens, log)))
	mux.HandleFunc("DELETE /api/tokens/{id}", change(handleRevokeAPIToken(tokens, log)))
	registerSettingsRoutes(mux, deps.ReceiverManager, deps.Trust, deps.Settings, admin, change, log)
	registerJobConfigRoutes(mux, deps.Jobs, admin, codeChange, log)
	registerReportRoutes(mux, db, deps.Reporter, admin, change, log)

	if deps.Runner != nil {
		mux.HandleFunc("GET /api/docker-containers", admin(handleListDockerContainers(deps.Runner, log)))
	}

	registerGPGKeyRoutes(mux, deps.GPGKeyring, deps.Jobs != nil && deps.Jobs.Editing(), admin, codeChange, log)

	if opts.ExtraRoutes != nil {
		opts.ExtraRoutes(mux)
	}
}

// registerSettingsRoutes mounts /api/receiver-configs,
// /api/trusted-servers, /api/notification-configs, /api/report-config, and
// /api/log-retention-config on mux: each listing wrapped in admin, each
// change in change (admin plus the audit log).
func registerSettingsRoutes(mux *http.ServeMux, receiverManager *receiver.Manager, trustManager *trust.Manager, settingsManager *settings.Manager, admin, change func(http.HandlerFunc) http.HandlerFunc, log *slog.Logger) {
	mux.HandleFunc("GET /api/receiver-configs", admin(handleListReceiverConfigs(receiverManager, log)))
	mux.HandleFunc("POST /api/receiver-configs", change(handleCreateReceiverConfig(receiverManager, log)))
	mux.HandleFunc("PUT /api/receiver-configs/{id}", change(handleUpdateReceiverConfig(receiverManager, log)))
	mux.HandleFunc("DELETE /api/receiver-configs/{id}", change(handleDeleteReceiverConfig(receiverManager, log)))
	mux.HandleFunc("GET /api/trusted-servers", admin(handleListTrustedServers(trustManager, log)))
	mux.HandleFunc("POST /api/trusted-servers", change(handleCreateTrustedServer(trustManager, log)))
	mux.HandleFunc("PUT /api/trusted-servers/{id}", change(handleUpdateTrustedServer(trustManager, log)))
	mux.HandleFunc("DELETE /api/trusted-servers/{id}", change(handleDeleteTrustedServer(trustManager, log)))
	mux.HandleFunc("GET /api/notification-configs", admin(handleListNotificationConfigs(settingsManager, log)))
	mux.HandleFunc("POST /api/notification-configs", change(handleCreateNotificationConfig(settingsManager, log)))
	mux.HandleFunc("PUT /api/notification-configs/{id}", change(handleUpdateNotificationConfig(settingsManager, log)))
	mux.HandleFunc("DELETE /api/notification-configs/{id}", change(handleDeleteNotificationConfig(settingsManager, log)))
	mux.HandleFunc("GET /api/report-config", admin(handleGetReportConfig(settingsManager, log)))
	mux.HandleFunc("PUT /api/report-config", change(handleUpdateReportConfig(settingsManager, log)))
	mux.HandleFunc("GET /api/log-retention-config", admin(handleGetLogRetentionConfig(settingsManager, log)))
	mux.HandleFunc("PUT /api/log-retention-config", change(handleUpdateLogRetentionConfig(settingsManager, log)))
}

// Shutdown gracefully stops the web UI server, waiting for it to finish.
func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.stopLive()

	_ = s.http.Shutdown(ctx)

	<-s.done
}

// writeJSON encodes v as the response body with a JSON content type,
// writing a 500 if encoding fails. Shared by every simple "serve the
// current snapshot as JSON" handler in this file. v is marshaled before
// anything is written, so a failure can still become a real 500 rather than
// a 200 with a truncated body. The 500 carries a generic message; the
// encoding error itself is only logged (see noteResponseError).
func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		noteResponseError(w, fmt.Errorf("encoding JSON response: %w", err))
		http.Error(w, "encoding the response failed", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(append(b, '\n'))
}
