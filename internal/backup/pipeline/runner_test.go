package pipeline

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// plainTestJob builds a job (no start-time, no interval — a one-shot run)
// that, when run, appends "run\n" to a marker file. Used by the
// runOnce/state-persistence tests below.
func plainTestJob(t *testing.T, marker string) *config.Config {
	t.Helper()

	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	t.Setenv("MARKER_FILE", marker)

	return &config.Config{
		Name:       "test",
		Cmd:        `echo run >> "$MARKER_FILE"`,
		Key:        "backup-{time}.gpg",
		Symmetric:  true,
		Passphrase: "unit-test-passphrase",
		GPGBin:     "gpg",
		Targets: []config.Target{
			{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: t.TempDir()},
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

	stateDB, err := store.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := backup.NewStatusStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	r.runOnce(context.Background(), job)

	run, ok, err := stateDB.GetLastRun(context.Background(), job.Name)
	if err != nil {
		t.Fatalf("ReadLastRun() error: %v", err)
	}

	if !ok {
		t.Fatal("ReadLastRun() ok = false after runOnce, want true")
	}

	if run.State != string(backup.StateOK) {
		t.Errorf("run.State = %q, want ok", run.State)
	}

	if run.End.Before(run.Start) {
		t.Errorf("run.End %v is before run.Start %v", run.End, run.Start)
	}
}

// TestRunOnceRefreshesEachTargetIndependently verifies the live status
// store reflects a fast target's outcome as soon as that target finishes,
// without waiting for a slower target (here, a remote target whose response
// the test holds back deliberately) still in progress — i.e. the web UI's
// /api/status can show one target as done while another is still running,
// rather than every target flipping from running to its final state
// together only once the whole job completes. See runPipeline's
// onTargetDone and runOnce's wiring of it to store.TargetDone.
func TestRunOnceRefreshesEachTargetIndependently(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	t.Parallel()

	dir := t.TempDir()

	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // held open until the test says the slow target may finish

		_, _ = io.Copy(io.Discard, r.Body)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	job := &config.Config{
		Name:       "test",
		Cmd:        "echo hi",
		Key:        "backup-{time}.gpg",
		Symmetric:  true,
		Passphrase: "unit-test-passphrase",
		GPGBin:     "gpg",
		Targets: []config.Target{
			{ServerName: "slow-remote", Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"},
			{ServerName: "fast-local", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	statusStore := backup.NewStatusStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, identity: testServerIdentity(t)}

	done := make(chan struct{})

	go func() {
		defer close(done)

		r.runOnce(context.Background(), job)
	}()

	deadline := time.Now().Add(5 * time.Second)

	for {
		if time.Now().After(deadline) {
			close(release)
			<-done

			t.Fatal("timed out waiting for the fast local target to finish independently of the slow remote target")
		}

		snap := statusStore.Snapshot()
		remoteState := snap[0].Targets[0].State
		localState := snap[0].Targets[1].State

		if localState == backup.StateOK {
			if remoteState != backup.StateRunning {
				close(release)
				<-done

				t.Fatalf("local target finished, but remote target state = %q, want %q (it should still be in flight, held by the test)", remoteState, backup.StateRunning)
			}

			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	close(release)
	<-done

	snap := statusStore.Snapshot()
	if snap[0].Targets[0].State != backup.StateOK {
		t.Errorf("remote target final state = %q, want %q", snap[0].Targets[0].State, backup.StateOK)
	}

	if snap[0].Targets[1].State != backup.StateOK {
		t.Errorf("local target final state = %q, want %q", snap[0].Targets[1].State, backup.StateOK)
	}
}

// TestRunOnceReportsIncompleteWhenSomeTargetsFail is an end-to-end check
// (real gpg, real runOnce) that a run where one target succeeds and one
// fails is reported as incomplete rather than failed, in both the live
// status store and the persisted state db — and that it still counts
// toward the process's overall exit code, since a partial backup still
// warrants attention.
func TestRunOnceReportsIncompleteWhenSomeTargetsFail(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	t.Parallel()

	dir := t.TempDir()

	// Make the "bad" target's parent directory path a regular file, so
	// WriteLocalObject's os.MkdirAll for it fails deterministically.
	badParent := filepath.Join(dir, "blocked")
	if err := os.WriteFile(badParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("setting up blocked path: %v", err)
	}

	job := &config.Config{
		Name:       "test",
		Cmd:        "echo hi",
		Key:        "backup-{time}.gpg",
		Symmetric:  true,
		Passphrase: "unit-test-passphrase",
		GPGBin:     "gpg",
		Targets: []config.Target{
			{ServerName: "bad", Kind: config.ServerKindLocal, Bucket: "blocked/sub", LocalPath: dir},
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	stateDB, err := store.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := backup.NewStatusStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	r.runOnce(context.Background(), job)

	snap := statusStore.Snapshot()[0]
	if snap.State != backup.StateIncomplete {
		t.Errorf("job State = %q, want incomplete", snap.State)
	}

	if snap.Size == "" {
		t.Error("job Size is empty, want the backup's size (the good target got a complete copy)")
	}

	if snap.Targets[0].State != backup.StateFailed {
		t.Errorf("bad target state = %q, want failed", snap.Targets[0].State)
	}

	if snap.Targets[1].State != backup.StateOK {
		t.Errorf("good target state = %q, want ok", snap.Targets[1].State)
	}

	if !r.failed.Load() {
		t.Error("r.failed = false, want true (an incomplete run still fails the process's exit code)")
	}

	run, ok, err := stateDB.GetLastRun(context.Background(), job.Name)
	if err != nil {
		t.Fatalf("ReadLastRun() error: %v", err)
	}

	if !ok {
		t.Fatal("ReadLastRun() ok = false, want true")
	}

	if run.State != string(backup.StateIncomplete) {
		t.Errorf("persisted run.State = %q, want incomplete", run.State)
	}

	if run.Size == 0 {
		t.Error("persisted run.Size = 0, want the backup's actual size")
	}
}

// TestRunOnceDeletesStagedFileEvenWhenTargetFails is an end-to-end check
// (real gpg, real runOnce) that a job's staged file is removed once runOnce
// returns even when a target fails: a failed target is never retried, so
// nothing keeps the staged file around afterward.
func TestRunOnceDeletesStagedFileEvenWhenTargetFails(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	t.Parallel()

	dir := t.TempDir()
	stagingDir := t.TempDir()

	// Make the target's parent directory path a regular file, so
	// WriteLocalObject's os.MkdirAll for it fails deterministically.
	badParent := filepath.Join(dir, "blocked")
	if err := os.WriteFile(badParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("setting up blocked path: %v", err)
	}

	job := &config.Config{
		Name:       "test",
		Cmd:        "echo hi",
		Key:        "backup-{time}.gpg",
		Symmetric:  true,
		Passphrase: "unit-test-passphrase",
		GPGBin:     "gpg",
		StagingDir: stagingDir,
		Targets: []config.Target{
			{ServerName: "bad", Kind: config.ServerKindLocal, Bucket: "blocked/sub", LocalPath: dir},
		},
	}

	stateDB, err := store.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := backup.NewStatusStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	r.runOnce(context.Background(), job)

	staged, err := filepath.Glob(filepath.Join(stagingDir, "go-backup-tool-*.staged"))
	if err != nil {
		t.Fatalf("globbing staging dir: %v", err)
	}

	if len(staged) != 0 {
		t.Errorf("staged files in %q = %v, want none (a failed target is never retried)", stagingDir, staged)
	}
}

// TestRunOnceDeletesStagedFileOnSuccess is the success-path counterpart to
// TestRunOnceDeletesStagedFileEvenWhenTargetFails: the staged file is
// removed once every target has succeeded.
func TestRunOnceDeletesStagedFileOnSuccess(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	t.Parallel()

	dir := t.TempDir()
	stagingDir := t.TempDir()

	job := &config.Config{
		Name:       "test",
		Cmd:        "echo hi",
		Key:        "backup-{time}.gpg",
		Symmetric:  true,
		Passphrase: "unit-test-passphrase",
		GPGBin:     "gpg",
		StagingDir: stagingDir,
		Targets: []config.Target{
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	stateDB, err := store.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := backup.NewStatusStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	r.runOnce(context.Background(), job)

	staged, err := filepath.Glob(filepath.Join(stagingDir, "go-backup-tool-*.staged"))
	if err != nil {
		t.Fatalf("globbing staging dir: %v", err)
	}

	if len(staged) != 0 {
		t.Errorf("staged files in %q = %v, want none", stagingDir, staged)
	}
}

// TestSeedStatusFromStateAcrossRestart simulates a restart: a first runner
// (with its own status store) runs a job and persists its outcome; a
// second, independent status store — standing in for the fresh one a
// restarted process builds — is seeded from that same state db and must
// already show the prior run instead of starting idle.
func TestSeedStatusFromStateAcrossRestart(t *testing.T) { //nolint:paralleltest // plainTestJob calls t.Setenv, which is incompatible with t.Parallel
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	job := plainTestJob(t, marker)

	stateDB, err := store.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	firstStore := backup.NewStatusStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: firstStore, stateDB: stateDB}
	r.runOnce(context.Background(), job)

	restartedStore := backup.NewStatusStore([]*config.Config{job})

	if got := restartedStore.Snapshot()[0].State; got != backup.StateIdle {
		t.Fatalf("restartedStore before seeding: State = %q, want idle", got)
	}

	SeedStatusFromState(context.Background(), stateDB, []*config.Config{job}, restartedStore, discardLogger)

	snap := restartedStore.Snapshot()[0]
	if snap.State != backup.StateOK {
		t.Errorf("restartedStore after seeding: State = %q, want ok", snap.State)
	}

	if snap.LastStart.IsZero() || snap.LastEnd.IsZero() {
		t.Error("restartedStore after seeding: LastStart/LastEnd still zero, want the prior run's times")
	}
}

func TestSubstituteKeyTime(t *testing.T) {
	t.Parallel()

	got := substituteKeyTime("prefix-{time}-suffix.gpg")

	if strings.Contains(got, "{time}") {
		t.Errorf("substituteKeyTime() = %q, want {time} placeholder substituted", got)
	}

	if !strings.HasPrefix(got, "prefix-") || !strings.HasSuffix(got, "-suffix.gpg") {
		t.Errorf("substituteKeyTime() = %q, want prefix-<timestamp>-suffix.gpg", got)
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
// file — used by the Schedule() integration tests below to count executions
// without depending on exact timing of gpg/sh subprocess output.
func startTimeTestJob(t *testing.T, marker string, startTime time.Time, interval time.Duration) *config.Config {
	t.Helper()

	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	t.Setenv("MARKER_FILE", marker)

	return &config.Config{
		Name:       "test",
		Cmd:        `echo run >> "$MARKER_FILE"`,
		Key:        "backup-{time}.gpg",
		Symmetric:  true,
		Passphrase: "unit-test-passphrase",
		GPGBin:     "gpg",
		Interval:   interval,
		StartTime:  startTime,
		Targets: []config.Target{
			{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: t.TempDir()},
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
// at all here, matching an unopened/unavailable state db), so Schedule must
// fire a single catch-up run right away rather than waiting for the next
// grid slot.
func TestScheduleStartTimeCatchesUpMissedRun(t *testing.T) { //nolint:paralleltest // startTimeTestJob calls t.Setenv, which is incompatible with t.Parallel
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	job := startTimeTestJob(t, marker, time.Now().Add(-10*time.Minute), time.Second)

	statusStore := backup.NewStatusStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	r.Schedule(ctx, job)

	if got := countMarkerRuns(t, marker); got != 1 {
		t.Errorf("marker recorded %d runs, want exactly 1 (one immediate catch-up run, no extra runs before the next 1s grid slot)", got)
	}
}

// TestScheduleStartTimeSkipsCatchUpWhenAlreadyRecorded covers the "not
// actually missed" case: start-time's most recent due slot already has a
// recorded success (as if this is an ordinary restart shortly after an
// on-time run), so Schedule must not fire an extra run and should instead
// wait for the next future grid slot.
func TestScheduleStartTimeSkipsCatchUpWhenAlreadyRecorded(t *testing.T) { //nolint:paralleltest // startTimeTestJob calls t.Setenv, which is incompatible with t.Parallel
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	startTime := time.Now().Add(-10 * time.Minute)
	interval := time.Second

	job := startTimeTestJob(t, marker, startTime, interval)

	stateDB, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	due, ok := lastDueSlot(startTime, interval, time.Now())
	if !ok {
		t.Fatal("lastDueSlot() ok = false, want true (start-time is in the past)")
	}

	if err := stateDB.SaveLastSuccess(context.Background(), job.Name, due); err != nil {
		t.Fatalf("SaveLastSuccess() error: %v", err)
	}

	statusStore := backup.NewStatusStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	r.Schedule(ctx, job)

	if got := countMarkerRuns(t, marker); got != 0 {
		t.Errorf("marker recorded %d runs, want 0 (already-covered slot must not trigger a catch-up run)", got)
	}
}
