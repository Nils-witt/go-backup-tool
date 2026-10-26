// Package app wires up and runs a backup-tool process: parsing flags,
// loading the server identity, starting the web UI and receiver API, and
// scheduling every configured job.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/dockerexec"
	"nilswitt.dev/go-backup-tool/internal/backup/gpgkeys"
	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/jobs"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/periodic"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/reporter"
	"nilswitt.dev/go-backup-tool/internal/backup/settings"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
	"nilswitt.dev/go-backup-tool/internal/backup/webui"
	"nilswitt.dev/go-backup-tool/internal/version"
)

func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

func newRunLogger(stderr io.Writer, rc *config.RunConfig) (*slog.Logger, *webui.LogRingBuffer) {
	if rc.Listen == "" || !rc.LogViewer {
		return newLogger(stderr, rc.LogLevel), nil
	}

	logs := webui.NewLogRingBuffer(webui.LogBufferCapacity)

	return newLogger(io.MultiWriter(stderr, logs), rc.LogLevel), logs
}

// withRunTimeout bounds ctx by the config file's timeout:, if set (> 0),
// returning the derived context and its cancel func.
func withRunTimeout(ctx context.Context, timeout time.Duration, log *slog.Logger) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}

	log.Debug("run timeout set", "timeout", timeout)

	return context.WithTimeout(ctx, timeout)
}

// checkPublicKeyReceivers refuses a state db holding receivers that still
// authenticate senders only by a public key (removed in v8.0.0): they would
// silently reject every upload. -export-settings/-import-settings run
// before this check, so they can fix such receivers.
func checkPublicKeyReceivers(ctx context.Context, db store.Store) error {
	if db == nil {
		return nil
	}

	ids, err := db.ListReceiversRelyingOnPublicKey(ctx)
	if err != nil {
		return err
	}

	if len(ids) == 0 {
		return nil
	}

	return fmt.Errorf("receivers %s still authenticate senders only by public key, which was removed in v8.0.0: "+
		"add each sender as a trusted server and allow it on the receiver (run -export-settings, add the sender under trusted-servers: and its id under the receiver's allowed-servers:, then -import-settings), "+
		"or make the same change or delete the receiver in v7.3's web UI", strings.Join(ids, ", "))
}

// teeLogFile returns w extended to also write to the log file at path, and
// a func closing that file; with path empty it returns w and a no-op.
func teeLogFile(w io.Writer, path string) (io.Writer, func(), error) {
	if path == "" {
		return w, func() {}, nil
	}

	f, err := openLogFile(path)
	if err != nil {
		return nil, nil, err
	}

	return io.MultiWriter(w, f), func() { _ = f.Close() }, nil
}

// openLogFile opens path for appending log output, creating it (and its
// parent directory) if needed. The caller must Close it.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}

	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640) //nolint:gosec // path comes from the operator's own config file
}

// Run parses args and executes every configured backup job, writing errors
// and per-job status messages to stderr. It returns the process exit code:
// 0 if every job succeeded (or -h/-help), 2 on a flag/config error, 1 if any
// job failed.
//
// A job with a nonzero interval repeats on its own schedule until ctx is
// canceled (Ctrl-C/SIGTERM, or the config file's timeout elapsing) instead
// of running once; in that case Run blocks for as long as any such job
// keeps running. Jobs
// run concurrently with each other, each on its own schedule. A job failing
// doesn't stop the others: the remaining jobs, and any later repeats, still
// get a chance to complete, since a partial backup run is better than none.
//
// If the config file sets listen:, Run also serves a web UI dashboard of
// every job's and target's live status (see webui.StartWebUI) and, even
// once every job is a one-shot run that has already finished, keeps
// running to keep that dashboard reachable until ctx is canceled. It also
// starts the stale-receiver webhook monitor (see
// receiver.MonitorStaleReceivers) for any receivers: entry with
// stale-after: set.
func Run(args []string, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runWithContext(ctx, args, stderr)
}

