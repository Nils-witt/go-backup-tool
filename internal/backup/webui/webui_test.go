package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
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

	handleDashboard(dashboardIndexHTML)(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}

	if !strings.Contains(rec.Body.String(), `<div id="root">`) {
		t.Error("dashboard HTML doesn't contain the SPA's root element")
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

// handleMeta reports the configured instance name (trimmed), falling back to
// INSTANCE_NAME, so the SPA can show it on the login page and in the app bar.
func TestHandleMetaIncludesInstanceName(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		env        string
		want       string
	}{
		{name: "env fallback", configured: "", env: "  prod-nas  ", want: "prod-nas"},
		{name: "config wins over env", configured: "  primary  ", env: "prod-nas", want: "primary"},
		{name: "blank config falls back", configured: "   ", env: "prod-nas", want: "prod-nas"},
		{name: "neither set", configured: "", env: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(instanceNameEnv, tt.env)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/meta", nil)
			rec := httptest.NewRecorder()

			handleMeta(tt.configured)(rec, req)

			var meta metaJSON
			if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
				t.Fatalf("decoding response body: %v", err)
			}

			if meta.InstanceName != tt.want {
				t.Errorf("InstanceName = %q, want %q", meta.InstanceName, tt.want)
			}
		})
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

	receivers := map[string]config.ResolvedReceiver{
		"a": {ID: "a", Path: root, StaleAfter: time.Hour},
	}
	store := backup.NewReceiverStatusStore(receivers)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers", nil)
	rec := httptest.NewRecorder()

	handleReceiverStatus(backup.NewReceiverRegistry(receivers), store, discardLogger)(rec, req)

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

	receivers := map[string]config.ResolvedReceiver{
		"a": {ID: "a", Path: root, StaleAfter: time.Hour},
	}
	store := backup.NewReceiverStatusStore(receivers)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers", nil)
	rec := httptest.NewRecorder()

	handleReceiverStatus(backup.NewReceiverRegistry(receivers), store, discardLogger)(rec, req)

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

	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}}
	store := backup.NewReceiverStatusStore(receivers)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers", nil)
	rec := httptest.NewRecorder()

	handleReceiverStatus(backup.NewReceiverRegistry(receivers), store, discardLogger)(rec, req)

	var snapshots []backup.ReceiverSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshots); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(snapshots) != 1 || snapshots[0].StaleAfter != "" || snapshots[0].Stale {
		t.Errorf("snapshots = %+v, want empty stale_after and stale false", snapshots)
	}
}

