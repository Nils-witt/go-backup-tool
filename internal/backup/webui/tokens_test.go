package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// postJSON issues POST path on srv with token and body encoded as JSON,
// returning the status code and decoding a 2xx body into out (if non-nil).
func postJSON(t *testing.T, srv *Server, path, token string, body, out any) int {
	t.Helper()

	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding body: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+srv.addr+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
	}

	return resp.StatusCode
}

// startTokenWebUI starts an SSO-enabled web UI backed by its own state db,
// returned too so tests can inspect it, with one receiver "a" holding
// backup.gpg.
func startTokenWebUI(t *testing.T, idp *testIDP) (*Server, *store.Store) {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "backup.gpg"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	statusStore, _ := newTestStore()
	db := openTestStateDB(t)
	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: root}}

	srv := StartWebUI("127.0.0.1:0", statusStore, nil, nil, nil, backup.NewReceiverRegistry(receivers), nil, nil, nil, nil, discardLogger, db, nil, idp.settings(), nil, false, false, 0, "", nil, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	return srv, db
}

// checkAPITokenIsReadOnly asserts tok (named "grafana") can view the
// dashboard but not download, see audit logs, or manage tokens.
func checkAPITokenIsReadOnly(t *testing.T, srv *Server, tok string) {
	t.Helper()

	var me meJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/me", tok, &me); code != http.StatusOK {
		t.Fatalf("GET /api/me with api token = %d, want 200", code)
	}

	if me.Username != "token:grafana" || me.Admin || !slices.Equal(me.Permissions, []string{"view"}) {
		t.Errorf("me = %+v, want token:grafana with only view", me)
	}

	for path, want := range map[string]int{
		"/api/status":            http.StatusOK,
		"/api/receivers/a/files": http.StatusOK,
		"/api/login-events":      http.StatusForbidden,
		"/api/download-events":   http.StatusForbidden,
		"/api/tokens":            http.StatusForbidden,
	} {
		if got := doJSON(t, srv, http.MethodGet, path, tok, nil); got != want {
			t.Errorf("api token GET %s = %d, want %d", path, got, want)
		}
	}

	if code := doJSON(t, srv, http.MethodPost, "/api/receivers/a/download/backup.gpg", tok, nil); code != http.StatusForbidden {
		t.Errorf("api token minting a download ticket = %d, want 403", code)
	}

	if code := postJSON(t, srv, "/api/tokens", tok, createAPITokenRequest{Name: "x", LifetimeDays: 1}, nil); code != http.StatusForbidden {
		t.Errorf("api token creating another token = %d, want 403", code)
	}
}

// createGrafanaToken checks that only an admin may issue an API token, then
// issues a 30-day one named "grafana" as admin (erin).
func createGrafanaToken(t *testing.T, srv *Server, admin, viewer string) createdAPITokenJSON {
	t.Helper()

	body := createAPITokenRequest{Name: "grafana", LifetimeDays: 30}
	if code := postJSON(t, srv, "/api/tokens", viewer, body, nil); code != http.StatusForbidden {
		t.Errorf("non-admin create = %d, want 403", code)
	}

	var created createdAPITokenJSON
	if code := postJSON(t, srv, "/api/tokens", admin, body, &created); code != http.StatusCreated {
		t.Fatalf("admin create = %d, want 201", code)
	}

	if created.Token == "" || created.Name != "grafana" || created.CreatedBy != "erin" {
		t.Fatalf("created = %+v, want a signed token named grafana by erin", created)
	}

	if d := created.ExpiresAt.Sub(created.CreatedAt); d != 30*24*time.Hour {
		t.Errorf("lifetime = %v, want 30 days", d)
	}

	return created
}

// TestAPITokenLifecycle checks, end to end, that an admin-issued API token
// grants read-only access (no downloads, audit logs or admin endpoints)
// until it is revoked.
func TestAPITokenLifecycle(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, db := startTokenWebUI(t, idp)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	created := createGrafanaToken(t, srv, admin, viewer)
	tok := created.Token
	checkAPITokenIsReadOnly(t, srv, tok)

	var list []apiTokenJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/tokens", admin, &list); code != http.StatusOK {
		t.Fatalf("GET /api/tokens = %d", code)
	}

	if len(list) != 1 || list[0].ID != created.ID || list[0].RevokedAt != nil {
		t.Fatalf("token list = %+v, want just the unrevoked grafana token", list)
	}

	if code := doJSON(t, srv, http.MethodDelete, "/api/tokens/"+created.ID, viewer, nil); code != http.StatusForbidden {
		t.Errorf("non-admin revoke = %d, want 403", code)
	}

	if code := doJSON(t, srv, http.MethodDelete, "/api/tokens/"+created.ID, admin, nil); code != http.StatusNoContent {
		t.Fatalf("admin revoke = %d, want 204", code)
	}

	if code := doJSON(t, srv, http.MethodDelete, "/api/tokens/"+created.ID, admin, nil); code != http.StatusNotFound {
		t.Errorf("revoking again = %d, want 404", code)
	}

	if code := doJSON(t, srv, http.MethodGet, "/api/status", tok, nil); code != http.StatusUnauthorized {
		t.Errorf("revoked api token = %d, want 401", code)
	}

	events, err := db.ListLoginEvents(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListLoginEvents() error: %v", err)
	}

	if len(events) != 1 || events[0].Success || events[0].Method != loginMethodAPIToken {
		t.Errorf("login events = %+v, want one failed api-token attempt", events)
	}
}

func TestCreateAPITokenValidation(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, _ := startTokenWebUI(t, idp)
	admin := idp.token(t, "admin", map[string]any{"groups": []string{"admins"}})

	for name, body := range map[string]createAPITokenRequest{
		"blank name":        {Name: "  ", LifetimeDays: 30},
		"long name":         {Name: string(make([]byte, maxAPITokenNameLen+1)), LifetimeDays: 30},
		"zero lifetime":     {Name: "x", LifetimeDays: 0},
		"too long lifetime": {Name: "x", LifetimeDays: maxAPITokenLifetimeDays + 1},
	} {
		if code := postJSON(t, srv, "/api/tokens", admin, body, nil); code != http.StatusBadRequest {
			t.Errorf("%s: create = %d, want 400", name, code)
		}
	}
}

// TestAPITokenRejectsForgedAndExpired checks that a token needs both this
// instance's signing key and an unexpired, known id.
func TestAPITokenRejectsForgedAndExpired(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, db := startTokenWebUI(t, idp)

	key, err := db.TokenSigningKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Truncate(time.Second)
	rec := store.APIToken{ID: "t1", Name: "old", CreatedBy: "admin", CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)}

	if err := db.SaveAPIToken(t.Context(), rec); err != nil {
		t.Fatal(err)
	}

	issuer := &apiTokens{key: key, db: db}
	forger := &apiTokens{key: bytes.Repeat([]byte{1}, 32), db: db}

	expired, err := issuer.sign(rec.ID, rec.CreatedAt, rec.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}

	forged, err := forger.sign(rec.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	unknown, err := issuer.sign("no-such-id", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	for name, tok := range map[string]string{"expired": expired, "forged": forged, "unknown id": unknown} {
		if code := doJSON(t, srv, http.MethodGet, "/api/status", tok, nil); code != http.StatusUnauthorized {
			t.Errorf("%s token = %d, want 401", name, code)
		}
	}
}
