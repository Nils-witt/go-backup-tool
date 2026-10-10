package jobs

import (
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

var discardLogger = slog.New(slog.DiscardHandler)

func openTestStateDB(t *testing.T) *store.Store {
	t.Helper()

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

type testManager struct {
	*Manager

	status *backup.StatusStore
	db     *store.Store
}

func newTestManager(t *testing.T, db *store.Store, settings Settings) testManager {
	t.Helper()

	status := backup.NewStatusStore(nil)
	notifications := notify.NewRegistry(map[string]notify.Notification{"ops": {ID: "ops", Webhook: &notify.Webhook{URL: "http://example.invalid"}}})
	runner := pipeline.NewRunner(discardLogger, status, db, nil, nil, notifications)

	if settings.GPG.Bin == "" {
		settings.GPG.Bin = "gpg"
	}

	return testManager{Manager: NewManager(db, status, runner, notifications, settings, discardLogger), status: status, db: db}
}

func localServer(name string) config.FileServer {
	return config.FileServer{Name: name, Type: "local", Path: "/tmp/" + name}
}

// runCommands is the commands every test manager loads — "run", the source
// job uses, plus alternatives for tests that switch a job's command — and
// then extra.
func runCommands(extra ...config.FileCommand) []config.FileCommand {
	return append([]config.FileCommand{
		{ID: "run", Cmd: "exit 1"}, {ID: "new", Cmd: "echo new"}, {ID: "fail2", Cmd: "exit 2"},
	}, extra...)
}

// job is a valid job uploading to server, repeating hourly from a start
// time far in the future, so it never actually runs during a test.
func job(name, server string) config.FileJob {
	return config.FileJob{
		Name: name, Command: "run", Recipients: []string{"me@example.com"},
		Interval: "1h", StartTime: "2999-01-01T00:00:00Z",
		Targets: []config.FileJobTarget{{Server: server, Bucket: "b"}},
	}
}

func statusNames(s *backup.StatusStore) []string {
	var names []string
	for _, j := range s.Snapshot() {
		names = append(names, j.Name)
	}

	return names
}

func TestLoadImportsConfigFile(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()

	// Stored before: edits made in the web UI win over the config file.
	if err := db.CreateJobConfig(ctx, toStoreJob(normalizeJob(job("db", "edited")))); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, db, Settings{Editing: true})

	err := m.Load(ctx,
		[]config.FileServer{localServer("nas"), {Name: "broken", Type: "nope"}},
		runCommands(config.FileCommand{ID: "page", Cmd: "echo"}),
		[]config.FileJob{job("db", "nas"), job("files", "nas"), job("orphan", "broken")},
	)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// db keeps its stored (edited) definition, whose server doesn't exist.
	if got := m.Names(); !slices.Equal(got, []string{"files"}) {
		t.Errorf("Names() = %v, want [files]", got)
	}

	if got := statusNames(m.status); !slices.Equal(got, []string{"files"}) {
		t.Errorf("status jobs = %v, want [files]", got)
	}

	checkLoadedLists(t, m)
}

// checkLoadedLists checks TestLoadImportsConfigFile's listings: only job
// files resolves, server broken doesn't, and nas is used by files.
func checkLoadedLists(t *testing.T, m testManager) {
	t.Helper()

	ctx := t.Context()

	jobs, err := m.ListJobs(ctx)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("ListJobs() = %+v, %v", jobs, err)
	}

	for _, j := range jobs {
		wantErr := j.Name != "files"
		if (j.Error != "") != wantErr {
			t.Errorf("job %s error = %q, want error: %v", j.Name, j.Error, wantErr)
		}
	}

	servers, err := m.ListServers(ctx)
	if err != nil || len(servers) != 2 || servers[0].Name != "broken" || servers[0].Error == "" || !slices.Equal(servers[1].UsedBy, []string{"job files"}) {
		t.Errorf("ListServers() = %+v, %v", servers, err)
	}

	if got := m.CommandIDs(); !slices.Equal(got, []string{"fail2", "new", "page", "run"}) {
		t.Errorf("CommandIDs() = %v, want [fail2 new page run]", got)
	}
}