// startWebUIIfConfigured starts the web UI dashboard and receiver API (see
// webui.StartWebUI, receiver.RegisterRoutes) when rc.Listen is set, along
// with the stale-receiver webhook monitor (see
// receiver.MonitorStaleReceivers) and the per-receiver retention sweep (see
// receiver.MonitorReceiverRetention). It returns nil, doing nothing else,
// when rc.Listen is unset. deps.GPGKeyring is set here from rc.GPG, so
// callers leave it nil. The monitors and the receiver API share deps'
// live receiver set (see newReceivers) and notification queue.
func startWebUIIfConfigured(ctx context.Context, rc *config.RunConfig, deps webui.Deps) *webui.Server {
	if rc.Listen == "" {
		return nil
	}

	go receiver.MonitorReceiverRetention(ctx, deps.DB, deps.Receivers, deps.Log)

	deps.GPGKeyring = gpgkeys.New(rc.GPG.Bin, rc.GPG.Homedir)
	srv := webui.StartWebUI(deps, webui.Options{
		Addr:              rc.Listen,
		InstanceName:      rc.ServerName,
		OIDC:              rc.OIDC,
		TrustProxyHeaders: rc.TrustProxyHeaders,
		DevMode:           rc.DevMode,
		EventLogLimit:     rc.EventLogLimit,
		CORSGetOrigins:    rc.CORSGetOrigins,
		RateLimit:         rc.RateLimit,
		ExtraRoutes: func(mux *http.ServeMux) {
			receiver.RegisterRoutes(mux, deps.Receivers, deps.ReceiverStatus, deps.Log, deps.DB, func(r *http.Request) string {
				return webui.ClientAddr(r, rc.TrustProxyHeaders)
			})
		},
	})

	go receiver.MonitorStaleReceivers(ctx, deps.Receivers, deps.NotifyQueue, deps.Log)

	return srv
}

// newReceivers loads every receiver (see receiver.Manager.Load, which also
// imports the config file's deprecated receivers: into the state db) into a
// fresh registry and status store — shared between the dashboard's receiver
// views, the receiver API's write path, the monitors, and the report, so a
// write or a web UI edit is reflected everywhere immediately.
// The trusted servers are loaded first, into the registry receivers verify
// requests against, and their manager returned for the web UI.
// notifications must already be loaded (see newSettings), since receivers
// are checked against it.
func newReceivers(ctx context.Context, rc *config.RunConfig, stateDB store.Store, notifications *notify.Registry, log *slog.Logger) (*receiver.Registry, *status.ReceiverStore, *receiver.Manager, *trust.Manager) {
	// Trusted servers load first: every receiver resolves its allowed
	// servers against them.
	trustManager := trust.NewManager(stateDB, trust.NewRegistry(nil), log)
	trustManager.Load(ctx)

	registry := receiver.NewRegistry(nil)
	receiverStatus := status.NewReceiverStore(nil)
	manager := receiver.NewManager(stateDB, registry, receiverStatus, notifications, trustManager.Registry(), rc.ServerName, rc.ReceiversBaseDir, log)
	manager.Load(ctx)

	return registry, receiverStatus, manager, trustManager
}

// newSettings loads every notification and the report settings (see
// settings.Manager.Load, which also imports the config file's deprecated
// notifications:/report: into the state db) into a fresh live registry and
// report holder — read by every job failure, receiver, and report trigger
// at fire time, so a web UI edit applies without a restart.
func newSettings(ctx context.Context, rc *config.RunConfig, stateDB store.Store, log *slog.Logger) (*notify.Registry, *report.Live, *settings.Manager) {
	notifications := notify.NewRegistry(nil)
	live := report.NewLive(report.Settings{})
	manager := settings.NewManager(stateDB, notifications, live, rc.SMTP, rc.GPG, log)
	manager.Load(ctx)

	return notifications, live, manager
}

// newJobs loads every server, command, and job (see jobs.Manager.Load, which
// also imports the config file's deprecated servers:/commands:/jobs: into the
// state db) into a fresh jobs.Manager, mirrored into statusStore and run on
// runner — not scheduled until its Start. notifications must already be
// loaded (see newSettings), since job failure-notifications are checked
// against it. The error is an unknown -job.
func newJobs(ctx context.Context, rc *config.RunConfig, stateDB store.Store, statusStore *status.Store, runner *pipeline.Runner, notifications *notify.Registry, log *slog.Logger) (*jobs.Manager, error) {
	manager := jobs.NewManager(stateDB, statusStore, runner, notifications, jobs.Settings{
		GPG: rc.GPG, ServerName: rc.ServerName, Editing: rc.JobEditing, Filter: rc.JobFilter,
	}, log)

	if err := manager.Load(ctx, rc.FileServers, rc.FileCommands, rc.FileJobs); err != nil {
		return nil, err
	}

	return manager, nil
}

