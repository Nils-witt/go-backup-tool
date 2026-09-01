package pipeline

import (
	"context"
	"log/slog"
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

		log.Debug("seeded job status from state db", "job", j.Name, "state", run.State, "last_end", run.End)

		statusStore.SeedLastRun(j.Name, run.Start, run.End, backup.RunState(run.State), run.Error, run.Size)
	}

	for _, j := range jobs {
		targetRuns, err := db.ListTargetRuns(ctx, j.Name)
		if err != nil {
			log.Warn("reading target runs from state db", "job", j.Name, "err", err)
			continue
		}

		for _, tr := range targetRuns {
			statusStore.SeedTargetRun(j.Name, tr.Index, backup.RunState(tr.State), tr.Error)
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

	t, ok, err := r.stateDB.GetLastSuccess(ctx, name)
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
func (r *Runner) recordJobSuccess(ctx context.Context, name string) {
	if r.stateDB == nil {
		return
	}

	if err := r.stateDB.SaveLastSuccess(ctx, name, time.Now()); err != nil {
		r.log.Warn("recording job success to state db", "job", name, "err", err)
	}
}

// recordLastRun persists job name's just-finished run (whether it fully
// succeeded, partly succeeded, or failed outright, and regardless of
// whether it uses start-time), so a future restart's web UI can still show
// it via SeedStatusFromState — unlike recordJobSuccess, which only tracks
// full successes and only matters for start-time-anchored jobs' catch-up
// scheduling. state is whatever StatusStore.Finished just computed and
// recorded live, so the persisted summary matches it rather than
// re-deriving its own (possibly inconsistent) view from err alone.
func (r *Runner) recordLastRun(ctx context.Context, name string, start time.Time, state backup.RunState, err error, size int64) {
	if r.stateDB == nil {
		return
	}

	run := store.LastRun{Start: start, End: time.Now(), State: string(state), Size: size}

	if err != nil {
		run.Error = err.Error()
	}

	if state == backup.StateFailed {
		// No target got a copy of the backup, so there's no size worth
		// reporting (unlike stateIncomplete, where at least one did).
		run.Size = 0
	}

	if err := r.stateDB.SaveLastRun(ctx, name, run); err != nil {
		r.log.Warn("recording last run to state db", "job", name, "err", err)
	}
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

	// Refreshes this target's state (in both the live status store and the
	// state db) the moment its own outcome is known, independently of every
	// other target and without waiting for the whole job to finish — see
	// runPipeline's onTargetDone doc.
	onTargetDone := func(index int, terr error) {
		r.store.TargetDone(job.Name, index, terr)
		r.persistTargetRun(ctx, job.Name, index, terr)

		if terr != nil && index >= 0 && index < len(run.Targets) {
			r.recordTargetError(ctx, job.Name, index, &run.Targets[index], terr)
		}
	}

	bytesWritten, err := runPipeline(ctx, &run, log, onTargetDone)
	duration := time.Since(start)

	state := r.store.Finished(job.Name, err, bytesWritten)
	r.recordLastRun(ctx, job.Name, start, state, err, bytesWritten)

	if err != nil {
		r.failed.Store(true)

		if state == backup.StateIncomplete {
			// Some targets got the backup and some didn't — worth flagging,
			// but distinct from (and less severe than) every target failing.
			log.Warn("job incomplete: some targets failed", "duration", duration, "err", config.JobError(job, err))
		} else {
			log.Error("job failed", "duration", duration, "err", config.JobError(job, err))
		}

		return
	}

	if !job.StartTime.IsZero() {
		r.recordJobSuccess(ctx, job.Name)
	}

	log.Info("job finished", "duration", duration, "bytes", bytesWritten)

	for i := range run.Targets {
		t := &run.Targets[i]

		switch t.Kind {
		case config.ServerKindLocal:
			log.Info("wrote target", "target", targetLabel(t), "path", backup.LocalObjectPath(&run, t))
		case config.ServerKindRemote:
			log.Info("uploaded target", "target", targetLabel(t), "url", RemoteObjectURL(t, run.Key))
		}
	}
}

// persistTargetRun records target index's just-finished success/failure to
// the state db, mirroring recordLastRun's per-job persistence one level
// down. Best-effort: a db hiccup here shouldn't fail the run, matching
// recordLastRun's own reasoning.
func (r *Runner) persistTargetRun(ctx context.Context, jobName string, index int, terr error) {
	if r.stateDB == nil {
		return
	}

	var (
		state   backup.RunState
		errText string
	)

	backup.SetOutcome(&state, &errText, terr)

	if err := r.stateDB.SaveTargetRun(ctx, jobName, index, string(state), errText, time.Now()); err != nil {
		r.log.Warn("recording target run to state db", "job", jobName, "target", index, "err", err)
	}
}

// recordTargetError appends a target_errors row for job jobName's target at
// index (t), best-effort: a db hiccup here shouldn't fail the run, matching
// persistTargetRun's own reasoning. Unlike persistTargetRun's target_runs
// write, which overwrites the target's previous outcome, this appends one
// row per failure so a history of every occurrence is kept.
func (r *Runner) recordTargetError(ctx context.Context, jobName string, index int, t *config.Target, targetErr error) {
	if r.stateDB == nil {
		return
	}

	if err := r.stateDB.SaveTargetError(ctx, jobName, index, t.ServerName, t.Bucket, time.Now(), targetErr); err != nil {
		r.log.Warn("recording target error to state db", "job", jobName, "target", index, "err", err)
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
