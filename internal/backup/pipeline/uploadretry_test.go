package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
)

// TestOutstandingUploadMonitorProcessAllSequential verifies that
// processAll retries multiple outstanding uploads one at a time, never two
// in flight at once, regardless of which job or target they belong to.
func TestOutstandingUploadMonitorProcessAllSequential(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		inFlight    int
		maxInFlight int
	)

	enter := func() {
		mu.Lock()
		inFlight++

		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()

		time.Sleep(30 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
	}

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enter()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srvA.Close()

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enter()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srvB.Close()

	db := openTestStateDB(t)
	ctx := context.Background()
	identity := testServerIdentity(t)

	jobA := &backup.Config{Name: "job-a", Retries: 3, Targets: []backup.Target{{ServerName: "a", Kind: backup.ServerKindRemote, Endpoint: srvA.URL, Bucket: "b"}}}
	jobB := &backup.Config{Name: "job-b", Retries: 3, Targets: []backup.Target{{ServerName: "b", Kind: backup.ServerKindRemote, Endpoint: srvB.URL, Bucket: "b"}}}

	pathA := stageTestContent(t, "hello a")
	pathB := stageTestContent(t, "hello b")

	if err := backup.QueueOutstandingUpload(ctx, db, "job-a", 0, pathA, "key-a.gpg", time.Now(), errors.New("boom a")); err != nil {
		t.Fatalf("backup.QueueOutstandingUpload() job-a error: %v", err)
	}

	if err := backup.QueueOutstandingUpload(ctx, db, "job-b", 0, pathB, "key-b.gpg", time.Now(), errors.New("boom b")); err != nil {
		t.Fatalf("backup.QueueOutstandingUpload() job-b error: %v", err)
	}

	store := backup.NewStatusStore([]*backup.Config{jobA, jobB})
	monitor := &OutstandingUploadMonitor{
		db:         db,
		jobsByName: map[string]*backup.Config{"job-a": jobA, "job-b": jobB},
		store:      store,
		identity:   identity,
		log:        discardLogger,
	}

	start := time.Now()

	monitor.processAll(ctx)

	elapsed := time.Since(start)

	if maxInFlight != 1 {
		t.Errorf("max concurrent upload attempts = %d, want 1 (processAll must be sequential)", maxInFlight)
	}

	if elapsed < 60*time.Millisecond {
		t.Errorf("processAll() took %v, want at least 60ms (two sequential 30ms attempts)", elapsed)
	}

	rows, err := backup.ListOutstandingUploads(ctx, db)
	if err != nil {
		t.Fatalf("backup.ListOutstandingUploads() error: %v", err)
	}

	if len(rows) != 0 {
		t.Errorf("backup.ListOutstandingUploads() after both succeed = %+v, want none", rows)
	}
}

// TestOutstandingUploadMonitorSuccessResolvesRow verifies that a retry that
// finally succeeds deletes the outstanding row, updates the live status
// store and persisted target_runs, and removes the staged file since
// nothing else is outstanding for it.
func TestOutstandingUploadMonitorSuccessResolvesRow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := openTestStateDB(t)
	ctx := context.Background()

	job := &backup.Config{Name: "job-a", Retries: 3, Targets: []backup.Target{{ServerName: "nas", Kind: backup.ServerKindLocal, Bucket: "sub", LocalPath: dir}}}
	stagingPath := stageTestContent(t, "hello")

	if err := backup.QueueOutstandingUpload(ctx, db, "job-a", 0, stagingPath, "key.gpg", time.Now(), errors.New("boom")); err != nil {
		t.Fatalf("backup.QueueOutstandingUpload() error: %v", err)
	}

	store := backup.NewStatusStore([]*backup.Config{job})
	monitor := &OutstandingUploadMonitor{db: db, jobsByName: map[string]*backup.Config{"job-a": job}, store: store, log: discardLogger}

	monitor.processAll(ctx)

	rows, err := backup.ListOutstandingUploads(ctx, db)
	if err != nil {
		t.Fatalf("backup.ListOutstandingUploads() error: %v", err)
	}

	if len(rows) != 0 {
		t.Fatalf("backup.ListOutstandingUploads() = %+v, want none after success", rows)
	}

	snap := store.Snapshot()[0]
	if snap.Targets[0].State != backup.StateOK {
		t.Errorf("store target state = %q, want %q", snap.Targets[0].State, backup.StateOK)
	}

	targetRuns, err := backup.ReadTargetRuns(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("backup.ReadTargetRuns() error: %v", err)
	}

	if len(targetRuns) != 1 || targetRuns[0].State != backup.StateOK {
		t.Errorf("backup.ReadTargetRuns() = %+v, want a single ok entry", targetRuns)
	}

	if _, err := os.Stat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged file %q still exists after success with nothing else outstanding", stagingPath)
	}
}

