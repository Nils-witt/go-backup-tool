package peerstatus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// Hub is the central side of the status push: it accepts pushes from
// trusted servers (see RegisterRoutes), keeps the last one of each in its
// status.PeerStore and the state db, and serves them to the web UI (see
// Snapshot). An instance no longer trusted drops out of Snapshot at once,
// and its stored status is deleted at the next startup (see Load). Safe for
// concurrent use.
type Hub struct {
	db      store.Store // nil if the state db couldn't be opened: pushes are then kept in memory only
	peers   *status.PeerStore
	trusted *trust.Registry
	log     *slog.Logger

	// usedTokens are the push tokens already accepted (see
	// remoteauth.ReplayCache).
	usedTokens remoteauth.ReplayCache
}

// NewHub returns a Hub accepting pushes from trusted's servers, keeping
// them in db (nil keeps them in memory only). Call Load before serving.
func NewHub(db store.Store, trusted *trust.Registry, log *slog.Logger) *Hub {
	return &Hub{db: db, peers: status.NewPeerStore(), trusted: trusted, log: log}
}

// Load seeds the hub with every status stored in the state db, deleting
// the ones of servers that are no longer trusted. A stored status that
// doesn't decode is logged and deleted too: the instance's next push
// replaces it anyway.
func (h *Hub) Load(ctx context.Context) {
	if h.db == nil {
		return
	}

	stored, err := h.db.ListPeerStatuses(ctx)
	if err != nil {
		h.log.Error("reading pushed instance statuses from the state db", "err", err)
		return
	}

	for _, ps := range stored {
		if _, ok := h.trusted.Get(ps.ServerID); !ok {
			h.deleteStored(ctx, ps.ServerID, "no longer a trusted server")
			continue
		}

		var p Payload
		if err := json.Unmarshal(ps.Payload, &p); err != nil || p.Version != Version {
			h.deleteStored(ctx, ps.ServerID, "unreadable")
			continue
		}

		h.peers.Upsert(p.report(ps.ServerID, ps.ReceivedAt))
	}
}

// deleteStored deletes serverID's stored status at Load, logging why.
func (h *Hub) deleteStored(ctx context.Context, serverID, why string) {
	if err := h.db.DeletePeerStatus(ctx, serverID); err != nil {
		h.log.Warn("deleting a pushed instance status", "server", serverID, "err", err)
		return
	}

	h.log.Info("deleted a pushed instance status", "server", serverID, "reason", why)
}

// Snapshot returns every trusted instance's last pushed status, as of now.
// A nil hub has none.
func (h *Hub) Snapshot() []status.PeerSnapshot {
	if h == nil {
		return []status.PeerSnapshot{}
	}

	return h.peers.Snapshot(time.Now(), func(id string) (string, bool) {
		srv, ok := h.trusted.Get(id)
		return srv.Name, ok
	})
}

// Changed returns a channel that is closed on the next push or Forget.
func (h *Hub) Changed() <-chan struct{} { return h.peers.Changed() }

// Forget drops instance id's status from the dashboard and the state db,
// reporting false if there was none. It shows up again with its next push,
// unless it's no longer trusted.
func (h *Hub) Forget(ctx context.Context, id string) (bool, error) {
	known := h.peers.Remove(id)

	if h.db != nil {
		if err := h.db.DeletePeerStatus(ctx, id); err != nil {
			return known, err
		}
	}

	return known, nil
}

// ClientAddrFunc returns the address a push came from, for logging a
// rejected one (see receiver.ClientAddrFunc); nil uses the request's raw
// TCP peer address.
type ClientAddrFunc func(*http.Request) string

// RegisterRoutes mounts the status push endpoint, POST
// remoteclient.StatusPath, on mux. Requests failing authentication are
// logged as warnings and answered 401; their volume is bounded by the web
// UI's rate limit on failed authentications, which wraps this mux too.
func (h *Hub) RegisterRoutes(mux *http.ServeMux, clientAddr ClientAddrFunc) {
	mux.HandleFunc("POST "+remoteclient.StatusPath, h.handlePush(clientAddr))
}

// handlePush serves POST remoteclient.StatusPath (see RegisterRoutes).
func (h *Hub) handlePush(clientAddr ClientAddrFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		issuer, bound, err := h.authorize(r)
		if err != nil {
			h.reject(w, r, clientAddr, err)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
				http.Error(w, "status too large", http.StatusRequestEntityTooLarge)
				return
			}

			http.Error(w, "reading body failed", http.StatusBadRequest)

			return
		}

		if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != bound.BodySHA256 {
			h.reject(w, r, clientAddr, errors.New("body does not match the request token's digest"))
			return
		}

		var p Payload
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&p); err != nil {
			http.Error(w, "invalid status payload", http.StatusBadRequest)
			return
		}

		if p.Version != Version {
			http.Error(w, fmt.Sprintf("unsupported status payload version %d, want %d", p.Version, Version), http.StatusBadRequest)
			return
		}

		h.record(r.Context(), issuer, p, body)
		w.WriteHeader(http.StatusNoContent)
	}
}

// authorize verifies r's push token: issued by a trusted server, valid,
// scoped to remoteclient.StatusAudience, bound to a POST of
// remoteclient.StatusKey with a body digest, and not used before. It
// returns the issuer and the request the token is bound to.
func (h *Hub) authorize(r *http.Request) (string, remoteauth.Request, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", remoteauth.Request{}, errors.New("no bearer token")
	}

	issuer, err := remoteauth.UnverifiedIssuer(token)
	if err != nil {
		return "", remoteauth.Request{}, err
	}

	srv, ok := h.trusted.Get(issuer)
	if !ok {
		return "", remoteauth.Request{}, fmt.Errorf("issuer %q is not a trusted server", issuer)
	}

	verified, err := remoteauth.VerifyRemoteAuthToken(token, srv.PublicKey, remoteclient.StatusAudience, issuer)
	if err != nil {
		return "", remoteauth.Request{}, err
	}

	bound := verified.Request

	switch {
	case bound.Method != http.MethodPost || bound.Key != remoteclient.StatusKey || bound.File != "":
		return "", remoteauth.Request{}, fmt.Errorf("token is for %s %q, not a status push", bound.Method, bound.Key)
	case bound.BodySHA256 == "":
		return "", remoteauth.Request{}, errors.New("token binds no body digest")
	case !h.usedTokens.Use(issuer, verified.ID, verified.Expiry, time.Now()):
		return "", remoteauth.Request{}, errors.New("token was already used")
	}

	return issuer, bound, nil
}

// reject answers r with 401, logging why (kept out of the response).
func (h *Hub) reject(w http.ResponseWriter, r *http.Request, clientAddr ClientAddrFunc, reason error) {
	client := r.RemoteAddr
	if clientAddr != nil {
		client = clientAddr(r)
	}

	h.log.Warn("status push: rejected request", "client", client, "err", reason)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// record keeps p, pushed by serverID with body as its encoding, as that
// instance's current status. A state db failure is logged: the push still
// shows on the dashboard, and the next one is stored again.
func (h *Hub) record(ctx context.Context, serverID string, p Payload, body []byte) {
	now := time.Now()
	h.peers.Upsert(p.report(serverID, now))

	if h.db == nil {
		return
	}

	if err := h.db.SavePeerStatus(ctx, store.PeerStatus{ServerID: serverID, ReceivedAt: now, Payload: body}); err != nil {
		h.log.Warn("status push: storing the pushed status failed", "server", serverID, "err", err)
	}
}
