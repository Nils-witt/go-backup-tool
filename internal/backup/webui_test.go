package backup

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
)

func TestHandleDashboardServesHTML(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handleDashboard(rec, req)

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
	store.starting("test")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()

	handleStatus(store)(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}

	var jobs []jobSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(jobs) != 1 || jobs[0].Name != "test" || jobs[0].State != stateRunning {
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

	receivers := map[string]resolvedReceiver{
		"a": {id: "a", path: root, staleAfter: time.Hour, webhook: resolvedWebhook{url: "https://example.com/hook", method: http.MethodPost}},
	}
	store := newReceiverStatusStore(receivers)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers", nil)
	rec := httptest.NewRecorder()

	handleReceiverStatus(receivers, store, discardLogger)(rec, req)

	var snapshots []receiverSnapshot
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

	receivers := map[string]resolvedReceiver{
		"a": {id: "a", path: root, staleAfter: time.Hour, webhook: resolvedWebhook{url: "https://example.com/hook", method: http.MethodPost}},
	}
	store := newReceiverStatusStore(receivers)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers", nil)
	rec := httptest.NewRecorder()

	handleReceiverStatus(receivers, store, discardLogger)(rec, req)

	var snapshots []receiverSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshots); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(snapshots) != 1 || snapshots[0].StaleAfter != time.Hour.String() || snapshots[0].Stale {
		t.Errorf("snapshots = %+v, want stale_after %q and stale false", snapshots, time.Hour.String())
	}
}

