package receiver

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
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
		recv, cfg, t, key, bound, ok := resolveReceiverRequest(w, r, receivers, db, log)
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

		var body io.Reader = r.Body
		if bound.BodySHA256 != "" {
			body = &digestReader{r: r.Body, h: sha256.New(), want: bound.BodySHA256}
		}

		if err := backup.WriteLocalObject(cfg, t, body); err != nil {
			log.Warn("receiver: writing object failed", "id", recv.ID, "key", key, "err", err)
			status.Record(recv.ID, key, err)
			recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, store.ReceiverEventReceive, key, 0, err)

			if errors.Is(err, errBodyDigestMismatch) {
				http.Error(w, errBodyDigestMismatch.Error(), http.StatusBadRequest)
				return
			}

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
		recv, cfg, t, key, _, ok := resolveReceiverRequest(w, r, receivers, db, log)
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
// either the id is unknown or the token doesn't verify. bound is the
// request the token is bound to (see remoteAuth.Request), for the caller
// to check against r once it has sanitized the key.
func authorizeReceiver(w http.ResponseWriter, r *http.Request, receivers *backup.ReceiverRegistry, log *slog.Logger) (recv config.ResolvedReceiver, bound remoteAuth.Request, ok bool) {
	recv, exists := receivers.Get(r.PathValue("id"))
	if !exists {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
		return config.ResolvedReceiver{}, remoteAuth.Request{}, false
	}

	token, hasToken := bearerToken(r)
	if !hasToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return config.ResolvedReceiver{}, remoteAuth.Request{}, false
	}

	issuer, bound, err := verifyReceiverToken(recv, token)
	if err != nil {
		log.Debug("receiver: rejected request", "id", recv.ID, "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return config.ResolvedReceiver{}, remoteAuth.Request{}, false
	}

	if issuer == "" {
		warnLegacyKeyOnce(recv.ID, token, log)
	}

	if bound.IsZero() {
		warnUnboundTokenOnce(recv.ID, token, log)
	}

	return recv, bound, true
}

// verifyReceiverToken verifies token as a request token (see
// remoteAuth.SignRemoteAuthToken) for recv, returning the trusted server id
// it was verified as coming from. A token whose issuer is one of recv's
// allowed servers must verify against that server's current key; any other
// token is checked against recv's deprecated public-key: instead, if set,
// returning "" as the issuer on success. Either way it also returns the
// request the token is bound to (see remoteAuth.Request).
func verifyReceiverToken(recv config.ResolvedReceiver, token string) (string, remoteAuth.Request, error) {
	issuer, err := remoteAuth.UnverifiedIssuer(token)
	if err != nil {
		return "", remoteAuth.Request{}, err
	}

	if slices.Contains(recv.AllowedServers, issuer) {
		srv, ok := recv.TrustedServers.Get(issuer)
		if !ok {
			return "", remoteAuth.Request{}, fmt.Errorf("allowed server %q is no longer trusted", issuer)
		}

		bound, err := remoteAuth.VerifyRemoteAuthToken(token, srv.PublicKey, recv.ID, issuer)
		if err != nil {
			return "", remoteAuth.Request{}, err
		}

		return issuer, bound, nil
	}

	if recv.PublicKey == nil {
		return "", remoteAuth.Request{}, fmt.Errorf("issuer %q is not an allowed server", issuer)
	}

	bound, err := remoteAuth.VerifyRemoteAuthToken(token, recv.PublicKey, recv.ID, "")
	if err != nil {
		return "", remoteAuth.Request{}, err
	}

	return "", bound, nil
}

// errBodyDigestMismatch reports that a PUT's body isn't the one its request
// token was signed for (see remoteAuth.Request.BodySHA256).
var errBodyDigestMismatch = errors.New("body does not match the request token's digest")

// checkRequestBinding reports whether r, whose sanitized key is key, is
// the request bound names: same method, key and creationTime, and — for a
// PUT — a body digest to verify while it streams (see digestReader). A
// zero bound (an older sender's unbound token) is accepted as before.
func checkRequestBinding(r *http.Request, key string, bound remoteAuth.Request) error {
	if bound.IsZero() {
		return nil
	}

	switch {
	case bound.Method != r.Method:
		return fmt.Errorf("token is for method %q, not %q", bound.Method, r.Method)
	case bound.Key != key:
		return fmt.Errorf("token is for key %q, not %q", bound.Key, key)
	case bound.CreationTime != r.URL.Query().Get("creationTime"):
		return errors.New("token is for a different creationTime")
	case r.Method == http.MethodPut && bound.BodySHA256 == "":
		return errors.New("token has no body digest for a PUT")
	}

	return nil
}

// digestReader passes r through while hashing it into h, failing the final
// read with errBodyDigestMismatch unless the hex digest equals want — so
// WriteLocalObject never renames a body the token wasn't signed for into
// place.
type digestReader struct {
	r    io.Reader
	h    hash.Hash
	want string
}

func (d *digestReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.h.Write(p[:n])

	if errors.Is(err, io.EOF) && hex.EncodeToString(d.h.Sum(nil)) != d.want {
		return n, errBodyDigestMismatch
	}

	return n, err
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

// unboundTokenWarned records the receiver ids warnUnboundTokenOnce has
// already warned about this run.
var unboundTokenWarned sync.Map

// warnUnboundTokenOnce logs, once per receiver per run, that recv accepted
// a token from a sender too old to bind its tokens to their request (see
// remoteAuth.Request), which leaves such tokens replayable for other
// methods, keys and bodies until the sender is upgraded.
func warnUnboundTokenOnce(receiverID, token string, log *slog.Logger) {
	if _, loaded := unboundTokenWarned.LoadOrStore(receiverID, struct{}{}); loaded {
		return
	}

	issuer, _ := remoteAuth.UnverifiedIssuer(token)
	log.Warn("receiver: accepted a request token not bound to its request; upgrade the sending instance",
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
func resolveReceiverRequest(w http.ResponseWriter, r *http.Request, receivers *backup.ReceiverRegistry, db *store.Store, log *slog.Logger) (recv config.ResolvedReceiver, cfg *config.Config, t *config.Target, key string, bound remoteAuth.Request, ok bool) {
	recv, bound, ok = authorizeReceiver(w, r, receivers, log)
	if !ok {
		return config.ResolvedReceiver{}, nil, nil, "", remoteAuth.Request{}, false
	}

	key, err := backup.SanitizeObjectKey(r.PathValue("key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return config.ResolvedReceiver{}, nil, nil, "", remoteAuth.Request{}, false
	}

	if err := checkRequestBinding(r, key, bound); err != nil {
		log.Debug("receiver: rejected request", "id", recv.ID, "key", key, "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return config.ResolvedReceiver{}, nil, nil, "", remoteAuth.Request{}, false
	}

	return recv, &config.Config{Key: key, StateDB: db}, backup.ReceiverTarget(recv), key, bound, true
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
