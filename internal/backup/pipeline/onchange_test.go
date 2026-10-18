package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// onChangeFixture is an on-change job wired to a real directory receiver
// served over HTTP, with a state db and a real gpg keyring.
type onChangeFixture struct {
	job     *config.Config
	env     runEnv
	srcDir  string
	recvDir string

	// failing makes the receiver answer every request with a 500.
	failing atomic.Bool
}

func newOnChangeFixture(t *testing.T) *onChangeFixture {
	t.Helper()

	homedir := testGPGKeyring(t)
	id, key := testServerIdentityAndKey(t)

	f := &onChangeFixture{srcDir: t.TempDir(), recvDir: t.TempDir()}

	recv := config.ResolvedReceiver{
		ID: "files", Kind: config.ReceiverKindDirectory, AllowedServers: []string{"test-sender-uuid"}, Path: f.recvDir,
		TrustedServers: trust.NewRegistry(map[string]trust.Server{"test-sender-uuid": {ID: "test-sender-uuid", Name: "sender", PublicKey: &key.PublicKey}}),
	}
	receivers := map[string]config.ResolvedReceiver{recv.ID: recv}

	mux := http.NewServeMux()
	receiver.RegisterRoutes(mux, receiver.NewRegistry(receivers), status.NewReceiverStore(receivers), discardLogger, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.failing.Load() {
			http.Error(w, "down for maintenance", http.StatusInternalServerError)
			return
		}

		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	db, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	f.env = runEnv{db: db, identity: id}
	f.job = &config.Config{
		Name:       "docs",
		Kind:       config.JobKindOnChange,
		Source:     config.OnChangeSource{Path: f.srcDir, Exclude: []string{"*.tmp"}, Debounce: time.Second},
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
		StagingDir: t.TempDir(),
		Targets:    []config.Target{{ServerName: "offsite", Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: recv.ID}},
	}

	return f
}

// write writes content to rel under the fixture's source, with mod time
// at, so a test controls exactly when a file looks changed.
func (f *onChangeFixture) write(t *testing.T, rel, content string, at time.Time) {
	t.Helper()

	p := filepath.Join(f.srcDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// run runs the job once over changed (nil: everything).
func (f *onChangeFixture) run(t *testing.T, changed []string) onChangeResult {
	t.Helper()

	return runOnChange(t.Context(), f.env, f.job, changed, discardLogger)
}

// versions returns the receiver's stored versions as "path@version".
func (f *onChangeFixture) versions(t *testing.T) []string {
	t.Helper()

	versions, err := objectstore.ListFileVersions(f.recvDir)
	if err != nil {
		t.Fatalf("ListFileVersions() error: %v", err)
	}

	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.Path + "@" + strconv.Itoa(v.Version)
	}

	return out
}

// decrypt returns the plaintext of the receiver's version key.
func (f *onChangeFixture) decrypt(t *testing.T, key string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "gpg", "--homedir", f.job.GPGHomedir, "--batch", "--quiet", "--decrypt", filepath.Join(f.recvDir, filepath.FromSlash(key))) //nolint:gosec // test paths

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gpg --decrypt %s: %v", key, err)
	}

	return string(out)
}

func checkOnChangeResult(t *testing.T, step string, res onChangeResult, wantUploaded, wantStored int) {
	t.Helper()

	if err := res.err(); err != nil {
		t.Fatalf("%s: runOnChange() error: %v", step, err)
	}

	if res.uploaded != wantUploaded || res.stored != wantStored {
		t.Errorf("%s: uploaded %d, stored %d; want %d, %d", step, res.uploaded, res.stored, wantUploaded, wantStored)
	}
}