// TestLoadFromConfigFile checks that with editing off, jobs come from the
// config file alone: nothing is imported, jobs stored by an earlier run with
// editing on are ignored, and the listings show the config file's entries.
func TestLoadFromConfigFile(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()

	if err := db.CreateJobConfig(ctx, toStoreJob(normalizeJob(job("stale", "nas")))); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, db, Settings{})
	if err := m.Load(ctx, []config.FileServer{localServer("nas")}, runCommands(config.FileCommand{ID: "page", Cmd: "echo"}), []config.FileJob{job("db", "nas")}); err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if got := m.Names(); !slices.Equal(got, []string{"db"}) {
		t.Errorf("Names() = %v, want [db]", got)
	}

	if stored, err := db.ListServerConfigs(ctx); err != nil || len(stored) != 0 {
		t.Errorf("state db servers = %+v, %v; want nothing imported", stored, err)
	}

	checkConfigFileLists(t, m)

	if err := m.UpdateJob(ctx, "erin", job("db", "nas")); !errors.Is(err, ErrEditingDisabled) {
		t.Errorf("UpdateJob() error = %v, want ErrEditingDisabled", err)
	}
}

// checkConfigFileLists checks TestLoadFromConfigFile's listings show the
// config file's job db on server nas.
func checkConfigFileLists(t *testing.T, m testManager) {
	t.Helper()

	ctx := t.Context()

	jobs, err := m.ListJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Name != "db" || jobs[0].CreatedBy != configFileUser {
		t.Errorf("ListJobs() = %+v, %v; want the config file's db", jobs, err)
	}

	servers, err := m.ListServers(ctx)
	if err != nil || len(servers) != 1 || !slices.Equal(servers[0].UsedBy, []string{"job db"}) {
		t.Errorf("ListServers() = %+v, %v", servers, err)
	}
}

func TestLoadJobFilter(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := openTestStateDB(t)

	m := newTestManager(t, db, Settings{Filter: "files"})
	if err := m.Load(ctx, []config.FileServer{localServer("nas")}, runCommands(), []config.FileJob{job("db", "nas"), job("files", "nas")}); err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if got := m.Names(); !slices.Equal(got, []string{"files"}) {
		t.Errorf("Names() = %v, want [files]", got)
	}

	m2 := newTestManager(t, db, Settings{Filter: "nope"})
	if err := m2.Load(ctx, nil, runCommands(), nil); err == nil || !strings.Contains(err.Error(), "no such job") {
		t.Errorf("Load() error = %v, want no such job", err)
	}
}

func TestLoadWithoutStateDB(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil, Settings{Editing: true})
	if err := m.Load(t.Context(), []config.FileServer{localServer("nas")}, runCommands(), []config.FileJob{job("db", "nas")}); err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if got := m.Names(); !slices.Equal(got, []string{"db"}) {
		t.Errorf("Names() = %v, want [db]", got)
	}

	if err := m.CreateJob(t.Context(), "erin", job("x", "nas")); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("CreateJob() error = %v, want ErrStoreUnavailable", err)
	}
}

func TestMutationsNeedEditing(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t), Settings{})
	ctx := t.Context()

	if err := m.Load(ctx, []config.FileServer{localServer("nas")}, runCommands(), nil); err != nil {
		t.Fatal(err)
	}

	for name, err := range map[string]error{
		"CreateJob":     m.CreateJob(ctx, "erin", job("db", "nas")),
		"UpdateServer":  m.UpdateServer(ctx, "erin", localServer("nas")),
		"DeleteServer":  m.DeleteServer(ctx, "erin", "nas"),
		"CreateCommand": m.CreateCommand(ctx, "erin", config.FileCommand{ID: "c", Cmd: "echo"}),
	} {
		if !errors.Is(err, ErrEditingDisabled) {
			t.Errorf("%s() error = %v, want ErrEditingDisabled", name, err)
		}
	}
}

