package backup

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestSanitizeObjectKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key     string
		wantErr bool
	}{
		{key: "backup-20260101.gpg", wantErr: false},
		{key: "subdir/backup-20260101.gpg", wantErr: false},
		{key: "", wantErr: true},
		{key: "/etc/passwd", wantErr: true},
		{key: "..", wantErr: true},
		{key: "../etc/passwd", wantErr: true},
		{key: "subdir/../../etc/passwd", wantErr: true},
		{key: "subdir//double-slash", wantErr: true},
		{key: ".", wantErr: true},
	}

	for _, tt := range tests {
		got, err := sanitizeObjectKey(tt.key)
		if (err != nil) != tt.wantErr {
			t.Errorf("sanitizeObjectKey(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			continue
		}

		if err == nil && got != tt.key {
			t.Errorf("sanitizeObjectKey(%q) = %q, want unchanged", tt.key, got)
		}
	}
}

func TestBuildReceivers(t *testing.T) {
	t.Parallel()

	receivers, err := buildReceivers([]fileReceiver{
		{ID: "a", Token: "tok-a", Path: "/mnt/a"},
		{ID: "b", Token: "tok-b", Path: "/mnt/b", Retention: "7d"},
	})
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := map[string]resolvedReceiver{
		"a": {id: "a", token: "tok-a", path: "/mnt/a"},
		"b": {id: "b", token: "tok-b", path: "/mnt/b", retention: 7 * 24 * time.Hour},
	}

	if len(receivers) != len(want) {
		t.Fatalf("buildReceivers() = %+v, want %+v", receivers, want)
	}

	for id, w := range want {
		if got := receivers[id]; !reflect.DeepEqual(got, w) {
			t.Errorf("buildReceivers()[%q] = %+v, want %+v", id, got, w)
		}
	}
}

func TestBuildReceiversRequiresID(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]fileReceiver{{Token: "t", Path: "/mnt/a"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for missing id, got nil")
	}
}

func TestBuildReceiversRequiresPath(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]fileReceiver{{ID: "a", Token: "t"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for missing path, got nil")
	}
}

func TestListReceiverFilesMissingRoot(t *testing.T) {
	t.Parallel()

	recv := resolvedReceiver{id: "a", path: filepath.Join(t.TempDir(), "does-not-exist")}

	files, err := listReceiverFiles(recv)
	if err != nil {
		t.Fatalf("listReceiverFiles() unexpected error: %v", err)
	}

	if len(files) != 0 {
		t.Errorf("listReceiverFiles() = %+v, want empty", files)
	}
}

func TestListReceiverFilesListsNestedObjectsSortedByCreatedTimeAscending(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	newerPath := filepath.Join(root, "b.gpg")
	olderPath := filepath.Join(root, "subdir", "a.gpg")

	writeFile(t, newerPath, "bbb")
	writeFile(t, olderPath, "a")
	writeFile(t, filepath.Join(root, ".hidden.tmp"), "temp")

	older := time.Now().Add(-time.Hour)
	newer := time.Now()

	if err := os.Chtimes(olderPath, older, older); err != nil {
		t.Fatalf("os.Chtimes(%q) unexpected error: %v", olderPath, err)
	}

	if err := os.Chtimes(newerPath, newer, newer); err != nil {
		t.Fatalf("os.Chtimes(%q) unexpected error: %v", newerPath, err)
	}

	recv := resolvedReceiver{id: "a", path: root}

	files, err := listReceiverFiles(recv)
	if err != nil {
		t.Fatalf("listReceiverFiles() unexpected error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("listReceiverFiles() = %+v, want 2 entries", files)
	}

	if files[0].Key != "subdir/a.gpg" || files[0].Size != 1 {
		t.Errorf("files[0] = %+v, want key subdir/a.gpg size 1 (oldest)", files[0])
	}

	if files[1].Key != "b.gpg" || files[1].Size != 3 {
		t.Errorf("files[1] = %+v, want key b.gpg size 3 (newest)", files[1])
	}
}

// writeFile creates path (and any missing parent directories) with contents.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating directory for %q: %v", path, err)
	}

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %q: %v", path, err)
	}
}

func TestParseStaleAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty disables it", in: "", want: 0},
		{name: "hours", in: "6h", want: 6 * time.Hour},
		{name: "days", in: "1d", want: 24 * time.Hour},
		{name: "zero is not meaningful", in: "0", wantErr: true},
		{name: "negative is not meaningful", in: "-1h", wantErr: true},
		{name: "garbage", in: "not-a-duration", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseStaleAfter(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseStaleAfter(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}

			if err == nil && got != tt.want {
				t.Errorf("parseStaleAfter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildReceiversStaleAfterAndWebhook(t *testing.T) {
	t.Parallel()

	receivers, err := buildReceivers([]fileReceiver{
		{ID: "a", Token: "tok", Path: "/mnt/a", StaleAfter: "6h", Webhook: fileWebhook{URL: "https://example.com/hook"}},
	})
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := resolvedReceiver{
		id: "a", token: "tok", path: "/mnt/a",
		staleAfter: 6 * time.Hour,
		webhook:    resolvedWebhook{url: "https://example.com/hook", method: http.MethodPost},
	}

	if got := receivers["a"]; !reflect.DeepEqual(got, want) {
		t.Errorf("buildReceivers()[%q] = %+v, want %+v", "a", got, want)
	}
}

func TestBuildReceiversStaleAfterRequiresWebhook(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]fileReceiver{{ID: "a", Token: "t", Path: "/mnt/a", StaleAfter: "6h"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for stale-after without webhook.url, got nil")
	}
}

func TestBuildReceiversWebhookRequiresStaleAfter(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]fileReceiver{{ID: "a", Token: "t", Path: "/mnt/a", Webhook: fileWebhook{URL: "https://example.com/hook"}}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for webhook.url without stale-after, got nil")
	}
}

func TestBuildReceiversWebhookMethodHeadersAndBody(t *testing.T) {
	t.Parallel()

	receivers, err := buildReceivers([]fileReceiver{
		{
			ID: "a", Token: "tok", Path: "/mnt/a", StaleAfter: "6h",
			Webhook: fileWebhook{
				URL:     "https://example.com/hook",
				Method:  "put",
				Headers: map[string]string{"Content-Type": "application/json; charset=utf-8", "Authorization": "Bearer tok"},
				Body:    `{"text":"{receiver_id} stale since {last_received}"}`,
			},
		},
	})
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := resolvedWebhook{
		url:    "https://example.com/hook",
		method: http.MethodPut, // lowercase "put" in the config file is normalized to uppercase
		headers: map[string]string{
			"Content-Type":  "application/json; charset=utf-8",
			"Authorization": "Bearer tok",
		},
		body: `{"text":"{receiver_id} stale since {last_received}"}`,
	}

	if got := receivers["a"].webhook; !reflect.DeepEqual(got, want) {
		t.Errorf("buildReceivers()[%q].webhook = %+v, want %+v", "a", got, want)
	}
}

func TestBuildReceiversWebhookMethodDefaultsToPost(t *testing.T) {
	t.Parallel()

	receivers, err := buildReceivers([]fileReceiver{
		{ID: "a", Token: "t", Path: "/mnt/a", StaleAfter: "6h", Webhook: fileWebhook{URL: "https://example.com/hook"}},
	})
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	if got := receivers["a"].webhook.method; got != http.MethodPost {
		t.Errorf("webhook.method = %q, want %q when unset", got, http.MethodPost)
	}
}

func TestBuildReceiversWebhookBodyRequiresURL(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]fileReceiver{{ID: "a", Token: "t", Path: "/mnt/a", Webhook: fileWebhook{Body: "custom"}}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for webhook.body without webhook.url, got nil")
	}
}

func TestBuildReceiversWebhookHeadersRequireURL(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]fileReceiver{
		{ID: "a", Token: "t", Path: "/mnt/a", Webhook: fileWebhook{Headers: map[string]string{"X-Test": "y"}}},
	})
	if err == nil {
		t.Fatal("buildReceivers() expected error for webhook.headers without webhook.url, got nil")
	}
}

func TestBuildReceiversWebhookMethodRequiresURL(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]fileReceiver{{ID: "a", Token: "t", Path: "/mnt/a", Webhook: fileWebhook{Method: "PUT"}}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for webhook.method without webhook.url, got nil")
	}
}

func TestRenderStaleWebhookPayload(t *testing.T) {
	t.Parallel()

	recv := resolvedReceiver{id: "a", path: "/mnt/a", staleAfter: 6 * time.Hour}
	tmpl := "{receiver_id} at {path} stale after {stale_after}, last received {last_received}"
	lastSeen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	got := renderStaleWebhookPayload(tmpl, recv, lastSeen)
	want := "a at /mnt/a stale after 6h0m0s, last received 2026-01-02T03:04:05Z"

	if got != want {
		t.Errorf("renderStaleWebhookPayload() = %q, want %q", got, want)
	}
}

