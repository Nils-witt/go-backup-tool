package webui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

// handleStatus serves store's current job/target statuses as JSON.
func handleStatus(store *status.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, store.Snapshot())
	}
}

// handleRetryFailedTargets serves POST /api/jobs/{name}/retry (see the
// dashboard's "Retry failed targets" button, dashboard.js): it kicks off a
// re-run of the named job's pipeline for whichever of its targets
// /api/status currently reports as failed (see
// pipeline.Runner.RetryFailedTargets's doc comment for why that means
// re-running the whole pipeline, not just re-uploading). Gated on admin
// rather than plain view/download, since it
// re-executes the job's configured backup command — a broader capability
// than anything else those two permissions grant.
//
// The retry runs in the background: this handler only starts it and
// returns immediately, since a backup can take far longer than an HTTP
// client wants to wait for a response. The dashboard's live status
// WebSocket (see handleLive), or its /api/status polling fallback, picks up
// the retry's progress instead.
// context.WithoutCancel detaches the retry from this request's own
// context, so it isn't cut short the moment the response is written.
func handleRetryFailedTargets(lookupJob pipeline.JobLookup, statusStore *status.Store, runner *pipeline.Runner, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")

		job, ok := lookupJob(name)
		if !ok {
			http.Error(w, "unknown job", http.StatusNotFound)
			return
		}

		targets := statusStore.FailedTargets(name)
		if len(targets) == 0 {
			http.Error(w, "no failed targets to retry", http.StatusConflict)
			return
		}

		retryCtx := context.WithoutCancel(r.Context())

		go func() {
			if err := runner.RetryFailedTargets(retryCtx, job, targets); err != nil {
				log.Warn("web UI: retrying failed targets", "job", name, "targets", targets, "err", err)
			}
		}()

		w.WriteHeader(http.StatusAccepted)
	}
}

// handleRunJob serves POST /api/jobs/{name}/run (see the dashboard's "Run
// now" button): it starts a run of the named job right away, outside its
// schedule. Gated on admin for the same reason as handleRetryFailedTargets,
// and, like it, only starts the run in the background (see
// pipeline.Runner.RunNow) and returns 202; the live status picks up its
// progress. A job that's already running gets a 409 instead.
func handleRunJob(lookupJob pipeline.JobLookup, runner *pipeline.Runner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := lookupJob(r.PathValue("name"))
		if !ok {
			http.Error(w, "unknown job", http.StatusNotFound)
			return
		}

		err := runner.RunNow(context.WithoutCancel(r.Context()), job)
		if errors.Is(err, pipeline.ErrJobRunning) {
			http.Error(w, "job is already running", http.StatusConflict)
			return
		}

		if err != nil {
			http.Error(w, "starting run failed", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusAccepted)
	}
}
