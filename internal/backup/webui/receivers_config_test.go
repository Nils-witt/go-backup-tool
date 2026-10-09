package webui

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"path/filepath"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
)

// putJSON issues PUT path on srv with token and body encoded as JSON,
// returning the status code.
func putJSON(t *testing.T, srv *Server, path, token string, body any) int {
	t.Helper()

	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding body: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://"+srv.addr+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

func testPublicKeyPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// startReceiverConfigWebUI starts an SSO-enabled web UI whose receivers
// are managed by a fresh receiver.Manager over its own state db, with
// notification "ops" defined and receiver paths restricted to the returned
// base dir.
func startReceiverConfigWebUI(t *testing.T, idp *testIDP) (*Server, *backup.ReceiverRegistry, string) {
	t.Helper()

	db := openTestStateDB(t)
	baseDir := t.TempDir()
	registry := backup.NewReceiverRegistry(nil)
	status := backup.NewReceiverStatusStore(nil)
	notifications := notify.NewRegistry(map[string]notify.Notification{"ops": {ID: "ops", Webhook: &notify.Webhook{URL: "http://example.invalid"}}})
	manager := receiver.NewManager(db, registry, status, notifications, "", baseDir, discardLogger)
	statusStore, _ := newTestStore()

	srv := StartWebUI("127.0.0.1:0", statusStore, nil, nil, registry, status, manager, nil, discardLogger, db, nil, idp.settings(), nil, false, false, "", nil, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	return srv, registry, baseDir
}

// receiverConfigCall is one request against /api/receiver-configs and the
// status it must answer with.
type receiverConfigCall struct {
	name   string
	method string
	path   string
	token  string
	body   any
	want   int
}

func (c receiverConfigCall) run(t *testing.T, srv *Server) {
	t.Helper()

	var got int

	switch c.method {
	case http.MethodPost:
		got = postJSON(t, srv, c.path, c.token, c.body, nil)
	case http.MethodPut:
		got = putJSON(t, srv, c.path, c.token, c.body)
	default:
		got = doJSON(t, srv, c.method, c.path, c.token, nil)
	}

	if got != c.want {
		t.Errorf("%s: %s %s = %d, want %d", c.name, c.method, c.path, got, c.want)
	}
}

// TestReceiverConfigAPI checks, end to end, that only an admin can manage
// receivers, that validation errors surface as 400/404/409, and that a
// created receiver shows up on /api/receivers immediately.
func TestReceiverConfigAPI(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, registry, baseDir := startReceiverConfigWebUI(t, idp)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	body := config.FileReceiver{ID: "a", PublicKey: testPublicKeyPEM(t), Path: filepath.Join(baseDir, "a"), DownloadNotifications: []string{"ops"}}
	outside := body
	outside.ID, outside.Path = "b", "/etc"
	updated := body
	updated.Retention = "7d"

	for _, c := range []receiverConfigCall{
		{"non-admin list", http.MethodGet, "/api/receiver-configs", viewer, nil, http.StatusForbidden},
		{"non-admin create", http.MethodPost, "/api/receiver-configs", viewer, body, http.StatusForbidden},
		{"admin create", http.MethodPost, "/api/receiver-configs", admin, body, http.StatusCreated},
		{"duplicate create", http.MethodPost, "/api/receiver-configs", admin, body, http.StatusConflict},
		{"path outside base dir", http.MethodPost, "/api/receiver-configs", admin, outside, http.StatusBadRequest},
		{"admin update", http.MethodPut, "/api/receiver-configs/a", admin, updated, http.StatusNoContent},
		{"id change", http.MethodPut, "/api/receiver-configs/a", admin, outside, http.StatusBadRequest},
		{"update missing", http.MethodPut, "/api/receiver-configs/missing", admin, config.FileReceiver{PublicKey: body.PublicKey, Path: body.Path}, http.StatusNotFound},
	} {
		c.run(t, srv)
	}

	var snaps []backup.ReceiverSnapshot
	if code := doJSON(t, srv, http.MethodGet, "/api/receivers", viewer, &snaps); code != http.StatusOK || len(snaps) != 1 || snaps[0].ID != "a" {
		t.Errorf("GET /api/receivers = %d %+v, want just a", code, snaps)
	}

	var list receiverConfigListJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/receiver-configs", admin, &list); code != http.StatusOK {
		t.Fatalf("admin list = %d", code)
	}

	if list.BaseDir != baseDir || len(list.Notifications) != 1 || len(list.Receivers) != 1 ||
		list.Receivers[0].Retention != "7d" || list.Receivers[0].CreatedBy != "erin" {
		t.Errorf("list = %+v", list)
	}

	receiverConfigCall{"admin delete", http.MethodDelete, "/api/receiver-configs/a", admin, nil, http.StatusNoContent}.run(t, srv)
	receiverConfigCall{"second delete", http.MethodDelete, "/api/receiver-configs/a", admin, nil, http.StatusNotFound}.run(t, srv)

	if _, ok := registry.Get("a"); ok {
		t.Error("deleted receiver still in registry")
	}
}