func TestLastReceivedAtMissingRoot(t *testing.T) {
	t.Parallel()

	recv := resolvedReceiver{id: "a", path: filepath.Join(t.TempDir(), "does-not-exist")}

	_, ok, err := lastReceivedAt(recv)
	if err != nil {
		t.Fatalf("lastReceivedAt() unexpected error: %v", err)
	}

	if ok {
		t.Error("lastReceivedAt() ok = true, want false for a missing root")
	}
}

func TestLastReceivedAtReturnsNewestModTime(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	older := filepath.Join(root, "older.gpg")
	newer := filepath.Join(root, "subdir", "newer.gpg")

	writeFile(t, older, "a")
	writeFile(t, newer, "b")
	writeFile(t, filepath.Join(root, ".hidden.tmp"), "temp")

	olderTime := time.Now().Add(-2 * time.Hour)
	newerTime := time.Now().Add(-1 * time.Hour)

	if err := os.Chtimes(older, olderTime, olderTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", older, err)
	}

	if err := os.Chtimes(newer, newerTime, newerTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", newer, err)
	}

	// A hidden temp file, if it were counted, would appear newest of all
	// (its default mtime is "now"), so this also verifies it's skipped.
	got, ok, err := lastReceivedAt(resolvedReceiver{id: "a", path: root})
	if err != nil {
		t.Fatalf("lastReceivedAt() unexpected error: %v", err)
	}

	if !ok {
		t.Fatal("lastReceivedAt() ok = false, want true")
	}

	if !got.Equal(newerTime) {
		t.Errorf("lastReceivedAt() = %v, want %v", got, newerTime)
	}
}

// webhookCall is one request captured by a test webhook server (see
// newTestWebhookServer).
type webhookCall struct {
	payload staleReceiverPayload
}

// newTestWebhookServer starts an httptest.Server that decodes every request
// body as a staleReceiverPayload and appends it to calls, guarded by mu so
// tests can safely read it after the monitor's check has returned.
func newTestWebhookServer(t *testing.T, mu *sync.Mutex, calls *[]webhookCall) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload staleReceiverPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("webhook server: decoding request body: %v", err)
		}

		mu.Lock()

		*calls = append(*calls, webhookCall{payload: payload})
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))

	t.Cleanup(srv.Close)

	return srv
}

func TestStaleReceiverMonitorCheckFreshFileDoesNotFire(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls []webhookCall
	)

	srv := newTestWebhookServer(t, &mu, &calls)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "recent.gpg"), "a")

	recv := resolvedReceiver{id: "a", path: root, staleAfter: time.Hour, webhook: resolvedWebhook{url: srv.URL, method: http.MethodPost}}

	newStaleReceiverMonitor().check(recv, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if len(calls) != 0 {
		t.Errorf("webhook calls = %+v, want none for a fresh file", calls)
	}
}

func TestStaleReceiverMonitorCheckStaleFileFires(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls []webhookCall
	)

	srv := newTestWebhookServer(t, &mu, &calls)

	root := t.TempDir()
	stale := filepath.Join(root, "old.gpg")
	writeFile(t, stale, "a")

	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", stale, err)
	}

	recv := resolvedReceiver{id: "recv-a", path: root, staleAfter: time.Hour, webhook: resolvedWebhook{url: srv.URL, method: http.MethodPost}}

	monitor := newStaleReceiverMonitor()
	monitor.check(recv, discardLogger)

	mu.Lock()
	if len(calls) != 1 {
		mu.Unlock()
		t.Fatalf("webhook calls = %d, want 1 for a stale file", len(calls))
	}

	got := calls[0].payload
	mu.Unlock()

	if got.ReceiverID != "recv-a" || got.Path != root || got.StaleAfter != time.Hour.String() || got.LastReceived.IsZero() {
		t.Errorf("webhook payload = %+v, want receiver_id recv-a, path %q, stale_after %q, non-zero last_received", got, root, time.Hour.String())
	}

	// A second check of the same still-stale gap must not fire again.
	monitor.check(recv, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if len(calls) != 1 {
		t.Errorf("webhook calls = %d after a second check of the same gap, want still 1", len(calls))
	}
}

