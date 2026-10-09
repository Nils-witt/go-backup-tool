package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// Runner tracks whether any job run has failed across the concurrently
// scheduled jobs.
type Runner struct {
	log      *slog.Logger
	store    *backup.StatusStore
	stateDB  *store.Store             // nil only if the db couldn't be opened
	identity *identity.ServerIdentity // nil if loadServerIdentity failed at startup; see Config.Identity
	queue    *notify.Queue            // retries a failed job-failure email later; see notify.SendMailQueued

	// notifications is the live registry jobs' failure-notifications ids
	// are looked up in when a run fails (see notifyJobFailure).
	notifications *notify.Registry
	failed        atomic.Bool

	// targetFailureMu guards targetFailures, the in-memory (not persisted)
	// consecutive-failure streak per (job, target) — reset to 0 on any
	// success, incremented on any failure, both via handleTargetOutcome.
	// Deliberately in-memory only and not backed by the state db: a
	// consecutive-failure streak is only meaningful within one process's
	// uptime, and resetting it across a restart is an accepted tradeoff for
	// simplicity (see the on-error/on-recover feature's design notes).
	targetFailureMu sync.Mutex
	targetFailures  map[targetKey]int

	// dockerHost is the Docker daemon a job's or command's container: runs
	// through (see UseDocker); copied onto each run's config.
	dockerHost string
}

// targetKey identifies one (job, target) pair for Runner.targetFailures.
// bucket is included alongside server because a job may list the same
// server twice with different buckets (distinct targets); job+server alone
// would conflate them.
type targetKey struct {
	job    string
	server string
	bucket string
}

// NewRunner builds a Runner sharing store/stateDB/identity/queue across
// every job scheduled through it in this run.
func NewRunner(log *slog.Logger, statusStore *backup.StatusStore, stateDB *store.Store, identity *identity.ServerIdentity, queue *notify.Queue, notifications *notify.Registry) *Runner {
	return &Runner{log: log, store: statusStore, stateDB: stateDB, identity: identity, queue: queue, notifications: notifications, targetFailures: make(map[targetKey]int)}
}

// UseDocker sets the Docker daemon address (see dockerexec.New) that jobs
// and commands with a container: run through. Call it before any job runs.
func (r *Runner) UseDocker(host string) {
	r.dockerHost = host
}

// Failed reports whether any job run has failed so far.
func (r *Runner) Failed() bool {
	return r.failed.Load()
}

// SeedStatusFromState initializes store's jobs from previously persisted
// last-run info (see backup.ReadLastRun), so a restart's web UI can still
// show when each job last ran instead of every job reverting to "never"
// until it next runs. Called once at startup, before the jobs' own
// goroutines start.
func SeedStatusFromState(ctx context.Context, db *store.Store, jobs []*config.Config, statusStore *backup.StatusStore, log *slog.Logger) {
	for _, j := range jobs {
		run, ok, err := db.GetLastRun(ctx, j.Name)
		if err != nil {
			log.Warn("reading last run from state db", "job", j.Name, "err", err)
			continue
		}

		if !ok {
			continue
		}

		log.Debug("seeded job status from state db", "job", j.Name, "success", run.Success, "last_end", run.End)

		state := backup.StateFailed
		if run.Success {
			state = backup.StateOK
		}

		statusStore.SeedLastRun(j.Name, run.Start, run.End, state, run.Error, run.Size)
	}

	for _, j := range jobs {
		targetRuns, err := db.ListTargetRuns(ctx, j.Name)
		if err != nil {
			log.Warn("reading target runs from state db", "job", j.Name, "err", err)
			continue
		}

		for _, tr := range targetRuns {
			statusStore.SeedTargetRun(j.Name, tr.Target, backup.RunState(tr.State), tr.Error)
		}
	}
}

