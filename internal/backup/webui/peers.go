package webui

import (
	"log/slog"
	"net/http"

	"nilswitt.dev/go-backup-tool/internal/backup/peerstatus"
)

// handlePeers serves GET /api/peers: the status every trusted instance
// last pushed to this one (see peerstatus.Hub), for the dashboard to show
// read-only next to this instance's own. Gated on view, like /api/status.
func handlePeers(peers *peerstatus.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, peers.Snapshot())
	}
}

// handleForgetPeer serves DELETE /api/peers/{id} (admin only): it drops
// the instance's last pushed status, e.g. one that was decommissioned. A
// still trusted instance shows up again with its next push.
func handleForgetPeer(peers *peerstatus.Hub, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if peers == nil {
			http.Error(w, "unknown instance", http.StatusNotFound)
			return
		}

		known, err := peers.Forget(r.Context(), r.PathValue("id"))
		if err != nil {
			log.Error("web UI: forgetting a pushed instance status", "id", r.PathValue("id"), "err", err)
			http.Error(w, "forgetting the instance failed", http.StatusInternalServerError)

			return
		}

		if !known {
			http.Error(w, "unknown instance", http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