func TestHandleReceiverStatusWithoutStaleAfterOmitsStaleness(t *testing.T) {
	t.Parallel()

	receivers := map[string]resolvedReceiver{"a": {id: "a", path: t.TempDir()}}
	store := newReceiverStatusStore(receivers)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers", nil)
	rec := httptest.NewRecorder()

	handleReceiverStatus(receivers, store, discardLogger)(rec, req)

	var snapshots []receiverSnapshot
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

	srv := startWebUI("127.0.0.1:0", store, nil, discardLogger, nil, nil, "", "", nil, nil)
	if srv == nil {
		t.Fatal("startWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.shutdown)

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
	h := requireWebUISession(false, sessions, true, func(http.ResponseWriter, *http.Request) { called = true })

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

func TestRequireWebUISessionRedirectsWithoutValidSession(t *testing.T) {
	t.Parallel()

	called := false
	sessions := newSessionStore()
	h := requireWebUISession(true, sessions, true, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	h(rec, req)

	if called {
		t.Error("handler was called despite no session cookie")
	}

	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login?next=") {
		t.Errorf("Location = %q, want a /login?next=... redirect", loc)
	}
}

func TestRequireWebUISessionReportsUnauthorizedWithoutRedirect(t *testing.T) {
	t.Parallel()

	called := false
	sessions := newSessionStore()
	h := requireWebUISession(true, sessions, false, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()

	h(rec, req)

	if called {
		t.Error("handler was called despite no session cookie")
	}

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestRequireWebUISessionAcceptsValidSession(t *testing.T) {
	t.Parallel()

	called := false
	sessions := newSessionStore()

	id, err := sessions.create("alice")
	if err != nil {
		t.Fatalf("sessions.create(): %v", err)
	}

	h := requireWebUISession(true, sessions, true, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: webUISessionCookie, Value: id}) //nolint:gosec // a request Cookie header, not a Set-Cookie response; Secure/HttpOnly/SameSite don't apply

	rec := httptest.NewRecorder()

	h(rec, req)

	if !called {
		t.Error("handler wasn't called despite a valid session cookie")
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestStartWebUIWithLoginRequiresSession(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	srv := startWebUI("127.0.0.1:0", store, nil, discardLogger, nil, nil, "admin", "secret", nil, nil)
	if srv == nil {
		t.Fatal("startWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.shutdown)

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

	_ = loginResp.Body.Close()

	cookies := loginResp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != webUISessionCookie {
		t.Fatalf("login cookies = %+v, want one %s cookie", cookies, webUISessionCookie)
	}

	req.AddCookie(cookies[0])

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

	receivers := map[string]resolvedReceiver{"a": {id: "a", path: root}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/files", nil)
	req.SetPathValue("id", "a")

	rec := httptest.NewRecorder()

	handleReceiverFiles(receivers, discardLogger)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var files []receiverFile
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

	handleReceiverFiles(map[string]resolvedReceiver{}, discardLogger)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestStartWebUIBadAddrReturnsNil(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	// Port 0 is valid (means "pick one"); an unparseable address is not.
	srv := startWebUI("not-a-valid-address", store, nil, discardLogger, nil, nil, "", "", nil, nil)
	if srv != nil {
		t.Cleanup(srv.shutdown)
		t.Fatal("startWebUI() with an invalid address = non-nil, want nil")
	}
}

func TestHandleDownloadFileServesContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "secret data")

	receivers := map[string]resolvedReceiver{"a": {id: "a", path: root}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg", nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")

	rec := httptest.NewRecorder()

	handleDownloadFile(receivers, discardLogger, nil, newSessionStore())(rec, req)

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

func TestHandleWebUILoginWrongCredentialsDoesNotStartSession(t *testing.T) {
	t.Parallel()

	sessions := newSessionStore()

	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	handleWebUILogin("admin", "secret", false, sessions, nil, discardLogger)(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (re-rendered form)", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "incorrect username or password") {
		t.Error("response body doesn't mention the incorrect credentials")
	}

	if len(rec.Result().Cookies()) != 0 {
		t.Error("a session cookie was set despite the wrong password")
	}
}

func TestHandleWebUILoginWithoutUsernameConfiguredRedirects(t *testing.T) {
	t.Parallel()

	sessions := newSessionStore()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login?next=/", nil)
	rec := httptest.NewRecorder()

	handleWebUILogin("", "", false, sessions, nil, discardLogger)(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want %q", loc, "/")
	}
}

func TestHandleWebUILoginCorrectCredentialsStartsSessionAndRedirects(t *testing.T) {
	t.Parallel()

	sessions := newSessionStore()

	form := url.Values{"username": {"admin"}, "password": {"secret"}, "next": {"/"}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	handleWebUILogin("admin", "secret", false, sessions, nil, discardLogger)(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want %q", loc, "/")
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != webUISessionCookie {
		t.Fatalf("cookies = %+v, want one %s cookie", cookies, webUISessionCookie)
	}

	if !sessions.valid(cookies[0].Value) {
		t.Error("the session cookie's value isn't a valid session")
	}
}

func TestHandleWebUILogoutRevokesSession(t *testing.T) {
	t.Parallel()

	sessions := newSessionStore()

	id, err := sessions.create("alice")
	if err != nil {
		t.Fatalf("sessions.create(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: webUISessionCookie, Value: id}) //nolint:gosec // a request Cookie header, not a Set-Cookie response; Secure/HttpOnly/SameSite don't apply

	rec := httptest.NewRecorder()

	handleWebUILogout(sessions)(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}

	if sessions.valid(id) {
		t.Error("session is still valid after logout")
	}
}

func TestLogRingBufferSnapshotOrdersOldestFirst(t *testing.T) {
	t.Parallel()

	buf := newLogRingBuffer(3)

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

	buf := newLogRingBuffer(2)

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

	buf := newLogRingBuffer(10)
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

func TestHandleWebUILoginRecordsLoginEvents(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	sessions := newSessionStore()

	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.1:4321"

	rec := httptest.NewRecorder()

	handleWebUILogin("admin", "secret", false, sessions, db, discardLogger)(rec, req)

	form = url.Values{"username": {"admin"}, "password": {"secret"}}
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.2:4321"

	rec = httptest.NewRecorder()

	handleWebUILogin("admin", "secret", false, sessions, db, discardLogger)(rec, req)

	events, err := readLoginEvents(t.Context(), db, 10)
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

func TestHandleLoginEventsServesJSON(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	if err := recordLoginEvent(t.Context(), db, loginEvent{At: time.Now(), Username: "admin", Method: "password", Success: true, RemoteAddr: "127.0.0.1:1"}); err != nil {
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
	sessions := newSessionStore()

	id, err := sessions.create("alice")
	if err != nil {
		t.Fatalf("sessions.create() error: %v", err)
	}

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "secret data")

	receivers := map[string]resolvedReceiver{"a": {id: "a", path: root}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg", nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")
	req.AddCookie(&http.Cookie{Name: webUISessionCookie, Value: id}) //nolint:gosec // a request Cookie header, not a Set-Cookie response; Secure/HttpOnly/SameSite don't apply
	req.RemoteAddr = "198.51.100.1:4321"

	handleDownloadFile(receivers, discardLogger, db, sessions)(httptest.NewRecorder(), req)

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/missing.gpg", nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "missing.gpg")
	req.RemoteAddr = "198.51.100.2:4321"

	handleDownloadFile(receivers, discardLogger, db, sessions)(httptest.NewRecorder(), req)

	events, err := readDownloadEvents(t.Context(), db, 10)
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

	if err := recordDownloadEvent(t.Context(), db, downloadEvent{At: time.Now(), Username: "admin", ReceiverID: "a", Key: "backup.gpg", Success: true, RemoteAddr: "127.0.0.1:1"}); err != nil {
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
