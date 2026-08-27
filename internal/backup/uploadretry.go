package backup

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"time"
)

// outstandingUploadCheckInterval is how often monitorOutstandingUploads
// re-checks the outstanding_uploads table for uploads to retry.
const outstandingUploadCheckInterval = time.Minute

// outstandingUploadMonitor retries every outstanding upload (see
// queueOutstandingUpload) roughly once a minute, one at a time, until it
// either succeeds or hits its job's configured cfg.retries total attempts.
// A single monitor serves every job in this run, since the state db (and so
// the outstanding_uploads table) is shared across all of them.
type outstandingUploadMonitor struct {
	db         *sql.DB
	jobsByName map[string]*config
	store      *statusStore
	identity   *serverIdentity
	log        *slog.Logger
}

// run checks once immediately (so an outstanding upload left behind by a
// prior crash doesn't wait up to a minute to be picked up), then every
// outstandingUploadCheckInterval, until ctx is done.
func (m *outstandingUploadMonitor) run(ctx context.Context) {
	if m.db == nil {
		return
	}

	m.processAll(ctx)

	ticker := time.NewTicker(outstandingUploadCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.processAll(ctx)
		}
	}
}

// processAll retries every currently queued outstanding upload, oldest
// first, sequentially — one upload attempt in flight at a time, system-wide,
// regardless of how many jobs or targets are involved — rather than the
// fan-out-per-target concurrency uploadStagedToTargets uses for a job's
// initial run.
func (m *outstandingUploadMonitor) processAll(ctx context.Context) {
	rows, err := listOutstandingUploads(ctx, m.db)
	if err != nil {
		m.log.Warn("listing outstanding uploads", "err", err)
		return
	}

	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}

		m.processOne(ctx, row)
	}
}

// processOne retries a single outstanding upload row: skips it if its job
// isn't part of this run, gives up on it if its staged file or target index
// no longer exist, otherwise attempts the upload once and either resolves it
// (success, or hitting the job's max attempts) or leaves it queued for the
// next tick.
func (m *outstandingUploadMonitor) processOne(ctx context.Context, row outstandingUpload) {
	job, ok := m.jobsByName[row.JobName]
	if !ok {
		// This process was started with -job scoped to a different set of
		// jobs; the row stays queued for a future invocation that includes
		// row.JobName.
		m.log.Debug("outstanding upload references a job not in this run; skipping", "job", row.JobName)
		return
	}

	if row.TargetIdx < 0 || row.TargetIdx >= len(job.targets) {
		m.log.Error("outstanding upload references an out-of-range target index; dropping stale row", "job", row.JobName, "target_idx", row.TargetIdx)
		_ = deleteOutstandingUpload(ctx, m.db, row.ID)

		return
	}

	t := &job.targets[row.TargetIdx]

	if _, err := os.Stat(row.StagingPath); err != nil {
		// Unrecoverable regardless of how many attempts remain: there is
		// nothing left to retry with (e.g. an OS-temp stagingDir was cleared
		// across a reboot). Distinct from hitting the max-attempts limit
		// below, which abandons a still-retryable failure.
		m.log.Error("staged file for outstanding upload no longer exists; giving up", "job", row.JobName, "target", targetLabel(t), "path", row.StagingPath, "err", err)
		_ = deleteOutstandingUpload(ctx, m.db, row.ID)
		cleanupStagedFileIfIdle(ctx, m.db, row.StagingPath, m.log)

		return
	}

	// job (from m.jobsByName) is the run's original, unresolved *config: it
	// never gets stateDB/identity/a resolved key assigned the way
	// runner.runOnce's per-run copy does, so those are set explicitly here.
	retryCfg := *job
	retryCfg.key = row.Key
	retryCfg.stateDB = m.db
	retryCfg.identity = m.identity

	attemptErr := uploadTargetAttempt(ctx, &retryCfg, t, row.StagingPath, m.log)
	if attemptErr != nil {
		newAttempts := row.Attempts + 1
		maxAttempts := max(job.retries, 1)

		if newAttempts >= maxAttempts {
			m.log.Error("giving up on outstanding upload after max attempts", "job", row.JobName, "target", targetLabel(t), "attempts", newAttempts, "err", attemptErr)
			_ = deleteOutstandingUpload(ctx, m.db, row.ID)
			cleanupStagedFileIfIdle(ctx, m.db, row.StagingPath, m.log)

			return
		}

		m.log.Warn("retrying outstanding upload failed again", "job", row.JobName, "target", targetLabel(t), "attempts", newAttempts, "max_attempts", maxAttempts, "err", attemptErr)

		if err := recordOutstandingUploadAttempt(ctx, m.db, row.ID, time.Now(), attemptErr); err != nil {
			m.log.Error("recording outstanding upload attempt", "err", err)
		}

		return
	}

	m.log.Info("outstanding upload succeeded on retry", "job", row.JobName, "target", targetLabel(t), "attempts", row.Attempts+1)

	if err := deleteOutstandingUpload(ctx, m.db, row.ID); err != nil {
		m.log.Error("deleting resolved outstanding upload row", "err", err)
	}

	m.store.targetDone(row.JobName, row.TargetIdx, nil)

	if err := writeTargetRun(ctx, m.db, row.JobName, row.TargetIdx, stateOK, "", time.Now()); err != nil {
		m.log.Warn("recording target run to state db after retry success", "job", row.JobName, "err", err)
	}

	cleanupStagedFileIfIdle(ctx, m.db, row.StagingPath, m.log)
}
