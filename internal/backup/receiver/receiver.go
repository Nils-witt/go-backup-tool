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
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// RegisterRoutes mounts the receiver API's PUT object endpoint on
// mux, for the composition root to combine onto the same mux/port the web
// UI dashboard serves from (see webui.Options.ExtraRoutes). receiverStatus
// is the live receiver status store, shared with whatever also displays it
// (e.g. the dashboard's /api/receivers), so a write here is reflected there
// immediately.
func RegisterRoutes(mux *http.ServeMux, receivers *Registry, receiverStatus *status.ReceiverStore, log *slog.Logger, db store.Store) {
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", HandleReceiveObject(receivers, receiverStatus, log, db))
	mux.HandleFunc("PUT /api/v1/backups/{id}/{key...}", HandleReceiveBackupFile(receivers, receiverStatus, log, db))
	mux.HandleFunc("PUT /api/v1/files/{id}/{path...}", HandleReceiveFile(receivers, receiverStatus, log, db))
}

// HandleReceiveBackupFile serves PUT /api/v1/backups/{id}/{key...}?file=: one
// file (the required file query parameter, a single path segment — see
// config.ValidateFileName) of the multi-file backup key, stored at
// key/file under the receiver's path, alongside the folder's
// objectstore.MultiFileMarker. Otherwise it behaves exactly as
// HandleReceiveObject. It's a route of its own, rather than a parameter on
// that one, so an older receiver that knows nothing of multi-file backups
// answers 404 instead of storing every file of a backup at the same key.
// Only request-bound tokens (see remoteauth.Request) are accepted, since
// every sender that knows this route also binds its tokens.
func HandleReceiveBackupFile(receivers *Registry, receiverStatus *status.ReceiverStore, log *slog.Logger, db store.Store) http.HandlerFunc {
	return receiveObject(receivers, receiverStatus, log, db, true)
}

// HandleReceiveObject serves PUT /api/v1/objects/{id}/{key...}: after
// authorizing the request against receivers (see authorizeReceiver), it
// writes the request body to disk exactly as a type: local target would
// (see objectstore.ReceiverTarget), so a remote target's PUT and this instance's
// own local-target writes share the same on-disk behavior (atomic
// temp-file-then-rename) and retention tracking. It records the write for
// retention (objectstore.RecordObjectWrite) but doesn't sweep for expired objects
// itself — MonitorReceiverRetention sweeps every receiver on its own
// one-minute timer instead, so a PUT's latency isn't paying for a sweep. An
// optional creationTime query parameter (RFC3339, must be strictly before
// the upload time — see parseCreationTime) lets the caller base
// retention on when the backup content was actually produced rather than on
// upload time; when given, it's also stamped as the written file's mtime
// (objectstore.SetObjectModTime) so the dashboard's file listing agrees with it.
// Every attempt is recorded to status, win or lose, so /api/receivers
// reflects it.
func HandleReceiveObject(receivers *Registry, receiverStatus *status.ReceiverStore, log *slog.Logger, db store.Store) http.HandlerFunc {
	return receiveObject(receivers, receiverStatus, log, db, false)
}

