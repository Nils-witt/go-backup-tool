package telemetry

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getsentry/sentry-go"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

const testDSN = "https://public@sentry.example/1"

// newTestHub returns a hub whose events land in the returned transport
// instead of being sent anywhere.
func newTestHub(t *testing.T) (*sentry.Hub, *sentry.MockTransport) {
	t.Helper()

	transport := &sentry.MockTransport{}

	client, err := sentry.NewClient(sentry.ClientOptions{Dsn: testDSN, Transport: transport})
	if err != nil {
		t.Fatalf("sentry.NewClient() error = %v", err)
	}

	return sentry.NewHub(client, sentry.NewScope()), transport
}

func TestInit_Disabled(t *testing.T) {
	t.Parallel()

	enabled, err := Init(config.SentrySettings{}, "")
	if err != nil || enabled {
		t.Fatalf("Init(disabled) = %v, %v, want false, nil", enabled, err)
	}
}

func TestScrubRequest(t *testing.T) {
	t.Parallel()

	event := sentry.NewEvent()
	event.Request = &sentry.Request{
		URL: "https://backup.example/api/download", Method: http.MethodGet, QueryString: "ticket=secret",
		Cookies: "session=secret", Headers: map[string]string{"Authorization": "Bearer secret"},
		Env: map[string]string{"REMOTE_ADDR": "192.0.2.1"},
	}

	scrubRequest(event)

	expected := sentry.Request{URL: "https://backup.example/api/download", Method: http.MethodGet}
	if event.Request.URL != expected.URL || event.Request.Method != expected.Method ||
		event.Request.QueryString != "" || event.Request.Cookies != "" || event.Request.Headers != nil || event.Request.Env != nil {
		t.Errorf("scrubRequest() left %+v, want only %+v", *event.Request, expected)
	}

	scrubRequest(sentry.NewEvent()) // no request: must not panic
}

//nolint:paralleltest // swaps the global sentry hub, which Recover reports to
func TestRecover(t *testing.T) {
	transport := &sentry.MockTransport{}
	if err := sentry.Init(sentry.ClientOptions{Dsn: testDSN, Transport: transport}); err != nil {
		t.Fatalf("sentry.Init() error = %v", err)
	}

	t.Cleanup(func() { _ = sentry.Init(sentry.ClientOptions{}) })

	got := func() (r any) {
		defer func() { r = recover() }()
		defer Recover()

		panic("boom")
	}()

	if got != "boom" {
		t.Fatalf("Recover() re-panicked with %v, want boom", got)
	}

	events := transport.Events()
	if len(events) != 1 || events[0].Message != "boom" {
		t.Fatalf("Recover() sent %d events (%+v), want one with message boom", len(events), events)
	}
}

func TestHTTPMiddleware(t *testing.T) {
	t.Parallel()

	called := false
	h := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true

		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if !called || rec.Code != http.StatusTeapot {
		t.Fatalf("HTTPMiddleware: called = %v, code = %d, want true, %d", called, rec.Code, http.StatusTeapot)
	}
}

func TestHandler_Enabled(t *testing.T) {
	t.Parallel()

	h := &handler{}

	tests := []struct {
		name     string
		level    slog.Level
		expected bool
	}{
		{name: "debug", level: slog.LevelDebug, expected: false},
		{name: "info", level: slog.LevelInfo, expected: true},
		{name: "error", level: slog.LevelError, expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := h.Enabled(t.Context(), tt.level); got != tt.expected {
				t.Errorf("Enabled(%v) = %v, want %v", tt.level, got, tt.expected)
			}
		})
	}
}

func TestHandler_Handle(t *testing.T) {
	t.Parallel()

	hub, transport := newTestHub(t)
	log := slog.New(&handler{hub: hub}).With("job", "db").WithGroup("run")

	log.Info("starting", "attempt", 1)
	log.Warn("slow upload")
	log.Error("upload failed", "err", errors.New("connection refused"), slog.Group("target", "server", "nas"))

	events := transport.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (only the error record)", len(events))
	}

	e := events[0]
	if e.Message != "upload failed: connection refused" || e.Level != sentry.LevelError {
		t.Errorf("event message/level = %q/%q, want %q/%q", e.Message, e.Level, "upload failed: connection refused", sentry.LevelError)
	}

	if len(e.Fingerprint) != 2 || e.Fingerprint[1] != "upload failed" {
		t.Errorf("event fingerprint = %v, want it to group by the log message", e.Fingerprint)
	}

	data := e.Contexts["log"]
	for key, want := range map[string]any{"job": "db", "run.err": "connection refused", "run.target.server": "nas"} {
		if data[key] != want {
			t.Errorf("event log context[%q] = %v, want %v (context: %v)", key, data[key], want, data)
		}
	}

	if len(e.Breadcrumbs) != 2 || e.Breadcrumbs[0].Message != "starting" || e.Breadcrumbs[1].Level != sentry.LevelWarning {
		t.Errorf("event breadcrumbs = %+v, want the info and warn records", e.Breadcrumbs)
	}
}
