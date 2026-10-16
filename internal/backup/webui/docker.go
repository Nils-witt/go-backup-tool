package webui

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/dockerexec"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
)

// dockerListTimeout bounds asking the Docker daemon for its containers, so a
// hung socket doesn't hold the command dialog's request open.
const dockerListTimeout = 5 * time.Second

// dockerContainerListJSON is GET /api/docker-containers' response.
type dockerContainerListJSON struct {
	Containers []dockerexec.Container `json:"containers"`
}

// handleListDockerContainers serves GET /api/docker-containers (admin only):
// the running containers on the Docker daemon commands with a container:
// run in, for the command dialog to offer. A daemon that can't be reached
// or answers with an error is a 502 carrying its message, since having no
// Docker at all is a normal setup the dialog just notes.
func handleListDockerContainers(runner *pipeline.Runner, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client, err := dockerexec.New(runner.DockerHost())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), dockerListTimeout)
		defer cancel()

		containers, err := client.ListContainers(ctx)
		if err != nil {
			log.Debug("web UI: listing docker containers failed", "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)

			return
		}

		writeJSON(w, dockerContainerListJSON{Containers: containers})
	}
}
