package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plainTestJob builds a job (no start-time, no interval — a one-shot run)
// that, when run, appends "run\n" to a marker file. Used by the
// runOnce/state-persistence tests below.
func plainTestJob(t *testing.T, marker string) *config {
	t.Helper()

	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	t.Setenv("MARKER_FILE", marker)

	return &config{
		name:       "test",
		cmd:        `echo run >> "$MARKER_FILE"`,
		key:        "backup-{time}.gpg",
		symmetric:  true,
		passphrase: "unit-test-passphrase",
		gpgBin:     "gpg",
		targets: []target{
			{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: t.TempDir()},
		},
	}
}

// TestRunOnceRecordsLastRunToStateDB covers the fix for "the web UI doesn't
// report a job's last runtime after a restart": runOnce must persist every
// run (success or failure) to the state db, for every job, not just
// start-time-anchored ones.
func TestRunOnceRecordsLastRunToStateDB(t *testing.T) { //nolint:paralleltest // plainTestJob calls t.Setenv, which is incompatible with t.Parallel
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	job := plainTestJob(t, marker)

	stateDB, err := openScheduleStateDB(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("openScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	store := newStatusStore([]*config{job})
	r := &runner{log: discardLogger, store: store, stateDB: stateDB}

	r.runOnce(context.Background(), job)

	run, ok, err := readLastRun(context.Background(), stateDB, job.name)
	if err != nil {
		t.Fatalf("readLastRun() error: %v", err)
	}

	if !ok {
		t.Fatal("readLastRun() ok = false after runOnce, want true")
	}

	if run.State != stateOK {
		t.Errorf("run.State = %q, want ok", run.State)
	}

	if run.End.Before(run.Start) {
		t.Errorf("run.End %v is before run.Start %v", run.End, run.Start)
	}
}

// TestSeedStatusFromStateAcrossRestart simulates a restart: a first runner
// (with its own statusStore) runs a job and persists its outcome; a second,
// independent statusStore — standing in for the fresh one a restarted
// process builds — is seeded from that same state db and must already show
// the prior run instead of starting idle.
func TestSeedStatusFromStateAcrossRestart(t *testing.T) { //nolint:paralleltest // plainTestJob calls t.Setenv, which is incompatible with t.Parallel
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	job := plainTestJob(t, marker)

	stateDB, err := openScheduleStateDB(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("openScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	firstStore := newStatusStore([]*config{job})
	r := &runner{log: discardLogger, store: firstStore, stateDB: stateDB}
	r.runOnce(context.Background(), job)

	restartedStore := newStatusStore([]*config{job})

	if got := restartedStore.snapshot()[0].State; got != stateIdle {
		t.Fatalf("restartedStore before seeding: State = %q, want idle", got)
	}

	seedStatusFromState(context.Background(), stateDB, []*config{job}, restartedStore, discardLogger)

	snap := restartedStore.snapshot()[0]
	if snap.State != stateOK {
		t.Errorf("restartedStore after seeding: State = %q, want ok", snap.State)
	}

	if snap.LastStart.IsZero() || snap.LastEnd.IsZero() {
		t.Error("restartedStore after seeding: LastStart/LastEnd still zero, want the prior run's times")
	}
}

func TestNextGridTime(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	interval := time.Hour

	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"now before start", start.Add(-time.Minute), start.Add(interval)},
		{"now exactly on start", start, start.Add(interval)},
		{"now partway into first slot", start.Add(30 * time.Minute), start.Add(interval)},
		{"now exactly on a later grid point", start.Add(2 * time.Hour), start.Add(3 * time.Hour)},
		{"now partway past several intervals", start.Add(2*time.Hour + 30*time.Minute), start.Add(3 * time.Hour)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := nextGridTime(start, interval, tt.now); !got.Equal(tt.want) {
				t.Errorf("nextGridTime(%v, %v, %v) = %v, want %v", start, interval, tt.now, got, tt.want)
			}
		})
	}
}

func TestLastDueSlot(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	interval := time.Hour

	tests := []struct {
		name    string
		now     time.Time
		wantDue time.Time
		wantOK  bool
	}{
		{"now before start", start.Add(-time.Minute), time.Time{}, false},
		{"now exactly on start", start, start, true},
		{"now partway into first slot", start.Add(30 * time.Minute), start, true},
		{"now exactly on a later grid point", start.Add(2 * time.Hour), start.Add(2 * time.Hour), true},
		{"now partway past several intervals", start.Add(2*time.Hour + 30*time.Minute), start.Add(2 * time.Hour), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			due, ok := lastDueSlot(start, interval, tt.now)
			if ok != tt.wantOK {
				t.Fatalf("lastDueSlot() ok = %v, want %v", ok, tt.wantOK)
			}

			if ok && !due.Equal(tt.wantDue) {
				t.Errorf("lastDueSlot() due = %v, want %v", due, tt.wantDue)
			}
		})
	}
}

