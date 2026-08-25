package backup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
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

func TestStartWebUIServesRequests(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	srv := startWebUI("127.0.0.1:0", store, nil, discardLogger, nil)
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
	srv := startWebUI("not-a-valid-address", store, nil, discardLogger, nil)
	if srv != nil {
		t.Cleanup(srv.shutdown)
		t.Fatal("startWebUI() with an invalid address = non-nil, want nil")
	}
}
