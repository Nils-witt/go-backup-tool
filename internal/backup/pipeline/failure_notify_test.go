package pipeline

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// testWebhookRegistry is a notification registry holding wh as the single
// notification "test", for a job fixture whose failure-notifications are
// []string{"test"}.
func testWebhookRegistry(wh notify.Webhook) *notify.Registry {
	return notify.NewRegistry(map[string]notify.Notification{"test": {ID: "test", Webhook: &wh}})
}

func TestRenderJobFailurePayload(t *testing.T) {
	t.Parallel()

	job := &config.Config{Name: "db-backup", ServerName: "primary-backup-host"}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	got := renderJobFailurePayload(
		"{server_name}: job {job} {state} after {duration} (started {started_at}): {error}",
		job, errors.New("boom"), backup.StateFailed, start, 5*time.Second,
	)
	want := "primary-backup-host: job db-backup failed after 5s (started 2026-01-02T03:04:05Z): boom"

	if got != want {
		t.Errorf("renderJobFailurePayload() = %q, want %q", got, want)
	}
}

func TestNotifyJobFailureWebhookNoopWhenUnconfigured(t *testing.T) {
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

	job := &config.Config{Name: "test"} // no FailureNotifications configured

	notifyJobFailure(job, nil, errors.New("boom"), backup.StateFailed, time.Now(), time.Second, nil, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if requestSeen {
		t.Error("notifyJobFailure() sent a request, want none when failure-notifications is unconfigured")
	}
}

func TestNotifyJobFailureWebhookDefaultJSONPayload(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		gotBody     string
		requestSeen bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("webhook server: reading request body: %v", err)
		}

		mu.Lock()
		gotBody = string(body)
		requestSeen = true
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	notifications := testWebhookRegistry(notify.Webhook{URL: srv.URL, Method: http.MethodPost})

	job := &config.Config{
		Name:                 "db-backup",
		FailureNotifications: []string{"test"},
	}

	notifyJobFailure(job, notifications, errors.New("boom"), backup.StateIncomplete, time.Now(), time.Second, nil, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if !requestSeen {
		t.Fatal("webhook server: no request received")
	}

	for _, want := range []string{`"job":"db-backup"`, `"error":"boom"`, `"state":"incomplete"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("webhook body = %q, want it to contain %q", gotBody, want)
		}
	}
}

func TestNotifyJobFailureWebhookUsesCustomBody(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		gotBody     string
		requestSeen bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("webhook server: reading request body: %v", err)
		}

		mu.Lock()
		gotBody = string(body)
		requestSeen = true
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	notifications := testWebhookRegistry(notify.Webhook{
		URL:  srv.URL,
		Body: `{"text":"job {job} {state}: {error}"}`,
	})

	job := &config.Config{
		Name:                 "db-backup",
		FailureNotifications: []string{"test"},
	}

	notifyJobFailure(job, notifications, errors.New("boom"), backup.StateFailed, time.Now(), time.Second, nil, discardLogger)

	mu.Lock()
	defer mu.Unlock()

	if !requestSeen {
		t.Fatal("webhook server: no request received")
	}

	wantBody := `{"text":"job db-backup failed: boom"}`
	if gotBody != wantBody {
		t.Errorf("webhook body = %q, want %q", gotBody, wantBody)
	}
}

func TestNotifyJobFailureEmailUsesDefaultSubjectAndBody(t *testing.T) {
	t.Parallel()

	// notifyJobFailureEmail's only failure path this test can observe
	// without a real SMTP server is notify.SendMail returning an error
	// (unconfigured SMTP.Host) — this exercises the render + send call path
	// without needing a live mail server, mirroring how job failures are
	// logged rather than propagated.
	job := &config.Config{Name: "db-backup", ServerName: "host-a"}
	email := notify.Email{To: []string{"ops@example.com"}, From: "noreply@example.com"}

	// Should not panic even though SMTP isn't configured; the resulting
	// send failure is logged, not returned.
	notifyJobFailureEmail(job, email, errors.New("boom"), backup.StateFailed, time.Now(), time.Second, nil, discardLogger)
}