func TestRunOnChange(t *testing.T) {
	t.Parallel()

	f := newOnChangeFixture(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	f.write(t, "a.txt", "alpha", t0)
	f.write(t, "sub/b.txt", "bravo", t0)
	f.write(t, "scratch.tmp", "excluded", t0)

	checkOnChangeResult(t, "first scan", f.run(t, nil), 2, 2)

	if got, want := f.versions(t), []string{"a.txt@1", "sub/b.txt@1"}; !slices.Equal(got, want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}

	checkOnChangeResult(t, "unchanged rescan", f.run(t, nil), 0, 0)

	// Touched, same content: rehashed, not uploaded.
	f.write(t, "a.txt", "alpha", t0.Add(time.Minute))
	checkOnChangeResult(t, "touched", f.run(t, nil), 0, 0)

	f.write(t, "sub/b.txt", "bravo, edited", t0.Add(time.Minute))
	checkOnChangeResult(t, "edited", f.run(t, nil), 1, 1)

	if got, want := f.versions(t), []string{"a.txt@1", "sub/b.txt@1", "sub/b.txt@2"}; !slices.Equal(got, want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}

	versions, err := objectstore.ListFileVersions(f.recvDir)
	if err != nil {
		t.Fatal(err)
	}

	if got := f.decrypt(t, versions[2].Key); got != "bravo, edited" {
		t.Errorf("decrypted newest sub/b.txt = %q, want %q", got, "bravo, edited")
	}
}

func TestRunOnChangeChangedPaths(t *testing.T) {
	t.Parallel()

	f := newOnChangeFixture(t)
	t0 := time.Now().Add(-time.Hour)

	f.write(t, "a.txt", "alpha", t0)
	f.write(t, "sub/b.txt", "bravo", t0)
	f.write(t, "sub/deep/c.txt", "charlie", t0)

	// A changed directory stands for everything below it; a changed path
	// that's gone (deleted) is skipped.
	checkOnChangeResult(t, "changed sub", f.run(t, []string{"sub", "deleted.txt"}), 2, 2)

	if got, want := f.versions(t), []string{"sub/b.txt@1", "sub/deep/c.txt@1"}; !slices.Equal(got, want) {
		t.Errorf("versions = %v, want %v", got, want)
	}
}

func TestRunOnChangeRetriesFailedUploads(t *testing.T) {
	t.Parallel()

	f := newOnChangeFixture(t)
	f.write(t, "a.txt", "alpha", time.Now().Add(-time.Hour))

	f.failing.Store(true)

	res := f.run(t, nil)
	if err := res.targetErrs[0]; err == nil || !strings.Contains(err.Error(), "down for maintenance") {
		t.Fatalf("failing run: target error = %v, want the receiver's", err)
	}

	f.failing.Store(false)

	// Nothing was recorded for the failed upload, so the next scan sends it.
	checkOnChangeResult(t, "recovered", f.run(t, nil), 1, 1)
}

func TestRunOnChangeRequiresStateDB(t *testing.T) {
	t.Parallel()

	job := &config.Config{Name: "docs", Kind: config.JobKindOnChange, Targets: []config.Target{{ServerName: "offsite"}}}

	res := runOnChange(t.Context(), runEnv{}, job, nil, discardLogger)
	if err := res.targetErrs[0]; err == nil || !strings.Contains(err.Error(), "state db") {
		t.Errorf("runOnChange() target error = %v, want it to need the state db", err)
	}
}

func TestExcludedPath(t *testing.T) {
	t.Parallel()

	src := config.OnChangeSource{Exclude: []string{"cache", "*.tmp"}}

	for _, tc := range []struct {
		rel  string
		want bool
	}{
		{rel: "a.txt", want: false},
		{rel: "x.tmp", want: true},
		{rel: "cache/deep/a.txt", want: true},
		{rel: "docs/a.gbtv/x", want: true},
		{rel: "docs/a.txt", want: false},
	} {
		if got := excludedPath(src, tc.rel); got != tc.want {
			t.Errorf("excludedPath(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

func TestWatchSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := config.OnChangeSource{Path: dir, Exclude: []string{"*.tmp"}, Watch: true, Debounce: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	batches := make(chan []string, 10)

	var busy atomic.Int32

	busy.Store(1) // the first flush finds the job busy and must be offered again

	done := make(chan error, 1)

	go func() {
		done <- watchSource(ctx, src, discardLogger, func(changed []string) (bool, bool) {
			if busy.Add(-1) >= 0 {
				return false, false
			}

			batches <- changed

			return true, false
		})
	}()

	// Give the watcher a moment to add its watches.
	time.Sleep(200 * time.Millisecond)

	if err := os.MkdirAll(filepath.Join(dir, "new"), 0o750); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"a.txt", "skip.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := waitBatch(t, batches)
	if !slices.Contains(got, "a.txt") || !slices.Contains(got, "new") || slices.Contains(got, "skip.tmp") {
		t.Errorf("batch = %v, want a.txt and new but not the excluded skip.tmp", got)
	}

	// A file in a directory created after the watch started is seen too.
	if err := os.WriteFile(filepath.Join(dir, "new", "b.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := waitBatch(t, batches); !slices.Contains(got, "new/b.txt") {
		t.Errorf("second batch = %v, want new/b.txt", got)
	}

	cancel()

	if err := <-done; err != nil {
		t.Errorf("watchSource() error: %v", err)
	}
}

// waitBatch returns the next batch a watcher flushed into batches.
func waitBatch(t *testing.T, batches <-chan []string) []string {
	t.Helper()

	select {
	case got := <-batches:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("no batch flushed within 10s")
		return nil
	}
}

func TestRunnerRunOnChangeOnceRecording(t *testing.T) {
	t.Parallel()

	f := newOnChangeFixture(t)
	f.write(t, "a.txt", "alpha", time.Now().Add(-time.Hour))

	statusStore := status.NewStore([]*config.Config{f.job})
	r := &Runner{log: discardLogger, store: statusStore, stateDB: f.env.db, identity: f.env.identity, targetFailures: map[targetKey]int{}}

	countRuns := func() int {
		events, err := f.env.db.ListJobRunEvents(t.Context(), 100)
		if err != nil {
			t.Fatalf("ListJobRunEvents() error: %v", err)
		}

		return len(events)
	}

	if err := r.runOnChangeOnce(t.Context(), f.job, nil, nil); err != nil {
		t.Fatalf("full scan error: %v", err)
	}

	if n := countRuns(); n != 1 {
		t.Fatalf("after a full scan: %d recorded runs, want 1", n)
	}

	// A watch-triggered run with nothing new to upload isn't recorded.
	if err := r.runOnChangeOnce(t.Context(), f.job, []string{"a.txt"}, nil); err != nil {
		t.Fatalf("triggered run error: %v", err)
	}

	if n := countRuns(); n != 1 {
		t.Errorf("after an idle triggered run: %d recorded runs, want still 1", n)
	}

	f.write(t, "a.txt", "alpha, edited", time.Now())

	if err := r.runOnChangeOnce(t.Context(), f.job, []string{"a.txt"}, nil); err != nil {
		t.Fatalf("triggered run error: %v", err)
	}

	if n := countRuns(); n != 2 {
		t.Errorf("after a triggered upload: %d recorded runs, want 2", n)
	}

	if snaps := statusStore.Snapshot(); len(snaps) != 1 || snaps[0].State != status.StateOK || len(snaps[0].Files) != 1 {
		t.Errorf("status = %+v, want the job ok with its one uploaded file", snaps)
	}
}

func TestScheduleLiveOnChangeWatchBlocksUntilStopped(t *testing.T) {
	t.Parallel()

	f := newOnChangeFixture(t)
	f.job.Source.Watch = true

	r := &Runner{log: discardLogger, store: status.NewStore([]*config.Config{f.job}), stateDB: f.env.db, identity: f.env.identity, targetFailures: map[targetKey]int{}}

	stop, cancel := context.WithCancel(t.Context())
	returned := make(chan struct{})

	go func() {
		defer close(returned)

		r.ScheduleLive(t.Context(), stop, f.job, func(string) (*config.Config, bool) { return f.job, true }, false)
	}()

	// No interval: the schedule itself has nothing left after its first
	// scan, but the watch keeps ScheduleLive running.
	select {
	case <-returned:
		t.Fatal("ScheduleLive() returned while its source was still being watched")
	case <-time.After(500 * time.Millisecond):
	}

	cancel()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("ScheduleLive() still running 10s after stop")
	}
}