// lastJobSuccess returns job name's last recorded successful run, or the
// zero Time if none is recorded (or state tracking is unavailable) — which
// correctly makes an unknown job look due for a catch-up run.
func (r *Runner) lastJobSuccess(ctx context.Context, name string) time.Time {
	if r.stateDB == nil {
		return time.Time{}
	}

	t, ok, err := r.stateDB.GetLastJobSuccess(ctx, name)
	if err != nil {
		r.log.Warn("reading last success from state db", "job", name, "err", err)
		return time.Time{}
	}

	if !ok {
		return time.Time{}
	}

	return t
}

// JobLookup returns the current definition of the job name, reporting false
// if there's no such job (any more) — e.g. jobs.Manager.Get, which reflects
// web UI edits as they're made.
type JobLookup func(name string) (*config.Config, bool)

// StaticJobs is a JobLookup over a fixed set of jobs.
func StaticJobs(jobs []*config.Config) JobLookup {
	byName := make(map[string]*config.Config, len(jobs))
	for _, j := range jobs {
		byName[j.Name] = j
	}

	return func(name string) (*config.Config, bool) {
		j, ok := byName[name]
		return j, ok
	}
}

// Schedule runs job on its configured cadence until ctx is done — see
// ScheduleLive, which this is for a job that never changes.
func (r *Runner) Schedule(ctx context.Context, job *config.Config) {
	r.ScheduleLive(ctx, ctx, job, StaticJobs([]*config.Config{job}), false)
}

// ScheduleLive runs job on its configured cadence until stop is done.
//
// A job with no start-time runs once immediately, then, if job.Interval > 0,
// keeps re-running it every interval.
//
// A job with start-time set runs on the start-time, start-time+interval,
// start-time+2*interval, ... grid. If the most recent due grid slot has no
// recorded successful run (see lastJobSuccess), it's a genuinely missed run
// (e.g. the process was down through it) and ScheduleLive catches up with a
// single immediate run; otherwise it just waits for the next future slot.
// Every subsequent run recomputes its next slot from start-time rather than
// accumulating +interval, so the schedule stays exactly grid-aligned
// regardless of how long a run takes.
//
// job's interval and start-time fix the schedule; everything else is looked
// up afresh (via lookup, by job's name) right before every run, so an edit
// that leaves the schedule alone applies from the next run on, and a job
// that no longer exists ends the schedule. Runs use runCtx, while stop only
// ends the waiting between them: canceling stop (e.g. to reschedule a job
// whose interval changed) never interrupts a run in flight.
//
// resumed marks a job rescheduled after such an edit rather than starting
// fresh: without start-time, it then doesn't run immediately but waits out
// its interval from its last run's start — and a job that doesn't repeat
// doesn't run again at all.
func (r *Runner) ScheduleLive(runCtx, stop context.Context, job *config.Config, lookup JobLookup, resumed bool) {
	name := job.Name
	log := r.log.With("job", name)

	run := func() bool {
		if stop.Err() != nil {
			return false
		}

		cur, ok := lookup(name)
		if !ok {
			return false
		}

		r.runOnce(runCtx, cur)

		return true
	}

	if job.StartTime.IsZero() {
		r.scheduleInterval(stop, name, job.Interval, resumed, run, log)
		return
	}

	// job.StartTime is already UTC (see config's parsing of start-time), so
	// every grid slot computed from it is too.
	next := job.StartTime

	if due, ok := lastDueSlot(job.StartTime, job.Interval, time.Now()); ok &&
		!r.lastJobSuccess(runCtx, name).Before(due) {
		// The most recent due slot is already covered by a recorded
		// success (e.g. we restarted moments after an on-time run) — no
		// run was actually missed, so don't fire an extra one now.
		next = nextGridTime(job.StartTime, job.Interval, time.Now())
		log.Debug("most recent due slot already recorded, waiting for next slot", "due", due, "next_run", next)
	} else {
		log.Debug("scheduling on start-time grid", "start_time", job.StartTime, "next_run", next)
	}

	r.store.SetNextRun(name, next)

	for {
		if !waitUntil(stop, next) || !run() {
			return
		}

		next = nextGridTime(job.StartTime, job.Interval, time.Now())
		r.store.SetNextRun(name, next)
		log.Debug("scheduled next run", "next_run", next)
	}
}

