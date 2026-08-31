package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
)

// writeFile writes contents to path, failing the test on any error.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
}

// newTestSessionStore returns a fresh sessionStore, failing the test if key
// generation fails (which in practice it never does — see newSessionStore).
// Shared by every test in this package that needs one, including
// oidc_test.go.
func newTestSessionStore(t *testing.T) *sessionStore {
	t.Helper()

	sessions, err := newSessionStore(nil, nil)
	if err != nil {
		t.Fatalf("newSessionStore(): %v", err)
	}

	return sessions
}

func TestHandleDashboardServesHTML(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handleDashboard(dashboardHTML)(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}

	if !strings.Contains(rec.Body.String(), "/api/status") {
		t.Error("dashboard HTML doesn't reference /api/status")
	}
}

func TestHandleStatusServesJSON(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()
	store.Starting("test")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()

	handleStatus(store)(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}

	var jobs []backup.JobSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(jobs) != 1 || jobs[0].Name != "test" || jobs[0].State != backup.StateRunning {
		t.Errorf("jobs = %+v, want one running job named test", jobs)
	}
}

func TestHandleReceiverStatusIncludesStaleness(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stale := filepath.Join(root, "old.gpg")
	writeFile(t, stale, "a")

	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", stale, err)
	}

	receivers := map[string]backup.ResolvedReceiver{
		"a": {ID: "a", Path: root, StaleAfter: time.Hour, Webhook: backup.ResolvedWebhook{URL: "https://example.com/hook", Method: http.MethodPost}},
	}
	store := backup.NewReceiverStatusStore(receivers)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers", nil)
	rec := httptest.NewRecorder()

	handleReceiverStatus(receivers, store, discardLogger)(rec, req)

	var snapshots []backup.ReceiverSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshots); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %+v, want 1 entry", snapshots)
	}

	if snapshots[0].StaleAfter != time.Hour.String() || !snapshots[0].Stale {
		t.Errorf("snapshots[0] = %+v, want stale_after %q and stale true", snapshots[0], time.Hour.String())
	}
}

func TestHandleReceiverStatusFreshFileIsNotStale(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "recent.gpg"), "a")

	receivers := map[string]backup.ResolvedReceiver{
		"a": {ID: "a", Path: root, StaleAfter: time.Hour, Webhook: backup.ResolvedWebhook{URL: "https://example.com/hook", Method: http.MethodPost}},
	}
	store := backup.NewReceiverStatusStore(receivers)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers", nil)
	rec := httptest.NewRecorder()

	handleReceiverStatus(receivers, store, discardLogger)(rec, req)

	var snapshots []backup.ReceiverSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshots); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(snapshots) != 1 || snapshots[0].StaleAfter != time.Hour.String() || snapshots[0].Stale {
		t.Errorf("snapshots = %+v, want stale_after %q and stale false", snapshots, time.Hour.String())
	}
}

func TestHandleReceiverStatusWithoutStaleAfterOmitsStaleness(t *testing.T) {
	t.Parallel()

	receivers := map[string]backup.ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}}
	store := backup.NewReceiverStatusStore(receivers)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers", nil)
	rec := httptest.NewRecorder()

	handleReceiverStatus(receivers, store, discardLogger)(rec, req)

	var snapshots []backup.ReceiverSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshots); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(snapshots) != 1 || snapshots[0].StaleAfter != "" || snapshots[0].Stale {
		t.Errorf("snapshots = %+v, want empty stale_after and stale false", snapshots)
	}
}

func TestStartWebUIServesRequests(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	srv := StartWebUI("127.0.0.1:0", store, nil, nil, discardLogger, nil, nil, "", "", nil, nil, false, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.addr+"/api/status", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/status status = %d, want 200", resp.StatusCode)
	}
}