// receiveObject is HandleReceiveObject, or HandleReceiveBackupFile when
// multiFile is set. key, as recorded in status and the receiver's event log,
// is the object's path under the receiver: key/file for a backup's file.
func receiveObject(receivers *Registry, receiverStatus *status.ReceiverStore, log *slog.Logger, db store.Store, multiFile bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, cfg, t, bound, ok := resolveReceiverRequest(w, r, receivers, log, multiFile)
		if !ok {
			return
		}

		key := path.Join(cfg.Key, cfg.File)

		createdAt, err := parseCreationTime(r.URL.Query().Get("creationTime"), time.Now())
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

		if err := objectstore.WriteLocalObject(cfg, t, body); err != nil {
			log.Warn("receiver: writing object failed", "id", recv.ID, "key", key, "err", err)
			receiverStatus.Record(recv.ID, key, err)
			recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, store.ReceiverEventReceive, key, 0, err)

			if errors.Is(err, errBodyDigestMismatch) {
				http.Error(w, errBodyDigestMismatch.Error(), http.StatusBadRequest)
				return
			}

			http.Error(w, "writing object failed", http.StatusInternalServerError)

			return
		}

		if !cfg.CreatedAt.IsZero() {
			if err := objectstore.SetObjectModTime(cfg, t, cfg.CreatedAt); err != nil {
				log.Warn("receiver: setting object mod time failed", "id", recv.ID, "key", key, "err", err)
			}
		}

		if err := objectstore.RecordObjectWrite(r.Context(), db, cfg, t, log); err != nil {
			log.Warn("receiver: retention tracking failed", "id", recv.ID, "key", key, "err", err)
		}

		receiverStatus.Record(recv.ID, key, nil)

		var size int64
		if info, statErr := os.Stat(objectstore.LocalObjectPath(cfg, t)); statErr == nil { //nolint:gosec // cfg.Key/cfg.File are resolveReceiverRequest's already-validated key and file, not raw untrusted input
			size = info.Size()
		}

		recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, store.ReceiverEventReceive, key, size, nil)

		log.Info("receiver: object written", "id", recv.ID, "key", key, "path", objectstore.LocalObjectPath(cfg, t))
		w.WriteHeader(http.StatusCreated)
	}
}

// authorizeReceiver looks up the receiver named by the request's {id} path
// value and verifies its Authorization: Bearer <token> header (see
// verifyReceiverToken), writing an error response and returning ok=false if
// either the id is unknown or the token doesn't verify. bound is the
// request the token is bound to (see remoteauth.Request), for the caller
// to check against r once it has sanitized the key.
func authorizeReceiver(w http.ResponseWriter, r *http.Request, receivers *Registry, log *slog.Logger) (recv config.ResolvedReceiver, bound remoteauth.Request, ok bool) {
	recv, exists := receivers.Get(r.PathValue("id"))
	if !exists {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
		return config.ResolvedReceiver{}, remoteauth.Request{}, false
	}

	token, hasToken := bearerToken(r)
	if !hasToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return config.ResolvedReceiver{}, remoteauth.Request{}, false
	}

	bound, err := verifyReceiverToken(recv, token)
	if err != nil {
		log.Debug("receiver: rejected request", "id", recv.ID, "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return config.ResolvedReceiver{}, remoteauth.Request{}, false
	}

	if bound.IsZero() {
		warnUnboundTokenOnce(recv.ID, token, log)
	}

	return recv, bound, true
}

// verifyReceiverToken verifies token as a request token (see
// remoteauth.SignRemoteAuthToken) for recv: its issuer must be one of recv's
// allowed servers, and it must verify against that server's current key.
// It returns the request the token is bound to (see remoteauth.Request).
func verifyReceiverToken(recv config.ResolvedReceiver, token string) (remoteauth.Request, error) {
	issuer, err := remoteauth.UnverifiedIssuer(token)
	if err != nil {
		return remoteauth.Request{}, err
	}

	if !slices.Contains(recv.AllowedServers, issuer) {
		return remoteauth.Request{}, fmt.Errorf("issuer %q is not an allowed server", issuer)
	}

	srv, ok := recv.TrustedServers.Get(issuer)
	if !ok {
		return remoteauth.Request{}, fmt.Errorf("allowed server %q is no longer trusted", issuer)
	}

	return remoteauth.VerifyRemoteAuthToken(token, srv.PublicKey, recv.ID, issuer)
}

// errBodyDigestMismatch reports that a PUT's body isn't the one its request
// token was signed for (see remoteauth.Request.BodySHA256).
var errBodyDigestMismatch = errors.New("body does not match the request token's digest")

