package webui

import (
	"net/http"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/gpgkeys"
	"nilswitt.dev/go-backup-tool/internal/backup/jobs"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
)

// startJobsWebUI starts an SSO-enabled web UI whose jobs, servers, and
// commands are managed by a fresh jobs.Manager over its own state db.
func startJobsWebUI(t *testing.T, idp *testIDP, editing bool) (*Server, *jobs.Manager) {
	t.Helper()

	return startJobsWebUIWithKeyring(t, idp, editing, nil)
}

// startJobsWebUIWithKeyring is startJobsWebUI also serving keyring's GPG
// keys.
func startJobsWebUIWithKeyring(t *testing.T, idp *testIDP, editing bool, keyring *gpgkeys.Keyring) (*Server, *jobs.Manager) {
	t.Helper()

	db := openTestStateDB(t)
	status := backup.NewStatusStore(nil)
	notifications := notify.NewRegistry(nil)
	runner := pipeline.NewRunner(discardLogger, status, db, nil, nil, notifications)
	manager := jobs.NewManager(db, status, runner, notifications, jobs.Settings{GPG: notify.GPGSettings{Bin: "gpg"}, Editing: editing}, discardLogger)

	if err := manager.Load(t.Context(), nil, nil, nil); err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	srv := StartWebUI("127.0.0.1:0", status, manager, keyring, runner, nil, nil, nil, nil, nil, discardLogger, db, nil, idp.settings(), nil, false, false, 0, "", nil, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	return srv, manager
}

func TestJobConfigAPI(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, manager := startJobsWebUI(t, idp, true)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	nas := config.FileServer{Name: "nas", Type: "local", Path: t.TempDir()}
	page := config.FileCommand{ID: "page", Cmd: "echo page"}
	job := config.FileJob{
		Name: "db", Cmd: "exit 1", Recipients: []string{"me@example.com"}, Interval: "1h", StartTime: "2999-01-01T00:00:00Z",
		Targets: []config.FileJobTarget{{Server: "nas", Bucket: "b", OnError: &config.FileTargetOnError{Command: "page", After: 1}}},
	}
	renamed := job
	renamed.Name = "other"
	unnamed := job
	unnamed.Name = ""

	for _, c := range []receiverConfigCall{
		{"non-admin list", http.MethodGet, "/api/job-configs", viewer, nil, http.StatusForbidden},
		{"non-admin create", http.MethodPost, "/api/server-configs", viewer, nas, http.StatusForbidden},
		{"job without server", http.MethodPost, "/api/job-configs", admin, job, http.StatusBadRequest},
		{"create server", http.MethodPost, "/api/server-configs", admin, nas, http.StatusCreated},
		{"duplicate server", http.MethodPost, "/api/server-configs", admin, nas, http.StatusConflict},
		{"invalid server", http.MethodPost, "/api/server-configs", admin, config.FileServer{Name: "x", Type: "ftp"}, http.StatusBadRequest},
		{"create command", http.MethodPost, "/api/command-configs", admin, page, http.StatusCreated},
		{"create job", http.MethodPost, "/api/job-configs", admin, job, http.StatusCreated},
		{"update job", http.MethodPut, "/api/job-configs/db", admin, job, http.StatusNoContent},
		{"rename job", http.MethodPut, "/api/job-configs/db", admin, renamed, http.StatusBadRequest},
		{"update missing job", http.MethodPut, "/api/job-configs/missing", admin, unnamed, http.StatusNotFound},
		{"update command", http.MethodPut, "/api/command-configs/page", admin, config.FileCommand{Cmd: "echo new"}, http.StatusNoContent},
		{"delete server in use", http.MethodDelete, "/api/server-configs/nas", admin, nil, http.StatusConflict},
		{"delete command in use", http.MethodDelete, "/api/command-configs/page", admin, nil, http.StatusConflict},
	} {
		c.run(t, srv)
	}

	checkJobConfigLists(t, srv, admin)

	if j, ok := manager.Get("db"); !ok || j.Targets[0].OnErrorCommand.Cmd != "echo new" {
		t.Errorf("Get(db) = %+v, %v; want the updated command applied", j, ok)
	}

	for _, c := range []receiverConfigCall{
		{"delete job", http.MethodDelete, "/api/job-configs/db", admin, nil, http.StatusNoContent},
		{"delete server", http.MethodDelete, "/api/server-configs/nas", admin, nil, http.StatusNoContent},
		{"delete command", http.MethodDelete, "/api/command-configs/page", admin, nil, http.StatusNoContent},
	} {
		c.run(t, srv)
	}
}

// checkJobConfigLists checks TestJobConfigAPI's listings once job db is set
// up on server nas with command page.
func checkJobConfigLists(t *testing.T, srv *Server, admin string) {
	t.Helper()

	var list jobConfigListJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/job-configs", admin, &list); code != http.StatusOK {
		t.Fatalf("admin list = %d", code)
	}

	if !list.Editing || len(list.Jobs) != 1 || list.Jobs[0].Name != "db" || list.Jobs[0].CreatedBy != "erin" ||
		len(list.Servers) != 1 || len(list.Commands) != 1 || list.Jobs[0].Targets[0].OnError == nil {
		t.Errorf("list = %+v", list)
	}

	var servers serverConfigListJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/server-configs", admin, &servers); code != http.StatusOK {
		t.Fatalf("admin server list = %d", code)
	}

	if len(servers.Servers) != 1 || len(servers.Servers[0].UsedBy) != 1 || servers.Servers[0].UsedBy[0] != "job db" {
		t.Errorf("servers = %+v", servers)
	}
}

func TestJobConfigAPIEditingDisabled(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, _ := startJobsWebUI(t, idp, false)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})

	for _, c := range []receiverConfigCall{
		{"list", http.MethodGet, "/api/command-configs", admin, nil, http.StatusOK},
		{"create", http.MethodPost, "/api/command-configs", admin, config.FileCommand{ID: "c", Cmd: "echo"}, http.StatusForbidden},
	} {
		c.run(t, srv)
	}

	var list commandConfigListJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/command-configs", admin, &list); code != http.StatusOK || list.Editing {
		t.Errorf("list = %d, %+v; want editing false", code, list)
	}
}
