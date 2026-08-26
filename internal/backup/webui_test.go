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

	srv := startWebUI("127.0.0.1:0", store, nil, discardLogger, nil, "", nil)
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
	srv := startWebUI("not-a-valid-address", store, nil, discardLogger, nil, "", nil)
	if srv != nil {
		t.Cleanup(srv.shutdown)
		t.Fatal("startWebUI() with an invalid address = non-nil, want nil")
	}
}

func TestHandleDownloadFileWithoutSessionRedirectsToLogin(t *testing.T) {
	t.Parallel()

	receivers := map[string]resolvedReceiver{"a": {id: "a", path: t.TempDir()}}
	sessions := newDownloadSessionStore()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg", nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")

	rec := httptest.NewRecorder()

	handleDownloadFile(receivers, sessions, discardLogger)(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login?next=") {
		t.Errorf("Location = %q, want a /login?next=... redirect", loc)
	}
}

func TestHandleDownloadFileWithSessionServesContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "secret data")

	receivers := map[string]resolvedReceiver{"a": {id: "a", path: root}}
	sessions := newDownloadSessionStore()

	id, err := sessions.create()
	if err != nil {
		t.Fatalf("sessions.create(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg", nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")
	req.AddCookie(&http.Cookie{Name: downloadSessionCookie, Value: id}) //nolint:gosec // a request Cookie header, not a Set-Cookie response; Secure/HttpOnly/SameSite don't apply

	rec := httptest.NewRecorder()

	handleDownloadFile(receivers, sessions, discardLogger)(rec, req)

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

func TestHandleLoginWrongTokenDoesNotStartSession(t *testing.T) {
	t.Parallel()

	sessions := newDownloadSessionStore()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader("token=wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	handleLogin("correct-token", sessions)(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (re-rendered form)", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "incorrect token") {
		t.Error("response body doesn't mention the incorrect token")
	}

	if len(rec.Result().Cookies()) != 0 {
		t.Error("a session cookie was set despite the wrong token")
	}
}

func TestHandleLoginEmptyDownloadTokenIsAlwaysUnconfigured(t *testing.T) {
	t.Parallel()

	sessions := newDownloadSessionStore()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader("token="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	handleLogin("", sessions)(rec, req)

	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Error("response body doesn't report downloads as unconfigured")
	}

	if len(rec.Result().Cookies()) != 0 {
		t.Error("a session cookie was set despite download-token being unset")
	}
}

func TestHandleLoginCorrectTokenStartsSessionAndRedirects(t *testing.T) {
	t.Parallel()

	sessions := newDownloadSessionStore()

	form := url.Values{"token": {"correct-token"}, "next": {"/api/receivers/a/download/backup.gpg"}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	handleLogin("correct-token", sessions)(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	if loc := rec.Header().Get("Location"); loc != "/api/receivers/a/download/backup.gpg" {
		t.Errorf("Location = %q, want the requested next path", loc)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != downloadSessionCookie {
		t.Fatalf("cookies = %+v, want one %s cookie", cookies, downloadSessionCookie)
	}

	if !sessions.valid(cookies[0].Value) {
		t.Error("the session cookie's value isn't a valid session")
	}
}

func TestHandleLogoutRevokesSession(t *testing.T) {
	t.Parallel()

	sessions := newDownloadSessionStore()

	id, err := sessions.create()
	if err != nil {
		t.Fatalf("sessions.create(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: downloadSessionCookie, Value: id}) //nolint:gosec // a request Cookie header, not a Set-Cookie response; Secure/HttpOnly/SameSite don't apply

	rec := httptest.NewRecorder()

	handleLogout(sessions)(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
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