// parseRunConfig parses args into a RunConfig. When that fails (or -help
// was given), rc is nil and code is the exit code runWithContext returns.
func parseRunConfig(args []string, stderr io.Writer) (rc *config.RunConfig, code int) {
	rc, err := config.ParseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0
		}

		// The desired log level lives in rc, which parsing itself failed to
		// produce; fall back to the default so this one message still gets
		// out.
		newLogger(stderr, slog.LevelInfo).Error("parsing flags", "err", err)

		return nil, 2
	}

	return rc, 0
}

func runWithContext(ctx context.Context, args []string, stderr io.Writer) int {
	rc, code := parseRunConfig(args, stderr)
	if rc == nil {
		return code
	}

	logOut, closeLog, err := teeLogFile(stderr, rc.LogFile)
	if err != nil {
		newLogger(stderr, rc.LogLevel).Error("opening log file", "path", rc.LogFile, "err", err)

		return 1
	}

	defer closeLog()

	stderr = logOut

	log, logs := newRunLogger(stderr, rc)

	log.Info("go-backup-tool starting", "version", version.Version, "commit", version.Commit)

	serverIdentity, err := identity.LoadServerIdentityAtStartup(log, rc.KeysDir)
	if err != nil {
		log.Error("loading server identity", "err", err)
		return 1
	}

	ctx, cancelTimeout := withRunTimeout(ctx, rc.Timeout, log)
	defer cancelTimeout()

	stateDB, closeStateDB, err := openStateDB(ctx, rc, log)
	if err != nil {
		log.Error("opening state db", "err", err)
		return 1
	}

	defer closeStateDB()

	if rc.ExportSettings != "" || rc.ImportSettings != "" {
		return runSettingsFile(ctx, rc, stateDB, log)
	}

	if err := checkPublicKeyReceivers(ctx, stateDB); err != nil {
		log.Error("refusing to start", "err", err)
		return 1
	}

	statusStore := status.NewStore(nil)

	// Holds any notification email SendMailQueued couldn't deliver, retrying
	// it every notify.RetryInterval instead of losing it; shared by every
	// email-sending notification below.
	mailQueue := notify.NewQueue()
	go mailQueue.Run(ctx, log)

	notifications, reportSettings, settingsManager := newSettings(ctx, rc, stateDB, log)

	r := pipeline.NewRunner(log, statusStore, stateDB, serverIdentity, mailQueue, notifications)
	r.UseDocker(dockerexec.ResolveHost(rc.DockerSocket))

	jobsManager, err := newJobs(ctx, rc, stateDB, statusStore, r, notifications, log)
	if err != nil {
		log.Error("loading jobs", "err", err)
		return 2
	}

	settingsManager.UseJobs(jobsManager.NotificationRefs)

	go pruneRunLogs(ctx, stateDB, settingsManager, log)

	// Without the web UI there's no way to add a job later, so nothing to
	// stay running for.
	if len(jobsManager.Names()) == 0 && rc.Listen == "" {
		log.Error("no jobs to run: define at least one job, or set webui.enabled: true to run without any (and manage jobs there)")
		return 2
	}

	pipeline.SweepStartupRetention(ctx, stateDB, jobsManager.Jobs(), log)

	// Keeps retrying any target upload that failed during a run, every
	// pipeline.TargetUploadRetryInterval, until it succeeds — see
	// pipeline.Runner.RunOutstandingUploadRetries.
	go r.RunOutstandingUploadRetries(ctx, jobsManager.Get)

	receivers, receiverStore, receiverManager, trustManager := newReceivers(ctx, rc, stateDB, notifications, log)

	// Independent of the web UI: a daily report is useful for anyone
	// monitoring receivers by inbox, not just those watching the dashboard.
	// reporter.Run idles while the report is disabled.
	// Every report is also kept for the dashboard's report history.
	reports := reporter.New(reporter.Deps{
		ServerName: rc.ServerName, JobNames: jobsManager.Names, Settings: reportSettings, Notifications: notifications,
		Receivers: receivers.Snapshot, DB: stateDB, Queue: mailQueue, Log: log,
	})
	go reports.Run(ctx)

	srv := startWebUIIfConfigured(ctx, rc, webui.Deps{
		Log:             log,
		DB:              stateDB,
		Status:          statusStore,
		Jobs:            jobsManager,
		Runner:          r,
		Receivers:       receivers,
		ReceiverStatus:  receiverStore,
		ReceiverManager: receiverManager,
		Settings:        settingsManager,
		Trust:           trustManager,
		Identity:        serverIdentity,
		Logs:            logs,
		NotifyQueue:     mailQueue,
		Reporter:        reports,
	})

	log.Info("starting jobs", "count", len(jobsManager.Names()))
	jobsManager.Start(ctx)

	if srv != nil {
		// Keep the dashboard reachable — and jobs editable there — until
		// the user stops the process, even once every one-shot job (no
		// interval) has finished, instead of tearing it down the instant
		// the backups complete.
		<-ctx.Done()
		srv.Shutdown()
	}

	// Every schedule ends once ctx is done (a repeating job's) or its one
	// run finished (a one-shot job's); wait for any run still in flight.
	jobsManager.Wait()

	if r.Failed() {
		log.Warn("run finished with failures")

		return 1
	}

	log.Info("run finished")

	return 0
}

