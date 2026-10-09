// Package app wires up and runs a backup-tool process: parsing flags,
// loading the server identity, starting the web UI and receiver API, and
// scheduling every configured job.
package app

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/settings"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
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
// when rc.Listen is unset. queue lets a download/stale notification email
// that fails to send be retried later (see notify.Queue). receivers,
// receiverStore and receiverManager are the live receiver set, its
// dashboard status, and the web UI's way of changing it (see
// newReceivers).
func startWebUIIfConfigured(ctx context.Context, rc *config.RunConfig, statusStore *backup.StatusStore, stateDB *store.Store, logs *webui.LogRingBuffer, serverIdentity *identity.ServerIdentity, runner *pipeline.Runner, queue *notify.Queue, receivers *backup.ReceiverRegistry, receiverStore *backup.ReceiverStatusStore, receiverManager *receiver.Manager, settingsManager *settings.Manager, log *slog.Logger) *webui.Server {
	if rc.Listen == "" {
		return nil
	}

	go receiver.MonitorReceiverRetention(ctx, stateDB, receivers, log)

	srv := webui.StartWebUI(rc.Listen, statusStore, rc.Jobs, runner, receivers, receiverStore, receiverManager, settingsManager, log, stateDB, logs, rc.OIDC, serverIdentity, rc.TrustProxyHeaders, rc.DevMode, rc.ServerName, func(mux *http.ServeMux) {
		receiver.RegisterRoutes(mux, receivers, receiverStore, log, stateDB)
	}, queue)

	go receiver.MonitorStaleReceivers(ctx, receivers, queue, log)

	return srv
}

// newReceivers loads every receiver (see receiver.Manager.Load, which also
// imports the config file's deprecated receivers: into the state db) into a
// fresh registry and status store — shared between the dashboard's receiver
// views, the receiver API's write path, the monitors, and the report, so a
// write or a web UI edit is reflected everywhere immediately.
// notifications must already be loaded (see newSettings), since receivers
// are checked against it.
func newReceivers(ctx context.Context, rc *config.RunConfig, stateDB *store.Store, notifications *notify.Registry, log *slog.Logger) (*backup.ReceiverRegistry, *backup.ReceiverStatusStore, *receiver.Manager) {
	registry := backup.NewReceiverRegistry(nil)
	status := backup.NewReceiverStatusStore(nil)
	manager := receiver.NewManager(stateDB, registry, status, notifications, rc.ServerName, rc.ReceiversBaseDir, log)
	manager.Load(ctx, rc.FileReceivers)

	return registry, status, manager
}

// newSettings loads every notification and the report settings (see
// settings.Manager.Load, which also imports the config file's deprecated
// notifications:/report: into the state db) into a fresh live registry and
// report holder — read by every job failure, receiver, and report trigger
// at fire time, so a web UI edit applies without a restart.
func newSettings(ctx context.Context, rc *config.RunConfig, stateDB *store.Store, log *slog.Logger) (*notify.Registry, *report.Live, *settings.Manager) {
	notifications := notify.NewRegistry(nil)
	live := report.NewLive(report.Settings{})
	manager := settings.NewManager(stateDB, notifications, live, rc.SMTP, rc.GPG, rc.Jobs, log)
	manager.Load(ctx, rc.FileNotifications, rc.FileReport)

	return notifications, live, manager
}

// runWithContext is Run's implementation, taking an externally supplied base
// context instead of deriving one from OS signals. This lets a Windows
// service (see service_windows.go) drive shutdown from Service Control
// Manager stop/shutdown requests, which — unlike Ctrl-C/SIGTERM — aren't
// delivered to a service process the normal OS-signal way.
func runWithContext(ctx context.Context, args []string, stderr io.Writer) int {
	rc, err := config.ParseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}

		// The desired log level lives in rc, which parsing itself failed to
		// produce; fall back to the default so this one message still gets
		// out.
		newLogger(stderr, slog.LevelInfo).Error("parsing flags", "err", err)

		return 2
	}

	if rc.LogFile != "" {
		f, err := openLogFile(rc.LogFile)
		if err != nil {
			newLogger(stderr, rc.LogLevel).Error("opening log file", "path", rc.LogFile, "err", err)

			return 1
		}

		defer func() { _ = f.Close() }()

		stderr = io.MultiWriter(stderr, f)
	}

	log, logs := newRunLogger(stderr, rc)

	log.Info("go-backup-tool starting", "version", version.Version, "commit", version.Commit)

	serverIdentity, err := identity.LoadServerIdentityAtStartup(log, rc.KeysDir)
	if err != nil {
		log.Error("loading server identity", "err", err)
		return 1
	}

	if rc.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, rc.Timeout)
		defer cancel()

		log.Debug("run timeout set", "timeout", rc.Timeout)
	}

	var stateDB *store.Store

	// Always opened: backs catch-up scheduling, the web UI, retention
	// tracking, and the target-error log, which every run needs regardless
	// of those other features.
	path := store.StateDBPath(rc.ConfigPath)

	db, err := store.Open(ctx, path)
	if err != nil {
		log.Warn("opening job state db", "path", path, "err", err)
	} else {
		stateDB = db
		defer func() { _ = db.Close() }()

		log.Debug("opened job state db", "path", path)
	}

	pipeline.SweepStartupRetention(ctx, stateDB, rc.Jobs, log)

	statusStore := backup.NewStatusStore(rc.Jobs)

	if stateDB != nil {
		pipeline.SeedStatusFromState(ctx, stateDB, rc.Jobs, statusStore, log)
	}

	// Holds any notification email SendMailQueued couldn't deliver, retrying
	// it every notify.RetryInterval instead of losing it; shared by every
	// email-sending notification below.
	mailQueue := notify.NewQueue()
	go mailQueue.Run(ctx, log)

	notifications, reportSettings, settingsManager := newSettings(ctx, rc, stateDB, log)

	r := pipeline.NewRunner(log, statusStore, stateDB, serverIdentity, mailQueue, notifications)

	// Keeps retrying any target upload that failed during a run, every
	// pipeline.TargetUploadRetryInterval, until it succeeds — see
	// pipeline.Runner.RunOutstandingUploadRetries.
	go r.RunOutstandingUploadRetries(ctx, rc.Jobs)

	receivers, receiverStore, receiverManager := newReceivers(ctx, rc, stateDB, notifications, log)

	// Independent of the web UI: a daily report is useful for anyone
	// monitoring receivers by inbox, not just those watching the dashboard.
	// RunReportLoop idles while the report is disabled.
	go pipeline.RunReportLoop(ctx, rc, reportSettings, notifications, receivers, stateDB, mailQueue, log)

	srv := startWebUIIfConfigured(ctx, rc, statusStore, stateDB, logs, serverIdentity, r, mailQueue, receivers, receiverStore, receiverManager, settingsManager, log)

	var wg sync.WaitGroup

	log.Info("starting jobs", "count", len(rc.Jobs))

	for _, job := range rc.Jobs {
		pipeline.WarnIfKeyWontChange(log, job)

		wg.Go(func() {
			r.Schedule(ctx, job)
		})
	}

	wg.Wait()

	if srv != nil {
		// One-shot jobs (no interval) finish as soon as wg.Wait returns
		// above; keep the dashboard reachable until the user stops the
		// process instead of tearing it down the instant the backups
		// complete. A job with an interval already keeps schedule (and so
		// wg.Wait) running until ctx is done, so this is a no-op then.
		<-ctx.Done()
		srv.Shutdown()
	}

	if r.Failed() {
		log.Warn("run finished with failures")

		return 1
	}

	log.Info("run finished")

	return 0
}
