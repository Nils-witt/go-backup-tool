package pipeline

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// plainTestJob builds a job (no start-time, no interval — a one-shot run)
// that, when run, appends "run\n" to a marker file. Used by the
// runOnce/state-persistence tests below.
func plainTestJob(t *testing.T, marker string) *config.Config {
	t.Helper()

	homedir := testGPGKeyring(t)

	t.Setenv("MARKER_FILE", marker)

	return &config.Config{
		Name:       "test",
		Sources:    []config.JobSource{{ID: "test", Cmd: `echo run >> "$MARKER_FILE"`, Name: "test.gpg"}},
		Key:        "backup-{time}.gpg",
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
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

	stateDB, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	r.runOnce(context.Background(), job)

	run, ok, err := stateDB.GetLastRun(context.Background(), job.Name)
	if err != nil {
		t.Fatalf("ReadLastRun() error: %v", err)
	}

	if !ok {
		t.Fatal("ReadLastRun() ok = false after runOnce, want true")
	}

	if !run.Success {
		t.Errorf("run.Success = %v, want true", run.Success)
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
	homedir := testGPGKeyring(t)

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
		Sources:    []config.JobSource{{ID: "test", Cmd: "echo hi", Name: "test.gpg"}},
		Key:        "backup-{time}.gpg",
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
		Targets: []config.Target{
			{ServerName: "slow-remote", Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"},
			{ServerName: "fast-local", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	statusStore := status.NewStore([]*config.Config{job})
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

		if localState == status.StateOK {
			if remoteState != status.StateRunning {
				close(release)
				<-done

				t.Fatalf("local target finished, but remote target state = %q, want %q (it should still be in flight, held by the test)", remoteState, status.StateRunning)
			}

			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	close(release)
	<-done

	snap := statusStore.Snapshot()
	if snap[0].Targets[0].State != status.StateOK {
		t.Errorf("remote target final state = %q, want %q", snap[0].Targets[0].State, status.StateOK)
	}

	if snap[0].Targets[1].State != status.StateOK {
		t.Errorf("local target final state = %q, want %q", snap[0].Targets[1].State, status.StateOK)
	}
}

// TestRunOnceReportsIncompleteWhenSomeTargetsFail is an end-to-end check
// (real gpg, real runOnce) that a run where one target succeeds and one
// fails is reported as incomplete rather than failed, in both the live
// status store and the persisted state db — and that it still counts
// toward the process's overall exit code, since a partial backup still
// warrants attention.
func TestRunOnceReportsIncompleteWhenSomeTargetsFail(t *testing.T) {
	homedir := testGPGKeyring(t)

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
		Sources:    []config.JobSource{{ID: "test", Cmd: "echo hi", Name: "test.gpg"}},
		Key:        "backup-{time}.gpg",
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
		Targets: []config.Target{
			{ServerName: "bad", Kind: config.ServerKindLocal, Bucket: "blocked/sub", LocalPath: dir},
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	stateDB, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	r.runOnce(context.Background(), job)

	snap := statusStore.Snapshot()[0]
	if snap.State != status.StateIncomplete {
		t.Errorf("job State = %q, want incomplete", snap.State)
	}

	if snap.Size == "" {
		t.Error("job Size is empty, want the backup's size (the good target got a complete copy)")
	}

	if snap.Targets[0].State != status.StateFailed {
		t.Errorf("bad target state = %q, want failed", snap.Targets[0].State)
	}

	if snap.Targets[1].State != status.StateOK {
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

	if run.Success {
		t.Error("persisted run.Success = true, want false (an incomplete run still counts as not fully successful)")
	}

	if run.Size == 0 {
		t.Error("persisted run.Size = 0, want the backup's actual size")
	}
}

// multiFileTestJob builds a one-shot job whose sources are cmds (named
// "<id>.gpg"), using fakeGPG so each stored file is exactly its command's
// output, with one local target under dir.
func multiFileTestJob(t *testing.T, dir string, cmds map[string]string) *config.Config {
	t.Helper()

	ids := slices.Sorted(maps.Keys(cmds))
	sources := make([]config.JobSource, len(ids))

	for i, id := range ids {
		sources[i] = config.JobSource{ID: id, Cmd: cmds[id], Name: id + ".gpg"}
	}

	return &config.Config{
		Name:       "multi",
		Sources:    sources,
		Key:        "backup",
		Recipients: []string{"unused@example.com"},
		GPGBin:     fakeGPG(t),
		Targets:    []config.Target{{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir}},
	}
}

func TestRunOnceStoresEachFileUnderKey(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	dir := t.TempDir()
	job := multiFileTestJob(t, dir, map[string]string{"db": "echo db-dump", "files": "echo file-dump"})

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore}

	r.runOnce(context.Background(), job)

	backupDir := filepath.Join(dir, "sub", "backup")

	for name, expected := range map[string]string{"db.gpg": "db-dump\n", "files.gpg": "file-dump\n"} {
		got, err := os.ReadFile(filepath.Join(backupDir, name)) //nolint:gosec // backupDir is t.TempDir() plus fixed test literals
		if err != nil || string(got) != expected {
			t.Errorf("%s = %q, %v; want %q", name, got, err, expected)
		}
	}

	if _, err := os.Stat(filepath.Join(backupDir, objectstore.MultiFileMarker)); err != nil {
		t.Errorf("multi-file marker missing: %v", err)
	}

	snap := statusStore.Snapshot()[0]
	if snap.State != status.StateOK || len(snap.Files) != 2 {
		t.Fatalf("job = state %q, files %+v; want ok with two files", snap.State, snap.Files)
	}

	for _, f := range snap.Files {
		if f.State != status.StateOK || f.Size == "" {
			t.Errorf("file %+v, want ok with a size", f)
		}
	}
}

func TestRunOnceUploadsFilesThatSucceeded(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	dir := t.TempDir()
	job := multiFileTestJob(t, dir, map[string]string{"broken": "exit 3", "ok": "echo fine"})

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore}

	r.runOnce(context.Background(), job)

	backupDir := filepath.Join(dir, "sub", "backup")

	got, err := os.ReadFile(filepath.Join(backupDir, "ok.gpg")) //nolint:gosec // backupDir is t.TempDir() plus fixed test literals
	if err != nil || string(got) != "fine\n" {
		t.Errorf("ok.gpg = %q, %v; want the successful command's output", got, err)
	}

	if _, err := os.Stat(filepath.Join(backupDir, "broken.gpg")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("broken.gpg stat error = %v, want not exist", err)
	}

	snap := statusStore.Snapshot()[0]
	if snap.State != status.StateIncomplete {
		t.Errorf("job State = %q, want incomplete", snap.State)
	}

	if snap.Targets[0].State != status.StateOK {
		t.Errorf("target State = %q, want ok (it got every file that was produced)", snap.Targets[0].State)
	}

	if !strings.Contains(snap.Error, `file "broken.gpg"`) {
		t.Errorf("job Error = %q, want it to name the failed file", snap.Error)
	}

	if len(snap.Files) != 2 || snap.Files[0].State != status.StateFailed || snap.Files[1].State != status.StateOK {
		t.Errorf("Files = %+v, want broken failed and ok succeeded", snap.Files)
	}
}

func TestRunOnceFailsWhenNoFileIsProduced(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	dir := t.TempDir()
	job := multiFileTestJob(t, dir, map[string]string{"a": "exit 1", "b": "exit 2"})

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore}

	r.runOnce(context.Background(), job)

	snap := statusStore.Snapshot()[0]
	if snap.State != status.StateFailed || snap.Targets[0].State != status.StateFailed {
		t.Errorf("job = %q, target = %q; want both failed", snap.State, snap.Targets[0].State)
	}

	if _, err := os.Stat(filepath.Join(dir, "sub", "backup")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("backup folder stat error = %v, want nothing written", err)
	}
}

// TestRunOnceFiresFailureNotificationOnFailure verifies runOnce fires a
// job's failure-notifications: when its run ends incomplete (some targets
// failed), mirroring TestRunOnceReportsIncompleteWhenSomeTargetsFail's setup.
func TestRunOnceFiresFailureNotificationOnFailure(t *testing.T) {
	homedir := testGPGKeyring(t)

	t.Parallel()

	dir := t.TempDir()

	badParent := filepath.Join(dir, "blocked")
	if err := os.WriteFile(badParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("setting up blocked path: %v", err)
	}

	var (
		mu          sync.Mutex
		gotBody     string
		requestSeen bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("webhook server: reading request body: %v", err)
		}

		mu.Lock()
		gotBody = string(body)
		requestSeen = true
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	notifications := testWebhookRegistry(notify.Webhook{URL: srv.URL, Method: http.MethodPost})

	job := &config.Config{
		Name:                 "test",
		Sources:              []config.JobSource{{ID: "test", Cmd: "echo hi", Name: "test.gpg"}},
		Key:                  "backup-{time}.gpg",
		Recipients:           []string{testGPGRecipient},
		GPGBin:               "gpg",
		GPGHomedir:           homedir,
		FailureNotifications: []string{"test"},
		Targets: []config.Target{
			{ServerName: "bad", Kind: config.ServerKindLocal, Bucket: "blocked/sub", LocalPath: dir},
		},
	}

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, notifications: notifications}

	r.runOnce(context.Background(), job)

	mu.Lock()
	defer mu.Unlock()

	if !requestSeen {
		t.Fatal("failure-notifications webhook: no request received on job failure")
	}

	if !strings.Contains(gotBody, `"job":"test"`) {
		t.Errorf("webhook body = %q, want it to name the job", gotBody)
	}
}

// TestRunOnceNoFailureNotificationOnSuccess is the success-path counterpart
// to TestRunOnceFiresFailureNotificationOnFailure: a job that finishes
// without error never fires its failure-notifications:.
func TestRunOnceNoFailureNotificationOnSuccess(t *testing.T) {
	homedir := testGPGKeyring(t)

	t.Parallel()

	dir := t.TempDir()

	var (
		mu          sync.Mutex
		requestSeen bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestSeen = true
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	notifications := testWebhookRegistry(notify.Webhook{URL: srv.URL, Method: http.MethodPost})

	job := &config.Config{
		Name:                 "test",
		Sources:              []config.JobSource{{ID: "test", Cmd: "echo hi", Name: "test.gpg"}},
		Key:                  "backup-{time}.gpg",
		Recipients:           []string{testGPGRecipient},
		GPGBin:               "gpg",
		GPGHomedir:           homedir,
		FailureNotifications: []string{"test"},
		Targets: []config.Target{
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, notifications: notifications}

	r.runOnce(context.Background(), job)

	mu.Lock()
	defer mu.Unlock()

	if requestSeen {
		t.Error("failure-notifications webhook: request received, want none on a successful run")
	}
}

// TestRunOnceKeepsStagedFileForRetryWhenTargetFails is an end-to-end check
// (real gpg, real runOnce) that a job's staged file is kept, and an
// outstanding target upload is recorded for it, when a target fails: the
// file is still needed for RunOutstandingUploadRetries to retry that target
// from later (see uploadStagedToTargets/removeStagingFileIfUnreferenced).
func TestRunOnceKeepsStagedFileForRetryWhenTargetFails(t *testing.T) {
	homedir := testGPGKeyring(t)

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
		Sources:    []config.JobSource{{ID: "test", Cmd: "echo hi", Name: "test.gpg"}},
		Key:        "backup-{time}.gpg",
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
		StagingDir: stagingDir,
		Targets: []config.Target{
			{ServerName: "bad", Kind: config.ServerKindLocal, Bucket: "blocked/sub", LocalPath: dir},
		},
	}

	stateDB, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	r.runOnce(context.Background(), job)

	staged, err := filepath.Glob(filepath.Join(stagingDir, "go-backup-tool-*.staged"))
	if err != nil {
		t.Fatalf("globbing staging dir: %v", err)
	}

	if len(staged) != 1 {
		t.Fatalf("staged files in %q = %v, want exactly one (kept for the outstanding retry)", stagingDir, staged)
	}

	due, err := stateDB.ListDueOutstandingTargetUploads(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ListDueOutstandingTargetUploads() error: %v", err)
	}

	if len(due) != 1 || due[0].FileName != staged[0] || due[0].JobName != job.Name || due[0].Target != "bad" {
		t.Errorf("ListDueOutstandingTargetUploads() = %+v, want one row for job %q target %q file %q", due, job.Name, "bad", staged[0])
	}
}

// TestRunOnceDeletesStagedFileOnSuccess is the success-path counterpart to
// TestRunOnceDeletesStagedFileEvenWhenTargetFails: the staged file is
// removed once every target has succeeded.
func TestRunOnceDeletesStagedFileOnSuccess(t *testing.T) {
	homedir := testGPGKeyring(t)

	t.Parallel()

	dir := t.TempDir()
	stagingDir := t.TempDir()

	job := &config.Config{
		Name:       "test",
		Sources:    []config.JobSource{{ID: "test", Cmd: "echo hi", Name: "test.gpg"}},
		Key:        "backup-{time}.gpg",
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
		StagingDir: stagingDir,
		Targets: []config.Target{
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	stateDB, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := status.NewStore([]*config.Config{job})
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

// TestRetryFailedTargetsRetriesOnlyNamedTargets is an end-to-end check (real
// gpg, real runOnce/RetryFailedTargets) that retrying re-runs the whole
// pipeline (since the original run's staged file is already gone — see
// RetryFailedTargets's doc comment) for just the named target, leaving the
// other target's already-recorded outcome untouched, and that a fixed
// target which failed on the first run can succeed on retry.
func TestRetryFailedTargetsRetriesOnlyNamedTargets(t *testing.T) {
	homedir := testGPGKeyring(t)

	t.Parallel()

	dir := t.TempDir()
	goodDir := t.TempDir()
	badDir := t.TempDir()

	// Make the "bad" target's parent directory path a regular file, so
	// WriteLocalObject's os.MkdirAll for it fails deterministically on the
	// first run; removing it before the retry lets that same target
	// succeed the second time.
	badParent := filepath.Join(badDir, "blocked")
	if err := os.WriteFile(badParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("setting up blocked path: %v", err)
	}

	job := &config.Config{
		Name:       "test",
		Sources:    []config.JobSource{{ID: "test", Cmd: "echo hi", Name: "test.gpg"}},
		Key:        "backup-{time}.gpg",
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
		Targets: []config.Target{
			{ServerName: "bad", Kind: config.ServerKindLocal, Bucket: "blocked/sub", LocalPath: badDir},
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: goodDir},
		},
	}

	stateDB, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	r.runOnce(context.Background(), job)

	snap := statusStore.Snapshot()[0]
	if snap.Targets[0].State != status.StateFailed {
		t.Fatalf("bad target state after first run = %q, want failed", snap.Targets[0].State)
	}

	if snap.Targets[1].State != status.StateOK {
		t.Fatalf("good target state after first run = %q, want ok", snap.Targets[1].State)
	}

	goodLastError := snap.Targets[1].Error

	// Unblock the bad target so its retry can actually succeed.
	if err := os.Remove(badParent); err != nil {
		t.Fatalf("unblocking bad target: %v", err)
	}

	if err := r.RetryFailedTargets(context.Background(), job, []string{"bad"}); err != nil {
		t.Fatalf("RetryFailedTargets() error: %v", err)
	}

	snap = statusStore.Snapshot()[0]

	if snap.Targets[0].State != status.StateOK {
		t.Errorf("bad target state after retry = %q, want ok", snap.Targets[0].State)
	}

	if snap.Targets[1].State != status.StateOK || snap.Targets[1].Error != goodLastError {
		t.Errorf("good target after retry = {state: %q, error: %q}, want unchanged {ok, %q}", snap.Targets[1].State, snap.Targets[1].Error, goodLastError)
	}

	if snap.State != status.StateOK {
		t.Errorf("job State after retry = %q, want ok", snap.State)
	}
}

// TestRetryFailedTargetsFiresFailureNotificationOnFailure verifies
// RetryFailedTargets fires a job's failure-notifications: when the retry
// itself still fails, mirroring TestRunOnceFiresFailureNotificationOnFailure
// for the manual "retry failed targets" web UI action.
func TestRetryFailedTargetsFiresFailureNotificationOnFailure(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		requestSeen bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestSeen = true
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	notifications := testWebhookRegistry(notify.Webhook{URL: srv.URL, Method: http.MethodPost})

	job := &config.Config{
		Name:                 "test",
		FailureNotifications: []string{"test"},
		Targets: []config.Target{
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: t.TempDir()},
		},
	}

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, notifications: notifications}

	// RetryFailedTargets itself returns an error before running the
	// pipeline when no named target matches the job — this doesn't
	// exercise notifyJobFailure (there's no run to fail), so instead retry
	// the job's own "good" target, but with a job Cmd that fails, forcing
	// the retried run itself to fail.
	job.Sources = []config.JobSource{{ID: "test", Cmd: "false", Name: "test.gpg"}}

	if err := r.RetryFailedTargets(context.Background(), job, []string{"good"}); err == nil {
		t.Fatal("RetryFailedTargets() error = nil, want an error since the retried run's cmd always fails")
	}

	mu.Lock()
	defer mu.Unlock()

	if !requestSeen {
		t.Error("failure-notifications webhook: no request received on retry failure")
	}
}

// TestRetryFailedTargetsUnknownTargetNameReturnsError verifies
// RetryFailedTargets rejects a request naming no target that actually
// exists on the job, rather than silently doing nothing.
func TestRetryFailedTargetsUnknownTargetNameReturnsError(t *testing.T) {
	t.Parallel()

	job := &config.Config{
		Name: "test",
		Targets: []config.Target{
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: t.TempDir()},
		},
	}

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore}

	if err := r.RetryFailedTargets(context.Background(), job, []string{"nope"}); err == nil {
		t.Fatal("RetryFailedTargets() error = nil, want an error for a target name that doesn't exist on the job")
	}
}

// TestRunNowRunsJobInBackground verifies RunNow returns at once and runs
// the job to completion in the background, releasing its running claim
// afterwards so the job can run again.
func TestRunNowRunsJobInBackground(t *testing.T) { //nolint:paralleltest // plainTestJob calls t.Setenv, which is incompatible with t.Parallel
	marker := filepath.Join(t.TempDir(), "marker")
	job := plainTestJob(t, marker)

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore}

	if err := r.RunNow(context.Background(), job); err != nil {
		t.Fatalf("RunNow() error: %v", err)
	}

	waitForRelease(t, r, job.Name)

	if got := countMarkerRuns(t, marker); got != 1 {
		t.Errorf("marker recorded %d runs, want 1", got)
	}

	if state := statusStore.Snapshot()[0].State; state != status.StateOK {
		t.Errorf("job state = %q, want ok", state)
	}
}

// waitForRelease waits (up to 5s) until job name no longer has a run in
// flight on r.
func waitForRelease(t *testing.T, r *Runner, name string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		if _, running := r.running.Load(name); !running {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the background run to finish")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// TestRunNowRejectsRunningJob verifies RunNow refuses to start a job that
// already has a run in flight, rather than overlapping it.
func TestRunNowRejectsRunningJob(t *testing.T) {
	t.Parallel()

	job := &config.Config{Name: "test"}
	r := &Runner{log: discardLogger, store: status.NewStore([]*config.Config{job})}

	r.claim(job.Name)

	if err := r.RunNow(context.Background(), job); !errors.Is(err, ErrJobRunning) {
		t.Errorf("RunNow() error = %v, want ErrJobRunning", err)
	}
}

// TestRetryFailedTargetsRejectsRunningJob verifies RetryFailedTargets
// refuses to overlap a run already in flight for the same job.
func TestRetryFailedTargetsRejectsRunningJob(t *testing.T) {
	t.Parallel()

	job := &config.Config{
		Name: "test",
		Targets: []config.Target{
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: t.TempDir()},
		},
	}
	r := &Runner{log: discardLogger, store: status.NewStore([]*config.Config{job})}

	r.claim(job.Name)

	if err := r.RetryFailedTargets(context.Background(), job, []string{"good"}); !errors.Is(err, ErrJobRunning) {
		t.Errorf("RetryFailedTargets() error = %v, want ErrJobRunning", err)
	}
}

// TestScheduleSkipsRunWhileJobIsRunning verifies a scheduled run that comes
// due while the job is already running (e.g. started by RunNow) is skipped
// instead of overlapping it.
func TestScheduleSkipsRunWhileJobIsRunning(t *testing.T) { //nolint:paralleltest // plainTestJob calls t.Setenv, which is incompatible with t.Parallel
	marker := filepath.Join(t.TempDir(), "marker")
	job := plainTestJob(t, marker)

	r := &Runner{log: discardLogger, store: status.NewStore([]*config.Config{job})}

	r.claim(job.Name)

	// job has no interval, so Schedule makes its single immediate run
	// attempt and returns.
	r.Schedule(context.Background(), job)

	if got := countMarkerRuns(t, marker); got != 0 {
		t.Errorf("marker recorded %d runs, want 0 (the scheduled run should have been skipped)", got)
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

	stateDB, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	firstStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: firstStore, stateDB: stateDB}
	r.runOnce(context.Background(), job)

	restartedStore := status.NewStore([]*config.Config{job})

	if got := restartedStore.Snapshot()[0].State; got != status.StateIdle {
		t.Fatalf("restartedStore before seeding: State = %q, want idle", got)
	}

	SeedStatusFromState(context.Background(), stateDB, []*config.Config{job}, restartedStore, discardLogger)

	snap := restartedStore.Snapshot()[0]
	if snap.State != status.StateOK {
		t.Errorf("restartedStore after seeding: State = %q, want ok", snap.State)
	}

	if snap.LastStart.IsZero() || snap.LastEnd.IsZero() {
		t.Error("restartedStore after seeding: LastStart/LastEnd still zero, want the prior run's times")
	}
}

func TestSubstituteKeyTime(t *testing.T) {
	t.Parallel()

	got := substituteKeyTime("prefix-{time}-suffix.gpg", time.Now().UTC())

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

	homedir := testGPGKeyring(t)

	t.Setenv("MARKER_FILE", marker)

	return &config.Config{
		Name:       "test",
		Sources:    []config.JobSource{{ID: "test", Cmd: `echo run >> "$MARKER_FILE"`, Name: "test.gpg"}},
		Key:        "backup-{time}.gpg",
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
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

	job := startTimeTestJob(t, marker, time.Now().Add(-10*time.Minute), time.Hour)

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	r.Schedule(ctx, job)

	if got := countMarkerRuns(t, marker); got != 1 {
		t.Errorf("marker recorded %d runs, want exactly 1 (one immediate catch-up run, no extra runs before the next grid slot)", got)
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
	interval := time.Hour

	job := startTimeTestJob(t, marker, startTime, interval)

	stateDB, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	due, ok := lastDueSlot(startTime, interval, time.Now())
	if !ok {
		t.Fatal("lastDueSlot() ok = false, want true (start-time is in the past)")
	}

	if err := stateDB.SaveJobRun(context.Background(), job.Name, string(status.StateOK), true, due, due, 0, ""); err != nil {
		t.Fatalf("SaveJobRun() error: %v", err)
	}

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	r.Schedule(ctx, job)

	if got := countMarkerRuns(t, marker); got != 0 {
		t.Errorf("marker recorded %d runs, want 0 (already-covered slot must not trigger a catch-up run)", got)
	}
}

// TestRetryOutstandingUploadSucceedsAfterTargetRecovers is an end-to-end
// check that a target upload recorded as outstanding by a failed runOnce
// (see uploadStagedToTargets) is later retried successfully by
// retryOutstandingUpload once the target recovers, updating the live status
// store, the target-run history, and cleaning up both the outstanding row
// and its staged file — exactly what runOnce itself would have done had the
// upload succeeded on the first attempt.
func TestRetryOutstandingUploadSucceedsAfterTargetRecovers(t *testing.T) {
	homedir := testGPGKeyring(t)

	t.Parallel()

	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "temporarily down", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	identity := testServerIdentity(t)
	stagingDir := t.TempDir()

	job := &config.Config{
		Name:       "test",
		Sources:    []config.JobSource{{ID: "test", Cmd: "echo hi", Name: "test.gpg"}},
		Key:        "backup-{time}.gpg",
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
		StagingDir: stagingDir,
		Targets: []config.Target{
			{ServerName: "flaky", Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"},
		},
	}

	stateDB, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB, identity: identity}

	r.runOnce(context.Background(), job)

	if got := requests.Load(); got != 1 {
		t.Fatalf("requests after runOnce = %d, want 1 (the first attempt should have failed)", got)
	}

	staged, err := filepath.Glob(filepath.Join(stagingDir, "go-backup-tool-*.staged"))
	if err != nil || len(staged) != 1 {
		t.Fatalf("staged files = %v, %v, want exactly one kept for retry", staged, err)
	}

	due, err := stateDB.ListDueOutstandingTargetUploads(context.Background(), time.Now().Add(time.Hour))
	if err != nil || len(due) != 1 {
		t.Fatalf("ListDueOutstandingTargetUploads() = %+v, %v, want one outstanding row", due, err)
	}

	r.retryOutstandingUpload(context.Background(), StaticJobs([]*config.Config{job}), due[0], discardLogger)

	if got := requests.Load(); got != 2 {
		t.Fatalf("requests after retry = %d, want 2 (the retry should have succeeded)", got)
	}

	assertOutstandingUploadResolved(t, stateDB, statusStore, job.Name, staged[0])
}

// assertOutstandingUploadResolved is
// TestRetryOutstandingUploadSucceedsAfterTargetRecovers's post-retry
// verification, split out to keep that test's cyclomatic complexity down:
// once a retry succeeds, the outstanding row and its staged file must both
// be gone, and the target/job's live and persisted state must show success.
func assertOutstandingUploadResolved(t *testing.T, stateDB store.Store, statusStore *status.Store, jobName, stagedPath string) {
	t.Helper()

	stillDue, err := stateDB.ListDueOutstandingTargetUploads(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ListDueOutstandingTargetUploads() after retry error: %v", err)
	}

	if len(stillDue) != 0 {
		t.Errorf("ListDueOutstandingTargetUploads() after retry = %+v, want none (the outstanding row should be forgotten)", stillDue)
	}

	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Errorf("staged file %q still exists after the last reference resolved, want removed", stagedPath)
	}

	targetRuns, err := stateDB.ListTargetRuns(context.Background(), jobName)
	if err != nil {
		t.Fatalf("ListTargetRuns() error: %v", err)
	}

	if len(targetRuns) != 1 || targetRuns[0].State != string(status.StateOK) {
		t.Errorf("ListTargetRuns() = %+v, want a single ok entry recording the retry's success", targetRuns)
	}

	snapshot := statusStore.Snapshot()
	if len(snapshot) != 1 || len(snapshot[0].Targets) != 1 {
		t.Fatalf("statusStore.Snapshot() = %+v, want one job with one target", snapshot)
	}

	if got := snapshot[0].Targets[0].State; got != status.StateOK {
		t.Errorf("snapshot target state = %q, want %q (retry succeeded)", got, status.StateOK)
	}

	if got := snapshot[0].State; got != status.StateOK {
		t.Errorf("snapshot job state = %q, want %q (the only target's retry succeeded)", got, status.StateOK)
	}
}

// TestRetryOutstandingUploadGivesUpWhenStagedFileIsGone verifies that
// retryOutstandingUpload forgets an outstanding upload rather than retrying
// it forever when its staged file is no longer on disk (e.g. the process
// restarted after StagingDir was cleaned).
func TestRetryOutstandingUploadGivesUpWhenStagedFileIsGone(t *testing.T) {
	t.Parallel()

	stateDB, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = stateDB.Close() })

	missing := filepath.Join(t.TempDir(), "no-such-file.staged")

	if err := stateDB.AddOutstandingTargetUpload(context.Background(), store.OutstandingTargetUpload{JobName: "test", Target: "flaky", FileName: missing, Key: "backup.gpg", CreatedAt: time.Now(), RetryAt: time.Now()}); err != nil {
		t.Fatalf("AddOutstandingTargetUpload() error: %v", err)
	}

	due, err := stateDB.ListDueOutstandingTargetUploads(context.Background(), time.Now())
	if err != nil || len(due) != 1 {
		t.Fatalf("ListDueOutstandingTargetUploads() = %+v, %v, want one row", due, err)
	}

	job := &config.Config{
		Name:    "test",
		Targets: []config.Target{{ServerName: "flaky", Kind: config.ServerKindRemote, Endpoint: "http://127.0.0.1:0", Bucket: "instance-a"}},
	}

	statusStore := status.NewStore([]*config.Config{job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: stateDB}

	r.retryOutstandingUpload(context.Background(), StaticJobs([]*config.Config{job}), due[0], discardLogger)

	stillDue, err := stateDB.ListDueOutstandingTargetUploads(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("ListDueOutstandingTargetUploads() after giving up error: %v", err)
	}

	if len(stillDue) != 0 {
		t.Errorf("ListDueOutstandingTargetUploads() after giving up = %+v, want none (a missing staged file can never be retried)", stillDue)
	}
}
