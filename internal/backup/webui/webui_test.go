package webui

import (
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
	sessions := newSessionStore()
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
	sessions := newSessionStore()
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
	sessions := newSessionStore()

	id, err := sessions.create("alice")
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

	sessions := newSessionStore()

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

	sessions := newSessionStore()

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

	sessions := newSessionStore()

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

	sessions := newSessionStore()

	id, err := sessions.create("alice")
	if err != nil {
		t.Fatalf("sessions.create(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/logout", nil)
	req.Header.Set("Authorization", "Bearer "+id)

	rec := httptest.NewRecorder()

	handleAPILogout(sessions)(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	if sessions.valid(id) {
		t.Error("session is still valid after logout")
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
	sessions := newSessionStore()

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
	sessions := newSessionStore()

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
