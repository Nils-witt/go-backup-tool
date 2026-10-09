package webui

import (
	"net/http"
	"path/filepath"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// TestTrustedServersAPI checks, end to end, that only an admin can manage
// trusted servers, that a receiver can allow one, and that a server can't
// be deleted while a receiver still allows it.
func TestTrustedServersAPI(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, registry, baseDir := startReceiverConfigWebUI(t, idp)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	const id = "3f2a6c1e-7b4d-4e8a-9c1e-0a1b2c3d4e5f"

	pemText := testPublicKeyPEM(t)
	server := trust.Input{ID: id, Name: "nas", PublicKey: pemText}
	receiverBody := config.FileReceiver{ID: "a", AllowedServers: []string{id}, Path: filepath.Join(baseDir, "a")}

	for _, c := range []receiverConfigCall{
		{"non-admin list", http.MethodGet, "/api/trusted-servers", viewer, nil, http.StatusForbidden},
		{"non-admin create", http.MethodPost, "/api/trusted-servers", viewer, server, http.StatusForbidden},
		{"receiver before server exists", http.MethodPost, "/api/receiver-configs", admin, receiverBody, http.StatusBadRequest},
		{"admin create", http.MethodPost, "/api/trusted-servers", admin, server, http.StatusCreated},
		{"duplicate create", http.MethodPost, "/api/trusted-servers", admin, server, http.StatusConflict},
		{"invalid id", http.MethodPost, "/api/trusted-servers", admin, trust.Input{ID: "nas", Name: "nas", PublicKey: pemText}, http.StatusBadRequest},
		{"receiver allowing it", http.MethodPost, "/api/receiver-configs", admin, receiverBody, http.StatusCreated},
		{"admin update", http.MethodPut, "/api/trusted-servers/" + id, admin, trust.Input{Name: "nas-2", PublicKey: pemText}, http.StatusNoContent},
		{"id change", http.MethodPut, "/api/trusted-servers/" + id, admin, trust.Input{ID: "6f1c0e0a-0000-4000-8000-000000000000", Name: "x", PublicKey: pemText}, http.StatusBadRequest},
		{"update missing", http.MethodPut, "/api/trusted-servers/6f1c0e0a-0000-4000-8000-000000000000", admin, trust.Input{Name: "x", PublicKey: pemText}, http.StatusNotFound},
		{"delete in use", http.MethodDelete, "/api/trusted-servers/" + id, admin, nil, http.StatusConflict},
	} {
		c.run(t, srv)
	}

	if recv, ok := registry.Get("a"); !ok || len(recv.AllowedServers) != 1 || recv.AllowedServers[0] != id {
		t.Errorf("registry receiver a = %+v, %v; want it to allow %s", recv, ok, id)
	}

	checkTrustedServerLists(t, srv, admin, id)

	receiverConfigCall{"delete receiver", http.MethodDelete, "/api/receiver-configs/a", admin, nil, http.StatusNoContent}.run(t, srv)
	receiverConfigCall{"delete server", http.MethodDelete, "/api/trusted-servers/" + id, admin, nil, http.StatusNoContent}.run(t, srv)
}

// checkTrustedServerLists checks GET /api/trusted-servers and GET
// /api/receiver-configs after TestTrustedServersAPI's calls: one server
// "nas-2", allowed by receiver a, offered as an option on the receiver list.
func checkTrustedServerLists(t *testing.T, srv *Server, admin, id string) {
	t.Helper()

	var servers trustedServerListJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/trusted-servers", admin, &servers); code != http.StatusOK {
		t.Fatalf("admin list = %d", code)
	}

	if len(servers.Servers) != 1 || servers.Servers[0].Name != "nas-2" || servers.Servers[0].Fingerprint == "" ||
		len(servers.Servers[0].UsedBy) != 1 || servers.Servers[0].UpdatedBy != "erin" {
		t.Errorf("trusted servers = %+v", servers)
	}

	var receivers receiverConfigListJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/receiver-configs", admin, &receivers); code != http.StatusOK {
		t.Fatalf("receiver list = %d", code)
	}

	if len(receivers.TrustedServers) != 1 || receivers.TrustedServers[0] != (trustedServerOptionJSON{ID: id, Name: "nas-2"}) ||
		len(receivers.Receivers) != 1 || len(receivers.Receivers[0].AllowedServers) != 1 || receivers.Receivers[0].PublicKey != "" {
		t.Errorf("receiver list = %+v", receivers)
	}
}
