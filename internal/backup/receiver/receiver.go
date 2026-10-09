package receiver

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteAuth"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// RegisterRoutes mounts the receiver API's PUT/DELETE object endpoints on
// mux, for the composition root to combine onto the same mux/port the web
// UI dashboard serves from (see webui.StartWebUI's registerExtraRoutes
// parameter). status is the live receiver status store, shared with
// whatever also displays it (e.g. the dashboard's /api/receivers), so a
// write here is reflected there immediately.
func RegisterRoutes(mux *http.ServeMux, receivers *backup.ReceiverRegistry, status *backup.ReceiverStatusStore, log *slog.Logger, db *store.Store) {
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", HandleReceiveObject(receivers, status, log, db))
	mux.HandleFunc("DELETE /api/v1/objects/{id}/{key...}", HandleDeleteObject(receivers, status, log, db))
}

// HandleReceiveObject serves PUT /api/v1/objects/{id}/{key...}: after
// authorizing the request against receivers (see authorizeReceiver), it
// writes the request body to disk exactly as a type: local target would
// (see backup.ReceiverTarget), so a remote target's PUT and this instance's
// own local-target writes share the same on-disk behavior (atomic
// temp-file-then-rename) and retention tracking. It records the write for
// retention (backup.RecordObjectWrite) but doesn't sweep for expired objects
// itself — MonitorReceiverRetention sweeps every receiver on its own
// one-minute timer instead, so a PUT's latency isn't paying for a sweep. An
// optional creationTime query parameter (RFC3339, must be strictly before
// the upload time — see backup.ParseCreationTime) lets the caller base
// retention on when the backup content was actually produced rather than on
// upload time; when given, it's also stamped as the written file's mtime
// (backup.SetObjectModTime) so the dashboard's file listing agrees with it.
// Every attempt is recorded to status, win or lose, so /api/receivers
// reflects it.
func HandleReceiveObject(receivers *backup.ReceiverRegistry, status *backup.ReceiverStatusStore, log *slog.Logger, db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, cfg, t, key, ok := resolveReceiverRequest(w, r, receivers, db, log)
		if !ok {
			return
		}

		createdAt, err := backup.ParseCreationTime(r.URL.Query().Get("creationTime"), time.Now())
		if err != nil {
			log.Warn("receiver: invalid creationTime", "id", recv.ID, "key", key, "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		cfg.CreatedAt = createdAt

		if err := backup.WriteLocalObject(cfg, t, r.Body); err != nil {
			log.Warn("receiver: writing object failed", "id", recv.ID, "key", key, "err", err)
			status.Record(recv.ID, key, err)
			recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, store.ReceiverEventReceive, key, 0, err)
			http.Error(w, "writing object failed", http.StatusInternalServerError)

			return
		}

		if !cfg.CreatedAt.IsZero() {
			if err := backup.SetObjectModTime(cfg, t, cfg.CreatedAt); err != nil {
				log.Warn("receiver: setting object mod time failed", "id", recv.ID, "key", key, "err", err)
			}
		}

		if err := backup.RecordObjectWrite(r.Context(), cfg, t, log); err != nil {
			log.Warn("receiver: retention tracking failed", "id", recv.ID, "key", key, "err", err)
		}

		status.Record(recv.ID, key, nil)

		var size int64
		if info, statErr := os.Stat(backup.LocalObjectPath(cfg, t)); statErr == nil { //nolint:gosec // cfg.Key is resolveReceiverRequest's already-SanitizeObjectKey-validated key, not raw untrusted input
			size = info.Size()
		}

		recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, store.ReceiverEventReceive, key, size, nil)

		log.Info("receiver: object written", "id", recv.ID, "key", key, "path", backup.LocalObjectPath(cfg, t))
		w.WriteHeader(http.StatusCreated)
	}
}

// HandleDeleteObject serves DELETE /api/v1/objects/{id}/{key...}, the
// receiver API's client-facing counterpart to pipeline's deleteRemoteObject.
// Every attempt is recorded to status, win or lose, so /api/receivers
// reflects it.
func HandleDeleteObject(receivers *backup.ReceiverRegistry, status *backup.ReceiverStatusStore, log *slog.Logger, db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, cfg, t, key, ok := resolveReceiverRequest(w, r, receivers, db, log)
		if !ok {
			return
		}

		if err := backup.DeleteLocalObject(cfg, t); err != nil {
			log.Warn("receiver: deleting object failed", "id", recv.ID, "key", key, "err", err)
			status.Record(recv.ID, key, err)
			recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, store.ReceiverEventDelete, key, 0, err)
			http.Error(w, "deleting object failed", http.StatusInternalServerError)

			return
		}

		if err := backup.RemoveRetentionRecord(r.Context(), cfg, t); err != nil {
			log.Warn("receiver: removing retention record failed", "id", recv.ID, "key", key, "err", err)
		}

		status.Record(recv.ID, key, nil)
		recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, store.ReceiverEventDelete, key, 0, nil)
		log.Info("receiver: object deleted", "id", recv.ID, "key", key, "path", backup.LocalObjectPath(cfg, t))
		w.WriteHeader(http.StatusNoContent)
	}
}