// scheduleInterval is ScheduleLive for a job without start-time: run once
// (immediately, unless resumed — see ScheduleLive), then every interval, on
// a fixed rate from the end of that first run (a slot that passes while a
// run is still going is skipped, not queued). run reports false once the
// schedule should end.
func (r *Runner) scheduleInterval(stop context.Context, name string, interval time.Duration, resumed bool, run func() bool, log *slog.Logger) {
	if resumed {
		if interval <= 0 {
			r.store.SetNextRun(name, time.Time{})
			return
		}

		first := time.Now().UTC()
		if last := r.store.LastStart(name); !last.IsZero() && last.Add(interval).After(first) {
			first = last.Add(interval).UTC()
		}

		r.store.SetNextRun(name, first)

		if !waitUntil(stop, first) {
			return
		}
	}

	if !run() || interval <= 0 {
		return
	}

	next := time.Now().UTC().Add(interval)

	for {
		r.store.SetNextRun(name, next)
		log.Debug("scheduled next run", "interval", interval, "next_run", next)

		if !waitUntil(stop, next) || !run() {
			return
		}

		for now := time.Now(); !next.After(now); {
			next = next.Add(interval)
		}
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
func (r *Runner) runOnce(ctx context.Context, job *config.Config) {
	now := time.Now().UTC()

	run := *job
	run.Key = substituteKeyTime(job.Key, now)
	run.CreatedAt = now
	run.StateDB = r.stateDB
	run.Identity = r.identity
	run.DockerHost = r.dockerHost

	log := r.log.With("job", job.Name, "key", run.Key)

	start := time.Now()

	r.store.Starting(job.Name)
	log.Info("job starting", "targets", len(run.Targets))

	onTargetDone := func(index int, terr error) {
		r.store.TargetDone(job.Name, index, terr)

		if index < 0 || index >= len(run.Targets) {
			return
		}

		r.persistTargetRun(ctx, job.Name, terr == nil, job.Targets[index].ServerName, terr)
		r.handleTargetOutcome(ctx, job.Name, &job.Targets[index], terr, log)
	}

	bytesWritten, err := runPipeline(ctx, &run, log, onTargetDone)
	duration := time.Since(start)

	state := r.store.Finished(job.Name, err, bytesWritten)

	if err != nil {
		r.failed.Store(true)

		if state == backup.StateIncomplete {
			log.Warn("job incomplete: some targets failed", "duration", duration, "err", config.JobError(job, err))
		} else {
			log.Error("job failed", "duration", duration, "err", config.JobError(job, err))
		}

		notifyJobFailure(job, r.notifications, err, state, start, duration, r.queue, log)
		r.recordJobRun(ctx, job.Name, state, false, start, bytesWritten, config.JobError(job, err).Error())

		return
	}

	log.Info("job finished", "duration", duration, "bytes", bytesWritten)
	r.recordJobRun(ctx, job.Name, state, true, start, bytesWritten, "")
}

// RetryFailedTargets re-runs job for just the targets in job.Targets whose
// ServerName is in targetNames, leaving every other target's already-
// recorded status untouched. Unlike a live run's per-target handling (there
// is no per-target retry within a single run — see runPipeline's doc
// comment), this re-executes the whole pipeline for the given targets: the
// source command, gpg encryption, and staging, since the original run's
// staged file was already removed once it finished, leaving nothing to
// re-upload from. It's used by the web UI's "retry failed targets" action
// (see handleRetryFailedTargets in webui.go); ctx is expected to be
// detached from the triggering HTTP request's own cancellation (see
// context.WithoutCancel) so the retry isn't cut short just because that
// request has already returned its response.
func (r *Runner) RetryFailedTargets(ctx context.Context, job *config.Config, targetNames []string) error {
	indices := make([]int, 0, len(targetNames))

	for i, t := range job.Targets {
		if slices.Contains(targetNames, t.ServerName) {
			indices = append(indices, i)
		}
	}

	if len(indices) == 0 {
		return fmt.Errorf("no matching targets to retry among %v", targetNames)
	}

	now := time.Now().UTC()

	run := *job
	run.Key = substituteKeyTime(job.Key, now)
	run.CreatedAt = now
	run.StateDB = r.stateDB
	run.Identity = r.identity
	run.DockerHost = r.dockerHost
	run.Targets = make([]config.Target, len(indices))

	for i, idx := range indices {
		run.Targets[i] = job.Targets[idx]
	}

	log := r.log.With("job", job.Name, "key", run.Key, "retry_targets", targetNames)

	start := time.Now()

	r.store.RetryStarting(job.Name, targetNames)
	log.Info("retrying failed targets", "targets", len(run.Targets))

	// onTargetDone is called with indices into run.Targets (the retried
	// subset); indices[localIndex] maps that back to the target's original
	// position in job.Targets, which is what the status store and target-run
	// persistence are keyed on.
	onTargetDone := func(localIndex int, terr error) {
		if localIndex < 0 || localIndex >= len(indices) {
			return
		}

		origIndex := indices[localIndex]

		r.store.TargetDone(job.Name, origIndex, terr)
		r.persistTargetRun(ctx, job.Name, terr == nil, job.Targets[origIndex].ServerName, terr)
		r.handleTargetOutcome(ctx, job.Name, &job.Targets[origIndex], terr, log)
	}

	bytesWritten, err := runPipeline(ctx, &run, log, onTargetDone)
	duration := time.Since(start)

	state := r.store.Finished(job.Name, err, bytesWritten)

	if err != nil {
		r.failed.Store(true)

		if state == backup.StateIncomplete {
			log.Warn("retry incomplete: some targets still failing", "duration", duration, "err", config.JobError(job, err))
		} else {
			log.Error("retry failed", "duration", duration, "err", config.JobError(job, err))
		}

		notifyJobFailure(job, r.notifications, err, state, start, duration, r.queue, log)
		r.recordJobRun(ctx, job.Name, state, false, start, bytesWritten, config.JobError(job, err).Error())

		return err
	}

	log.Info("retry finished", "duration", duration, "bytes", bytesWritten)
	r.recordJobRun(ctx, job.Name, state, true, start, bytesWritten, "")

	return nil
}

// recordJobRun persists job name's just-finished run (whether it fully
// succeeded, partly succeeded, or failed outright), so a future restart's
// web UI can still show it via SeedStatusFromState. Best-effort: a db
// hiccup here shouldn't fail the run.
func (r *Runner) recordJobRun(ctx context.Context, name string, state backup.RunState, success bool, start time.Time, bytesWritten int64, errText string) {
	if r.stateDB == nil {
		return
	}

	if err := r.stateDB.SaveJobRun(ctx, name, string(state), success, start, time.Now(), bytesWritten, errText); err != nil {
		r.log.Warn("recording job run to state db", "job", name, "err", err)
	}
}

// persistTargetRun records target's just-finished success/failure to the
// state db, mirroring recordJobRun's per-job persistence one level down.
// Best-effort: a db hiccup here shouldn't fail the run, matching
// recordJobRun's own reasoning.
func (r *Runner) persistTargetRun(ctx context.Context, jobName string, success bool, target string, terr error) {
	if r.stateDB == nil {
		return
	}

	var (
		state   backup.RunState
		errText string
	)

	backup.SetOutcome(&state, &errText, terr)

	if err := r.stateDB.SaveTargetRun(ctx, jobName, success, target, string(state), errText, time.Now()); err != nil {
		r.log.Warn("recording target run to state db", "job", jobName, "target", target, "err", err)
	}
}

// handleTargetOutcome updates t's in-memory consecutive-failure streak for
// jobName (see Runner.targetFailures). On a failure, once that streak
// reaches t.OnErrorAfter, it fires t.OnErrorCommand — and, unless
// t.OnErrorOnce, again on every subsequent consecutive failure, until a
// success resets the streak. On a
// success that ends a streak of one or more failures, it fires
// t.OnRecoverCommand once. Called by both runOnce's and
// RetryFailedTargets's own onTargetDone closures, so the two entry points
// share identical on-error/on-recover semantics. A target with neither
// command configured (the common case) is a fast no-op.
func (r *Runner) handleTargetOutcome(ctx context.Context, jobName string, t *config.Target, terr error, log *slog.Logger) {
	if t.OnErrorCommand == nil && t.OnRecoverCommand == nil {
		return
	}

	key := targetKey{job: jobName, server: t.ServerName, bucket: t.Bucket}

	r.targetFailureMu.Lock()

	if terr == nil {
		streak := r.targetFailures[key]
		delete(r.targetFailures, key)
		r.targetFailureMu.Unlock()

		if t.OnRecoverCommand != nil && streak > 0 {
			fireTargetCommand(ctx, r.dockerHost, "on-recover", jobName, t, *t.OnRecoverCommand, streak, nil, log)
		}

		return
	}

	r.targetFailures[key]++
	streak := r.targetFailures[key]

	r.targetFailureMu.Unlock()

	if t.OnErrorCommand == nil || streak < t.OnErrorAfter || (t.OnErrorOnce && streak > t.OnErrorAfter) {
		return
	}

	fireTargetCommand(ctx, r.dockerHost, "on-error", jobName, t, *t.OnErrorCommand, streak, terr, log)
}

// fireTargetCommand runs cmd via runTargetCommand under a context detached
// from ctx's cancellation (the triggering run has already finished) and
// bounded by cmd.Timeout.
func fireTargetCommand(ctx context.Context, dockerHost, kind, jobName string, t *config.Target, cmd config.Command, streak int, terr error, log *slog.Logger) {
	cmdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cmd.Timeout)
	defer cancel()

	runTargetCommand(cmdCtx, dockerHost, kind, jobName, t, cmd, streak, terr, log)
}

// RunOutstandingUploadRetries retries every target upload recorded as
// outstanding (see uploadStagedToTargets) every TargetUploadRetryInterval,
// until ctx is done. It's meant to run for the process's lifetime in its own
// goroutine, mirroring notify.Queue.Run, started once by app.Run. lookup finds
// each outstanding row's job and target definition by name, since a row can
// outlive the run that recorded it. A nil r.stateDB (no persistence
// available, so nothing could have been recorded in the first place) makes
// this a no-op.
func (r *Runner) RunOutstandingUploadRetries(ctx context.Context, lookup JobLookup) {
	if r.stateDB == nil {
		return
	}

	log := r.log.With("component", "upload-retry-queue")

	ticker := time.NewTicker(TargetUploadRetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.retryOutstandingUploads(ctx, lookup, log)
		}
	}
}

