package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// downloadTicketTTL is how long a minted download ticket (see
// downloadTicketStore) stays redeemable: long enough for the dashboard's JS
// to mint one and immediately navigate the browser to it, short enough that
// a ticket leaking into a server log or browser history is useless almost
// immediately.
const downloadTicketTTL = 60 * time.Second

// downloadTicketEntry is one currently valid download ticket (see
// downloadTicketStore): the receiver/key it authorizes one download of, the
// identity that minted it (for the download log, since the download request
// itself carries no Authorization header to attribute it from — see
// handleDownloadFile), and its expiry.
type downloadTicketEntry struct {
	receiverID string
	key        string
	username   string
	expires    time.Time
}

// downloadTicketStore tracks currently valid download tickets, mapping each
// ticket id to its downloadTicketEntry. Tickets exist so a file download can
// stay a plain browser navigation — letting the browser handle the save
// itself, rather than the dashboard's JavaScript buffering the whole file in
// memory as a Blob — even though bearer-token auth can't ride along on one:
// the dashboard's JS mints a ticket with an authenticated fetch() (see
// handleMintDownloadTicket) and only then navigates to the download URL with
// it attached as a query parameter (see handleDownloadFile). Safe for
// concurrent use.
type downloadTicketStore struct {
	mu   sync.Mutex
	byID map[string]downloadTicketEntry
}

// newDownloadTicketStore returns an empty downloadTicketStore.
func newDownloadTicketStore() *downloadTicketStore {
	return &downloadTicketStore{byID: make(map[string]downloadTicketEntry)}
}

// create mints a new ticket authorizing one download of receiverID/key,
// attributed to username (best-effort, may be empty), valid for
// downloadTicketTTL.
func (s *downloadTicketStore) create(receiverID, key, username string) (string, error) {
	id, err := randomTicketID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.byID[id] = downloadTicketEntry{receiverID: receiverID, key: key, username: username, expires: time.Now().Add(downloadTicketTTL)}
	s.mu.Unlock()

	return id, nil
}

// consume redeems ticket id for receiverID/key, reporting the username it
// was minted for and ok=true only if id names a currently unexpired ticket
// that was minted for exactly this receiver/key. Either way, id is no
// longer valid afterwards — a ticket authorizes exactly one download,
// successful or not, so it can't be replayed from a server log or browser
// history.
func (s *downloadTicketStore) consume(id, receiverID, key string) (username string, ok bool) {
	if id == "" {
		return "", false
	}

	s.mu.Lock()
	e, exists := s.byID[id]
	delete(s.byID, id)
	s.mu.Unlock()

	if !exists || time.Now().After(e.expires) || e.receiverID != receiverID || e.key != key {
		return "", false
	}

	return e.username, true
}

// randomTicketID returns a 256-bit random value hex-encoded, unguessable
// enough to serve as a download ticket id (see downloadTicketStore.create).
func randomTicketID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

// ticketJSON is a freshly minted download or live status ticket's wire
// shape (see handleMintDownloadTicket and handleMintLiveTicket).
type ticketJSON struct {
	Ticket string `json:"ticket"`
}

// handleMintDownloadTicket serves POST /api/receivers/{id}/download/{key...}:
// it mints a short-lived, single-use download ticket (see
// downloadTicketStore) for the receiver/key named by the path, attributed to
// whoever is currently signed in (see currentUser). The
// dashboard's JS calls this — with its Authorization: Bearer header — right
// before navigating the browser to the matching GET, which can't carry that
// header itself (see handleDownloadFile).
func handleMintDownloadTicket(receivers *receiver.Registry, tickets *downloadTicketStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := lookupReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := objectstore.SanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var username string
		if user, ok := currentUser(r.Context()); ok {
			username = user.Username
		}

		ticket, err := tickets.create(recv.ID, key, username)
		if err != nil {
			http.Error(w, "minting download ticket failed", http.StatusInternalServerError)
			return
		}

		writeJSON(w, ticketJSON{Ticket: ticket})
	}
}

// handleDownloadFile serves GET /api/receivers/{id}/download/{key...}: the
// actual content of one object currently stored under receiver {id}'s path,
// for a person to save from the dashboard's file listing (see
// listReceiverFiles/handleReceiverFiles for the metadata-only listing this
// complements). Unlike the receiver API's own per-receiver JWT auth (see
// authorizeReceiver), and unlike every other dashboard endpoint (see
// requireUser), this is authorized by a one-time download ticket
// (see downloadTicketStore) rather than a bearer token: the request behind
// this is a plain browser navigation, which can't carry an Authorization
// header the way the dashboard's own fetch() calls can (see
// handleMintDownloadTicket, which the dashboard's JS calls first to obtain
// one). db, when non-nil, gets every attempt appended to the download log
// (see recordDownloadEvent), win or lose, for the dashboard's "Download log"
// section (see handleDownloadEvents); a write failure there is only logged,
// not surfaced to the browser, mirroring recordLogin's own tolerance for a
// login log write failure.
func handleDownloadFile(receivers *receiver.Registry, log *slog.Logger, db store.Store, tickets *downloadTicketStore, trustProxyHeaders bool, queue *notify.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := lookupReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := objectstore.SanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		username, ok := tickets.consume(r.URL.Query().Get("ticket"), recv.ID, key)
		if !ok {
			http.Error(w, "missing or expired download ticket", http.StatusForbidden)
			return
		}

		record := func(success bool, detail string) {
			if db == nil {
				return
			}

			ev := store.DownloadEvent{At: time.Now(), Username: username, ReceiverID: recv.ID, Key: key, Success: success, RemoteAddr: clientAddr(r, trustProxyHeaders), Detail: detail}
			// WithoutCancel: a client that aborted mid-download has
			// already canceled r's context, but that attempt still
			// belongs in the log.
			if err := db.SaveDownloadEvent(context.WithoutCancel(r.Context()), ev); err != nil {
				log.Warn("download: recording download event failed", "err", err)
			}
		}

		path := filepath.Join(recv.Path, filepath.FromSlash(key))

		f, err := os.Open(path) //nolint:gosec // key is sanitized by SanitizeObjectKey and joined under recv.Path, not attacker-controlled beyond that
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "not found", http.StatusNotFound)
				record(false, "not found")
			} else {
				log.Warn("download: opening file failed", "id", recv.ID, "key", key, "err", err)
				http.Error(w, "opening file failed", http.StatusInternalServerError)
				record(false, "opening file failed")
			}

			return
		}
		defer func() { _ = f.Close() }()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", attachmentDisposition(filepath.Base(key)))

		if info, err := f.Stat(); err == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		}

		// Recorded (and the webhook fired) only once the whole file has
		// actually been sent, so an aborted transfer isn't logged as a
		// successful download.
		if _, err := io.Copy(w, f); err != nil {
			log.Warn("download: streaming file failed", "id", recv.ID, "key", key, "err", err)
			record(false, "streaming file failed")

			return
		}

		record(true, "")

		go receiver.NotifyDownload(recv, receiver.DownloadWebhookEvent{Username: username, Key: key, At: time.Now()}, queue, log)
	}
}

// attachmentDisposition builds a Content-Disposition: attachment header
// value for filename, quoted/encoded as RFC 6266 requires (a bare
// filename="..." breaks on quotes and non-ASCII names).
func attachmentDisposition(filename string) string {
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": filename}); v != "" {
		return v
	}

	return "attachment"
}
