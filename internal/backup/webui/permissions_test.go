package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

// mustCreateSession mints a session token for username granting perm,
// failing the test on error.
func mustCreateSession(t *testing.T, sessions *sessionStore, username string, perm permission.Permission) string {
	t.Helper()

	token, err := sessions.create(username, perm)
	if err != nil {
		t.Fatalf("sessions.create(): %v", err)
	}

	return token
}

func TestRequirePermissionAllowsSufficientPermission(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)
	token := mustCreateSession(t, sessions, "alice", permission.PermissionDownload)

	called := false
	h := requirePermission(true, sessions, permission.PermissionView, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()

	h(rec, req)

	if !called {
		t.Error("handler wasn't called despite a session whose download permission implies view")
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRequirePermissionRejectsInsufficientPermission(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)
	token := mustCreateSession(t, sessions, "alice", permission.PermissionView)

	called := false
	h := requirePermission(true, sessions, permission.PermissionDownload, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()

	h(rec, req)

	if called {
		t.Error("handler was called despite a view-only session lacking download")
	}

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestRequirePermissionLoginLogAndDownloadLogAreIndependentOfView(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)
	token := mustCreateSession(t, sessions, "alice", permission.PermissionView|permission.PermissionDownload)

	loginLogCalled := false
	loginLog := requirePermission(true, sessions, permission.PermissionViewLoginLog, func(http.ResponseWriter, *http.Request) { loginLogCalled = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	loginLog(rec, req)

	if loginLogCalled {
		t.Error("login-log handler was called despite a view+download session lacking PermissionViewLoginLog")
	}

	if rec.Code != http.StatusForbidden {
		t.Errorf("login-log status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	downloadLogCalled := false
	downloadLog := requirePermission(true, sessions, permission.PermissionViewDownloadLog, func(http.ResponseWriter, *http.Request) { downloadLogCalled = true })

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec = httptest.NewRecorder()
	downloadLog(rec, req)

	if downloadLogCalled {
		t.Error("download-log handler was called despite a view+download session lacking PermissionViewDownloadLog")
	}

	if rec.Code != http.StatusForbidden {
		t.Errorf("download-log status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// PermissionAdmin implies both, unlike view+download above.
	adminToken := mustCreateSession(t, sessions, "erin", permission.PermissionAdmin)

	loginLogCalled = false
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)

	rec = httptest.NewRecorder()
	loginLog(rec, req)

	if !loginLogCalled || rec.Code != http.StatusOK {
		t.Errorf("admin session: login-log called=%v status=%d, want called=true status=%d", loginLogCalled, rec.Code, http.StatusOK)
	}

	downloadLogCalled = false
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)

	rec = httptest.NewRecorder()
	downloadLog(rec, req)

	if !downloadLogCalled || rec.Code != http.StatusOK {
		t.Errorf("admin session: download-log called=%v status=%d, want called=true status=%d", downloadLogCalled, rec.Code, http.StatusOK)
	}
}

func TestRequirePermissionBypassedWhenAuthDisabled(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)

	called := false
	h := requirePermission(false, sessions, permission.PermissionDownload, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	h(rec, req)

	if !called {
		t.Error("handler wasn't called despite authEnabled=false, which should skip the permission check")
	}
}

func TestRequireAdminAllowsAdminSessions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		username    string
		perm        permission.Permission
		configAdmin string
	}{
		{"configured admin's own session", "admin", permission.PermissionView, "admin"},
		{"PermissionAdmin holder with no config admin", "erin", permission.PermissionAdmin, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sessions := newTestSessionStore(t)
			token := mustCreateSession(t, sessions, tt.username, tt.perm)

			called := false
			h := requireAdmin(true, sessions, tt.configAdmin, func(http.ResponseWriter, *http.Request) { called = true })

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+token)

			rec := httptest.NewRecorder()

			h(rec, req)

			if !called {
				t.Error("handler wasn't called for an admin session")
			}
		})
	}
}

func TestRequireAdminRejectsNonAdmin(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)
	token := mustCreateSession(t, sessions, "alice", permission.PermissionView|permission.PermissionDownload)

	called := false
	h := requireAdmin(true, sessions, "admin", func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()

	h(rec, req)

	if called {
		t.Error("handler was called for a non-admin session, even with full view+download permissions")
	}

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestRequireAdminAllowsPermissionAdminHolder(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)
	token := mustCreateSession(t, sessions, "erin", permission.PermissionAdmin)

	called := false
	h := requireAdmin(true, sessions, "admin", func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()

	h(rec, req)

	if !called {
		t.Error("handler wasn't called for a session holding PermissionAdmin, even though it isn't the config-file admin")
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRequireAdminRejectsEveryoneWhenNoAdminConfigured(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)
	token := mustCreateSession(t, sessions, "alice", permission.PermissionView|permission.PermissionDownload)

	h := requireAdmin(true, sessions, "", func(http.ResponseWriter, *http.Request) {
		t.Error("handler was called despite no config-file admin being configured")
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()

	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleSessionInfoReportsPermissionsAndAdmin(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)
	token := mustCreateSession(t, sessions, "alice", permission.PermissionView)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()

	handleSessionInfo(sessions, true, "admin", false)(rec, req)

	var body sessionInfoJSON
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if body.Username != "alice" {
		t.Errorf("Username = %q, want %q", body.Username, "alice")
	}

	if len(body.Permissions) != 1 || body.Permissions[0] != "view" {
		t.Errorf("Permissions = %v, want [view]", body.Permissions)
	}

	if body.Admin {
		t.Error("Admin = true for a non-admin session")
	}
}

func TestHandleSessionInfoReportsAdminForPermissionAdminHolder(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)
	token := mustCreateSession(t, sessions, "erin", permission.PermissionAdmin)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()

	handleSessionInfo(sessions, true, "admin", false)(rec, req)

	var body sessionInfoJSON
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if !body.Admin {
		t.Error("Admin = false for a session holding PermissionAdmin, want true")
	}
}

func TestHandleSessionInfoAuthDisabledReportsFullAccess(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/session", nil)
	rec := httptest.NewRecorder()

	handleSessionInfo(sessions, false, "", true)(rec, req)

	var body sessionInfoJSON
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if !body.Admin {
		t.Error("Admin = false with auth disabled, want true (everything's open anyway)")
	}

	if len(body.Permissions) != 5 {
		t.Errorf("Permissions = %v, want view, download, admin, login-log, and download-log", body.Permissions)
	}

	if !body.OIDCEnabled {
		t.Error("OIDCEnabled = false, want true (passed through from StartWebUI's oidcAuth != nil)")
	}
}

// TestHandleWebUILoginConfigAdminGrantsEveryPermissionBit guards against a
// session created for the single config-file admin (webui.username/
// webui.password) coming back with only PermissionAdmin set: that alone
// would satisfy every Can* check server-side (PermissionAdmin implies the
// rest), but sessionInfoJSON.Permissions (see Names) only lists directly
// granted bits, and the dashboard's own JavaScript checks that list
// literally (e.g. canDownload, canViewLoginLog) — so a session meant to
// look and behave like full access needs every bit set explicitly.
func TestHandleWebUILoginConfigAdminGrantsEveryPermissionBit(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)

	form := url.Values{"username": {"admin"}, "password": {"secret"}}
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

	perm := sessions.permissionsFor(&http.Request{Header: http.Header{"Authorization": {"Bearer " + body.Token}}})

	want := permission.PermissionView | permission.PermissionDownload | permission.PermissionAdmin | permission.PermissionViewLoginLog | permission.PermissionViewDownloadLog
	if perm != want {
		t.Errorf("session permissions = %v, want %v", perm, want)
	}

	if names := perm.Names(); len(names) != 5 {
		t.Errorf("session permission names = %v, want all 5 permissions listed individually", names)
	}
}

func TestHandleWebUILoginDBUserGrantsStoredPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	sessions := newTestSessionStore(t)

	if err := db.SaveUser(context.Background(), "bob", "s3cret", "", permission.PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	form := url.Values{"username": {"bob"}, "password": {"s3cret"}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	// No config-file admin ("admin"/"secret") matches "bob", so this only
	// succeeds via the db-backed fallback in handleWebUILogin.
	handleWebUILogin("admin", "secret", false, sessions, db, discardLogger, false)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body loginResponseJSON
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	perm := sessions.permissionsFor(&http.Request{Header: http.Header{"Authorization": {"Bearer " + body.Token}}})
	if perm != permission.PermissionView {
		t.Errorf("session permissions = %v, want %v", perm, permission.PermissionView)
	}
}

func TestHandleWebUIUserAdminAPILifecycle(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	create := handleCreateUser(db, "admin", discardLogger)
	list := handleListUsers(db, discardLogger)
	update := handleUpdateUser(db, discardLogger)
	del := handleDeleteUser(db, discardLogger)

	// Create.
	body := strings.NewReader(`{"username":"carol","password":"pw123456","permissions":["view"]}`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/users", body)
	rec := httptest.NewRecorder()
	create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	// Reject a username colliding with the config admin.
	body = strings.NewReader(`{"username":"admin","password":"pw123456","permissions":["view"]}`)
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/users", body)
	rec = httptest.NewRecorder()
	create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("create with the admin's own username status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// List.
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/users", nil)
	rec = httptest.NewRecorder()
	list(rec, req)

	var users []userJSON
	if err := json.NewDecoder(rec.Body).Decode(&users); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}

	if len(users) != 1 || users[0].Username != "carol" {
		t.Fatalf("list = %+v, want exactly [carol]", users)
	}

	// Update permissions (and leave the password unchanged).
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/users/carol", strings.NewReader(`{"permissions":["view","download"]}`))
	req.SetPathValue("username", "carol")

	rec = httptest.NewRecorder()
	update(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("update status = %d, want %d, body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	perm, ok, err := db.VerifyUser(context.Background(), "carol", "pw123456")
	if err != nil || !ok {
		t.Fatalf("VerifyUser() after update = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if want := permission.PermissionView | permission.PermissionDownload; perm != want {
		t.Errorf("permissions after update = %v, want %v", perm, want)
	}

	// Delete.
	req = httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/users/carol", nil)
	req.SetPathValue("username", "carol")

	rec = httptest.NewRecorder()
	del(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	if _, ok, _ := db.VerifyUser(context.Background(), "carol", "pw123456"); ok {
		t.Error("carol still verifies after being deleted")
	}

	// Deleting again reports 404.
	req = httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/users/carol", nil)
	req.SetPathValue("username", "carol")

	rec = httptest.NewRecorder()
	del(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("delete of an already-deleted user status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleIssueWebUIUserToken(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	sessions := newTestSessionStore(t)

	if err := db.SaveUser(context.Background(), "erin", "s3cret1", "", permission.PermissionView|permission.PermissionDownload); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	issue := handleIssueWebUIUserToken(sessions, db, discardLogger)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/users/erin/tokens", strings.NewReader(`{"days":90}`))
	req.SetPathValue("username", "erin")

	rec := httptest.NewRecorder()
	issue(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body loginResponseJSON
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if body.Token == "" {
		t.Fatal("Token is empty")
	}

	authedReq := &http.Request{Header: http.Header{"Authorization": {"Bearer " + body.Token}}}

	if username := sessions.usernameFor(authedReq); username != "erin" {
		t.Errorf("token username = %q, want %q", username, "erin")
	}

	if perm := sessions.permissionsFor(authedReq); perm != permission.PermissionView|permission.PermissionDownload {
		t.Errorf("token permissions = %v, want %v", perm, permission.PermissionView|permission.PermissionDownload)
	}

	if want := time.Now().Add(90 * 24 * time.Hour); body.ExpiresAt.Before(want.Add(-time.Minute)) || body.ExpiresAt.After(want.Add(time.Minute)) {
		t.Errorf("ExpiresAt = %v, want roughly %v", body.ExpiresAt, want)
	}
}

func TestHandleIssueWebUIUserTokenUnknownUser(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	sessions := newTestSessionStore(t)

	issue := handleIssueWebUIUserToken(sessions, db, discardLogger)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/users/nobody/tokens", strings.NewReader(`{"days":90}`))
	req.SetPathValue("username", "nobody")

	rec := httptest.NewRecorder()
	issue(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleIssueWebUIUserTokenRejectsOutOfRangeDays(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	sessions := newTestSessionStore(t)

	if err := db.SaveUser(context.Background(), "erin", "s3cret1", "", permission.PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	issue := handleIssueWebUIUserToken(sessions, db, discardLogger)

	for _, days := range []string{"0", "-1", "3651"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/users/erin/tokens", strings.NewReader(`{"days":`+days+`}`))
		req.SetPathValue("username", "erin")

		rec := httptest.NewRecorder()
		issue(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("days=%s status = %d, want %d", days, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestHandleIssueWebUIUserTokenRequiresDB(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)
	issue := handleIssueWebUIUserToken(sessions, nil, discardLogger)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/users/erin/tokens", strings.NewReader(`{"days":90}`))
	req.SetPathValue("username", "erin")

	rec := httptest.NewRecorder()
	issue(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestHandleUpdateUserOIDCLinkLifecycle drives link/reject-conflict/unlink
// in order (each stage depends on state the previous one left in db)
// against handleUpdateUser's oidc_username field — the merged "Users"
// admin API's replacement for the old dedicated /api/oidc-users endpoints.
func TestHandleUpdateUserOIDCLinkLifecycle(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	if err := db.SaveUser(context.Background(), "dave", "pw123456", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SaveUser(context.Background(), "erin", "pw123456", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	list := handleListUsers(db, discardLogger)
	update := handleUpdateUser(db, discardLogger)

	// Link dave to an OIDC identity.
	putUser(t, update, "dave", `{"permissions":["view"],"oidc_username":"dave@example.com"}`, http.StatusNoContent)
	requireUserOIDCUsername(t, list, "dave", "dave@example.com")

	// Linking a different user to the same identity is rejected, and
	// leaves that user unlinked.
	putUser(t, update, "erin", `{"permissions":["view"],"oidc_username":"dave@example.com"}`, http.StatusConflict)
	requireUserOIDCUsername(t, list, "erin", "")

	// Unlinking dave clears it.
	putUser(t, update, "dave", `{"permissions":["view"],"oidc_username":""}`, http.StatusNoContent)
	requireUserOIDCUsername(t, list, "dave", "")
}

// putUser PUTs body for username through update and requires the response
// status to be wantStatus.
func putUser(t *testing.T, update http.HandlerFunc, username, body string, wantStatus int) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/users/"+username, strings.NewReader(body))
	req.SetPathValue("username", username)

	rec := httptest.NewRecorder()
	update(rec, req)

	if rec.Code != wantStatus {
		t.Fatalf("update status = %d, want %d, body: %s", rec.Code, wantStatus, rec.Body.String())
	}
}

// requireUserOIDCUsername requires list to report username's OIDCUsername
// as exactly want.
func requireUserOIDCUsername(t *testing.T, list http.HandlerFunc, username, want string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/users", nil)
	rec := httptest.NewRecorder()
	list(rec, req)

	var users []userJSON
	if err := json.NewDecoder(rec.Body).Decode(&users); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}

	for _, u := range users {
		if u.Username == username {
			if u.OIDCUsername != want {
				t.Errorf("%s.OIDCUsername = %q, want %q", username, u.OIDCUsername, want)
			}

			return
		}
	}

	t.Fatalf("list = %+v, want an entry for %q", users, username)
}