// retryOutstandingUploads attempts every outstanding target upload whose
// retry time has come due.
func (r *Runner) retryOutstandingUploads(ctx context.Context, lookup JobLookup, log *slog.Logger) {
	due, err := r.stateDB.ListDueOutstandingTargetUploads(ctx, time.Now())
	if err != nil {
		log.Warn("listing due outstanding target uploads", "err", err)
		return
	}

	for _, row := range due {
		r.retryOutstandingUpload(ctx, lookup, row, log)
	}
}

// retryOutstandingUpload attempts a single outstanding target upload. On
// success, it updates the target's live status and target-run history the
// same way a normal run's own onTargetDone would, then forgets row. It also
// gives up (forgetting row without ever succeeding) if row's job or target
// no longer exists in the current config, or if its staged file is gone —
// none of those are things a later retry could ever fix.
func (r *Runner) retryOutstandingUpload(ctx context.Context, lookup JobLookup, row store.OutstandingTargetUpload, log *slog.Logger) {
	log = log.With("job", row.JobName, "target", row.Target, "file", row.FileName)

	job, ok := lookup(row.JobName)
	if !ok {
		log.Warn("outstanding target upload references unknown job; giving up")
		r.forgetOutstandingUpload(ctx, row, log)

		return
	}

	index := slices.IndexFunc(job.Targets, func(t config.Target) bool { return t.ServerName == row.Target })
	if index < 0 {
		log.Warn("outstanding target upload references unknown target; giving up")
		r.forgetOutstandingUpload(ctx, row, log)

		return
	}

	if _, err := os.Stat(row.FileName); err != nil {
		log.Warn("staged backup no longer available for retry; giving up", "err", err)
		r.forgetOutstandingUpload(ctx, row, log)

		return
	}

	run := *job
	run.Key = row.Key
	run.CreatedAt = row.CreatedAt
	run.StateDB = r.stateDB
	run.Identity = r.identity
	run.DockerHost = r.dockerHost

	if err := uploadTargetAttempt(ctx, &run, &run.Targets[index], row.FileName, log); err != nil {
		log.Warn("outstanding target upload retry failed", "err", err)

		if err1 := r.stateDB.RescheduleOutstandingTargetUpload(ctx, row.ID, time.Now().Add(TargetUploadRetryInterval)); err1 != nil {
			log.Warn("rescheduling outstanding target upload", "err", err1)
		}

		return
	}

	log.Info("outstanding target upload retry succeeded")

	r.store.TargetDone(job.Name, index, nil)
	r.store.RefreshJobState(job.Name)
	r.persistTargetRun(ctx, job.Name, true, row.Target, nil)

	r.forgetOutstandingUpload(ctx, row, log)
}

