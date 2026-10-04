package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// countMarkerLines returns how many lines are in the file at path, or 0 if
// it doesn't exist yet (the on-error command hasn't fired).
func countMarkerLines(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is this test's own t.TempDir() file, not untrusted input
	if os.IsNotExist(err) {
		return 0
	}

	if err != nil {
		t.Fatalf("reading marker file: %v", err)
	}

	if len(data) == 0 {
		return 0
	}

	return strings.Count(string(data), "\n")
}

// TestHandleTargetOutcomeFiresOnceStreakReachesThresholdThenEveryTime covers
// the on-error feature's core semantics: a target's on-error.command fires
// once its consecutive-failure streak reaches on-error.after, again on every
// subsequent consecutive failure, and a success resets the streak so a later
// failure has to build the streak back up before firing again.
func TestHandleTargetOutcomeFiresOnceStreakReachesThresholdThenEveryTime(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "marker")

	cmd := config.Command{ID: "alert", Cmd: `echo fired >> "` + marker + `"`, Timeout: 5 * time.Second}
	target := &config.Target{ServerName: "s", Bucket: "b", OnErrorCommand: &cmd, OnErrorAfter: 3}

	r := &Runner{log: discardLogger, targetFailures: map[targetKey]int{}}

	ctx := context.Background()
	terr := errors.New("boom")

	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)
	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)

	if got := countMarkerLines(t, marker); got != 0 {
		t.Fatalf("after 2 failures (threshold 3): fired %d times, want 0", got)
	}

	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)

	if got := countMarkerLines(t, marker); got != 1 {
		t.Fatalf("after 3 failures (threshold 3): fired %d times, want 1", got)
	}

	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)

	if got := countMarkerLines(t, marker); got != 2 {
		t.Fatalf("after 4 failures: fired %d times, want 2 (fires every consecutive failure once armed)", got)
	}

	r.handleTargetOutcome(ctx, "job", target, nil, discardLogger) // success resets the streak

	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger) // streak restarts at 1

	if got := countMarkerLines(t, marker); got != 2 {
		t.Fatalf("after reset + 1 failure: fired %d times, want 2 (streak restarted, below threshold)", got)
	}
}

// TestHandleTargetOutcomeNilCommandIsNoOp covers the common case (no
// on-error: configured for a target) never touching targetFailures at all —
// including when it's nil, as it is for every existing Runner{} literal in
// this package's other tests that don't exercise the on-error feature.
func TestHandleTargetOutcomeNilCommandIsNoOp(t *testing.T) {
	t.Parallel()

	r := &Runner{log: discardLogger}
	target := &config.Target{ServerName: "s", Bucket: "b"}

	r.handleTargetOutcome(context.Background(), "job", target, errors.New("boom"), discardLogger)
}

// TestHandleTargetOutcomeConcurrentDistinctTargetsSafe drives
// handleTargetOutcome for many distinct targets concurrently, mirroring how
// uploadStagedToTargets calls onTargetDone from each target's own goroutine,
// to catch a data race on Runner.targetFailures under go test -race.
func TestHandleTargetOutcomeConcurrentDistinctTargetsSafe(t *testing.T) {
	t.Parallel()

	r := &Runner{log: discardLogger, targetFailures: map[targetKey]int{}}
	cmd := config.Command{ID: "noop", Cmd: "true", Timeout: 2 * time.Second}

	var wg sync.WaitGroup

	for i := range 20 {
		wg.Go(func() {
			target := &config.Target{ServerName: fmt.Sprintf("server-%d", i), Bucket: "b", OnErrorCommand: &cmd, OnErrorAfter: 1}
			r.handleTargetOutcome(context.Background(), "job", target, errors.New("boom"), discardLogger)
		})
	}

	wg.Wait()
}