// checkRequestBinding reports whether r, whose sanitized key and file are
// key and file, is the request bound names: same method, key, file and
// creationTime, and — for a PUT — a body digest to verify while it streams
// (see digestReader). A zero bound (an older sender's unbound token) is
// accepted as before.
func checkRequestBinding(r *http.Request, key, file string, bound remoteauth.Request) error {
	if bound.IsZero() {
		return nil
	}

	switch {
	case bound.Method != r.Method:
		return fmt.Errorf("token is for method %q, not %q", bound.Method, r.Method)
	case bound.Key != key:
		return fmt.Errorf("token is for key %q, not %q", bound.Key, key)
	case bound.File != file:
		return fmt.Errorf("token is for file %q, not %q", bound.File, file)
	case bound.CreationTime != r.URL.Query().Get("creationTime"):
		return errors.New("token is for a different creationTime")
	case r.Method == http.MethodPut && bound.BodySHA256 == "":
		return errors.New("token has no body digest for a PUT")
	case bound.PlainSHA256 != "":
		// Minted for a directory receiver's file upload (see
		// HandleReceiveFile), never for a backup's object.
		return errors.New("token is for a file upload")
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

// unboundTokenWarned records the receiver ids warnUnboundTokenOnce has
// already warned about this run.
var unboundTokenWarned sync.Map

// warnUnboundTokenOnce logs, once per receiver per run, that recv accepted
// a token from a sender too old to bind its tokens to their request (see
// remoteauth.Request), which leaves such tokens replayable for other
// methods, keys and bodies until the sender is upgraded.
func warnUnboundTokenOnce(receiverID, token string, log *slog.Logger) {
	if _, loaded := unboundTokenWarned.LoadOrStore(receiverID, struct{}{}); loaded {
		return
	}

	issuer, _ := remoteauth.UnverifiedIssuer(token)
	log.Warn("receiver: accepted a request token not bound to its request; upgrade the sending instance",
		"id", receiverID, "sender_server_id", issuer)
}

// resolveReceiverRequest authorizes r against receivers (see
// authorizeReceiver) and sanitizes its {key} path value (see
// objectstore.SanitizeObjectKey) and — when multiFile — its required file query
// parameter (see config.ValidateFileName), writing an error response and
// reporting ok as false if any step fails. On success it also builds the
// cfg/t pair receiveObject needs to reuse WriteLocalObject as a type: local
// target would (see objectstore.ReceiverTarget).
func resolveReceiverRequest(w http.ResponseWriter, r *http.Request, receivers *Registry, log *slog.Logger, multiFile bool) (recv config.ResolvedReceiver, cfg *config.Config, t *config.Target, bound remoteauth.Request, ok bool) {
	recv, bound, ok = authorizeReceiver(w, r, receivers, log)
	if !ok {
		return config.ResolvedReceiver{}, nil, nil, remoteauth.Request{}, false
	}

	if recv.Kind == config.ReceiverKindDirectory {
		http.Error(w, fmt.Sprintf("receiver %q is a directory receiver: it accepts files from on-change jobs, not backups", recv.ID), http.StatusConflict)
		return config.ResolvedReceiver{}, nil, nil, remoteauth.Request{}, false
	}

	key, err := objectstore.SanitizeObjectKey(r.PathValue("key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return config.ResolvedReceiver{}, nil, nil, remoteauth.Request{}, false
	}

	var file string

	if multiFile {
		file = r.URL.Query().Get("file")
		if err := config.ValidateFileName(file); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return config.ResolvedReceiver{}, nil, nil, remoteauth.Request{}, false
		}
	}

	bindErr := checkRequestBinding(r, key, file, bound)
	if bindErr == nil && multiFile && bound.IsZero() {
		bindErr = errors.New("token is not bound to its request")
	}

	if bindErr != nil {
		log.Debug("receiver: rejected request", "id", recv.ID, "key", key, "file", file, "err", bindErr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return config.ResolvedReceiver{}, nil, nil, remoteauth.Request{}, false
	}

	return recv, &config.Config{Key: key, File: file}, objectstore.ReceiverTarget(recv), bound, true
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
