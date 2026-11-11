package ctl

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

// fakeInstance is an httptest server standing in for a running instance's
// API, recording the Authorization header of the last request.
type fakeInstance struct {
	*httptest.Server

	mu   sync.Mutex
	auth string
}

// newFakeInstance serves healthCode/health on /api/health, and jobs and
// receivers on /api/status and /api/receivers to requests bearing token
// (401 otherwise).
func newFakeInstance(t *testing.T, healthCode int, health, token string, jobs []status.JobSnapshot, receivers []status.ReceiverSnapshot) *fakeInstance {
	t.Helper()

	f := &fakeInstance{}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(healthCode)
		_, _ = w.Write([]byte(health))
	})

	serveJSON := func(v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.auth = r.Header.Get("Authorization")
			f.mu.Unlock()

			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			_ = json.NewEncoder(w).Encode(v)
		}
	}

	mux.HandleFunc("GET /api/status", serveJSON(jobs))
	mux.HandleFunc("GET /api/receivers", serveJSON(receivers))

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)

	return f
}

func (f *fakeInstance) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.auth
}

func runCtl(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer

	code = Run(t.Context(), args[0], args[1:], &out, &errOut)

	return code, out.String(), errOut.String()
}

func TestIsCommand(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{"health": true, "status": true, "-config": false, "": false} {
		if got := IsCommand(name); got != want {
			t.Errorf("IsCommand(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRunHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		code     int
		body     string
		json     bool
		wantCode int
		wantOut  string
	}{
		{name: "healthy", code: http.StatusOK, body: `{"status":"ok","version":"v9","database":"ok"}`, wantCode: exitOK, wantOut: "ok (version v9, database ok)\n"},
		{name: "db down", code: http.StatusServiceUnavailable, body: `{"status":"degraded","version":"v9","database":"unavailable"}`, wantCode: exitError, wantOut: "degraded (version v9, database unavailable)\n"},
		{name: "json", code: http.StatusOK, body: `{"status":"ok","version":"v9","database":"ok"}`, json: true, wantCode: exitOK, wantOut: `{"status":"ok","version":"v9","database":"ok"}`},
		{name: "not our API", code: http.StatusNotFound, body: "404 page not found", wantCode: exitError, wantOut: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeInstance(t, tt.code, tt.body, "", nil, nil)

			args := []string{"health", "-url", f.URL}
			if tt.json {
				args = append(args, "-json")
			}

			code, out, _ := runCtl(t, args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}

			if out != tt.wantOut {
				t.Errorf("stdout = %q, want %q", out, tt.wantOut)
			}
		})
	}
}

func TestRunHealthUnreachable(t *testing.T) {
	t.Parallel()

	f := newFakeInstance(t, http.StatusOK, "", "", nil, nil)
	url := f.URL
	f.Close()

	code, _, errOut := runCtl(t, "health", "-url", url, "-timeout", "2s")
	if code != exitError || !strings.Contains(errOut, "reaching instance") {
		t.Errorf("exit code = %d, stderr = %q, want %d and a reaching-instance error", code, errOut, exitError)
	}
}

func TestRunStatus(t *testing.T) {
	t.Parallel()

	okJobs := []status.JobSnapshot{{
		Name: "nightly", State: status.StateOK, Duration: "1m2s", Size: "12 MB",
		LastStart: time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC),
		Targets:   []status.TargetSnapshot{{Server: "nas", Bucket: "db", State: status.StateOK}},
	}}
	failedJobs := []status.JobSnapshot{{
		Name: "nightly", State: status.StateFailed, Error: "exit status 1\nmore",
		Targets: []status.TargetSnapshot{{Server: "nas", Bucket: "db", State: status.StateFailed, Error: "boom"}},
	}}
	receivers := []status.ReceiverSnapshot{{ID: "inbox", Kind: "backup", State: status.StateIdle, StaleAfter: "24h"}}

	tests := []struct {
		name      string
		jobs      []status.JobSnapshot
		wantCode  int
		wantLines []string
	}{
		{name: "all ok", jobs: okJobs, wantCode: exitOK, wantLines: []string{"JOB", "nightly", "nas/db", "RECEIVER", "inbox"}},
		{name: "failed job", jobs: failedJobs, wantCode: exitAttention, wantLines: []string{"exit status 1 more", "boom"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeInstance(t, http.StatusOK, "", "secret", tt.jobs, receivers)

			code, out, errOut := runCtl(t, "status", "-url", f.URL, "-token", "secret")
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, errOut)
			}

			for _, want := range tt.wantLines {
				if !strings.Contains(out, want) {
					t.Errorf("stdout lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestRunStatusJSON(t *testing.T) {
	t.Parallel()

	f := newFakeInstance(t, http.StatusOK, "", "secret", []status.JobSnapshot{{Name: "nightly", State: status.StateOK}}, nil)

	code, out, _ := runCtl(t, "status", "-url", f.URL, "-token", "secret", "-json")
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}

	var got statusReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding stdout: %v\n%s", err, out)
	}

	if len(got.Jobs) != 1 || got.Jobs[0].Name != "nightly" || got.Receivers == nil {
		t.Errorf("stdout = %+v, want the one job and an empty receivers list", got)
	}
}

func TestRunStatusUnauthorized(t *testing.T) {
	t.Parallel()

	f := newFakeInstance(t, http.StatusOK, "", "secret", nil, nil)

	code, _, errOut := runCtl(t, "status", "-url", f.URL)
	if code != exitError || !strings.Contains(errOut, tokenEnv) {
		t.Errorf("exit code = %d, stderr = %q, want %d and a hint naming %s", code, errOut, exitError, tokenEnv)
	}
}

// Not parallel: it sets the environment.
func TestRunStatusTokenFromEnv(t *testing.T) {
	f := newFakeInstance(t, http.StatusOK, "", "from-env", nil, nil)

	t.Setenv(tokenEnv, "from-env")

	if code, _, errOut := runCtl(t, "status", "-url", f.URL); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, errOut)
	}

	if got := f.lastAuth(); got != "Bearer from-env" {
		t.Errorf("Authorization = %q, want the token from %s", got, tokenEnv)
	}
}

func TestRunUsageErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "unknown flag", args: []string{"status", "-nope"}, want: exitUsage},
		{name: "extra argument", args: []string{"health", "-url", "http://x", "extra"}, want: exitUsage},
		{name: "bad url", args: []string{"health", "-url", "localhost:8080"}, want: exitUsage},
		{name: "zero timeout", args: []string{"health", "-url", "http://x", "-timeout", "0s"}, want: exitUsage},
		{name: "help", args: []string{"health", "-help"}, want: exitOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if code, _, _ := runCtl(t, tt.args...); code != tt.want {
				t.Errorf("exit code = %d, want %d", code, tt.want)
			}
		})
	}
}