// TestHandleTargetOutcomeOnRecoverFiresOnceAfterFailures covers on-recover's
// semantics: it fires once, on the first success after one or more
// consecutive failures (with GBT_CONSECUTIVE_FAILURES set to the length of
// the streak that just ended), never on a success with no prior failure,
// and never again on a later consecutive success. on-recover is configured
// alone here, so it must not depend on on-error being set.
func TestHandleTargetOutcomeOnRecoverFiresOnceAfterFailures(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "marker")

	cmd := config.Command{ID: "all-clear", Cmd: `echo "$GBT_EVENT $GBT_CONSECUTIVE_FAILURES ${GBT_ERROR-unset}" >> "` + marker + `"`, Timeout: 5 * time.Second}
	target := &config.Target{ServerName: "s", Bucket: "b", OnRecoverCommand: &cmd}

	r := &Runner{log: discardLogger, targetFailures: map[targetKey]int{}}

	ctx := context.Background()
	terr := errors.New("boom")

	r.handleTargetOutcome(ctx, "job", target, nil, discardLogger)

	if got := countMarkerLines(t, marker); got != 0 {
		t.Fatalf("after success with no prior failure: fired %d times, want 0", got)
	}

	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)
	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)

	if got := countMarkerLines(t, marker); got != 0 {
		t.Fatalf("after 2 failures: fired %d times, want 0", got)
	}

	r.handleTargetOutcome(ctx, "job", target, nil, discardLogger)
	r.handleTargetOutcome(ctx, "job", target, nil, discardLogger)

	if got := countMarkerLines(t, marker); got != 1 {
		t.Fatalf("after recovery + another success: fired %d times, want 1", got)
	}

	data, err := os.ReadFile(marker) //nolint:gosec // path is this test's own t.TempDir() file, not untrusted input
	if err != nil {
		t.Fatalf("reading marker file: %v", err)
	}

	if got, want := strings.TrimSpace(string(data)), "recover 2 unset"; got != want {
		t.Errorf("on-recover env = %q, want %q", got, want)
	}
}

// TestHandleTargetOutcomeOnRecoverAndOnErrorShareStreak covers a target with
// both commands configured: on-recover fires after a streak even if it
// never reached on-error.after, and on-error still fires once armed.
func TestHandleTargetOutcomeOnRecoverAndOnErrorShareStreak(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	errMarker := filepath.Join(dir, "error")
	recoverMarker := filepath.Join(dir, "recover")

	errCmd := config.Command{ID: "alert", Cmd: `echo fired >> "` + errMarker + `"`, Timeout: 5 * time.Second}
	recoverCmd := config.Command{ID: "all-clear", Cmd: `echo fired >> "` + recoverMarker + `"`, Timeout: 5 * time.Second}
	target := &config.Target{ServerName: "s", Bucket: "b", OnErrorCommand: &errCmd, OnErrorAfter: 2, OnRecoverCommand: &recoverCmd}

	r := &Runner{log: discardLogger, targetFailures: map[targetKey]int{}}

	ctx := context.Background()
	terr := errors.New("boom")

	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)
	r.handleTargetOutcome(ctx, "job", target, nil, discardLogger)

	if got := countMarkerLines(t, errMarker); got != 0 {
		t.Fatalf("on-error fired %d times after 1 failure (threshold 2), want 0", got)
	}

	if got := countMarkerLines(t, recoverMarker); got != 1 {
		t.Fatalf("on-recover fired %d times after 1 failure + success, want 1", got)
	}

	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)
	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)
	r.handleTargetOutcome(ctx, "job", target, nil, discardLogger)

	if got := countMarkerLines(t, errMarker); got != 1 {
		t.Fatalf("on-error fired %d times after 2 failures, want 1", got)
	}

	if got := countMarkerLines(t, recoverMarker); got != 2 {
		t.Fatalf("on-recover fired %d times after second recovery, want 2", got)
	}
}

// TestHandleTargetOutcomeOnErrorOnceFiresOnlyAtThreshold covers
// on-error.repeat: false: the command fires exactly once per streak, when it
// reaches on-error.after, and a success re-arms it for the next streak.
func TestHandleTargetOutcomeOnErrorOnceFiresOnlyAtThreshold(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "marker")

	cmd := config.Command{ID: "alert", Cmd: `echo fired >> "` + marker + `"`, Timeout: 5 * time.Second}
	target := &config.Target{ServerName: "s", Bucket: "b", OnErrorCommand: &cmd, OnErrorAfter: 2, OnErrorOnce: true}

	r := &Runner{log: discardLogger, targetFailures: map[targetKey]int{}}

	ctx := context.Background()
	terr := errors.New("boom")

	for range 4 {
		r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)
	}

	if got := countMarkerLines(t, marker); got != 1 {
		t.Fatalf("after 4 failures (threshold 2, once): fired %d times, want 1", got)
	}

	r.handleTargetOutcome(ctx, "job", target, nil, discardLogger)
	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)
	r.handleTargetOutcome(ctx, "job", target, terr, discardLogger)

	if got := countMarkerLines(t, marker); got != 2 {
		t.Fatalf("after reset + 2 failures: fired %d times, want 2 (re-armed by success)", got)
	}
}
