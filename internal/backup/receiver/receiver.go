package receiver

import (
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteAuth"
)

// RegisterRoutes mounts the receiver API's PUT/DELETE object endpoints on
// mux, for the composition root to combine onto the same mux/port the web
// UI dashboard serves from (see webui.StartWebUI's registerExtraRoutes
// parameter). status is the live receiver status store, shared with
// whatever also displays it (e.g. the dashboard's /api/receivers), so a
// write here is reflected there immediately.
func RegisterRoutes(mux *http.ServeMux, receivers map[string]backup.ResolvedReceiver, status *backup.ReceiverStatusStore, log *slog.Logger, db *sql.DB) {
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", HandleReceiveObject(receivers, status, log, db))
	mux.HandleFunc("DELETE /api/v1/objects/{id}/{key...}", HandleDeleteObject(receivers, status, log, db))
}

// HandleReceiveObject serves PUT /api/v1/objects/{id}/{key...}: after
// authorizing the request against receivers (see authorizeReceiver), it
// writes the request body to disk exactly as a type: local target would
// (see backup.ReceiverTarget), so a remote target's PUT and this instance's
// own local-target writes share the same on-disk behavior (atomic
// temp-file-then-rename) and retention tracking. Every attempt is recorded
// to status, win or lose, so /api/receivers reflects it.
func HandleReceiveObject(receivers map[string]backup.ResolvedReceiver, status *backup.ReceiverStatusStore, log *slog.Logger, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := authorizeReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := backup.SanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		cfg := &backup.Config{Key: key, StateDB: db}
		t := backup.ReceiverTarget(recv)

		if err := backup.WriteLocalObject(cfg, t, r.Body); err != nil {
			log.Warn("receiver: writing object failed", "id", recv.ID, "key", key, "err", err)
			status.Record(recv.ID, key, err)
			recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, backup.ReceiverEventReceive, key, 0, err)
			http.Error(w, "writing object failed", http.StatusInternalServerError)

			return
		}

		if err := backup.RecordLocalWrite(r.Context(), cfg, t, log); err != nil {
			log.Warn("receiver: retention tracking failed", "id", recv.ID, "key", key, "err", err)
		}

		status.Record(recv.ID, key, nil)

		var size int64
		if info, statErr := os.Stat(backup.LocalObjectPath(cfg, t)); statErr == nil {
			size = info.Size()
		}

		recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, backup.ReceiverEventReceive, key, size, nil)

		log.Info("receiver: object written", "id", recv.ID, "key", key, "path", backup.LocalObjectPath(cfg, t))
		w.WriteHeader(http.StatusCreated)
	}
}

// HandleDeleteObject serves DELETE /api/v1/objects/{id}/{key...}, the
// receiver API's client-facing counterpart to pipeline's deleteRemoteObject.
// Every attempt is recorded to status, win or lose, so /api/receivers
// reflects it.
func HandleDeleteObject(receivers map[string]backup.ResolvedReceiver, status *backup.ReceiverStatusStore, log *slog.Logger, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := authorizeReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := backup.SanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		cfg := &backup.Config{Key: key, StateDB: db}
		t := backup.ReceiverTarget(recv)

		if err := backup.DeleteLocalObject(cfg, t); err != nil {
			log.Warn("receiver: deleting object failed", "id", recv.ID, "key", key, "err", err)
			status.Record(recv.ID, key, err)
			recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, backup.ReceiverEventDelete, key, 0, err)
			http.Error(w, "deleting object failed", http.StatusInternalServerError)

			return
		}

		if err := backup.RemoveRetentionRecord(r.Context(), cfg, t); err != nil {
			log.Warn("receiver: removing retention record failed", "id", recv.ID, "key", key, "err", err)
		}

		status.Record(recv.ID, key, nil)
		recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, backup.ReceiverEventDelete, key, 0, nil)
		log.Info("receiver: object deleted", "id", recv.ID, "key", key, "path", backup.LocalObjectPath(cfg, t))
		w.WriteHeader(http.StatusNoContent)
	}
}

// authorizeReceiver looks up the receiver named by the request's {id} path
// value and verifies its Authorization: Bearer <token> header as a JWT
// signed by that receiver's configured public-key: (see
// backup.VerifyRemoteAuthToken/backup.SignRemoteAuthToken), writing an
// error response and returning ok=false if either the id is unknown or the
// token doesn't verify.
func authorizeReceiver(w http.ResponseWriter, r *http.Request, receivers map[string]backup.ResolvedReceiver) (recv backup.ResolvedReceiver, ok bool) {
	recv, exists := receivers[r.PathValue("id")]
	if !exists {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
		return backup.ResolvedReceiver{}, false
	}

	token, hasToken := bearerToken(r)
	if !hasToken || remoteAuth.VerifyRemoteAuthToken(token, recv.PublicKey, recv.ID) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return backup.ResolvedReceiver{}, false
	}

	return recv, true
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// request header, reporting false if the header is missing or malformed.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}

	return strings.TrimPrefix(auth, prefix), true
}