// authorizeReceiver looks up the receiver named by the request's {id} path
// value and verifies its Authorization: Bearer <token> header (see
// verifyReceiverToken), writing an error response and returning ok=false if
// either the id is unknown or the token doesn't verify.
func authorizeReceiver(w http.ResponseWriter, r *http.Request, receivers *backup.ReceiverRegistry, log *slog.Logger) (recv config.ResolvedReceiver, ok bool) {
	recv, exists := receivers.Get(r.PathValue("id"))
	if !exists {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
		return config.ResolvedReceiver{}, false
	}

	token, hasToken := bearerToken(r)
	if !hasToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return config.ResolvedReceiver{}, false
	}

	issuer, err := verifyReceiverToken(recv, token)
	if err != nil {
		log.Debug("receiver: rejected request", "id", recv.ID, "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return config.ResolvedReceiver{}, false
	}

	if issuer == "" {
		warnLegacyKeyOnce(recv.ID, token, log)
	}

	return recv, true
}

// verifyReceiverToken verifies token as a request token (see
// remoteAuth.SignRemoteAuthToken) for recv, returning the trusted server id
// it was verified as coming from. A token whose issuer is one of recv's
// allowed servers must verify against that server's current key; any other
// token is checked against recv's deprecated public-key: instead, if set,
// returning "" as the issuer on success.
func verifyReceiverToken(recv config.ResolvedReceiver, token string) (string, error) {
	issuer, err := remoteAuth.UnverifiedIssuer(token)
	if err != nil {
		return "", err
	}

	if slices.Contains(recv.AllowedServers, issuer) {
		srv, ok := recv.TrustedServers.Get(issuer)
		if !ok {
			return "", fmt.Errorf("allowed server %q is no longer trusted", issuer)
		}

		if err := remoteAuth.VerifyRemoteAuthToken(token, srv.PublicKey, recv.ID, issuer); err != nil {
			return "", err
		}

		return issuer, nil
	}

	if recv.PublicKey == nil {
		return "", fmt.Errorf("issuer %q is not an allowed server", issuer)
	}

	if err := remoteAuth.VerifyRemoteAuthToken(token, recv.PublicKey, recv.ID, ""); err != nil {
		return "", err
	}

	return "", nil
}

// legacyKeyWarned records the receiver ids warnLegacyKeyOnce has already
// warned about this run.
var legacyKeyWarned sync.Map

// warnLegacyKeyOnce logs, once per receiver per run, that recv accepted a
// request through its deprecated public-key:, naming the (now verified)
// issuer so the operator can register it as a trusted server.
func warnLegacyKeyOnce(receiverID, token string, log *slog.Logger) {
	if _, loaded := legacyKeyWarned.LoadOrStore(receiverID, struct{}{}); loaded {
		return
	}

	issuer, _ := remoteAuth.UnverifiedIssuer(token)
	log.Warn("receiver: request authorized by the receiver's deprecated public key; add the sender as a trusted server and allow it on this receiver instead",
		"id", receiverID, "sender_server_id", issuer)
}

// resolveReceiverRequest authorizes r against receivers (see
// authorizeReceiver) and sanitizes its {key} path value (see
// backup.SanitizeObjectKey), writing an error response and reporting ok as
// false if either step fails. On success it also builds the cfg/t pair
// HandleReceiveObject/HandleDeleteObject both need to reuse
// WriteLocalObject/DeleteLocalObject as a type: local target would (see
// backup.ReceiverTarget). Shared by both handlers, which otherwise duplicate
// this exact preamble.
func resolveReceiverRequest(w http.ResponseWriter, r *http.Request, receivers *backup.ReceiverRegistry, db *store.Store, log *slog.Logger) (recv config.ResolvedReceiver, cfg *config.Config, t *config.Target, key string, ok bool) {
	recv, ok = authorizeReceiver(w, r, receivers, log)
	if !ok {
		return config.ResolvedReceiver{}, nil, nil, "", false
	}

	key, err := backup.SanitizeObjectKey(r.PathValue("key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return config.ResolvedReceiver{}, nil, nil, "", false
	}

	return recv, &config.Config{Key: key, StateDB: db}, backup.ReceiverTarget(recv), key, true
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
