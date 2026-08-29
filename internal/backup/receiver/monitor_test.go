package receiver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
)

// testServerIdentity builds a *backup.ServerIdentity backed by a freshly
// generated RSA key, for tests that need one to exercise signing without
// touching disk.
func testServerIdentity(t *testing.T) *identity.ServerIdentity {
	t.Helper()

	id, _ := testServerIdentityAndKey(t)

	return id
}

// testServerIdentityAndKey is testServerIdentity, additionally returning the
// generated private key directly, for tests that need to verify a signature
// against its public half (ServerIdentity itself keeps the key private).
func testServerIdentityAndKey(t *testing.T) (*identity.ServerIdentity, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	return identity.NewTestServerIdentity("test-sender-uuid", key), key
}

// discardLogger is a *slog.Logger that writes nowhere, for tests that need
// to pass one but don't assert on its output.
var discardLogger = slog.New(slog.DiscardHandler)

// openTestStateDB opens a fresh state db under t.TempDir(), closed
// automatically when the test ends.
func openTestStateDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.db")

	db, err := backup.OpenScheduleStateDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
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

func TestRenderStaleWebhookPayload(t *testing.T) {
	t.Parallel()

	recv := backup.ResolvedReceiver{ID: "a", Path: "/mnt/a", StaleAfter: 6 * time.Hour}
	tmpl := "{receiver_id} at {path} stale after {stale_after}, last received {last_received}"
	lastSeen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	got := renderStaleWebhookPayload(tmpl, recv, lastSeen)
	want := "a at /mnt/a stale after 6h0m0s, last received 2026-01-02T03:04:05Z"

	if got != want {
		t.Errorf("renderStaleWebhookPayload() = %q, want %q", got, want)
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

	recv := backup.ResolvedReceiver{ID: "a", Path: root, StaleAfter: time.Hour, Webhook: backup.ResolvedWebhook{URL: srv.URL, Method: http.MethodPost}}

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

	recv := backup.ResolvedReceiver{ID: "recv-a", Path: root, StaleAfter: time.Hour, Webhook: backup.ResolvedWebhook{URL: srv.URL, Method: http.MethodPost}}

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

	recv := backup.ResolvedReceiver{ID: "a", Path: t.TempDir(), StaleAfter: time.Hour, Webhook: backup.ResolvedWebhook{URL: srv.URL, Method: http.MethodPost}}

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

	recv := backup.ResolvedReceiver{ID: "a", Path: root, StaleAfter: time.Hour, Webhook: backup.ResolvedWebhook{URL: srv.URL, Method: http.MethodPost}}

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

	recv := backup.ResolvedReceiver{ID: "a", Path: t.TempDir(), Webhook: backup.ResolvedWebhook{URL: srv.URL, Method: http.MethodPost}} // staleAfter left at zero

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

	recv := backup.ResolvedReceiver{
		ID: "recv-a", Path: root, StaleAfter: time.Hour,
		Webhook: backup.ResolvedWebhook{
			URL:     srv.URL,
			Method:  http.MethodPut,
			Headers: map[string]string{"Content-Type": "application/json; charset=utf-8", "Authorization": "Bearer tok"},
			Body:    `{"text":"receiver {receiver_id} has been quiet since {last_received}"}`,
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

	recv := backup.ResolvedReceiver{
		ID: "a", Path: root, StaleAfter: time.Hour,
		Webhook: backup.ResolvedWebhook{URL: srv.URL, Method: http.MethodPost},
	}

	newStaleReceiverMonitor().check(recv, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if gotCT != defaultStaleWebhookContentType {
		t.Errorf("webhook Content-Type = %q, want %q", gotCT, defaultStaleWebhookContentType)
	}
}

func TestSeedReceiverStatusFromState(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if err := backup.RecordReceiverEvent(ctx, db, backup.ReceiverEvent{At: at, ReceiverID: "a", Kind: backup.ReceiverEventReceive, Key: "obj.gpg", Success: true}); err != nil {
		t.Fatalf("RecordReceiverEvent() error: %v", err)
	}

	receivers := map[string]backup.ResolvedReceiver{
		"a": {ID: "a", Path: t.TempDir()},
		"b": {ID: "b", Path: t.TempDir()}, // no events recorded: stays idle
	}
	store := backup.NewReceiverStatusStore(receivers)

	SeedReceiverStatusFromState(ctx, db, receivers, store, discardLogger)

	snap := store.Snapshot()

	byID := make(map[string]backup.ReceiverSnapshot, len(snap))
	for _, s := range snap {
		byID[s.ID] = s
	}

	if got := byID["a"]; got.State != backup.StateOK || got.LastKey != "obj.gpg" || !got.LastSeen.Equal(at) {
		t.Errorf("receiver a = %+v, want state ok, last_key obj.gpg, last_seen %v", got, at)
	}

	if got := byID["b"]; got.State != backup.StateIdle {
		t.Errorf("receiver b = %+v, want state idle", got)
	}
}
