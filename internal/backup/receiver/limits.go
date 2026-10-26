package receiver

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
)

// uploadLimit is how many bytes one upload to a receiver may have, and how
// a body over it is answered: 413 when the receiver's max-object-size: is
// the binding limit, 507 when its quota: is.
type uploadLimit struct {
	bytes  int64
	status int
	reason string
}

// receiverUploadLimit works out the uploadLimit for one upload to recv:
// its MaxObjectSize, lowered to what's left of its Quota, if any, when that
// is less. replacing is the object the upload overwrites, if any, whose
// current size counts as free since it's about to be replaced; "" for none.
func receiverUploadLimit(recv config.ResolvedReceiver, replacing string) (uploadLimit, error) {
	maxSize := recv.MaxObjectSize
	if maxSize <= 0 {
		maxSize = config.DefaultReceiverMaxObjectSize
	}

	limit := uploadLimit{
		bytes:  maxSize,
		status: http.StatusRequestEntityTooLarge,
		reason: fmt.Sprintf("upload exceeds the receiver's max-object-size of %d bytes", maxSize),
	}

	if recv.Quota <= 0 {
		return limit, nil
	}

	used, err := objectstore.DirSize(recv.Path)
	if err != nil {
		return uploadLimit{}, err
	}

	if replacing != "" {
		if info, err := os.Stat(replacing); err == nil && info.Mode().IsRegular() { //nolint:gosec // replacing is objectstore.LocalObjectPath of a key SanitizeObjectKey already validated, under recv.Path
			used -= info.Size()
		}
	}

	if free := max(recv.Quota-used, 0); free < limit.bytes {
		limit = uploadLimit{
			bytes:  free,
			status: http.StatusInsufficientStorage,
			reason: fmt.Sprintf("upload exceeds the receiver's quota of %d bytes (%d bytes free)", recv.Quota, free),
		}
	}

	return limit, nil
}

// limitUploadBody checks r's upload to recv against receiverUploadLimit
// (see there for replacing) and returns its body capped at that limit.
// An upload whose Content-Length is already over it is answered right
// away, as is a failure to measure the receiver's usage, and ok is false.
// A body without a Content-Length that turns out too large fails its
// read with an *http.MaxBytesError, which the caller answers via
// writeUploadLimitError.
func limitUploadBody(w http.ResponseWriter, r *http.Request, recv config.ResolvedReceiver, replacing string, log *slog.Logger) (io.Reader, uploadLimit, bool) {
	limit, err := receiverUploadLimit(recv, replacing)
	if err != nil {
		log.Warn("receiver: measuring quota usage failed", "id", recv.ID, "err", err)
		http.Error(w, "checking the receiver's quota failed", http.StatusInternalServerError)

		return nil, uploadLimit{}, false
	}

	if r.ContentLength > limit.bytes || (limit.bytes == 0 && r.ContentLength != 0) {
		log.Warn("receiver: rejected upload over its limit", "id", recv.ID, "content_length", r.ContentLength, "limit", limit.bytes)
		http.Error(w, limit.reason, limit.status)

		return nil, uploadLimit{}, false
	}

	return http.MaxBytesReader(w, r.Body, limit.bytes), limit, true
}

// writeUploadLimitError answers err with limit's status and reports true
// if err is (or wraps) the *http.MaxBytesError a body over limit fails
// with; otherwise it writes nothing and reports false.
func writeUploadLimitError(w http.ResponseWriter, err error, limit uploadLimit) bool {
	if _, ok := errors.AsType[*http.MaxBytesError](err); !ok {
		return false
	}

	http.Error(w, limit.reason, limit.status)

	return true
}
