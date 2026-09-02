package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// Runner tracks whether any job run has failed across the concurrently
// scheduled jobs.
type Runner struct {
	log      *slog.Logger
	store    *backup.StatusStore
	stateDB  *store.Store             // nil only if the db couldn't be opened
	identity *identity.ServerIdentity // nil if loadServerIdentity failed at startup; see Config.Identity
	failed   atomic.Bool
}

// NewRunner builds a Runner sharing store/stateDB/identity across every job
// scheduled through it in this run.
func NewRunner(log *slog.Logger, statusStore *backup.StatusStore, stateDB *store.Store, identity *identity.ServerIdentity) *Runner {
	return &Runner{log: log, store: statusStore, stateDB: stateDB, identity: identity}
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

// Schedule runs job on its configured cadence until ctx is done.
//
// A job with no start-time runs once immediately, then, if job.Interval > 0,
// keeps re-running it every interval — the original behavior, unchanged.
//
// A job with start-time set runs on the start-time, start-time+interval,
// start-time+2*interval, ... grid. If the most recent due grid slot has no
// recorded successful run (see lastJobSuccess), it's a genuinely missed run
// (e.g. the process was down through it) and Schedule catches up with a
// single immediate run; otherwise it just waits for the next future slot.
// Every subsequent run recomputes its next slot from start-time rather than
// accumulating +interval, so the schedule stays exactly grid-aligned
// regardless of how long a run takes.
func (r *Runner) Schedule(ctx context.Context, job *config.Config) {
	log := r.log.With("job", job.Name)

	if job.StartTime.IsZero() {
		r.runOnce(ctx, job)

		if job.Interval <= 0 {
			return
		}

		r.store.SetNextRun(job.Name, time.Now().Add(job.Interval))
		log.Debug("scheduled next run", "interval", job.Interval)

		backup.RunPeriodically(ctx, job.Interval, false, func() {
			r.runOnce(ctx, job)
			r.store.SetNextRun(job.Name, time.Now().Add(job.Interval))
			log.Debug("scheduled next run", "interval", job.Interval)
		})

		return
	}

	next := job.StartTime

	if due, ok := lastDueSlot(job.StartTime, job.Interval, time.Now()); ok &&
		!r.lastJobSuccess(ctx, job.Name).Before(due) {
		// The most recent due slot is already covered by a recorded
		// success (e.g. we restarted moments after an on-time run) — no
		// run was actually missed, so don't fire an extra one now.
		next = nextGridTime(job.StartTime, job.Interval, time.Now())
		log.Debug("most recent due slot already recorded, waiting for next slot", "due", due, "next_run", next)
	} else {
		log.Debug("scheduling on start-time grid", "start_time", job.StartTime, "next_run", next)
	}

	r.store.SetNextRun(job.Name, next)

	for {
		if !waitUntil(ctx, next) {
			return
		}

		r.runOnce(ctx, job)

		next = nextGridTime(job.StartTime, job.Interval, time.Now())
		r.store.SetNextRun(job.Name, next)
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
func (r *Runner) runOnce(ctx context.Context, job *config.Config) {
	run := *job
	run.Key = substituteKeyTime(job.Key)
	run.StateDB = r.stateDB
	run.Identity = r.identity

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

	run := *job
	run.Key = substituteKeyTime(job.Key)
	run.StateDB = r.stateDB
	run.Identity = r.identity
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

// substituteKeyTime replaces the {time} placeholder in key, if present,
// with the current UTC timestamp. Called fresh immediately before every
// run (see Runner.runOnce) rather than once at parse time, so a job that
// repeats gets a distinct object key on every run.
func substituteKeyTime(key string) string {
	return strings.ReplaceAll(key, "{time}", time.Now().UTC().Format("20060102-150405"))
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
