package backup

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

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
// that dashboard reachable until ctx is canceled.
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

		_, _ = fmt.Fprintln(stderr, "error:", err)

		return 2
	}

	if rc.timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, rc.timeout)
		defer cancel()
	}

	sweepStartupRetention(ctx, rc.jobs, stderr)

	var stateDB *sql.DB

	// Opened whenever a job needs it for catch-up scheduling, or whenever
	// the web UI is up to show it: without listen: set, nothing reads the
	// persisted last-run info, so skip the file entirely for a plain CLI
	// run.
	if needsScheduleState(rc.jobs) || rc.listen != "" {
		db, err := openScheduleStateDB(ctx, scheduleStateDBPath(rc.configPath))
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "warning: opening job state db:", err)
		} else {
			stateDB = db
			defer func() { _ = db.Close() }()
		}
	}

	store := newStatusStore(rc.jobs)

	if stateDB != nil {
		seedStatusFromState(ctx, stateDB, rc.jobs, store, stderr)
	}

	r := &runner{stderr: stderr, store: store, stateDB: stateDB}

	var srv *webUIServer
	if rc.listen != "" {
		srv = startWebUI(rc.listen, store, stderr)
	}

	var wg sync.WaitGroup

	for _, job := range rc.jobs {
		warnIfKeyWontChange(stderr, job)

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
		return 1
	}

	return 0
}

// runner tracks whether any job run has failed across the concurrently
// scheduled jobs.
type runner struct {
	stderr  io.Writer
	store   *statusStore
	stateDB *sql.DB // nil if no job uses start-time, or the db couldn't be opened
	failed  atomic.Bool
}

// seedStatusFromState initializes store's jobs from previously persisted
// last-run info (see readLastRun), so a restart's web UI can still show
// when each job last ran instead of every job reverting to "never" until it
// next runs. Called once at startup, before the jobs' own goroutines start.
func seedStatusFromState(ctx context.Context, db *sql.DB, jobs []*config, store *statusStore, stderr io.Writer) {
	for _, j := range jobs {
		run, ok, err := readLastRun(ctx, db, j.name)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "warning:", err)
			continue
		}

		if !ok {
			continue
		}

		store.seedLastRun(j.name, run)
	}
}

// needsScheduleState reports whether any job sets start-time, i.e. whether
// the state db needs to be opened at all.
func needsScheduleState(jobs []*config) bool {
	for _, j := range jobs {
		if !j.startTime.IsZero() {
			return true
		}
	}

	return false
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
		_, _ = fmt.Fprintln(r.stderr, "warning:", err)
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
		_, _ = fmt.Fprintln(r.stderr, "warning:", err)
	}
}

// recordLastRun persists job name's just-finished run (whether it succeeded
// or failed, and regardless of whether it uses start-time), so a future
// restart's web UI can still show it via seedStatusFromState — unlike
// recordJobSuccess, which only tracks successes and only matters for
// start-time-anchored jobs' catch-up scheduling.
func (r *runner) recordLastRun(ctx context.Context, name string, start time.Time, err error, size int64) {
	if r.stateDB == nil {
		return
	}

	run := lastRun{Start: start, End: time.Now(), State: stateOK, Size: size}

	if err != nil {
		run.State = stateFailed
		run.Error = err.Error()
		run.Size = 0
	}

	if err := writeLastRun(ctx, r.stateDB, name, run); err != nil {
		_, _ = fmt.Fprintln(r.stderr, "warning:", err)
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
	if job.startTime.IsZero() {
		r.runOnce(ctx, job)

		if job.interval <= 0 {
			return
		}

		r.store.setNextRun(job.name, time.Now().Add(job.interval))

		ticker := time.NewTicker(job.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runOnce(ctx, job)
				r.store.setNextRun(job.name, time.Now().Add(job.interval))
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
	}

	r.store.setNextRun(job.name, next)

	for {
		if !waitUntil(ctx, next) {
			return
		}

		r.runOnce(ctx, job)

		next = nextGridTime(job.startTime, job.interval, time.Now())
		r.store.setNextRun(job.name, next)
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
// run, and reports the outcome to r.stderr.
func (r *runner) runOnce(ctx context.Context, job *config) {
	run := *job
	run.key = substituteKeyTime(job.key)

	start := time.Now()

	r.store.starting(job.name)

	targetErrs, bytesWritten, err := runPipeline(ctx, &run)
	r.recordTargetResults(job.name, run.targets, targetErrs, err)
	r.store.finished(job.name, err, bytesWritten)
	r.recordLastRun(ctx, job.name, start, err, bytesWritten)

	if err != nil {
		r.failed.Store(true)

		_, _ = fmt.Fprintln(r.stderr, "error:", jobError(job, err))

		return
	}

	if !job.startTime.IsZero() {
		r.recordJobSuccess(ctx, job.name)
	}

	// Status/diagnostic message, not program output, so it goes to
	// stderr; stdout stays silent on success.
	for i := range run.targets {
		t := &run.targets[i]

		if t.kind == serverKindLocal {
			_, _ = fmt.Fprintf(r.stderr, "%swrote %s (server %q)\n", jobLabel(job), localObjectPath(&run, t), t.serverName)
			continue
		}

		_, _ = fmt.Fprintf(r.stderr, "%suploaded s3://%s/%s (server %q)\n", jobLabel(job), t.bucket, run.key, t.serverName)
	}
}

// recordTargetResults updates the status store's per-target state for a
// finished run. targetErrs is index-aligned with targets when runPipeline
// reached the upload stage; if it failed earlier (e.g. the source command
// never started), targetErrs is empty and every target is instead marked
// with the run's overall error, since no target-specific detail exists.
func (r *runner) recordTargetResults(jobName string, targets []target, targetErrs []error, err error) {
	if len(targetErrs) == len(targets) {
		for i, terr := range targetErrs {
			r.store.targetDone(jobName, i, terr)
		}

		return
	}

	if err != nil {
		for i := range targets {
			r.store.targetDone(jobName, i, err)
		}
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
func warnIfKeyWontChange(stderr io.Writer, job *config) {
	if job.interval <= 0 || strings.Contains(job.key, "{time}") {
		return
	}

	_, _ = fmt.Fprintf(stderr, "warning: %srepeats every %s but its key has no {time} placeholder; every run will overwrite the same object\n", jobLabel(job), job.interval)
}

// jobLabel returns "<name>: ", prefixing status output with the job that
// produced it.
func jobLabel(cfg *config) string {
	return cfg.name + ": "
}

// sweepStartupRetention runs one retention sweep, before any job starts, for
// every distinct local server (identified by root path) with retention: set
// among jobs' targets. Without this, a server whose jobs all run on long
// intervals would only get swept whenever one of them next happens to write
// to it — potentially long after files there actually expired, e.g. right
// after go-backup-tool restarts following a period of downtime.
func sweepStartupRetention(ctx context.Context, jobs []*config, stderr io.Writer) {
	seen := make(map[string]bool)

	for _, j := range jobs {
		for i := range j.targets {
			t := &j.targets[i]
			if t.kind != serverKindLocal || t.retention <= 0 || seen[t.localPath] {
				continue
			}

			seen[t.localPath] = true

			if err := sweepRetentionForTarget(ctx, t); err != nil {
				_, _ = fmt.Fprintf(stderr, "warning: retention sweep for server %q: %v\n", t.serverName, err)
			}
		}
	}
}
