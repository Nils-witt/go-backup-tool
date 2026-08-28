package backup

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// newLogger builds the structured logger every subsystem writes its
// diagnostic output through: timestamped, level-tagged lines to w. level
// gates verbosity — at the default (info) a run reports what it did (jobs
// started, objects written, warnings); at debug it additionally reports how
// (pipeline stage transitions, per-target timing, schedule/catch-up
// decisions), which is noisy for routine runs but valuable when
// troubleshooting one that isn't behaving as expected.
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// newRunLogger builds the logger runWithContext uses for the whole run, plus
// (only when rc.listen enables the web UI and rc.logViewer opts into its log
// viewer) the ring buffer backing that viewer (see logRingBuffer,
// handleLogs). Without a web UI to serve it back out, or with the viewer
// left off, that buffer would just be memory nothing ever reads (or an
// operator deliberately didn't want served over HTTP), so the returned
// *logRingBuffer is nil in both cases and log writes straight to stderr.
func newRunLogger(stderr io.Writer, rc *runConfig) (*slog.Logger, *logRingBuffer) {
	if rc.listen == "" || !rc.logViewer {
		return newLogger(stderr, rc.logLevel), nil
	}

	logs := newLogRingBuffer(logBufferCapacity)

	return newLogger(io.MultiWriter(stderr, logs), rc.logLevel), logs
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
// every job's and target's live status (see webui.go) and, even once every
// job is a one-shot run that has already finished, keeps running to keep
// that dashboard reachable until ctx is canceled. It also starts the
// stale-receiver webhook monitor (see monitorStaleReceivers in receiver.go)
// for any receivers: entry with stale-after: set.
func Run(args []string, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runWithContext(ctx, args, stderr)
}

// runWithContext is Run's implementation, taking an externally supplied base
// context instead of deriving one from OS signals. This lets a Windows
// service (see service_windows.go) drive shutdown from Service Control
// Manager stop/shutdown requests, which — unlike Ctrl-C/SIGTERM — aren't
// delivered to a service process the normal OS-signal way.
func runWithContext(ctx context.Context, args []string, stderr io.Writer) int {
	rc, err := parseFlags(args, stderr)
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

	log, logs := newRunLogger(stderr, rc)

	identity := loadServerIdentityAtStartup(log, rc.keysDir)

	if rc.timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, rc.timeout)
		defer cancel()

		log.Debug("run timeout set", "timeout", rc.timeout)
	}

	var stateDB *sql.DB

	// Always opened: besides catch-up scheduling, the web UI, and retention
	// tracking, it now also backs the outstanding-uploads retry queue (see
	// uploadretry.go), which every run needs regardless of those other
	// features — a target upload failure with no state db to queue it in
	// would otherwise be retried immediately or not at all.
	path := scheduleStateDBPath(rc.configPath)

	db, err := openScheduleStateDB(ctx, path)
	if err != nil {
		log.Warn("opening job state db", "path", path, "err", err)
	} else {
		stateDB = db
		defer func() { _ = db.Close() }()

		log.Debug("opened job state db", "path", path)
	}

	sweepStartupRetention(ctx, stateDB, rc.jobs, log)

	store := newStatusStore(rc.jobs)

	if stateDB != nil {
		seedStatusFromState(ctx, stateDB, rc.jobs, store, log)
	}

	r := &runner{log: log, store: store, stateDB: stateDB, identity: identity}

	if stateDB != nil {
		jobsByName := make(map[string]*config, len(rc.jobs))
		for _, j := range rc.jobs {
			jobsByName[j.name] = j
		}

		monitor := &outstandingUploadMonitor{db: stateDB, jobsByName: jobsByName, store: store, identity: identity, log: log}

		go monitor.run(ctx)
	}

	// Independent of the web UI: a daily report is useful for anyone
	// monitoring receivers by inbox, not just those watching the dashboard.
	// runDailyReportLoop itself no-ops when report.enabled isn't set.
	go runDailyReportLoop(ctx, rc, stateDB, log)

	var srv *webUIServer

	if rc.listen != "" {
		sweepStartupReceiverRetention(ctx, stateDB, rc.receivers, log)

		oAuth := setupOIDCAuth(ctx, rc.oidc, log)
		srv = startWebUI(rc.listen, store, rc.receivers, log, stateDB, logs, rc.webUIUsername, rc.webUIPassword, oAuth, identity)

		go monitorStaleReceivers(ctx, rc.receivers, log)
	}

	var wg sync.WaitGroup

	log.Info("starting jobs", "count", len(rc.jobs))

	for _, job := range rc.jobs {
		warnIfKeyWontChange(log, job)

		wg.Go(func() {
			r.schedule(ctx, job)
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
		srv.shutdown()
	}

	if r.failed.Load() {
		log.Warn("run finished with failures")

		return 1
	}

	log.Info("run finished")

	return 0
}

// runner tracks whether any job run has failed across the concurrently
// scheduled jobs.
type runner struct {
	log      *slog.Logger
	store    *statusStore
	stateDB  *sql.DB         // nil only if the db couldn't be opened
	identity *serverIdentity // nil if loadServerIdentity failed at startup; see config.identity
	failed   atomic.Bool
}

// seedStatusFromState initializes store's jobs from previously persisted
// last-run info (see readLastRun), so a restart's web UI can still show
// when each job last ran instead of every job reverting to "never" until it
// next runs. Called once at startup, before the jobs' own goroutines start.
func seedStatusFromState(ctx context.Context, db *sql.DB, jobs []*config, store *statusStore, log *slog.Logger) {
	for _, j := range jobs {
		run, ok, err := readLastRun(ctx, db, j.name)
		if err != nil {
			log.Warn("reading last run from state db", "job", j.name, "err", err)
			continue
		}

		if !ok {
			continue
		}

		log.Debug("seeded job status from state db", "job", j.name, "state", run.State, "last_end", run.End)

		store.seedLastRun(j.name, run)
	}

	for _, j := range jobs {
		targetRuns, err := readTargetRuns(ctx, db, j.name)
		if err != nil {
			log.Warn("reading target runs from state db", "job", j.name, "err", err)
			continue
		}

		for _, tr := range targetRuns {
			store.seedTargetRun(j.name, tr.Index, tr.State, tr.Error)
		}
	}
}

// lastJobSuccess returns job name's last recorded successful run, or the
// zero Time if none is recorded (or state tracking is unavailable) — which
// correctly makes an unknown job look due for a catch-up run.
func (r *runner) lastJobSuccess(ctx context.Context, name string) time.Time {
	if r.stateDB == nil {
		return time.Time{}
	}

	t, ok, err := readLastSuccess(ctx, r.stateDB, name)
	if err != nil {
		r.log.Warn("reading last success from state db", "job", name, "err", err)
		return time.Time{}
	}

	if !ok {
		return time.Time{}
	}

	return t
}

// recordJobSuccess persists that job name just completed successfully, so a
// future restart can tell this run's grid slot isn't missed.
func (r *runner) recordJobSuccess(ctx context.Context, name string) {
	if r.stateDB == nil {
		return
	}

	if err := writeLastSuccess(ctx, r.stateDB, name, time.Now()); err != nil {
		r.log.Warn("recording job success to state db", "job", name, "err", err)
	}
}

// recordLastRun persists job name's just-finished run (whether it fully
// succeeded, partly succeeded, or failed outright, and regardless of
// whether it uses start-time), so a future restart's web UI can still show
// it via seedStatusFromState — unlike recordJobSuccess, which only tracks
// full successes and only matters for start-time-anchored jobs' catch-up
// scheduling. state is whatever statusStore.finished just computed and
// recorded live, so the persisted summary matches it rather than
// re-deriving its own (possibly inconsistent) view from err alone.
func (r *runner) recordLastRun(ctx context.Context, name string, start time.Time, state runState, err error, size int64) {
	if r.stateDB == nil {
		return
	}

	run := lastRun{Start: start, End: time.Now(), State: state, Size: size}

	if err != nil {
		run.Error = err.Error()
	}

	if state == stateFailed {
		// No target got a copy of the backup, so there's no size worth
		// reporting (unlike stateIncomplete, where at least one did).
		run.Size = 0
	}

	if err := writeLastRun(ctx, r.stateDB, name, run); err != nil {
		r.log.Warn("recording last run to state db", "job", name, "err", err)
	}
}

// schedule runs job on its configured cadence until ctx is done.
//
// A job with no start-time runs once immediately, then, if job.interval > 0,
// keeps re-running it every interval — the original behavior, unchanged.
//
// A job with start-time set runs on the start-time, start-time+interval,
// start-time+2*interval, ... grid. If the most recent due grid slot has no
// recorded successful run (see lastJobSuccess), it's a genuinely missed run
// (e.g. the process was down through it) and schedule catches up with a
// single immediate run; otherwise it just waits for the next future slot.
// Every subsequent run recomputes its next slot from start-time rather than
// accumulating +interval, so the schedule stays exactly grid-aligned
// regardless of how long a run takes.
func (r *runner) schedule(ctx context.Context, job *config) {
	log := r.log.With("job", job.name)

	if job.startTime.IsZero() {
		r.runOnce(ctx, job)

		if job.interval <= 0 {
			return
		}

		r.store.setNextRun(job.name, time.Now().Add(job.interval))
		log.Debug("scheduled next run", "interval", job.interval)

		ticker := time.NewTicker(job.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runOnce(ctx, job)
				r.store.setNextRun(job.name, time.Now().Add(job.interval))
				log.Debug("scheduled next run", "interval", job.interval)
			}
		}
	}

	next := job.startTime

	if due, ok := lastDueSlot(job.startTime, job.interval, time.Now()); ok &&
		!r.lastJobSuccess(ctx, job.name).Before(due) {
		// The most recent due slot is already covered by a recorded
		// success (e.g. we restarted moments after an on-time run) — no
		// run was actually missed, so don't fire an extra one now.
		next = nextGridTime(job.startTime, job.interval, time.Now())
		log.Debug("most recent due slot already recorded, waiting for next slot", "due", due, "next_run", next)
	} else {
		log.Debug("scheduling on start-time grid", "start_time", job.startTime, "next_run", next)
	}

	r.store.setNextRun(job.name, next)

	for {
		if !waitUntil(ctx, next) {
			return
		}

		r.runOnce(ctx, job)

		next = nextGridTime(job.startTime, job.interval, time.Now())
		r.store.setNextRun(job.name, next)
		log.Debug("scheduled next run", "next_run", next)
	}
}

