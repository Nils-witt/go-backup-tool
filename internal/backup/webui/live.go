package webui

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"nilswitt.dev/go-backup-tool/internal/backup"
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
	// time-based staleness (see annotateReceiverStaleness) stays current.
	livePingInterval = 30 * time.Second

	// liveWriteTimeout bounds each write or ping, so a stalled client is
	// dropped instead of holding its handler goroutine forever.
	liveWriteTimeout = 10 * time.Second
)

// liveStatusJSON is the one message GET /api/live sends: the same job and
// receiver snapshots /api/status and /api/receivers serve, together.
type liveStatusJSON struct {
	Type      string                    `json:"type"`
	Jobs      []backup.JobSnapshot      `json:"jobs"`
	Receivers []backup.ReceiverSnapshot `json:"receivers"`
}

// liveTicketEntry is one currently valid live-status ticket: who minted it
// (for logging) and when it expires.
type liveTicketEntry struct {
	username string
	expires  time.Time
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

// create mints a new ticket attributed to username, valid for liveTicketTTL.
func (s *liveTicketStore) create(username string) (string, error) {
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

	s.byID[id] = liveTicketEntry{username: username, expires: now.Add(liveTicketTTL)}

	return id, nil
}

// consume redeems ticket id, reporting the username it was minted for and
// ok=true only if it names a currently unexpired ticket. Either way, id is
// no longer valid afterwards.
func (s *liveTicketStore) consume(id string) (username string, ok bool) {
	if id == "" {
		return "", false
	}

	s.mu.Lock()
	e, exists := s.byID[id]
	delete(s.byID, id)
	s.mu.Unlock()

	if !exists || time.Now().After(e.expires) {
		return "", false
	}

	return e.username, true
}

// handleMintLiveTicket serves POST /api/live/ticket: it mints a short-lived,
// single-use ticket for opening GET /api/live, attributed to whoever is
// currently signed in. Gated on view, the same permission /api/status and
// /api/receivers need, since the socket streams exactly what they serve.
func handleMintLiveTicket(tickets *liveTicketStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var username string
		if user, ok := currentUser(r.Context()); ok {
			username = user.Username
		}

		ticket, err := tickets.create(username)
		if err != nil {
			http.Error(w, "minting live status ticket failed", http.StatusInternalServerError)
			return
		}

		writeJSON(w, ticketJSON{Ticket: ticket})
	}
}

// handleLive serves GET /api/live?ticket=...: a WebSocket that pushes a
// liveStatusJSON whenever a job, target or receiver changes state (see
// backup.StatusStore.Changed/ReceiverStatusStore.Changed), plus once on
// connect and every livePingInterval. Clients never send anything; any
// data message from one closes the connection. baseCtx is cancelled by
// Server.Shutdown, which (unlike ordinary requests) http.Server.Shutdown
// neither waits for nor closes once the connection has been hijacked.
// receiverStore may be nil, which streams an empty receiver list. With
// trustProxyHeaders set, the browser's Origin is checked against the Host a
// reverse proxy reports in Forwarded/X-Forwarded-Host (see forwardedHost),
// since a proxy that rewrites Host would otherwise never match it.
func handleLive(baseCtx context.Context, statusStore *backup.StatusStore, receivers *backup.ReceiverRegistry, receiverStore *backup.ReceiverStatusStore, tickets *liveTicketStore, devMode, trustProxyHeaders bool, log *slog.Logger) http.HandlerFunc {
	var opts websocket.AcceptOptions
	if devMode {
		// Matches corsMiddleware: let a frontend dev server on another
		// origin connect too. Never enabled outside webui.dev-mode:.
		opts.OriginPatterns = []string{"*"}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		username, ok := tickets.consume(r.URL.Query().Get("ticket"))
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
			log.Debug("web UI: accepting live status websocket", "user", username, "err", err)
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

		ctx = conn.CloseRead(ctx)

		streamLiveStatus(ctx, conn, statusStore, receivers, receiverStore, log)
	}
}

// streamLiveStatus sends liveStatusJSON messages on conn until ctx ends or a
// write fails (see handleLive).
func streamLiveStatus(ctx context.Context, conn *websocket.Conn, statusStore *backup.StatusStore, receivers *backup.ReceiverRegistry, receiverStore *backup.ReceiverStatusStore, log *slog.Logger) {
	ticker := time.NewTicker(livePingInterval)
	defer ticker.Stop()

	coalesce := time.NewTimer(liveCoalesceDelay)
	coalesce.Stop()

	for {
		// Take the change channels before reading the snapshots, so a
		// change landing in between is never missed — at worst it's sent
		// twice.
		jobsChanged := statusStore.Changed()

		var receiversChanged <-chan struct{}

		msg := liveStatusJSON{Type: "status", Jobs: statusStore.Snapshot(), Receivers: []backup.ReceiverSnapshot{}}

		if receiverStore != nil {
			receiversChanged = receiverStore.Changed()
			msg.Receivers = receiverSnapshots(receivers, receiverStore, log)
		}

		if !writeWithTimeout(ctx, func(ctx context.Context) error { return wsjson.Write(ctx, conn, msg) }) {
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
		case <-jobsChanged:
		case <-receiversChanged:
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
