package receiver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

func TestRenderDownloadWebhookPayload(t *testing.T) {
	t.Parallel()

	recv := config.ResolvedReceiver{ID: "a", Path: "/mnt/a"}
	ev := DownloadWebhookEvent{Username: "alice", Key: "daily/backup.gpg", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	tmpl := "{user} downloaded {file} from {receiver} at {time}"

	got := renderDownloadWebhookPayload(tmpl, recv, ev)
	want := "alice downloaded daily/backup.gpg from a at 2026-01-02T03:04:05Z"

	if got != want {
		t.Errorf("renderDownloadWebhookPayload() = %q, want %q", got, want)
	}
}

func TestRenderDownloadWebhookPayloadServerName(t *testing.T) {
	t.Parallel()

	recv := config.ResolvedReceiver{ID: "a", Path: "/mnt/a", ServerName: "primary-backup-host"}
	ev := DownloadWebhookEvent{Username: "alice", Key: "k", At: time.Now()}

	got := renderDownloadWebhookPayload("[{server_name}] {user} downloaded {file}", recv, ev)

	want := "[primary-backup-host] alice downloaded k"
	if got != want {
		t.Errorf("renderDownloadWebhookPayload() = %q, want %q", got, want)
	}
}

func TestNotifyDownloadWebhookNoopWhenUnconfigured(t *testing.T) {
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

	recv := config.ResolvedReceiver{ID: "a", Path: "/mnt/a"} // no DownloadWebhook configured

	NotifyDownload(recv, DownloadWebhookEvent{Username: "alice", Key: "k", At: time.Now()}, nil, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if requestSeen {
		t.Error("NotifyDownload() sent a request, want none when download-webhook is unconfigured")
	}
}

func TestNotifyDownloadWebhookDefaultJSONPayload(t *testing.T) {
	t.Parallel()

	type payload struct {
		User     string `json:"user"`
		File     string `json:"file"`
		Receiver string `json:"receiver"`
	}

	var (
		mu    sync.Mutex
		got   payload
		gotCT string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("webhook server: decoding request body: %v", err)
		}

		gotCT = r.Header.Get("Content-Type")

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	recv := config.ResolvedReceiver{ID: "recv-a", Path: "/mnt/a", DownloadNotifications: []string{"test"}, Notifications: testWebhookRegistry(notify.Webhook{URL: srv.URL, Method: http.MethodPost})}

	NotifyDownload(recv, DownloadWebhookEvent{Username: "alice", Key: "daily/backup.gpg", At: time.Now()}, nil, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	want := payload{User: "alice", File: "daily/backup.gpg", Receiver: "recv-a"}
	if got != want {
		t.Errorf("webhook payload = %+v, want %+v", got, want)
	}

	if gotCT != defaultDownloadWebhookContentType {
		t.Errorf("webhook Content-Type = %q, want %q", gotCT, defaultDownloadWebhookContentType)
	}
}

func TestNotifyDownloadWebhookUsesCustomMethodHeadersAndBody(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		gotMethod   string
		gotBody     string
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
		gotAuth = r.Header.Get("Authorization")
		requestSeen = true
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	recv := config.ResolvedReceiver{
		ID: "recv-a", Path: "/mnt/a",
		DownloadNotifications: []string{"test"}, Notifications: testWebhookRegistry(notify.Webhook{
			URL:     srv.URL,
			Method:  http.MethodPut,
			Headers: map[string]string{"Authorization": "Bearer tok"},
			Body:    `{"text":"{user} downloaded {file}"}`,
		}),
	}

	NotifyDownload(recv, DownloadWebhookEvent{Username: "alice", Key: "daily/backup.gpg", At: time.Now()}, nil, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if !requestSeen {
		t.Fatal("webhook server: no request received")
	}

	if gotMethod != http.MethodPut {
		t.Errorf("webhook method = %q, want %q", gotMethod, http.MethodPut)
	}

	wantBody := `{"text":"alice downloaded daily/backup.gpg"}`
	if gotBody != wantBody {
		t.Errorf("webhook body = %q, want %q", gotBody, wantBody)
	}

	if gotAuth != "Bearer tok" {
		t.Errorf("webhook Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
}

func TestNotifyDownloadWebhookNonSuccessStatusIsLoggedNotReturned(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	recv := config.ResolvedReceiver{ID: "a", Path: "/mnt/a", DownloadNotifications: []string{"test"}, Notifications: testWebhookRegistry(notify.Webhook{URL: srv.URL, Method: http.MethodPost})}

	// NotifyDownloadWebhook returns nothing to assert on failure; this just
	// exercises the non-2xx path without panicking.
	NotifyDownload(recv, DownloadWebhookEvent{Username: "alice", Key: "k", At: time.Now()}, nil, discardLogger)
}