func TestCreateJobValidates(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t), Settings{Editing: true})
	ctx := t.Context()

	if err := m.Load(ctx, nil, runCommands(), nil); err != nil {
		t.Fatal(err)
	}

	if err := m.CreateJob(ctx, "erin", job("db", "nas")); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no server named") {
		t.Errorf("CreateJob(unknown server) error = %v, want ErrInvalid", err)
	}

	if err := m.CreateServer(ctx, "erin", localServer("nas")); err != nil {
		t.Fatalf("CreateServer() error: %v", err)
	}

	bad := job("db", "nas")
	bad.FailureNotifications = []string{"missing"}

	if err := m.CreateJob(ctx, "erin", bad); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "unknown notification") {
		t.Errorf("CreateJob(unknown notification) error = %v, want ErrInvalid", err)
	}

	if err := m.CreateJob(ctx, "erin", job("db", "nas")); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}
}

func TestJobCRUD(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t), Settings{Editing: true})
	ctx := t.Context()

	if err := m.Load(ctx, []config.FileServer{localServer("nas")}, runCommands(), nil); err != nil {
		t.Fatal(err)
	}

	if err := m.CreateJob(ctx, "erin", job(" db ", "nas")); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}

	if err := m.CreateJob(ctx, "erin", job("db", "nas")); !errors.Is(err, store.ErrJobExists) {
		t.Errorf("duplicate CreateJob() error = %v, want ErrJobExists", err)
	}

	updated := job("db", "nas")
	updated.Command = "new"

	if err := m.UpdateJob(ctx, "frank", updated); err != nil {
		t.Fatalf("UpdateJob() error: %v", err)
	}

	if got, ok := m.Get("db"); !ok || got.Source.Cmd != "echo new" {
		t.Errorf("Get(db) = %+v, %v; want the updated cmd", got, ok)
	}

	if err := m.UpdateJob(ctx, "frank", job("missing", "nas")); !errors.Is(err, store.ErrJobNotFound) {
		t.Errorf("UpdateJob(missing) error = %v, want ErrJobNotFound", err)
	}
}

func TestDeleteJob(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t), Settings{Editing: true})
	ctx := t.Context()

	if err := m.Load(ctx, []config.FileServer{localServer("nas")}, runCommands(), []config.FileJob{job("db", "nas")}); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteServer(ctx, "erin", "nas"); !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), "job db") {
		t.Errorf("DeleteServer(in use) error = %v, want ErrInUse naming job db", err)
	}

	if err := m.DeleteJob(ctx, "frank", "db"); err != nil {
		t.Fatalf("DeleteJob() error: %v", err)
	}

	if _, ok := m.Get("db"); ok || len(m.status.Snapshot()) != 0 {
		t.Errorf("after DeleteJob, job still active or on the dashboard")
	}

	if err := m.DeleteServer(ctx, "erin", "nas"); err != nil {
		t.Errorf("DeleteServer() error: %v", err)
	}
}

func TestServerAndCommandEditsApplyToJobs(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t), Settings{Editing: true})
	ctx := t.Context()

	j := job("db", "nas")
	j.Targets[0].Retention = "30d"
	j.Targets[0].OnRecover = &config.FileTargetOnRecover{Command: "resolve"}

	if err := m.Load(ctx, []config.FileServer{localServer("nas")}, runCommands(config.FileCommand{ID: "resolve", Cmd: "echo old"}), []config.FileJob{j}); err != nil {
		t.Fatal(err)
	}

	moved := localServer("nas")
	moved.Path = "/srv/elsewhere"

	if err := m.UpdateServer(ctx, "erin", moved); err != nil {
		t.Fatalf("UpdateServer() error: %v", err)
	}

	if err := m.UpdateCommand(ctx, "erin", config.FileCommand{ID: "resolve", Cmd: "echo new"}); err != nil {
		t.Fatalf("UpdateCommand() error: %v", err)
	}

	got, _ := m.Get("db")
	if got.Targets[0].LocalPath != "/srv/elsewhere" || got.Targets[0].OnRecoverCommand.Cmd != "echo new" {
		t.Errorf("job target = %+v, want the new server path and command", got.Targets[0])
	}

	// A retention override is only valid on a local server.
	if err := m.UpdateServer(ctx, "erin", config.FileServer{Name: "nas", Type: "remote", Endpoint: "https://x"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `job "db"`) {
		t.Errorf("UpdateServer(breaks job) error = %v, want ErrInvalid naming job db", err)
	}

	if err := m.DeleteCommand(ctx, "erin", "resolve"); !errors.Is(err, ErrInUse) {
		t.Errorf("DeleteCommand(in use) error = %v, want ErrInUse", err)
	}

	checkCommandUsers(t, m)
}