func TestStaleReceiverMonitorCheckNeverReceivedDoesNotFire(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls []webhookCall
	)

	srv := newTestWebhookServer(t, &mu, &calls)

	recv := resolvedReceiver{id: "a", path: t.TempDir(), staleAfter: time.Hour, webhook: resolvedWebhook{url: srv.URL, method: http.MethodPost}}

	newStaleReceiverMonitor().check(recv, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if len(calls) != 0 {
		t.Errorf("webhook calls = %+v, want none when nothing has ever been received", calls)
	}
}

func TestStaleReceiverMonitorCheckRefiresAfterGapReopens(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls []webhookCall
	)

	srv := newTestWebhookServer(t, &mu, &calls)

	root := t.TempDir()
	f := filepath.Join(root, "file.gpg")
	writeFile(t, f, "a")

	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(f, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", f, err)
	}

	recv := resolvedReceiver{id: "a", path: root, staleAfter: time.Hour, webhook: resolvedWebhook{url: srv.URL, method: http.MethodPost}}

	monitor := newStaleReceiverMonitor()
	monitor.check(recv, discardLogger)

	// A fresh file arrives, clearing the gap.
	writeFile(t, f, "b")
	monitor.check(recv, discardLogger)

	// Stale again.
	if err := os.Chtimes(f, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", f, err)
	}

	monitor.check(recv, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if len(calls) != 2 {
		t.Errorf("webhook calls = %d, want 2 (one per gap)", len(calls))
	}
}

func TestStaleReceiverMonitorCheckDisabledIsNoop(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls []webhookCall
	)

	srv := newTestWebhookServer(t, &mu, &calls)

	recv := resolvedReceiver{id: "a", path: t.TempDir(), webhook: resolvedWebhook{url: srv.URL, method: http.MethodPost}} // staleAfter left at zero

	newStaleReceiverMonitor().check(recv, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if len(calls) != 0 {
		t.Errorf("webhook calls = %+v, want none when stale-after is unset", calls)
	}
}

func TestStaleReceiverMonitorCheckUsesCustomMethodHeadersAndBody(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		gotMethod   string
		gotBody     string
		gotCT       string
		gotAuth     string
		requestSeen bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("webhook server: reading request body: %v", err)
		}

		mu.Lock()

		gotMethod = r.Method
		gotBody = string(body)
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		requestSeen = true
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	stale := filepath.Join(root, "old.gpg")
	writeFile(t, stale, "a")

	staleTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", stale, err)
	}

	recv := resolvedReceiver{
		id: "recv-a", path: root, staleAfter: time.Hour,
		webhook: resolvedWebhook{
			url:     srv.URL,
			method:  http.MethodPut,
			headers: map[string]string{"Content-Type": "application/json; charset=utf-8", "Authorization": "Bearer tok"},
			body:    `{"text":"receiver {receiver_id} has been quiet since {last_received}"}`,
		},
	}

	newStaleReceiverMonitor().check(recv, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if !requestSeen {
		t.Fatal("webhook server: no request received")
	}

	if gotMethod != http.MethodPut {
		t.Errorf("webhook method = %q, want %q", gotMethod, http.MethodPut)
	}

	wantBody := `{"text":"receiver recv-a has been quiet since 2026-01-02T03:04:05Z"}`
	if gotBody != wantBody {
		t.Errorf("webhook body = %q, want %q", gotBody, wantBody)
	}

	if gotCT != "application/json; charset=utf-8" {
		t.Errorf("webhook Content-Type = %q, want %q", gotCT, "application/json; charset=utf-8")
	}

	if gotAuth != "Bearer tok" {
		t.Errorf("webhook Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
}

func TestStaleReceiverMonitorCheckDefaultContentTypeWhenNoHeadersSet(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		gotCT string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotCT = r.Header.Get("Content-Type")
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	stale := filepath.Join(root, "old.gpg")
	writeFile(t, stale, "a")

	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", stale, err)
	}

	recv := resolvedReceiver{
		id: "a", path: root, staleAfter: time.Hour,
		webhook: resolvedWebhook{url: srv.URL, method: http.MethodPost},
	}

	newStaleReceiverMonitor().check(recv, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if gotCT != defaultStaleWebhookContentType {
		t.Errorf("webhook Content-Type = %q, want %q", gotCT, defaultStaleWebhookContentType)
	}
}