// openStateDB opens the state db the config file's database: entry
// selects and starts pruning its event logs (see pruneEventLogs). It is
// always opened: it backs catch-up scheduling, the web UI, retention
// tracking, and the target-error log, which every run needs regardless of
// those other features.
//
// A SQLite db that fails to open is only logged: openStateDB returns a nil
// store.Store, which those features treat as unavailable, and a no-op
// close. A PostgreSQL db is configured explicitly, so failing to reach it
// is returned as an error instead, and the caller refuses to start.
func openStateDB(ctx context.Context, rc *config.RunConfig, log *slog.Logger) (db store.Store, closeDB func(), err error) {
	switch rc.Database.Type {
	case config.DatabasePostgres:
		// The DSN may carry a password, so it's never logged; the
		// driver's own errors name only the host, user, and database.
		db, err = store.OpenPostgres(ctx, rc.Database.PostgresDSN)
		if err != nil {
			return nil, nil, err
		}

		log.Debug("opened job state db", "type", rc.Database.Type)
	case config.DatabaseSQLite, "": // "" is a RunConfig built without ParseFlags
		path := store.StateDBPath(rc.ConfigPath)

		db, err = store.OpenSQLite(ctx, path)
		if err != nil {
			log.Warn("opening job state db", "path", path, "err", err)

			return nil, func() {}, nil
		}

		log.Debug("opened job state db", "type", rc.Database.Type, "path", path)
	default:
		return nil, nil, fmt.Errorf("unknown database type %q", rc.Database.Type)
	}

	go pruneEventLogs(ctx, db, rc.EventLogRetention, log)

	return db, func() { _ = db.Close() }, nil
}

// pruneEventLogs deletes login, download, receiver, and audit event log
// rows older than retention (webui.event-log-retention) once at startup and
// then daily, until ctx is done. A zero retention keeps them forever.
func pruneEventLogs(ctx context.Context, db store.Store, retention time.Duration, log *slog.Logger) {
	if retention <= 0 {
		return
	}

	periodic.Run(ctx, 24*time.Hour, true, func() {
		n, err := db.PruneEvents(ctx, time.Now().Add(-retention))
		if err != nil {
			log.Warn("pruning event logs", "err", err)
			return
		}

		if n > 0 {
			log.Info("pruned old event log rows", "rows", n, "retention", retention)
		}
	})
}

// runLogPruneInterval is how often pruneRunLogs applies the log retention
// settings when they don't change.
const runLogPruneInterval = time.Hour

// pruneRunLogs deletes the job and target run log rows the log retention
// settings no longer keep (see settings.LogRetention): once at startup,
// then every runLogPruneInterval and right after each settings change,
// until ctx is done. Without a state db (db nil) there is nothing to prune.
func pruneRunLogs(ctx context.Context, db store.Store, m *settings.Manager, log *slog.Logger) {
	if db == nil {
		return
	}

	prune := func() {
		r, now := m.LogRetention(), time.Now()

		jobRuns, err := db.DeleteOldJobRuns(ctx, r.JobRunCutoff(now), r.JobRunMaxRows)
		if err != nil {
			log.Warn("pruning the job run log", "err", err)
		}

		targetRuns, err := db.DeleteOldTargetRuns(ctx, r.TargetRunCutoff(now), r.TargetRunMaxRows)
		if err != nil {
			log.Warn("pruning the target run log", "err", err)
		}

		if jobRuns > 0 || targetRuns > 0 {
			log.Info("pruned old run log rows", "job_runs", jobRuns, "target_runs", targetRuns)
		}
	}

	prune()

	ticker := time.NewTicker(runLogPruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		case <-m.LogRetentionChanged():
			prune()
		}
	}
}