// forgetOutstandingUpload removes row from the state db and, if no other
// outstanding upload still references its staged file, removes that file
// too — the cleanup runPipeline itself would have done immediately had the
// upload succeeded on its first attempt (see removeStagingFileIfUnreferenced).
func (r *Runner) forgetOutstandingUpload(ctx context.Context, row store.OutstandingTargetUpload, log *slog.Logger) {
	if err := r.stateDB.DeleteOutstandingTargetUpload(ctx, row.ID); err != nil {
		log.Warn("deleting outstanding target upload", "err", err)
		return
	}

	removeStagingFileIfUnreferenced(ctx, r.stateDB, row.FileName, log)
}

// substituteKeyTime replaces the {time} placeholder in key, if present,
// with now (formatted as a UTC timestamp). Called fresh immediately before
// every run (see Runner.runOnce) rather than once at parse time, so a job
// that repeats gets a distinct object key on every run; now is also stamped
// on that run's Config.CreatedAt, so the key and the retention basis sent to
// a receiver agree on the same instant.
func substituteKeyTime(key string, now time.Time) string {
	return strings.ReplaceAll(key, "{time}", now.Format("20060102-150405"))
}

// WarnIfKeyWontChange warns when a repeating job's key has no {time}
// placeholder: every run would then overwrite the same object, silently
// leaving only the most recent backup instead of a history of them.
func WarnIfKeyWontChange(log *slog.Logger, job *config.Config) {
	if job.Interval <= 0 || strings.Contains(job.Key, "{time}") {
		return
	}

	log.Warn("repeating job's key has no {time} placeholder; every run will overwrite the same object", "job", job.Name, "interval", job.Interval, "key", job.Key)
}

// SweepStartupRetention runs one retention sweep, before any job starts, for
// every distinct local server (identified by root path) with retention: set
// among jobs' targets. Without this, a server whose jobs all run on long
// intervals would only get swept whenever one of them next happens to write
// to it — potentially long after files there actually expired, e.g. right
// after go-backup-tool restarts following a period of downtime. A nil db
// (retention tracking unavailable this run) is a no-op.
func SweepStartupRetention(ctx context.Context, db *store.Store, jobs []*config.Config, log *slog.Logger) {
	if db == nil {
		return
	}

	seen := make(map[string]bool)

	for _, j := range jobs {
		for i := range j.Targets {
			t := &j.Targets[i]
			if t.Kind != config.ServerKindLocal || t.Retention <= 0 || seen[t.LocalPath] {
				continue
			}

			seen[t.LocalPath] = true

			log.Debug("startup retention sweep", "server", t.ServerName, "path", t.LocalPath, "retention", t.Retention)

			if err := backup.SweepRetentionForTarget(ctx, db, t, log); err != nil {
				log.Warn("startup retention sweep failed", "server", t.ServerName, "err", err)
			}
		}
	}
}
