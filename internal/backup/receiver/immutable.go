package receiver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// errImmutableObjectExists reports an upload to an immutable receiver whose
// key already holds a different object.
var errImmutableObjectExists = errors.New("an object with this key already exists on this immutable receiver")

// answerExistingObject handles an upload to an immutable receiver (see
// config.FileReceiver.Immutable) whose destination dst may already exist,
// reporting whether it answered the request. A missing dst isn't answered:
// the caller goes on to store the upload. An existing dst whose SHA-256 is
// want — the digest the request token binds the body to — is the same
// upload sent again (e.g. a retry after a lost response), answered 200
// without reading or rewriting anything; any other existing dst is
// answered 409 and recorded as a failed receive in the receiver's event
// log.
func answerExistingObject(ctx context.Context, w http.ResponseWriter, recv config.ResolvedReceiver, dst, key, want string, log *slog.Logger, db store.Store) bool {
	got, err := fileSHA256(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}

	if err != nil {
		log.Warn("receiver: hashing existing object failed", "id", recv.ID, "key", key, "err", err)
		http.Error(w, "checking the existing object failed", http.StatusInternalServerError)

		return true
	}

	if got == want {
		log.Info("receiver: identical object already stored", "id", recv.ID, "key", key)
		w.WriteHeader(http.StatusOK)

		return true
	}

	log.Warn("receiver: rejected overwrite on an immutable receiver", "id", recv.ID, "key", key)
	recordReceiverEventBestEffort(ctx, db, log, recv.ID, store.ReceiverEventReceive, key, 0, errImmutableObjectExists)
	http.Error(w, errImmutableObjectExists.Error(), http.StatusConflict)

	return true
}

// fileSHA256 returns the hex SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path is objectstore.LocalObjectPath of a key SanitizeObjectKey already validated, under the receiver's path
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %q: %w", path, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