func TestRequireWebUISessionWithoutUsernameAllowsRequest(t *testing.T) {
	t.Parallel()

	called := false
	sessions := newTestSessionStore(t)
	h := requireWebUISession(false, sessions, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	h(rec, req)

	if !called {
		t.Error("handler wasn't called despite auth being unconfigured")
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestRequireWebUISessionReportsUnauthorizedWithoutToken(t *testing.T) {
	t.Parallel()

	called := false
	sessions := newTestSessionStore(t)
	h := requireWebUISession(true, sessions, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()

	h(rec, req)

	if called {
		t.Error("handler was called despite no bearer token")
	}

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestRequireWebUISessionAcceptsValidToken(t *testing.T) {
	t.Parallel()

	called := false
	sessions := newTestSessionStore(t)

	id, err := sessions.create("alice", backup.PermissionView|backup.PermissionDownload)
	if err != nil {
		t.Fatalf("sessions.create(): %v", err)
	}

	h := requireWebUISession(true, sessions, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+id)

	rec := httptest.NewRecorder()

	h(rec, req)

	if !called {
		t.Error("handler wasn't called despite a valid bearer token")
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestStartWebUIWithLoginRequiresSession(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	srv := StartWebUI("127.0.0.1:0", store, nil, nil, discardLogger, nil, nil, "admin", "secret", nil, nil, false, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.addr+"/api/status", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /api/status status = %d, want 401", resp.StatusCode)
	}

	form := url.Values{"username": {"admin"}, "password": {"secret"}}

	loginReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+srv.addr+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("building login request: %v", err)
	}

	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	loginResp, err := client.Do(loginReq)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}

	defer func() { _ = loginResp.Body.Close() }()

	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /login status = %d, want 200", loginResp.StatusCode)
	}

	var loginBody loginResponseJSON
	if err := json.NewDecoder(loginResp.Body).Decode(&loginBody); err != nil {
		t.Fatalf("decoding login response: %v", err)
	}

	if loginBody.Token == "" {
		t.Fatal("login response has no token")
	}

	req.Header.Set("Authorization", "Bearer "+loginBody.Token)

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("authenticated GET /api/status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("authenticated GET /api/status status = %d, want 200", resp.StatusCode)
	}
}

// webUILogin logs into srv as username/password over the real HTTP mux
// StartWebUI wires up, returning the resulting session's bearer token and
// failing the test on any error or non-200 response.
func webUILogin(t *testing.T, client *http.Client, srv *Server, username, password string) string {
	t.Helper()

	form := url.Values{"username": {username}, "password": {password}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+srv.addr+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("building login request: %v", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /login status = %d, want 200", resp.StatusCode)
	}

	var body loginResponseJSON
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding login response: %v", err)
	}

	return body.Token
}

// webUIGetStatus issues an authenticated GET to path on srv's real HTTP mux,
// returning the response status code and failing the test if the request
// itself couldn't be made.
func webUIGetStatus(t *testing.T, client *http.Client, srv *Server, token, path string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.addr+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode
}

// TestStartWebUILoginLogAndDownloadLogRequireDedicatedPermission is an
// end-to-end check, through the real mux StartWebUI wires up, that
// /api/login-events and /api/download-events are gated on
// backup.PermissionViewLoginLog/PermissionViewDownloadLog rather than the
// general backup.PermissionView every other api(...) route uses — a
// view-only db-backed account can reach /api/status but not either log,
// granting just the dedicated permission (without "view") is enough for
// that one log alone, and the single config-file admin (webui.username/
// webui.password) can still reach both despite its session never holding
// PermissionView/PermissionDownload's usual db-backed-account shape (see
// handleWebUILogin's own perm assignment).
func TestStartWebUILoginLogAndDownloadLogRequireDedicatedPermission(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()
	db := openTestStateDB(t)

	if err := backup.CreateWebUIUser(context.Background(), db, "viewer", "s3cret1", backup.PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser(viewer) unexpected error: %v", err)
	}

	if err := backup.CreateWebUIUser(context.Background(), db, "auditor", "s3cret2", backup.PermissionViewLoginLog); err != nil {
		t.Fatalf("CreateWebUIUser(auditor) unexpected error: %v", err)
	}

	srv := StartWebUI("127.0.0.1:0", store, nil, nil, discardLogger, db, nil, "admin", "secret", nil, nil, false, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	client := &http.Client{}
	viewerToken := webUILogin(t, client, srv, "viewer", "s3cret1")
	auditorToken := webUILogin(t, client, srv, "auditor", "s3cret2")
	adminToken := webUILogin(t, client, srv, "admin", "secret")

	tests := []struct {
		name  string
		token string
		path  string
		want  int
	}{
		{"viewer can see status", viewerToken, "/api/status", http.StatusOK},
		{"viewer cannot see login log", viewerToken, "/api/login-events", http.StatusForbidden},
		{"viewer cannot see download log", viewerToken, "/api/download-events", http.StatusForbidden},
		{"login-log-only account can see login log", auditorToken, "/api/login-events", http.StatusOK},
		{"login-log-only account cannot see download log", auditorToken, "/api/download-events", http.StatusForbidden},
		{"config-file admin can see login log", adminToken, "/api/login-events", http.StatusOK},
		{"config-file admin can see download log", adminToken, "/api/download-events", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := webUIGetStatus(t, client, srv, tt.token, tt.path); got != tt.want {
				t.Errorf("GET %s status = %d, want %d", tt.path, got, tt.want)
			}
		})
	}
}

func TestHandleReceiverFilesServesJSON(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "data")

	receivers := map[string]backup.ResolvedReceiver{"a": {ID: "a", Path: root}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/files", nil)
	req.SetPathValue("id", "a")

	rec := httptest.NewRecorder()

	handleReceiverFiles(receivers, discardLogger)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var files []backup.ReceiverFile
	if err := json.Unmarshal(rec.Body.Bytes(), &files); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(files) != 1 || files[0].Key != "backup.gpg" || files[0].Size != 4 {
		t.Errorf("files = %+v, want one entry backup.gpg size 4", files)
	}
}

func TestHandleReceiverFilesUnknownID(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/missing/files", nil)
	req.SetPathValue("id", "missing")

	rec := httptest.NewRecorder()

	handleReceiverFiles(map[string]backup.ResolvedReceiver{}, discardLogger)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestStartWebUIBadAddrReturnsNil(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	// Port 0 is valid (means "pick one"); an unparseable address is not.
	srv := StartWebUI("not-a-valid-address", store, nil, nil, discardLogger, nil, nil, "", "", nil, nil, false, nil)
	if srv != nil {
		t.Cleanup(srv.Shutdown)
		t.Fatal("StartWebUI() with an invalid address = non-nil, want nil")
	}
}

func TestHandleDownloadFileServesContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "secret data")

	receivers := map[string]backup.ResolvedReceiver{"a": {ID: "a", Path: root}}

	tickets := newDownloadTicketStore()

	ticket, err := tickets.create("a", "backup.gpg", "")
	if err != nil {
		t.Fatalf("tickets.create(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg?ticket="+ticket, nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")

	rec := httptest.NewRecorder()

	handleDownloadFile(receivers, discardLogger, nil, tickets, false)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if rec.Body.String() != "secret data" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "secret data")
	}

	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="backup.gpg"`) {
		t.Errorf("Content-Disposition = %q, want it to name backup.gpg", cd)
	}
}

func TestHandleDownloadFileRejectsMissingTicket(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "secret data")

	receivers := map[string]backup.ResolvedReceiver{"a": {ID: "a", Path: root}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg", nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")

	rec := httptest.NewRecorder()

	handleDownloadFile(receivers, discardLogger, nil, newDownloadTicketStore(), false)(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestDownloadTicketStoreConsumeIsSingleUse(t *testing.T) {
	t.Parallel()

	tickets := newDownloadTicketStore()

	id, err := tickets.create("a", "backup.gpg", "alice")
	if err != nil {
		t.Fatalf("tickets.create(): %v", err)
	}

	username, ok := tickets.consume(id, "a", "backup.gpg")
	if !ok || username != "alice" {
		t.Fatalf("consume() = %q, %v, want %q, true", username, ok, "alice")
	}

	if _, ok := tickets.consume(id, "a", "backup.gpg"); ok {
		t.Error("consume() succeeded a second time for the same ticket, want single-use")
	}
}

func TestDownloadTicketStoreConsumeRejectsMismatch(t *testing.T) {
	t.Parallel()

	tickets := newDownloadTicketStore()

	id, err := tickets.create("a", "backup.gpg", "alice")
	if err != nil {
		t.Fatalf("tickets.create(): %v", err)
	}

	if _, ok := tickets.consume(id, "a", "other.gpg"); ok {
		t.Error("consume() succeeded for the wrong key, want false")
	}
}

func TestHandleWebUILoginWrongCredentialsDoesNotStartSession(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)

	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	handleWebUILogin("admin", "secret", false, sessions, nil, discardLogger, false)(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	var body loginErrorJSON
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if body.Error == "" {
		t.Error("response body doesn't mention the incorrect credentials")
	}
}

func TestHandleWebUILoginWithoutUsernameConfiguredRedirects(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login?next=/", nil)
	rec := httptest.NewRecorder()

	handleWebUILogin("", "", false, sessions, nil, discardLogger, false)(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want %q", loc, "/")
	}
}

func TestHandleWebUILoginCorrectCredentialsStartsSession(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)

	form := url.Values{"username": {"admin"}, "password": {"secret"}, "next": {"/"}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	handleWebUILogin("admin", "secret", false, sessions, nil, discardLogger, false)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body loginResponseJSON
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if body.Token == "" {
		t.Fatal("response has no token")
	}

	if !sessions.valid(body.Token) {
		t.Error("the response token isn't a valid session")
	}
}

func TestHandleAPILogoutRevokesSession(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)

	id, err := sessions.create("alice", backup.PermissionView|backup.PermissionDownload)
	if err != nil {
		t.Fatalf("sessions.create(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/logout", nil)
	req.Header.Set("Authorization", "Bearer "+id)

	rec := httptest.NewRecorder()

	handleAPILogout(sessions, nil, discardLogger)(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	if sessions.valid(id) {
		t.Error("session is still valid after logout")
	}
}

func TestHandleIssueWebUIUserTokenRecordsToken(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()

	if err := backup.CreateWebUIUser(ctx, db, "alice", "hunter2", backup.PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser(): %v", err)
	}

	sessions, err := newSessionStore(nil, db)
	if err != nil {
		t.Fatalf("newSessionStore(): %v", err)
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/users/alice/tokens", strings.NewReader(`{"days":30}`))
	req.SetPathValue("username", "alice")

	rec := httptest.NewRecorder()

	handleIssueWebUIUserToken(sessions, db, discardLogger)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body loginResponseJSON
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if body.Token == "" || !sessions.valid(body.Token) {
		t.Fatal("issued token is missing or not a valid session")
	}

	tokens, err := backup.ListAPITokensForUser(ctx, db, "alice")
	if err != nil {
		t.Fatalf("ListAPITokensForUser(): %v", err)
	}

	if len(tokens) != 1 {
		t.Fatalf("ListAPITokensForUser() returned %d tokens, want 1", len(tokens))
	}

	if tokens[0].RevokedAt != nil {
		t.Error("newly issued token is already recorded as revoked")
	}
}

func TestHandleListWebUIUserTokens(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()

	if err := backup.CreateWebUIUser(ctx, db, "alice", "hunter2", backup.PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser(): %v", err)
	}

	sessions, err := newSessionStore(nil, db)
	if err != nil {
		t.Fatalf("newSessionStore(): %v", err)
	}

	for range 2 {
		issueReq := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/users/alice/tokens", strings.NewReader(`{"days":30}`))
		issueReq.SetPathValue("username", "alice")

		issueRec := httptest.NewRecorder()

		handleIssueWebUIUserToken(sessions, db, discardLogger)(issueRec, issueReq)

		if issueRec.Code != http.StatusOK {
			t.Fatalf("issuing token: status = %d, want 200", issueRec.Code)
		}
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/users/alice/tokens", nil)
	req.SetPathValue("username", "alice")

	rec := httptest.NewRecorder()

	handleListWebUIUserTokens(db, discardLogger)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var tokens []apiTokenJSON
	if err := json.NewDecoder(rec.Body).Decode(&tokens); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(tokens) != 2 {
		t.Fatalf("handleListWebUIUserTokens() returned %d tokens, want 2", len(tokens))
	}

	for _, tok := range tokens {
		if tok.Revoked || tok.JTI == "" {
			t.Errorf("token %+v: want a non-empty jti and Revoked = false", tok)
		}
	}
}

// TestHandleRevokeWebUIUserTokenBlocksSessionAndIsIdempotent drives issue/
// wrong-user-revoke/revoke/re-revoke/unknown-jti in order (each stage
// depends on state the previous one left behind) against one shared db,
// session store, and issued token. The stages live in standalone helpers
// below rather than inline t.Run closures, since gocyclo counts a closure's
// branches against the enclosing function just as if they were inline.
func TestHandleRevokeWebUIUserTokenBlocksSessionAndIsIdempotent(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	if err := backup.CreateWebUIUser(t.Context(), db, "alice", "hunter2", backup.PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser(): %v", err)
	}

	sessions, err := newSessionStore(nil, db)
	if err != nil {
		t.Fatalf("newSessionStore(): %v", err)
	}

	issued := issueWebUIUserTokenForAlice(t, sessions, db)
	jti := requireOnlyAPITokenJTI(t, db, "alice")

	requireRevokeUnderWrongUsernameHasNoEffect(t, sessions, db, jti, issued.Token)

	revokeWebUIUserToken(t, sessions, db, "alice", jti, http.StatusNoContent)

	if sessions.valid(issued.Token) {
		t.Error("token is still valid after being revoked")
	}

	// Revoking the same token again is a no-op, not an error.
	revokeWebUIUserToken(t, sessions, db, "alice", jti, http.StatusNoContent)

	revokeWebUIUserToken(t, sessions, db, "alice", "no-such-jti", http.StatusNotFound)
}

// issueWebUIUserTokenForAlice issues alice a 30-day API token through
// handleIssueWebUIUserToken, requires success and that the token validates,
// and returns the decoded response.
func issueWebUIUserTokenForAlice(t *testing.T, sessions *sessionStore, db *sql.DB) loginResponseJSON {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/users/alice/tokens", strings.NewReader(`{"days":30}`))
	req.SetPathValue("username", "alice")

	rec := httptest.NewRecorder()
	handleIssueWebUIUserToken(sessions, db, discardLogger)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("issuing token: status = %d, want 200", rec.Code)
	}

	var issued loginResponseJSON
	if err := json.NewDecoder(rec.Body).Decode(&issued); err != nil {
		t.Fatalf("decoding issue response: %v", err)
	}

	if !sessions.valid(issued.Token) {
		t.Fatal("freshly issued token isn't valid")
	}

	return issued
}

// requireOnlyAPITokenJTI requires exactly one API token recorded for
// username and returns its JTI.
func requireOnlyAPITokenJTI(t *testing.T, db *sql.DB, username string) string {
	t.Helper()

	tokens, err := backup.ListAPITokensForUser(t.Context(), db, username)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("ListAPITokensForUser() = %+v, %v, want exactly one token", tokens, err)
	}

	return tokens[0].JTI
}

// requireRevokeUnderWrongUsernameHasNoEffect attempts to revoke jti as bob
// (not its owner) and requires the request to be rejected without revoking
// anything.
func requireRevokeUnderWrongUsernameHasNoEffect(t *testing.T, sessions *sessionStore, db *sql.DB, jti, token string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/users/bob/tokens/"+jti, nil)
	req.SetPathValue("username", "bob")
	req.SetPathValue("jti", jti)

	rec := httptest.NewRecorder()
	handleRevokeWebUIUserToken(sessions, db, discardLogger)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("revoking under the wrong username: status = %d, want 404", rec.Code)
	}

	if !sessions.valid(token) {
		t.Error("token was revoked despite the username mismatch")
	}

	// Checking sessions.valid alone isn't enough here: it only reflects
	// sessions' in-memory blocklist, which a rejected request never touches
	// either way. What must not have happened is the persistent record
	// itself being marked revoked, since that's what a later restart would
	// reload (see TestSessionStoreReloadsRevokedAPITokensAfterRestart).
	if stored, ok, err := backup.GetAPIToken(t.Context(), db, jti); err != nil || !ok || stored.RevokedAt != nil {
		t.Errorf("GetAPIToken() after a wrong-username revoke attempt = (%+v, %v, %v), want a still-unrevoked token", stored, ok, err)
	}
}

// revokeWebUIUserToken issues a DELETE for username's jti through
// handleRevokeWebUIUserToken and requires the response status to be
// wantStatus.
func revokeWebUIUserToken(t *testing.T, sessions *sessionStore, db *sql.DB, username, jti string, wantStatus int) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/users/"+username+"/tokens/"+jti, nil)
	req.SetPathValue("username", username)
	req.SetPathValue("jti", jti)

	rec := httptest.NewRecorder()
	handleRevokeWebUIUserToken(sessions, db, discardLogger)(rec, req)

	if rec.Code != wantStatus {
		t.Errorf("revoke status = %d, want %d", rec.Code, wantStatus)
	}
}

// TestSessionStoreReloadsRevokedAPITokensAfterRestart is the core guarantee
// behind making a long-lived API token revocable at all: since the token
// itself is a self-contained signed JWT (see sessionStore), the only way to
// end it early is a server-side revocation blocklist — and since such a
// token can outlive the process by years (see maxAPITokenDays), that
// blocklist has to survive a restart too, unlike an ordinary interactive
// session's own revocation. This simulates a restart by discarding the
// first sessionStore and building a second one against the same db.
func TestSessionStoreReloadsRevokedAPITokensAfterRestart(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()

	before, err := newSessionStore(nil, db)
	if err != nil {
		t.Fatalf("newSessionStore(): %v", err)
	}

	token, jti, err := before.createWithTTL("alice", backup.PermissionView, time.Hour)
	if err != nil {
		t.Fatalf("createWithTTL(): %v", err)
	}

	now := time.Now()

	if err := backup.RecordAPIToken(ctx, db, jti, "alice", backup.PermissionView, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("RecordAPIToken(): %v", err)
	}

	if !before.valid(token) {
		t.Fatal("token isn't valid before revocation")
	}

	revoked, err := backup.RevokeAPIToken(ctx, db, jti, now)
	if err != nil {
		t.Fatalf("RevokeAPIToken(): %v", err)
	}

	before.revokeJTI(revoked.JTI, revoked.ExpiresAt)

	if before.valid(token) {
		t.Fatal("token is still valid immediately after revocation")
	}

	// Simulate a restart: a fresh sessionStore against the same db, with
	// nothing carried over in memory.
	after, err := newSessionStore(nil, db)
	if err != nil {
		t.Fatalf("newSessionStore() after restart: %v", err)
	}

	if after.valid(token) {
		t.Error("token is valid again after a simulated restart — the revocation wasn't persisted")
	}
}

func TestLogRingBufferSnapshotOrdersOldestFirst(t *testing.T) {
	t.Parallel()

	buf := NewLogRingBuffer(3)

	for _, line := range []string{"one\n", "two\n", "three\n"} {
		if _, err := buf.Write([]byte(line)); err != nil {
			t.Fatalf("Write(%q): %v", line, err)
		}
	}

	got := buf.snapshot()
	want := []string{"one", "two", "three"}

	if !slices.Equal(got, want) {
		t.Errorf("snapshot() = %v, want %v", got, want)
	}
}

func TestLogRingBufferEvictsOldestPastCapacity(t *testing.T) {
	t.Parallel()

	buf := NewLogRingBuffer(2)

	for _, line := range []string{"one\n", "two\n", "three\n"} {
		if _, err := buf.Write([]byte(line)); err != nil {
			t.Fatalf("Write(%q): %v", line, err)
		}
	}

	got := buf.snapshot()
	want := []string{"two", "three"}

	if !slices.Equal(got, want) {
		t.Errorf("snapshot() = %v, want %v (oldest evicted)", got, want)
	}
}

func TestHandleLogsServesJSON(t *testing.T) {
	t.Parallel()

	buf := NewLogRingBuffer(10)
	_, _ = buf.Write([]byte("level=INFO msg=hello\n"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/logs", nil)
	rec := httptest.NewRecorder()

	handleLogs(buf)(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}

	var lines []string
	if err := json.Unmarshal(rec.Body.Bytes(), &lines); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if !slices.Equal(lines, []string{"level=INFO msg=hello"}) {
		t.Errorf("lines = %v, want one line", lines)
	}
}

func TestClientAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		trustProxyHeaders bool
		headers           map[string]string
		want              string
	}{
		{
			name: "no proxy headers trusted",
			headers: map[string]string{
				"X-Forwarded-For": "203.0.113.9",
			},
			want: "198.51.100.1:4321",
		},
		{
			name:              "forwarded header preferred, includes port",
			trustProxyHeaders: true,
			headers: map[string]string{
				"Forwarded":       "for=203.0.113.9:5678;proto=https",
				"X-Forwarded-For": "203.0.113.10",
			},
			want: "203.0.113.9:5678",
		},
		{
			name:              "forwarded header with quoted ipv6",
			trustProxyHeaders: true,
			headers: map[string]string{
				"Forwarded": `for="[2001:db8:cafe::17]:4711"`,
			},
			want: "[2001:db8:cafe::17]:4711",
		},
		{
			name:              "forwarded header takes first hop of a chain",
			trustProxyHeaders: true,
			headers: map[string]string{
				"Forwarded": "for=203.0.113.9, for=198.51.100.100",
			},
			want: "203.0.113.9",
		},
		{
			name:              "x-forwarded-for used when no forwarded header",
			trustProxyHeaders: true,
			headers: map[string]string{
				"X-Forwarded-For": "203.0.113.9, 198.51.100.100",
			},
			want: "203.0.113.9",
		},
		{
			name:              "x-real-ip used as last resort",
			trustProxyHeaders: true,
			headers: map[string]string{
				"X-Real-Ip": "203.0.113.9",
			},
			want: "203.0.113.9",
		},
		{
			name:              "falls back to remote addr without any header",
			trustProxyHeaders: true,
			want:              "198.51.100.1:4321",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.RemoteAddr = "198.51.100.1:4321"

			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			if got := clientAddr(req, tt.trustProxyHeaders); got != tt.want {
				t.Errorf("clientAddr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleWebUILoginRecordsLoginEvents(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	sessions := newTestSessionStore(t)

	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.1:4321"

	rec := httptest.NewRecorder()

	handleWebUILogin("admin", "secret", false, sessions, db, discardLogger, false)(rec, req)

	form = url.Values{"username": {"admin"}, "password": {"secret"}}
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.2:4321"

	rec = httptest.NewRecorder()

	handleWebUILogin("admin", "secret", false, sessions, db, discardLogger, false)(rec, req)

	events, err := backup.ReadLoginEvents(t.Context(), db, 10)
	if err != nil {
		t.Fatalf("readLoginEvents() error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("readLoginEvents() returned %d events, want 2", len(events))
	}

	if events[0].Username != "admin" || events[0].Method != "password" || !events[0].Success || events[0].RemoteAddr != "198.51.100.2:4321" {
		t.Errorf("readLoginEvents()[0] = %+v, want the successful attempt", events[0])
	}

	if events[1].Success || events[1].Detail == "" {
		t.Errorf("readLoginEvents()[1] = %+v, want the failed attempt with a detail", events[1])
	}
}

func TestHandleWebUILoginWithTrustProxyHeadersRecordsForwardedAddr(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	sessions := newTestSessionStore(t)

	form := url.Values{"username": {"admin"}, "password": {"secret"}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.RemoteAddr = "198.51.100.1:4321"

	rec := httptest.NewRecorder()

	handleWebUILogin("admin", "secret", false, sessions, db, discardLogger, true)(rec, req)

	events, err := backup.ReadLoginEvents(t.Context(), db, 10)
	if err != nil {
		t.Fatalf("readLoginEvents() error: %v", err)
	}

	if len(events) != 1 || events[0].RemoteAddr != "203.0.113.9" {
		t.Fatalf("readLoginEvents() = %+v, want one event with RemoteAddr from X-Forwarded-For", events)
	}
}

func TestHandleLoginEventsServesJSON(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	if err := backup.RecordLoginEvent(t.Context(), db, backup.LoginEvent{At: time.Now(), Username: "admin", Method: "password", Success: true, RemoteAddr: "127.0.0.1:1"}); err != nil {
		t.Fatalf("recordLoginEvent() error: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/login-events", nil)
	rec := httptest.NewRecorder()

	handleLoginEvents(db, discardLogger)(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}

	var got []loginEventJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(got) != 1 || got[0].Username != "admin" || !got[0].Success {
		t.Errorf("decoded events = %+v, want one successful admin login", got)
	}
}

func TestHandleLoginEventsWithoutDBServesEmptyList(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/login-events", nil)
	rec := httptest.NewRecorder()

	handleLoginEvents(nil, discardLogger)(rec, req)

	var got []loginEventJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("decoded events = %+v, want none", got)
	}
}

func TestHandleDownloadFileRecordsDownloadEvents(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	tickets := newDownloadTicketStore()

	ticket, err := tickets.create("a", "backup.gpg", "alice")
	if err != nil {
		t.Fatalf("tickets.create() error: %v", err)
	}

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "secret data")

	receivers := map[string]backup.ResolvedReceiver{"a": {ID: "a", Path: root}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg?ticket="+ticket, nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")
	req.RemoteAddr = "198.51.100.1:4321"

	handleDownloadFile(receivers, discardLogger, db, tickets, false)(httptest.NewRecorder(), req)

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/missing.gpg", nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "missing.gpg")
	req.RemoteAddr = "198.51.100.2:4321"

	// A missing ticket is reported as 403 without ever attempting to open
	// the file, so it can't produce a "not found" download event; mint one
	// for this receiver/key so the failure under test is the file lookup,
	// not the ticket.
	ticket2, err := tickets.create("a", "missing.gpg", "")
	if err != nil {
		t.Fatalf("tickets.create() error: %v", err)
	}

	req.URL.RawQuery = "ticket=" + ticket2

	handleDownloadFile(receivers, discardLogger, db, tickets, false)(httptest.NewRecorder(), req)

	events, err := backup.ReadDownloadEvents(t.Context(), db, 10)
	if err != nil {
		t.Fatalf("readDownloadEvents() error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("readDownloadEvents() returned %d events, want 2", len(events))
	}

	if events[0].Success || events[0].Key != "missing.gpg" || events[0].Detail != "not found" {
		t.Errorf("readDownloadEvents()[0] = %+v, want the failed attempt", events[0])
	}

	if !events[1].Success || events[1].Username != "alice" || events[1].ReceiverID != "a" || events[1].Key != "backup.gpg" || events[1].RemoteAddr != "198.51.100.1:4321" {
		t.Errorf("readDownloadEvents()[1] = %+v, want the successful attempt attributed to alice", events[1])
	}
}

func TestHandleDownloadEventsServesJSON(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	if err := backup.RecordDownloadEvent(t.Context(), db, backup.DownloadEvent{At: time.Now(), Username: "admin", ReceiverID: "a", Key: "backup.gpg", Success: true, RemoteAddr: "127.0.0.1:1"}); err != nil {
		t.Fatalf("recordDownloadEvent() error: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/download-events", nil)
	rec := httptest.NewRecorder()

	handleDownloadEvents(db, discardLogger)(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}

	var got []downloadEventJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(got) != 1 || got[0].Username != "admin" || got[0].Key != "backup.gpg" || !got[0].Success {
		t.Errorf("decoded events = %+v, want one successful admin download", got)
	}
}

func TestHandleDownloadEventsWithoutDBServesEmptyList(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/download-events", nil)
	rec := httptest.NewRecorder()

	handleDownloadEvents(nil, discardLogger)(rec, req)

	var got []downloadEventJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("decoded events = %+v, want none", got)
	}
}