func TestHandleRetryFailedTargetsUnknownJobReturns404(t *testing.T) {
	t.Parallel()

	statusStore, job := newTestStore()
	jobs := pipeline.StaticJobs([]*config.Config{job})
	runner := pipeline.NewRunner(discardLogger, statusStore, nil, nil, nil, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/jobs/nope/retry", nil)
	req.SetPathValue("name", "nope")

	rec := httptest.NewRecorder()

	handleRetryFailedTargets(jobs, statusStore, runner, discardLogger)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleRetryFailedTargetsNoFailedTargetsReturns409(t *testing.T) {
	t.Parallel()

	statusStore, job := newTestStore()
	jobs := pipeline.StaticJobs([]*config.Config{job})
	runner := pipeline.NewRunner(discardLogger, statusStore, nil, nil, nil, nil)

	// No run has happened yet, so every target is idle, not failed.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/jobs/test/retry", nil)
	req.SetPathValue("name", job.Name)

	rec := httptest.NewRecorder()

	handleRetryFailedTargets(jobs, statusStore, runner, discardLogger)(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

// TestHandleRetryFailedTargetsKicksOffRetry is an end-to-end check (real
// gpg) that a POST against a job with a failed target returns 202
// immediately and, in the background, actually retries that target —
// verified by polling /api/status until it turns ok, the same way
// TestRunOnceRefreshesEachTargetIndependently does for a live run.
func TestHandleRetryFailedTargetsKicksOffRetry(t *testing.T) {
	homedir := testGPGKeyring(t)

	t.Parallel()

	dir := t.TempDir()

	job := &config.Config{
		Name:       "test",
		Cmd:        "echo hi",
		Key:        "backup-{time}.gpg",
		Recipients: []string{testGPGRecipient},
		GPGBin:     "gpg",
		GPGHomedir: homedir,
		Targets: []config.Target{
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	statusStore := backup.NewStatusStore([]*config.Config{job})
	statusStore.Starting(job.Name)
	statusStore.TargetDone(job.Name, 0, context.DeadlineExceeded) // simulate a prior failure
	statusStore.Finished(job.Name, context.DeadlineExceeded, 0)

	jobs := pipeline.StaticJobs([]*config.Config{job})
	runner := pipeline.NewRunner(discardLogger, statusStore, nil, nil, nil, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/jobs/test/retry", nil)
	req.SetPathValue("name", job.Name)

	rec := httptest.NewRecorder()

	handleRetryFailedTargets(jobs, statusStore, runner, discardLogger)(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	deadline := time.Now().Add(5 * time.Second)

	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the background retry to finish")
		}

		if statusStore.Snapshot()[0].Targets[0].State == backup.StateOK {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// TestStartWebUIWithoutOIDCIsLocked checks that a web UI with no SSO
// configured stays locked rather than open: the public endpoints still
// answer, every dashboard data endpoint reports 401.
func TestStartWebUIWithoutOIDCIsLocked(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	srv := StartWebUI("127.0.0.1:0", store, nil, nil, nil, nil, nil, nil, nil, nil, discardLogger, nil, nil, config.OIDCSettings{}, nil, false, false, "", nil, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	client := &http.Client{}

	for path, want := range map[string]int{
		"/api/meta":       http.StatusOK,
		"/api/sso/status": http.StatusOK,
		"/api/status":     http.StatusUnauthorized,
		"/api/me":         http.StatusUnauthorized,
	} {
		if got := webUIGetStatus(t, client, srv, "", path); got != want {
			t.Errorf("GET %s status = %d, want %d", path, got, want)
		}
	}
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

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode
}

func TestHandleReceiverFilesServesJSON(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "data")

	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: root}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/files", nil)
	req.SetPathValue("id", "a")

	rec := httptest.NewRecorder()

	handleReceiverFiles(backup.NewReceiverRegistry(receivers), discardLogger)(rec, req)

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

	handleReceiverFiles(backup.NewReceiverRegistry(nil), discardLogger)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestStartWebUIBadAddrReturnsNil(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	// Port 0 is valid (means "pick one"); an unparseable address is not.
	srv := StartWebUI("not-a-valid-address", store, nil, nil, nil, nil, nil, nil, nil, nil, discardLogger, nil, nil, config.OIDCSettings{}, nil, false, false, "", nil, nil)
	if srv != nil {
		t.Cleanup(srv.Shutdown)
		t.Fatal("StartWebUI() with an invalid address = non-nil, want nil")
	}
}

func TestHandleDownloadFileServesContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "secret data")

	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: root}}

	tickets := newDownloadTicketStore()

	ticket, err := tickets.create("a", "backup.gpg", "")
	if err != nil {
		t.Fatalf("tickets.create(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg?ticket="+ticket, nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")

	rec := httptest.NewRecorder()

	handleDownloadFile(backup.NewReceiverRegistry(receivers), discardLogger, nil, tickets, false, nil)(rec, req)

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

	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: root}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg", nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")

	rec := httptest.NewRecorder()

	handleDownloadFile(backup.NewReceiverRegistry(receivers), discardLogger, nil, newDownloadTicketStore(), false, nil)(rec, req)

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

func TestHandleLoginEventsServesJSON(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	if err := db.SaveLoginEvent(t.Context(), store.LoginEvent{At: time.Now(), Username: "admin", Method: "password", Success: true, RemoteAddr: "127.0.0.1:1"}); err != nil {
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

	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: root}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg?ticket="+ticket, nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")
	req.RemoteAddr = "198.51.100.1:4321"

	handleDownloadFile(backup.NewReceiverRegistry(receivers), discardLogger, db, tickets, false, nil)(httptest.NewRecorder(), req)

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

	handleDownloadFile(backup.NewReceiverRegistry(receivers), discardLogger, db, tickets, false, nil)(httptest.NewRecorder(), req)

	events, err := db.ListDownloadEvents(t.Context(), 10)
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

// TestHandleDownloadFileFiresDownloadWebhookOnSuccess is an end-to-end check
// that a successful download through handleDownloadFile fires the
// receiver's configured download-webhook (see
// receiver.NotifyDownloadWebhook), in a background goroutine so it doesn't
// delay the file already streamed to the caller.
func TestHandleDownloadFileFiresDownloadWebhookOnSuccess(t *testing.T) {
	t.Parallel()

	type call struct {
		user, file, receiver string
	}

	calls := make(chan call, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			User     string `json:"user"`
			File     string `json:"file"`
			Receiver string `json:"receiver"`
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("webhook server: decoding request body: %v", err)
		}

		calls <- call{user: body.User, file: body.File, receiver: body.Receiver}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.gpg"), "secret data")

	receivers := map[string]config.ResolvedReceiver{
		"a": {ID: "a", Path: root, DownloadNotifications: []string{"test"}, Notifications: notify.NewRegistry(map[string]notify.Notification{"test": {ID: "test", Webhook: &notify.Webhook{URL: srv.URL, Method: http.MethodPost}}})},
	}

	tickets := newDownloadTicketStore()

	ticket, err := tickets.create("a", "backup.gpg", "alice")
	if err != nil {
		t.Fatalf("tickets.create(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/backup.gpg?ticket="+ticket, nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "backup.gpg")

	handleDownloadFile(backup.NewReceiverRegistry(receivers), discardLogger, nil, tickets, false, nil)(httptest.NewRecorder(), req)

	select {
	case got := <-calls:
		want := call{user: "alice", file: "backup.gpg", receiver: "a"}
		if got != want {
			t.Errorf("webhook call = %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download-webhook was not called within 5s")
	}
}

// TestHandleDownloadFileDoesNotFireDownloadWebhookOnFailure checks that a
// failed download (key not found) never fires the configured
// download-webhook, only a successful one.
func TestHandleDownloadFileDoesNotFireDownloadWebhookOnFailure(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		requestSeen bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestSeen = true
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()

	receivers := map[string]config.ResolvedReceiver{
		"a": {ID: "a", Path: root, DownloadNotifications: []string{"test"}, Notifications: notify.NewRegistry(map[string]notify.Notification{"test": {ID: "test", Webhook: &notify.Webhook{URL: srv.URL, Method: http.MethodPost}}})},
	}

	tickets := newDownloadTicketStore()

	ticket, err := tickets.create("a", "missing.gpg", "alice")
	if err != nil {
		t.Fatalf("tickets.create(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receivers/a/download/missing.gpg?ticket="+ticket, nil)
	req.SetPathValue("id", "a")
	req.SetPathValue("key", "missing.gpg")

	rec := httptest.NewRecorder()
	handleDownloadFile(backup.NewReceiverRegistry(receivers), discardLogger, nil, tickets, false, nil)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	// There's no notification to wait on for the negative case, so give any
	// wrongly-fired goroutine a moment to reach the fake server first.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if requestSeen {
		t.Error("download-webhook fired for a failed download, want none")
	}
}

func TestHandleDownloadEventsServesJSON(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	if err := db.SaveDownloadEvent(t.Context(), store.DownloadEvent{At: time.Now(), Username: "admin", ReceiverID: "a", Key: "backup.gpg", Success: true, RemoteAddr: "127.0.0.1:1"}); err != nil {
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

func TestHandleReceiverEventsServesJSON(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	if err := db.SaveReceiverEvent(t.Context(), store.ReceiverEvent{At: time.Now(), ReceiverID: "a", Kind: store.ReceiverEventReceive, Key: "backup.gpg", Size: 42, Success: true}); err != nil {
		t.Fatalf("SaveReceiverEvent() error: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receiver-events", nil)
	rec := httptest.NewRecorder()

	handleReceiverEvents(db, discardLogger)(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}

	var got []receiverEventJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(got) != 1 || got[0].ReceiverID != "a" || got[0].Kind != store.ReceiverEventReceive || got[0].Key != "backup.gpg" || got[0].Size != 42 || !got[0].Success {
		t.Errorf("decoded events = %+v, want one successful receive of backup.gpg", got)
	}
}

func TestHandleReceiverEventsWithoutDBServesEmptyList(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/receiver-events", nil)
	rec := httptest.NewRecorder()

	handleReceiverEvents(nil, discardLogger)(rec, req)

	var got []receiverEventJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("decoded events = %+v, want none", got)
	}
}