// TestOutstandingUploadMonitorGivesUpAfterMaxAttempts verifies that a target
// that keeps failing is abandoned once it hits its job's cfg.retries total
// attempts: the row is dropped, no further attempts follow, and the staged
// file is cleaned up since nothing else references it.
func TestOutstandingUploadMonitorGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "always fails", http.StatusInternalServerError)
	}))
	defer srv.Close()

	db := openTestStateDB(t)
	ctx := context.Background()

	// retries: 2 means 2 total attempts; queuing with attempts: 1 (as if the
	// initial in-run attempt already failed) means this retry is the second
	// and last one.
	job := &backup.Config{
		Name:     "job-a",
		Retries:  2,
		Identity: testServerIdentity(t),
		Targets:  []backup.Target{{ServerName: "always-down", Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "b"}},
	}
	stagingPath := stageTestContent(t, "hello")

	if err := backup.QueueOutstandingUpload(ctx, db, "job-a", 0, stagingPath, "key.gpg", time.Now(), errors.New("first failure")); err != nil {
		t.Fatalf("backup.QueueOutstandingUpload() error: %v", err)
	}

	store := backup.NewStatusStore([]*backup.Config{job})
	monitor := &OutstandingUploadMonitor{db: db, jobsByName: map[string]*backup.Config{"job-a": job}, store: store, identity: job.Identity, log: discardLogger}

	monitor.processAll(ctx)

	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (this retry's own attempt)", got)
	}

	rows, err := backup.ListOutstandingUploads(ctx, db)
	if err != nil {
		t.Fatalf("backup.ListOutstandingUploads() error: %v", err)
	}

	if len(rows) != 0 {
		t.Fatalf("backup.ListOutstandingUploads() = %+v, want none (max attempts reached, row dropped)", rows)
	}

	if _, err := os.Stat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged file %q still exists after giving up with nothing else outstanding", stagingPath)
	}

	// Running processAll again must not trigger any further attempt: the
	// row is gone, so there's nothing left to retry.
	monitor.processAll(ctx)

	if got := requests.Load(); got != 1 {
		t.Errorf("requests after a second processAll = %d, want still 1 (no further retries once given up)", got)
	}
}

// TestOutstandingUploadMonitorMissingStagedFileGivesUpImmediately verifies
// that an outstanding upload whose staged file no longer exists is dropped
// immediately, without ever attempting the upload — this is unrecoverable
// regardless of how many attempts remain, unlike hitting the max-attempts
// limit.
func TestOutstandingUploadMonitorMissingStagedFileGivesUpImmediately(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	db := openTestStateDB(t)
	ctx := context.Background()

	job := &backup.Config{
		Name:     "job-a",
		Retries:  5,
		Identity: testServerIdentity(t),
		Targets:  []backup.Target{{ServerName: "would-succeed", Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "b"}},
	}

	missingPath := filepath.Join(t.TempDir(), "gone.staged")

	if err := backup.QueueOutstandingUpload(ctx, db, "job-a", 0, missingPath, "key.gpg", time.Now(), errors.New("boom")); err != nil {
		t.Fatalf("backup.QueueOutstandingUpload() error: %v", err)
	}

	store := backup.NewStatusStore([]*backup.Config{job})
	monitor := &OutstandingUploadMonitor{db: db, jobsByName: map[string]*backup.Config{"job-a": job}, store: store, identity: job.Identity, log: discardLogger}

	monitor.processAll(ctx)

	if got := requests.Load(); got != 0 {
		t.Errorf("requests = %d, want 0 (the upload must never be attempted with no staged file)", got)
	}

	rows, err := backup.ListOutstandingUploads(ctx, db)
	if err != nil {
		t.Fatalf("backup.ListOutstandingUploads() error: %v", err)
	}

	if len(rows) != 0 {
		t.Fatalf("backup.ListOutstandingUploads() = %+v, want none (row dropped immediately)", rows)
	}
}

// TestOutstandingUploadMonitorSkipsUnknownJob verifies that a row whose job
// isn't part of this run (e.g. this process was started with -job scoped to
// a different set of jobs) is left queued rather than dropped, so a future
// invocation that includes that job can still retry it.
func TestOutstandingUploadMonitorSkipsUnknownJob(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()
	stagingPath := stageTestContent(t, "hello")

	if err := backup.QueueOutstandingUpload(ctx, db, "ghost-job", 0, stagingPath, "key.gpg", time.Now(), errors.New("boom")); err != nil {
		t.Fatalf("backup.QueueOutstandingUpload() error: %v", err)
	}

	store := backup.NewStatusStore(nil)
	monitor := &OutstandingUploadMonitor{db: db, jobsByName: map[string]*backup.Config{}, store: store, log: discardLogger}

	monitor.processAll(ctx)

	rows, err := backup.ListOutstandingUploads(ctx, db)
	if err != nil {
		t.Fatalf("backup.ListOutstandingUploads() error: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("backup.ListOutstandingUploads() = %+v, want the row left untouched", rows)
	}
}
