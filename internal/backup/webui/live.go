package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"nilswitt.dev/go-backup-tool/internal/backup/peerstatus"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

const (
	// liveTicketTTL is how long a minted live-status ticket (see
	// liveTicketStore) stays redeemable, matching downloadTicketTTL's
	// reasoning: the dashboard's JS mints one and connects immediately.
	liveTicketTTL = 60 * time.Second

	// liveCoalesceDelay is how long handleLive waits after a change before
	// sending, so a burst of store updates (a run's Starting, each target's
	// TargetDone, then Finished) goes out as one message rather than many.
	liveCoalesceDelay = 250 * time.Millisecond

	// livePingInterval is how often handleLive pings an otherwise idle
	// client to detect a dead peer, and re-sends the status so receivers'
	// time-based staleness (see receiver.Snapshots) stays current.
	livePingInterval = 30 * time.Second

	// liveWriteTimeout bounds each write or ping, so a stalled client is
	// dropped instead of holding its handler goroutine forever.
	liveWriteTimeout = 10 * time.Second

	// livePayloadMaxAge bounds how long liveStatusCache reuses one encoded
	// message while nothing changes. Every client woken by the same change
	// asks for it within moments of the others (each after its own
	// liveCoalesceDelay), so this covers that burst, while a ping-driven
	// resend, which exists to refresh receivers' time-based staleness,
	// never gets a message more than this old.
	livePayloadMaxAge = time.Second
)

// liveStatusJSON is the one message GET /api/live sends: the same job,
// receiver, and pushed instance snapshots /api/status, /api/receivers, and
// /api/peers serve, together.
type liveStatusJSON struct {
	Type      string                    `json:"type"`
	Jobs      []status.JobSnapshot      `json:"jobs"`
	Receivers []status.ReceiverSnapshot `json:"receivers"`
	Peers     []status.PeerSnapshot     `json:"peers"`
}

// liveChanges are the change channels of the stores a live status message
// is built from: when any is closed, the message is out of date. A nil
// channel (a store the web UI runs without) never fires.
type liveChanges struct {
	jobs, receivers, peers <-chan struct{}
}

// liveStatusCache builds the encoded liveStatusJSON once per change and
// shares it among every client handleLive streams to, rather than each
// client taking its own snapshots (including receivers' staleness checks)
// and marshaling them on every change and ping. receiverStore and peers
// may be nil, which encodes an empty list. Safe for concurrent use.
type liveStatusCache struct {
	statusStore   *status.Store
	receivers     *receiver.Registry
	receiverStore *status.ReceiverStore
	peers         *peerstatus.Hub
	log           *slog.Logger

	mu sync.Mutex
	// built are the change channels payload was built under: while they
	// are still the current ones, nothing has changed since.
	built   liveChanges
	builtAt time.Time
	payload []byte
}

// changes returns the stores' current change channels.
func (c *liveStatusCache) changes() liveChanges {
	ch := liveChanges{jobs: c.statusStore.Changed()}
	if c.receiverStore != nil {
		ch.receivers = c.receiverStore.Changed()
	}

	if c.peers != nil {
		ch.peers = c.peers.Changed()
	}

	return ch
}

// get returns the current encoded message, together with the change
// channels it's current for, which the caller waits on for the next
// change. The payload is shared: callers must not modify it.
func (c *liveStatusCache) get() ([]byte, liveChanges, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Take the change channels before reading the snapshots, so a change
	// landing in between is never missed — at worst it's sent twice.
	changes := c.changes()

	if c.payload != nil && changes == c.built && time.Since(c.builtAt) < livePayloadMaxAge {
		return c.payload, changes, nil
	}

	msg := liveStatusJSON{
		Type: "status", Jobs: c.statusStore.Snapshot(),
		Receivers: []status.ReceiverSnapshot{}, Peers: c.peers.Snapshot(),
	}
	if c.receiverStore != nil {
		msg.Receivers = receiver.Snapshots(c.receivers, c.receiverStore, c.log)
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, liveChanges{}, err
	}

	c.payload, c.built, c.builtAt = payload, changes, time.Now()

	return payload, changes, nil
}

// liveTicketEntry is one currently valid live-status ticket: who minted it
// (for logging, and to close the socket once a back-channel logout ends
// their session) and when it expires.
type liveTicketEntry struct {
	user    principal
	expires time.Time
}

// liveTicketStore tracks currently valid live-status tickets. Like
// downloadTicketStore, tickets exist because a browser can't attach an
// Authorization header to a WebSocket handshake: the dashboard's JS mints a
// ticket with an authenticated fetch() (see handleMintLiveTicket) and then
// opens the socket with it as a query parameter (see handleLive). Safe for
// concurrent use.
type liveTicketStore struct {
	mu   sync.Mutex
	byID map[string]liveTicketEntry
}

// newLiveTicketStore returns an empty liveTicketStore.
func newLiveTicketStore() *liveTicketStore {
	return &liveTicketStore{byID: make(map[string]liveTicketEntry)}
}

// create mints a new ticket attributed to user, valid for liveTicketTTL.
func (s *liveTicketStore) create(user principal) (string, error) {
	id, err := randomTicketID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Drop expired tickets that were minted but never redeemed, so a client
	// that keeps minting without connecting can't grow the map forever.
	now := time.Now()
	for k, e := range s.byID {
		if now.After(e.expires) {
			delete(s.byID, k)
		}
	}

	s.byID[id] = liveTicketEntry{user: user, expires: now.Add(liveTicketTTL)}

	return id, nil
}