func TestWaitUntilPastTimeReturnsImmediately(t *testing.T) {
	t.Parallel()

	start := time.Now()

	if !waitUntil(context.Background(), start.Add(-time.Hour)) {
		t.Fatal("waitUntil() = false for a past time, want true")
	}

	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("waitUntil() for a past time took %v, want near-instant", elapsed)
	}
}

func TestWaitUntilFutureTimeWaits(t *testing.T) {
	t.Parallel()

	const wait = 60 * time.Millisecond

	start := time.Now()

	if !waitUntil(context.Background(), start.Add(wait)) {
		t.Fatal("waitUntil() = false for a future time, want true")
	}

	if elapsed := time.Since(start); elapsed < wait {
		t.Errorf("waitUntil() returned after %v, want at least %v", elapsed, wait)
	}
}

func TestWaitUntilContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	start := time.Now()

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	if waitUntil(ctx, start.Add(time.Hour)) {
		t.Fatal("waitUntil() = true after ctx canceled, want false")
	}

	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("waitUntil() took %v after cancellation, want promptly after cancel", elapsed)
	}
}

// startTimeTestJob builds a job that, when run, appends "run\n" to a marker
// file — used by the schedule() integration tests below to count executions
// without depending on exact timing of gpg/sh subprocess output.
func startTimeTestJob(t *testing.T, marker string, startTime time.Time, interval time.Duration) *config {
	t.Helper()

	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	t.Setenv("MARKER_FILE", marker)

	return &config{
		name:       "test",
		cmd:        `echo run >> "$MARKER_FILE"`,
		key:        "backup-{time}.gpg",
		symmetric:  true,
		passphrase: "unit-test-passphrase",
		gpgBin:     "gpg",
		interval:   interval,
		startTime:  startTime,
		targets: []target{
			{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: t.TempDir()},
		},
	}
}

func countMarkerRuns(t *testing.T, marker string) int {
	t.Helper()

	data, err := os.ReadFile(marker) //nolint:gosec // marker is a test-generated path under t.TempDir()
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}

		t.Fatalf("reading marker file: %v", err)
	}

	return strings.Count(string(data), "run")
}

// TestScheduleStartTimeCatchesUpMissedRun covers the "real missed run"
// case: start-time is long overdue and no success is recorded (no state db
// at all here, matching an unopened/unavailable state db), so schedule must
// fire a single catch-up run right away rather than waiting for the next
// grid slot.
func TestScheduleStartTimeCatchesUpMissedRun(t *testing.T) { //nolint:paralleltest // startTimeTestJob calls t.Setenv, which is incompatible with t.Parallel
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	job := startTimeTestJob(t, marker, time.Now().Add(-10*time.Minute), time.Second)

	store := newStatusStore([]*config{job})
	r := &runner{log: discardLogger, store: store}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	r.schedule(ctx, job)

	if got := countMarkerRuns(t, marker); got != 1 {
		t.Errorf("marker recorded %d runs, want exactly 1 (one immediate catch-up run, no extra runs before the next 1s grid slot)", got)
	}
}

// TestScheduleStartTimeSkipsCatchUpWhenAlreadyRecorded covers the "not
// actually missed" case: start-time's most recent due slot already has a
// recorded success (as if this is an ordinary restart shortly after an
// on-time run), so schedule must not fire an extra run and should instead
// wait for the next future grid slot.
func TestScheduleStartTimeSkipsCatchUpWhenAlreadyRecorded(t *testing.T) { //nolint:paralleltest // startTimeTestJob calls t.Setenv, which is incompatible with t.Parallel
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	startTime := time.Now().Add(-10 * time.Minute)
	interval := time.Second

	job := startTimeTestJob(t, marker, startTime, interval)

	stateDB, err := openScheduleStateDB(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("openScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	due, ok := lastDueSlot(startTime, interval, time.Now())
	if !ok {
		t.Fatal("lastDueSlot() ok = false, want true (start-time is in the past)")
	}

	if err := writeLastSuccess(context.Background(), stateDB, job.name, due); err != nil {
		t.Fatalf("writeLastSuccess() error: %v", err)
	}

	store := newStatusStore([]*config{job})
	r := &runner{log: discardLogger, store: store, stateDB: stateDB}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	r.schedule(ctx, job)

	if got := countMarkerRuns(t, marker); got != 0 {
		t.Errorf("marker recorded %d runs, want 0 (already-covered slot must not trigger a catch-up run)", got)
	}
}
