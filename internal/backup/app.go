package backup

import (
	"context"
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
func Run(args []string, stderr io.Writer) int {
	rc, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}

		_, _ = fmt.Fprintln(stderr, "error:", err)

		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if rc.timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, rc.timeout)
		defer cancel()
	}

	r := &runner{stderr: stderr}

	var wg sync.WaitGroup

	for _, job := range rc.jobs {
		warnIfKeyWontChange(stderr, job)

		wg.Go(func() {
			r.schedule(ctx, job)
		})
	}

	wg.Wait()

	if r.failed.Load() {
		return 1
	}

	return 0
}

// runner tracks whether any job run has failed across the concurrently
// scheduled jobs.
type runner struct {
	stderr io.Writer
	failed atomic.Bool
}

// schedule runs job once immediately, then, if job.interval > 0, keeps
// re-running it every interval until ctx is done.
func (r *runner) schedule(ctx context.Context, job *config) {
	r.runOnce(ctx, job)

	if job.interval <= 0 {
		return
	}

	ticker := time.NewTicker(job.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx, job)
		}
	}
}

// runOnce runs job a single time, resolving a fresh {time} timestamp in its
// key first so a repeating job doesn't overwrite the same object on every
// run, and reports the outcome to r.stderr.
func (r *runner) runOnce(ctx context.Context, job *config) {
	run := *job
	run.key = substituteKeyTime(job.key)

	if err := runPipeline(ctx, &run); err != nil {
		r.failed.Store(true)

		_, _ = fmt.Fprintln(r.stderr, "error:", jobError(job, err))

		return
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
