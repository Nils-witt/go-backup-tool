package webui

import (
	"log/slog"
	"net/http"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

// handleReceiverStatus serves store's current receiver statuses as JSON,
// annotated with each receiver's live staleness (see receiver.Snapshots)
// for any entry in receivers with stale-after: set.
func handleReceiverStatus(receivers *receiver.Registry, store *status.ReceiverStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, receiver.Snapshots(receivers, store, log))
	}
}

// handleReceiverFiles serves GET /api/receivers/{id}/files: the objects
// currently stored under receiver {id}'s path (see listReceiverFiles), for
// the web UI dashboard's per-receiver file listing. Unlike the receiver API
// (receiver.HandleReceiveObject), this is dashboard-only and isn't
// JWT-authenticated, matching /api/receivers.
func handleReceiverFiles(receivers *receiver.Registry, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := lookupReceiver(w, r, receivers)
		if !ok {
			return
		}

		files, err := objectstore.ListReceiverFiles(recv)
		if err != nil {
			log.Warn("receiver: listing files failed", "id", recv.ID, "err", err)
			http.Error(w, "listing files failed", http.StatusInternalServerError)

			return
		}

		writeJSON(w, files)
	}
}

// lookupReceiver returns the receiver named by r's {id} path value, writing
// a 404 and reporting ok as false if it's unknown. Shared by
// handleReceiverFiles and handleDownloadFile.
func lookupReceiver(w http.ResponseWriter, r *http.Request, receivers *receiver.Registry) (config.ResolvedReceiver, bool) {
	recv, ok := receivers.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
	}

	return recv, ok
}