// checkCommandUsers checks that job db counts as using both the command it
// runs and its target's hook, and that the former can't be deleted.
func checkCommandUsers(t *testing.T, m testManager) {
	t.Helper()

	ctx := t.Context()

	commands, err := m.ListCommands(ctx)
	if err != nil || len(commands) != 4 {
		t.Fatalf("ListCommands() = %+v, %v", commands, err)
	}

	// "resolve" is db's target hook and "run" its own command: both count.
	for _, c := range commands {
		want := c.ID == "resolve" || c.ID == "run"
		if got := slices.Equal(c.UsedBy, []string{"job db"}); got != want {
			t.Errorf("command %s UsedBy = %v", c.ID, c.UsedBy)
		}
	}

	if err := m.DeleteCommand(ctx, "erin", "run"); !errors.Is(err, ErrInUse) {
		t.Errorf("DeleteCommand(job's own command) error = %v, want ErrInUse", err)
	}
}

// TestScheduling checks a created job runs right away, an edit that keeps
// its schedule doesn't run it again, and deleting it ends its schedule.
func TestScheduling(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t), Settings{Editing: true})
	ctx := t.Context()

	if err := m.Load(ctx, []config.FileServer{localServer("nas")}, runCommands(), nil); err != nil {
		t.Fatal(err)
	}

	m.Start(ctx)

	j := job("db", "nas")
	j.StartTime = "" // runs right away, then hourly

	if err := m.CreateJob(ctx, "erin", j); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}

	first := waitForRun(t, m.status, "db", time.Time{})

	j.Command = "fail2"
	if err := m.UpdateJob(ctx, "erin", j); err != nil {
		t.Fatalf("UpdateJob() error: %v", err)
	}

	// A schedule change reschedules, resumed: the next run is an interval
	// after the last one, not now.
	j.Interval = "2h"
	if err := m.UpdateJob(ctx, "erin", j); err != nil {
		t.Fatalf("UpdateJob() error: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	snap := m.status.Snapshot()
	if len(snap) != 1 || !snap[0].LastStart.Equal(first) {
		t.Fatalf("after edits, LastStart = %v, want unchanged %v", snap[0].LastStart, first)
	}

	if want := first.Add(2 * time.Hour); snap[0].NextRun.Sub(want).Abs() > time.Second {
		t.Errorf("NextRun = %v, want ~%v", snap[0].NextRun, want)
	}

	if err := m.DeleteJob(ctx, "erin", "db"); err != nil {
		t.Fatalf("DeleteJob() error: %v", err)
	}

	done := make(chan struct{})

	go func() {
		m.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() didn't return after the only job was deleted")
	}
}

// waitForRun waits for job name's run started after since to finish,
// returning its start.
func waitForRun(t *testing.T, s *backup.StatusStore, name string, since time.Time) time.Time {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		for _, j := range s.Snapshot() {
			if j.Name == name && j.LastStart.After(since) && j.State != backup.StateRunning && !j.LastEnd.IsZero() {
				return j.LastStart
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("job %s didn't run", name)

	return time.Time{}
}