// waitUntil blocks until t, or ctx is done (returning false). Returns
// immediately (true) if t is already in the past — this is what lets a
// start-time-anchored job catch up a missed run on startup instead of
// waiting out a full interval.
func waitUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return true
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// lastDueSlot returns the most recent grid slot (start, start+interval, ...)
// that is <= now, and false if start itself hasn't arrived yet.
func lastDueSlot(start time.Time, interval time.Duration, now time.Time) (time.Time, bool) {
	if now.Before(start) {
		return time.Time{}, false
	}

	steps := int64(now.Sub(start) / interval)

	return start.Add(interval * time.Duration(steps)), true
}

// nextGridTime returns the earliest time strictly after now that lies on
// start's interval grid (start, start+interval, start+2*interval, ...).
// Recomputing from start on every call (rather than accumulating
// next+interval) keeps a start-time-anchored job's repeats exactly aligned
// to that grid regardless of how long a run took or how late a catch-up run
// fired.
func nextGridTime(start time.Time, interval time.Duration, now time.Time) time.Time {
	if !now.After(start) {
		return start.Add(interval)
	}

	steps := int64(now.Sub(start)/interval) + 1

	return start.Add(interval * time.Duration(steps))
}

// runOnce runs job a single time, resolving a fresh {time} timestamp in its
// key first so a repeating job doesn't overwrite the same object on every
// run, and reports the outcome to r.log.
func (r *runner) runOnce(ctx context.Context, job *config) {
	run := *job
	run.key = substituteKeyTime(job.key)
	run.stateDB = r.stateDB
	run.identity = r.identity

	log := r.log.With("job", job.name, "key", run.key)

	start := time.Now()

	r.store.starting(job.name)
	log.Info("job starting", "targets", len(run.targets))

	// Refreshes this target's state (in both the live status store and the
	// state db) the moment its own outcome is known, independently of every
	// other target and without waiting for the whole job to finish — see
	// runPipeline's onTargetDone doc.
	onTargetDone := func(index int, terr error) {
		r.store.targetDone(job.name, index, terr)
		r.persistTargetRun(ctx, job.name, index, terr)
	}

	bytesWritten, err := runPipeline(ctx, &run, log, onTargetDone)
	duration := time.Since(start)

	state := r.store.finished(job.name, err, bytesWritten)
	r.recordLastRun(ctx, job.name, start, state, err, bytesWritten)

	if err != nil {
		r.failed.Store(true)

		if state == stateIncomplete {
			// Some targets got the backup and some didn't — worth flagging,
			// but distinct from (and less severe than) every target failing.
			log.Warn("job incomplete: some targets failed", "duration", duration, "err", jobError(job, err))
		} else {
			log.Error("job failed", "duration", duration, "err", jobError(job, err))
		}

		return
	}

	if !job.startTime.IsZero() {
		r.recordJobSuccess(ctx, job.name)
	}

	log.Info("job finished", "duration", duration, "bytes", bytesWritten)

	for i := range run.targets {
		t := &run.targets[i]

		switch t.kind {
		case serverKindLocal:
			log.Info("wrote target", "target", targetLabel(t), "path", localObjectPath(&run, t))
		case serverKindRemote:
			log.Info("uploaded target", "target", targetLabel(t), "url", remoteObjectURL(t, run.key))
		case serverKindS3:
			log.Info("uploaded target", "target", targetLabel(t), "url", fmt.Sprintf("s3://%s/%s", t.bucket, run.key))
		}
	}
}

