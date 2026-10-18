package pipeline

import (
	"context"
	"log/slog"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// watchRetryInterval is how long watchOnChange waits before trying again
// to watch a source it couldn't (e.g. one not mounted yet).
const watchRetryInterval = time.Minute

// onChangeMessages are the log lines finishRun writes for an on-change
// run.
var onChangeMessages = finishMessages{
	incomplete: "on-change run incomplete: some targets or files failed",
	failed:     "on-change run failed",
	finished:   "on-change run finished",
}

// runOnChangeOnce runs on-change job once (see runOnChange) — every file
// when changed is nil, otherwise just those — on every target, or on just
// job.Targets[indices] when indices is non-nil (RetryFailedTargets), and
// records the outcome the way runOnce does for a backup job. A run
// triggered by changes that uploaded nothing and failed nowhere isn't
// added to the state db's run log, and a triggered run's failure fires no
// failure notifications (see runResult). The caller holds job's claim.
func (r *Runner) runOnChangeOnce(ctx context.Context, job *config.Config, changed []string, indices []int) error {
	run := *job
	triggered := changed != nil

	if indices != nil {
		run.Targets = make([]config.Target, len(indices))
		names := make([]string, len(indices))

		for i, idx := range indices {
			run.Targets[i] = job.Targets[idx]
			names[i] = job.Targets[idx].ServerName
		}

		r.store.RetryStarting(job.Name, names)
	} else {
		r.store.Starting(job.Name)
	}

	log := r.log.With("job", job.Name)
	start := time.Now()

	if triggered {
		log.Debug("on-change run starting", "changed", len(changed))
	} else {
		log.Info("on-change scan starting", "path", job.Source.Path, "targets", len(run.Targets))
	}

	res := runOnChange(ctx, r.env(), &run, changed, log)
	err := res.err()
	unrecorded := triggered && res.uploaded == 0 && err == nil

	var outcomes targetOutcomes

	for i, terr := range res.targetErrs {
		orig := i
		if indices != nil {
			orig = indices[i]
		}

		r.store.TargetDone(job.Name, orig, terr)

		if !unrecorded {
			r.persistTargetRun(ctx, job.Name, terr == nil, job.Targets[orig].ServerName, terr)
		}

		outcomes.add(&job.Targets[orig], terr)
	}

	result := runResult{bytes: res.bytes, files: res.files, err: err, unrecorded: unrecorded, silent: triggered}

	return r.finishRun(ctx, job, log, onChangeMessages, start, result, &outcomes)
}

// watchOnChange watches on-change job name's source src (see watchSource)
// until stop is done or the job no longer exists, running the job on each
// batch of changes under runCtx. A batch that arrives while the job is
// already running waits for the next try. When src can't be watched, it
// tries again every watchRetryInterval; interval: scans keep covering the
// source meanwhile.
func (r *Runner) watchOnChange(runCtx, stop context.Context, name string, src config.OnChangeSource, lookup JobLookup, log *slog.Logger) {
	flush := func(changed []string) (ran, done bool) {
		cur, ok := lookup(name)
		if !ok || stop.Err() != nil {
			return false, true
		}

		if !r.claim(name) {
			return false, false
		}

		defer r.release(name)

		_ = r.runOnChangeOnce(runCtx, cur, changed, nil)

		return true, false
	}

	for {
		err := watchSource(stop, src, log, flush)
		if err == nil {
			return
		}

		log.Warn("on-change: can't watch source, retrying", "path", src.Path, "retry_in", watchRetryInterval, "error", err)

		if !waitUntil(stop, time.Now().Add(watchRetryInterval)) {
			return
		}
	}
}