// consume redeems ticket id, reporting the user it was minted for and
// ok=true only if it names a currently unexpired ticket. Either way, id is
// no longer valid afterwards.
func (s *liveTicketStore) consume(id string) (user principal, ok bool) {
	if id == "" {
		return principal{}, false
	}

	s.mu.Lock()
	e, exists := s.byID[id]
	delete(s.byID, id)
	s.mu.Unlock()

	if !exists || time.Now().After(e.expires) {
		return principal{}, false
	}

	return e.user, true
}

// handleMintLiveTicket serves POST /api/live/ticket: it mints a short-lived,
// single-use ticket for opening GET /api/live, attributed to whoever is
// currently signed in. Gated on view, the same permission /api/status and
// /api/receivers need, since the socket streams exactly what they serve.
func handleMintLiveTicket(tickets *liveTicketStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var user principal
		if u, ok := currentUser(r.Context()); ok {
			user = *u
		}

		ticket, err := tickets.create(user)
		if err != nil {
			http.Error(w, "minting live status ticket failed", http.StatusInternalServerError)
			return
		}

		writeJSON(w, ticketJSON{Ticket: ticket})
	}
}

// handleLive serves GET /api/live?ticket=...: a WebSocket that pushes
// cache's liveStatusJSON whenever a job, target, receiver, or pushed
// instance changes state (see liveChanges), plus once on connect and every
// livePingInterval. Clients never send anything; any
// data message from one closes the connection. baseCtx is cancelled by
// Server.Shutdown, which (unlike ordinary requests) http.Server.Shutdown
// neither waits for nor closes once the connection has been hijacked.
// With trustProxyHeaders set, the browser's Origin is checked against the Host a
// reverse proxy reports in Forwarded/X-Forwarded-Host (see forwardedHost),
// since a proxy that rewrites Host would otherwise never match it. The
// socket is closed once logouts records a back-channel logout ending the
// session of the user who minted the ticket.
func handleLive(baseCtx context.Context, cache *liveStatusCache, tickets *liveTicketStore, logouts *ssoLogouts, devMode, trustProxyHeaders bool, log *slog.Logger) http.HandlerFunc {
	var opts websocket.AcceptOptions
	if devMode {
		// Matches corsMiddleware: let a frontend dev server on another
		// origin connect too. Never enabled outside webui.dev-mode:.
		opts.OriginPatterns = []string{"*"}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := tickets.consume(r.URL.Query().Get("ticket"))
		if !ok {
			http.Error(w, "missing or expired live status ticket", http.StatusForbidden)
			return
		}

		if trustProxyHeaders {
			if host, ok := forwardedHost(r); ok {
				// websocket.Accept compares Origin against r.Host; give it
				// the client-facing host on a shallow copy.
				r = r.WithContext(r.Context())
				r.Host = host
			}
		}

		conn, err := websocket.Accept(w, r, &opts)
		if err != nil {
			log.Debug("web UI: accepting live status websocket", "user", user.Username, "err", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		stop := context.AfterFunc(baseCtx, func() {
			_ = conn.Close(websocket.StatusGoingAway, "server shutting down")

			cancel()
		})
		defer stop()

		// Like the shutdown hook above, close before cancelling: once ctx
		// ends, CloseRead's reader drops the connection without a close
		// frame.
		sessionCtx := logouts.untilEnded(ctx, user.session)

		stopSession := context.AfterFunc(sessionCtx, func() {
			if errors.Is(context.Cause(sessionCtx), errSSOSessionEnded) {
				_ = conn.Close(websocket.StatusPolicyViolation, "session ended")
			}

			cancel()
		})
		defer stopSession()

		ctx = conn.CloseRead(ctx)

		streamLiveStatus(ctx, conn, cache, log)
	}
}

// streamLiveStatus sends cache's liveStatusJSON messages on conn until ctx
// ends or a write fails (see handleLive).
func streamLiveStatus(ctx context.Context, conn *websocket.Conn, cache *liveStatusCache, log *slog.Logger) {
	ticker := time.NewTicker(livePingInterval)
	defer ticker.Stop()

	coalesce := time.NewTimer(liveCoalesceDelay)
	coalesce.Stop()

	for {
		payload, changes, err := cache.get()
		if err != nil {
			log.Warn("web UI: encoding live status", "err", err)
			return
		}

		if !writeWithTimeout(ctx, func(ctx context.Context) error { return conn.Write(ctx, websocket.MessageText, payload) }) {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !writeWithTimeout(ctx, conn.Ping) {
				return
			}

			continue
		case <-changes.jobs:
		case <-changes.receivers:
		case <-changes.peers:
		}

		coalesce.Reset(liveCoalesceDelay)

		select {
		case <-ctx.Done():
			coalesce.Stop()
			return
		case <-coalesce.C:
		}
	}
}

// writeWithTimeout runs write with a liveWriteTimeout-bounded context,
// reporting whether it succeeded.
func writeWithTimeout(ctx context.Context, write func(context.Context) error) bool {
	ctx, cancel := context.WithTimeout(ctx, liveWriteTimeout)
	defer cancel()

	return write(ctx) == nil
}
