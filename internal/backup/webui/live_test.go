package webui

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// startLiveWebUI starts an SSO-enabled web UI with one job ("test") and one
// receiver ("a"), returning the stores so tests can drive state changes.
// trustProxyHeaders is passed through as webui.trust-proxy-headers.
func startLiveWebUI(t *testing.T, idp *testIDP, trustProxyHeaders bool) (*Server, *backup.StatusStore, *backup.ReceiverStatusStore) {
	t.Helper()

	statusStore, _ := newTestStore()
	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}}
	receiverStore := backup.NewReceiverStatusStore(receivers)

	srv := StartWebUI("127.0.0.1:0", statusStore, nil, nil, nil, backup.NewReceiverRegistry(receivers), receiverStore, nil, nil, nil, discardLogger, openTestStateDB(t), nil, idp.settings(), nil, trustProxyHeaders, false, 0, "", nil, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	return srv, statusStore, receiverStore
}

// mintLiveTicket mints a live status ticket with token, failing the test
// unless that succeeds.
func mintLiveTicket(t *testing.T, srv *Server, token string) string {
	t.Helper()

	var out ticketJSON
	if code := doJSON(t, srv, http.MethodPost, "/api/live/ticket", token, &out); code != http.StatusOK {
		t.Fatalf("POST /api/live/ticket = %d, want 200", code)
	}

	return out.Ticket
}

// dialLive opens GET /api/live with ticket and any extra handshake header,
// returning the connection and the handshake's HTTP status code (for checks
// when dialing fails).
func dialLive(t *testing.T, srv *Server, ticket string, header http.Header) (*websocket.Conn, int, error) {
	t.Helper()

	var status int

	conn, resp, err := websocket.Dial(t.Context(), "ws://"+srv.addr+"/api/live?ticket="+ticket, &websocket.DialOptions{HTTPHeader: header})
	if resp != nil {
		status = resp.StatusCode

		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}

	if conn != nil {
		t.Cleanup(func() { _ = conn.CloseNow() })
	}

	return conn, status, err
}

// readLive reads the next live status message from conn, failing the test
// if none arrives within a few seconds.
func readLive(t *testing.T, conn *websocket.Conn) liveStatusJSON {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var msg liveStatusJSON
	if err := wsjson.Read(ctx, conn, &msg); err != nil {
		t.Fatalf("reading live status: %v", err)
	}

	return msg
}

func TestLiveStatusStreamsChanges(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, statusStore, receiverStore := startLiveWebUI(t, idp, false)

	conn, _, err := dialLive(t, srv, mintLiveTicket(t, srv, idp.token(t, "viewer", nil)), nil)
	if err != nil {
		t.Fatalf("dialing /api/live: %v", err)
	}

	msg := readLive(t, conn)
	if msg.Type != "status" || len(msg.Jobs) != 1 || msg.Jobs[0].State != backup.StateIdle || len(msg.Receivers) != 1 {
		t.Fatalf("initial message = %+v, want one idle job and one receiver", msg)
	}

	statusStore.Starting("test")

	if msg = readLive(t, conn); msg.Jobs[0].State != backup.StateRunning || msg.Jobs[0].Targets[0].State != backup.StateRunning {
		t.Errorf("after Starting, jobs = %+v, want the job and its targets running", msg.Jobs)
	}

	receiverStore.Record("a", "backup.gpg", nil)

	if msg = readLive(t, conn); msg.Receivers[0].State != backup.StateOK || msg.Receivers[0].LastKey != "backup.gpg" {
		t.Errorf("after Record, receivers = %+v, want a ok with last key backup.gpg", msg.Receivers)
	}
}

func TestLiveStatusTicketChecks(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, _, _ := startLiveWebUI(t, idp, false)

	if code := doJSON(t, srv, http.MethodPost, "/api/live/ticket", "", nil); code != http.StatusUnauthorized {
		t.Errorf("minting a ticket without a token = %d, want 401", code)
	}

	for name, ticket := range map[string]string{"missing": "", "unknown": "not-a-ticket"} {
		if _, status, err := dialLive(t, srv, ticket, nil); err == nil || status != http.StatusForbidden {
			t.Errorf("%s ticket: dial err = %v, want a 403 rejection", name, err)
		}
	}

	ticket := mintLiveTicket(t, srv, idp.token(t, "viewer", nil))
	if _, _, err := dialLive(t, srv, ticket, nil); err != nil {
		t.Fatalf("first use of ticket: %v", err)
	}

	if _, status, err := dialLive(t, srv, ticket, nil); err == nil || status != http.StatusForbidden {
		t.Errorf("reused ticket: dial err = %v, want a 403 rejection", err)
	}
}

func TestLiveStatusClosedOnShutdown(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, _, _ := startLiveWebUI(t, idp, false)

	conn, _, err := dialLive(t, srv, mintLiveTicket(t, srv, idp.token(t, "viewer", nil)), nil)
	if err != nil {
		t.Fatalf("dialing /api/live: %v", err)
	}

	readLive(t, conn)

	go srv.Shutdown()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, _, err = conn.Read(ctx)
	if status := websocket.CloseStatus(err); status != websocket.StatusGoingAway {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("socket still open 5s after Shutdown")
		}

		t.Errorf("close status after Shutdown = %v (err %v), want StatusGoingAway", status, err)
	}
}

func TestLiveStatusOriginBehindProxy(t *testing.T) {
	t.Parallel()

	// The browser's Origin names the public host, while the proxy in front
	// rewrote Host to this instance's own listen address.
	const origin = "https://backup.example.com"

	tests := []struct {
		name              string
		trustProxyHeaders bool
		header            http.Header
		wantOK            bool
	}{
		{
			name:   "forwarded host ignored without trust-proxy-headers",
			header: http.Header{"X-Forwarded-Host": {"backup.example.com"}},
		},
		{
			name:              "x-forwarded-host matches origin",
			trustProxyHeaders: true,
			header:            http.Header{"X-Forwarded-Host": {"backup.example.com"}},
			wantOK:            true,
		},
		{
			name:              "forwarded host= matches origin",
			trustProxyHeaders: true,
			header:            http.Header{"Forwarded": {"for=203.0.113.9;host=backup.example.com;proto=https"}},
			wantOK:            true,
		},
		{
			name:              "forwarded host differs from origin",
			trustProxyHeaders: true,
			header:            http.Header{"X-Forwarded-Host": {"other.example.com"}},
		},
		{
			name:              "no forwarded host",
			trustProxyHeaders: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			idp := newTestIDP(t)
			srv, _, _ := startLiveWebUI(t, idp, tt.trustProxyHeaders)

			header := tt.header.Clone()
			if header == nil {
				header = http.Header{}
			}

			header.Set("Origin", origin)

			_, status, err := dialLive(t, srv, mintLiveTicket(t, srv, idp.token(t, "viewer", nil)), header)
			if tt.wantOK && err != nil {
				t.Fatalf("dialing /api/live: %v", err)
			}

			if !tt.wantOK && (err == nil || status != http.StatusForbidden) {
				t.Fatalf("dialing /api/live = (%d, %v), want 403", status, err)
			}
		})
	}
}
