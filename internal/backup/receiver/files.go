package receiver

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// fileUploadResponse is HandleReceiveFile's JSON reply, read by
// remoteclient.UploadFile.
type fileUploadResponse struct {
	Stored  bool `json:"stored"`
	Version int  `json:"version"`
}

// HandleReceiveFile serves PUT /api/v1/files/{id}/{path...}?sha256=: one
// encrypted file of an on-change job, sent to a directory receiver
// (config.ReceiverKindDirectory) as path, relative to the job's source.
// sha256 is the hex SHA-256 of the file's plaintext: when path's newest
// version already has it, nothing new is stored and the reply is 200;
// otherwise the body becomes path's next version (see
// objectstore.WriteFileVersion) and the reply is 201. Either way the body
// is checked against its token's digest. Only tokens bound to this very
// request — method, path, and both digests — are accepted. A backup
// receiver answers 409.
func HandleReceiveFile(receivers *Registry, receiverStatus *status.ReceiverStore, log *slog.Logger, db store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, rel, bound, ok := resolveFileRequest(w, r, receivers, log)
		if !ok {
			return
		}

		body := &countingReader{r: &digestReader{r: r.Body, h: sha256.New(), want: bound.BodySHA256}}

		version, stored, err := objectstore.WriteFileVersion(recv.Path, rel, bound.PlainSHA256, body)
		if err != nil {
			log.Warn("receiver: writing file version failed", "id", recv.ID, "path", rel, "err", err)
			receiverStatus.Record(recv.ID, rel, err)
			recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, store.ReceiverEventReceive, rel, 0, err)

			if errors.Is(err, errBodyDigestMismatch) {
				http.Error(w, errBodyDigestMismatch.Error(), http.StatusBadRequest)
				return
			}

			http.Error(w, "writing file failed", http.StatusInternalServerError)

			return
		}

		receiverStatus.Record(recv.ID, rel, nil)

		code := http.StatusOK

		if stored {
			code = http.StatusCreated

			recordReceiverEventBestEffort(r.Context(), db, log, recv.ID, store.ReceiverEventReceive, rel, body.n, nil)
			log.Info("receiver: file version written", "id", recv.ID, "path", rel, "version", version)
		} else {
			log.Debug("receiver: file unchanged", "id", recv.ID, "path", rel, "version", version)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)

		if err := json.NewEncoder(w).Encode(fileUploadResponse{Stored: stored, Version: version}); err != nil {
			log.Debug("receiver: writing response failed", "id", recv.ID, "err", err)
		}
	}
}

// resolveFileRequest authorizes r (see authorizeReceiver) as a file upload
// to a directory receiver and validates its path (see
// objectstore.ValidateFilePath), writing an error response and reporting
// ok as false if any step fails.
func resolveFileRequest(w http.ResponseWriter, r *http.Request, receivers *Registry, log *slog.Logger) (recv config.ResolvedReceiver, rel string, bound remoteauth.Request, ok bool) {
	recv, bound, ok = authorizeReceiver(w, r, receivers, log)
	if !ok {
		return config.ResolvedReceiver{}, "", remoteauth.Request{}, false
	}

	if recv.Kind != config.ReceiverKindDirectory {
		http.Error(w, fmt.Sprintf("receiver %q is a backup receiver: it accepts backups, not files from on-change jobs", recv.ID), http.StatusConflict)
		return config.ResolvedReceiver{}, "", remoteauth.Request{}, false
	}

	rel = r.PathValue("path")
	if err := objectstore.ValidateFilePath(rel); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return config.ResolvedReceiver{}, "", remoteauth.Request{}, false
	}

	if err := checkFileBinding(r, rel, bound); err != nil {
		log.Debug("receiver: rejected request", "id", recv.ID, "path", rel, "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return config.ResolvedReceiver{}, "", remoteauth.Request{}, false
	}

	return recv, rel, bound, true
}

// checkFileBinding reports whether r, a file upload of rel, is the request
// bound names: a PUT of rel with the plaintext digest in its sha256 query
// value, and a body digest to verify while it streams.
func checkFileBinding(r *http.Request, rel string, bound remoteauth.Request) error {
	switch {
	case bound.IsZero():
		return errors.New("token is not bound to its request")
	case bound.Method != r.Method:
		return fmt.Errorf("token is for method %q, not %q", bound.Method, r.Method)
	case bound.Key != rel:
		return fmt.Errorf("token is for path %q, not %q", bound.Key, rel)
	case bound.File != "" || bound.CreationTime != "":
		return errors.New("token is for a backup's object, not a file upload")
	case bound.BodySHA256 == "":
		return errors.New("token has no body digest for a PUT")
	case bound.PlainSHA256 == "" || bound.PlainSHA256 != r.URL.Query().Get("sha256"):
		return errors.New("token is for a different content hash")
	}

	return nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	return n, err
}
