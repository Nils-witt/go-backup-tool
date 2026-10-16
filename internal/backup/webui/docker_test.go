package webui

import (
	"net/http"
	"runtime"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/dockerexec/dockerexectest"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
)

// startDockerWebUI starts an SSO-enabled web UI whose runner talks to the
// Docker daemon at dockerHost.
func startDockerWebUI(t *testing.T, idp *testIDP, dockerHost string) *Server {
	t.Helper()

	status := backup.NewStatusStore(nil)
	runner := pipeline.NewRunner(discardLogger, status, nil, nil, nil, notify.NewRegistry(nil))
	runner.UseDocker(dockerHost)

	srv := StartWebUI("127.0.0.1:0", status, nil, nil, runner, nil, nil, nil, nil, nil, discardLogger, nil, nil, idp.settings(), nil, false, false, 0, nil, "", nil, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	return srv
}

func TestHandleListDockerContainers(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the fake daemon listens on a Unix socket")
	}

	idp := newTestIDP(t)
	daemon := dockerexectest.Start(t, map[string]dockerexectest.Container{
		"db":  {Image: "postgres:16"},
		"old": {Stopped: true},
	})
	srv := startDockerWebUI(t, idp, "unix://"+daemon.Socket)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	if code := doJSON(t, srv, http.MethodGet, "/api/docker-containers", viewer, nil); code != http.StatusForbidden {
		t.Errorf("non-admin list = %d, want %d", code, http.StatusForbidden)
	}

	var list dockerContainerListJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/docker-containers", admin, &list); code != http.StatusOK {
		t.Fatalf("admin list = %d, want %d", code, http.StatusOK)
	}

	if len(list.Containers) != 1 || list.Containers[0].Name != "db" || list.Containers[0].Image != "postgres:16" {
		t.Errorf("containers = %+v, want only the running db", list.Containers)
	}
}

func TestHandleListDockerContainersUnreachableDaemon(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv := startDockerWebUI(t, idp, "unix:///nonexistent/docker.sock")

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})

	if code := doJSON(t, srv, http.MethodGet, "/api/docker-containers", admin, nil); code != http.StatusBadGateway {
		t.Errorf("list = %d, want %d", code, http.StatusBadGateway)
	}
}
