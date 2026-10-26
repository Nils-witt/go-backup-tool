package webui

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/peerstatus"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// startPeersWebUI starts an SSO-enabled web UI that accepts status pushes
// from the returned sender, mounted the way the composition root mounts
// them (see webui.Options.ExtraRoutes).
func startPeersWebUI(t *testing.T, idp *testIDP) (*Server, *identity.ServerIdentity) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	sender := identity.NewTestServerIdentity(uuid.NewString(), key)
	trusted := trust.NewRegistry(map[string]trust.Server{
		sender.UUID(): {ID: sender.UUID(), Name: "branch-office", PublicKey: &key.PublicKey},
	})

	db := openTestStateDB(t)
	hub := peerstatus.NewHub(db, trusted, discardLogger)
	statusStore, _ := newTestStore()

	srv := StartWebUI(Deps{
		Log:    discardLogger,
		DB:     db,
		Status: statusStore,
		Peers:  hub,
	}, Options{
		Addr:        "127.0.0.1:0",
		OIDC:        idp.settings(),
		ExtraRoutes: func(mux *http.ServeMux) { hub.RegisterRoutes(mux, nil) },
	})
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	return srv, sender
}

// pushStatus pushes a status with one job, "remote-db", from sender to
// srv.
func pushStatus(t *testing.T, srv *Server, sender *identity.ServerIdentity) {
	t.Helper()

	body, err := json.Marshal(peerstatus.Payload{
		Version: peerstatus.Version, InstanceName: "branch", IntervalSeconds: 30, SentAt: time.Now(),
		Jobs:      []status.JobSnapshot{{Name: "remote-db", State: status.StateOK, Targets: []status.TargetSnapshot{}}},
		Receivers: []status.ReceiverSnapshot{},
	})
	if err != nil {
		t.Fatal(err)
	}

	target := &config.Target{ServerName: "central", Kind: config.ServerKindRemote, Endpoint: "http://" + srv.addr}
	if err := remoteclient.PostStatus(t.Context(), sender, target, body); err != nil {
		t.Fatalf("PostStatus() error: %v", err)
	}
}

func TestPeersAPI(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, sender := startPeersWebUI(t, idp)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	pushStatus(t, srv, sender)

	if code := doJSON(t, srv, http.MethodGet, "/api/peers", "", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /api/peers = %d, want 401", code)
	}

	var peers []status.PeerSnapshot
	if code := doJSON(t, srv, http.MethodGet, "/api/peers", viewer, &peers); code != http.StatusOK {
		t.Fatalf("viewer GET /api/peers = %d, want 200", code)
	}

	if len(peers) != 1 || peers[0].ID != sender.UUID() || peers[0].Name != "branch-office" ||
		len(peers[0].Jobs) != 1 || peers[0].Jobs[0].Name != "remote-db" {
		t.Fatalf("GET /api/peers = %+v, want branch-office with job remote-db", peers)
	}

	path := "/api/peers/" + sender.UUID()

	for _, c := range []receiverConfigCall{
		{"non-admin forget", http.MethodDelete, path, viewer, nil, http.StatusForbidden},
		{"admin forget", http.MethodDelete, path, admin, nil, http.StatusNoContent},
		{"forget unknown", http.MethodDelete, path, admin, nil, http.StatusNotFound},
	} {
		c.run(t, srv)
	}

	if code := doJSON(t, srv, http.MethodGet, "/api/peers", viewer, &peers); code != http.StatusOK || len(peers) != 0 {
		t.Errorf("GET /api/peers after forgetting = (%d, %+v), want (200, empty)", code, peers)
	}
}

func TestLiveStatusStreamsPeers(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, sender := startPeersWebUI(t, idp)

	conn, _, err := dialLive(t, srv, mintLiveTicket(t, srv, idp.token(t, "viewer", nil)), nil)
	if err != nil {
		t.Fatalf("dialing /api/live: %v", err)
	}

	if msg := readLive(t, conn); msg.Peers == nil || len(msg.Peers) != 0 {
		t.Fatalf("initial message peers = %#v, want an empty list", msg.Peers)
	}

	pushStatus(t, srv, sender)

	if msg := readLive(t, conn); len(msg.Peers) != 1 || msg.Peers[0].ID != sender.UUID() {
		t.Errorf("message after a push = %+v, want the pushed instance", msg.Peers)
	}
}