// persistTargetRun records target index's just-finished success/failure to
// the state db, mirroring recordLastRun's per-job persistence one level
// down. Best-effort: a db hiccup here shouldn't fail the run, matching
// recordLastRun's own reasoning.
func (r *runner) persistTargetRun(ctx context.Context, jobName string, index int, terr error) {
	if r.stateDB == nil {
		return
	}

	state, errText := stateOK, ""
	if terr != nil {
		state, errText = stateFailed, terr.Error()
	}

	if err := writeTargetRun(ctx, r.stateDB, jobName, index, state, errText, time.Now()); err != nil {
		r.log.Warn("recording target run to state db", "job", jobName, "target", index, "err", err)
	}
}

// substituteKeyTime replaces the {time} placeholder in key, if present,
// with the current UTC timestamp. Called fresh immediately before every
// run (see runner.runOnce) rather than once at parse time, so a job that
// repeats gets a distinct object key on every run.
func substituteKeyTime(key string) string {
	return strings.ReplaceAll(key, "{time}", time.Now().UTC().Format("20060102-150405"))
}

// warnIfKeyWontChange warns when a repeating job's key has no {time}
// placeholder: every run would then overwrite the same S3 object, silently
// leaving only the most recent backup instead of a history of them.
func warnIfKeyWontChange(log *slog.Logger, job *config) {
	if job.interval <= 0 || strings.Contains(job.key, "{time}") {
		return
	}

	log.Warn("repeating job's key has no {time} placeholder; every run will overwrite the same object", "job", job.name, "interval", job.interval, "key", job.key)
}

// sweepStartupRetention runs one retention sweep, before any job starts, for
// every distinct local server (identified by root path) with retention: set
// among jobs' targets. Without this, a server whose jobs all run on long
// intervals would only get swept whenever one of them next happens to write
// to it — potentially long after files there actually expired, e.g. right
// after go-backup-tool restarts following a period of downtime. A nil db
// (retention tracking unavailable this run) is a no-op.
func sweepStartupRetention(ctx context.Context, db *sql.DB, jobs []*config, log *slog.Logger) {
	if db == nil {
		return
	}

	seen := make(map[string]bool)

	for _, j := range jobs {
		for i := range j.targets {
			t := &j.targets[i]
			if t.kind != serverKindLocal || t.retention <= 0 || seen[t.localPath] {
				continue
			}

			seen[t.localPath] = true

			log.Debug("startup retention sweep", "server", t.serverName, "path", t.localPath, "retention", t.retention)

			if err := sweepRetentionForTarget(ctx, db, t, log); err != nil {
				log.Warn("startup retention sweep failed", "server", t.serverName, "err", err)
			}
		}
	}
}
