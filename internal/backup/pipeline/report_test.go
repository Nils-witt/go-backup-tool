package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	reportpkg "nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// writeFile writes contents to path, failing the test on any error.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
}

// TestReportScheduleNext exercises the report package's actual use of
// cron.Schedule (RunReportLoop calls Next(time.Now()) each iteration), as a
// sanity check that a daily "0 7 * * *" schedule behaves the way the report
// loop's log/tests assume: today's occurrence if it hasn't passed yet,
// tomorrow's otherwise — including the exact-match instant, so a report sent
// right at its scheduled time doesn't loop and fire again immediately.
func TestReportScheduleNext(t *testing.T) {
	t.Parallel()

	sched, err := cron.ParseStandard("0 7 * * *")
	if err != nil {
		t.Fatalf("cron.ParseStandard() error: %v", err)
	}

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"later today", time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC), time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC)},
		{"already passed rolls to tomorrow", time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC), time.Date(2026, 8, 29, 7, 0, 0, 0, time.UTC)},
		{"exact match rolls to tomorrow", time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC), time.Date(2026, 8, 29, 7, 0, 0, 0, time.UTC)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := sched.Next(tc.now); !got.Equal(tc.want) {
				t.Errorf("Schedule.Next(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestBuildReportSummarizesReceiverEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()

	db, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	end := time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC)
	start := end.Add(-24 * time.Hour)
	inWindow := end.Add(-time.Hour)
	outOfWindow := end.Add(-25 * time.Hour) // just outside the window

	events := []store.ReceiverEvent{
		{At: inWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "a1.gpg", Size: 100, Success: true},
		{At: inWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "a2.gpg", Size: 200, Success: true},
		{At: inWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "a3.gpg", Success: false, Error: "disk full"},
		{At: inWindow, ReceiverID: "recv-b", Kind: store.ReceiverEventDelete, Key: "b1.gpg", Success: true},
		{At: outOfWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "old.gpg", Size: 999, Success: true},
	}

	for _, ev := range events {
		if err := db.SaveReceiverEvent(ctx, ev); err != nil {
			t.Fatalf("SaveReceiverEvent() error: %v", err)
		}
	}

	rc := &config.RunConfig{Receivers: map[string]config.ResolvedReceiver{
		"recv-a": {ID: "recv-a"},
		"recv-b": {ID: "recv-b"},
		"recv-c": {ID: "recv-c"}, // no events at all in the window
	}}

	report := buildReport(ctx, rc.ServerName, jobNamesOf(rc), rc.Receivers, db, start, end, discardLogger)

	if len(report.receivers) != 3 {
		t.Fatalf("report.receivers = %+v, want 3 entries (one per configured receiver)", report.receivers)
	}

	byID := make(map[string]receiverReportLine, len(report.receivers))
	for _, r := range report.receivers {
		byID[r.id] = r
	}

	want := map[string]receiverReportLine{
		"recv-a": {id: "recv-a", filesReceived: 2, bytesReceived: 300, errors: 1},
		"recv-b": {id: "recv-b"}, // its only event is a delete: no receives, no errors
		"recv-c": {id: "recv-c"}, // no events at all
	}

	for id, want := range want {
		if got := byID[id]; got != want {
			t.Errorf("%s summary = %+v, want %+v", id, got, want)
		}
	}

	wantErrors := []store.ReceiverErrorEvent{{At: inWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "a3.gpg", Error: "disk full"}}
	if !reflect.DeepEqual(report.errors, wantErrors) {
		t.Errorf("report.errors = %+v, want %+v", report.errors, wantErrors)
	}
}

func TestBuildReportSummarizesJobRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()

	db, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	end := time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC)
	start := end.Add(-24 * time.Hour)
	inWindow := end.Add(-time.Hour)
	outOfWindow := end.Add(-25 * time.Hour) // just outside the window

	runs := []struct {
		name         string
		success      bool
		endTime      time.Time
		bytesWritten int64
		errText      string
	}{
		{name: "job-a", success: true, endTime: inWindow, bytesWritten: 100},
		{name: "job-a", success: true, endTime: inWindow, bytesWritten: 200},
		{name: "job-a", success: false, endTime: inWindow, errText: "disk full"},
		{name: "job-a", success: true, endTime: outOfWindow, bytesWritten: 999},
	}

	for _, r := range runs {
		if err := db.SaveJobRun(ctx, r.name, "done", r.success, r.endTime.Add(-time.Minute), r.endTime, r.bytesWritten, r.errText); err != nil {
			t.Fatalf("SaveJobRun() error: %v", err)
		}
	}

	rc := &config.RunConfig{Jobs: []*config.Config{
		{Name: "job-a"},
		{Name: "job-b"}, // no runs at all in the window
	}}

	report := buildReport(ctx, rc.ServerName, jobNamesOf(rc), rc.Receivers, db, start, end, discardLogger)

	if len(report.jobs) != 2 {
		t.Fatalf("report.jobs = %+v, want 2 entries (one per configured job)", report.jobs)
	}

	byID := make(map[string]jobReportLine, len(report.jobs))
	for _, j := range report.jobs {
		byID[j.id] = j
	}

	want := map[string]jobReportLine{
		"job-a": {id: "job-a", runsCompleted: 2, bytesWritten: 300, errors: 1},
		"job-b": {id: "job-b"}, // no runs at all
	}

	for id, want := range want {
		if got := byID[id]; got != want {
			t.Errorf("%s summary = %+v, want %+v", id, got, want)
		}
	}

	wantErrors := []store.JobRunErrorEvent{{At: inWindow, JobName: "job-a", Error: "disk full"}}
	if !reflect.DeepEqual(report.jobErrors, wantErrors) {
		t.Errorf("report.jobErrors = %+v, want %+v", report.jobErrors, wantErrors)
	}
}

func TestBuildReportDetectsStaleReceiver(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	freshDir := t.TempDir()
	writeFile(t, filepath.Join(freshDir, "recent.gpg"), "a")

	staleDir := t.TempDir()
	staleFile := filepath.Join(staleDir, "old.gpg")
	writeFile(t, staleFile, "a")

	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staleFile, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes() error: %v", err)
	}

	neverDir := t.TempDir()

	rc := &config.RunConfig{Receivers: map[string]config.ResolvedReceiver{
		"fresh": {ID: "fresh", Path: freshDir, StaleAfter: time.Hour},
		"stale": {ID: "stale", Path: staleDir, StaleAfter: time.Hour},
		"never": {ID: "never", Path: neverDir, StaleAfter: time.Hour},
	}}

	now := time.Now()
	report := buildReport(ctx, rc.ServerName, jobNamesOf(rc), rc.Receivers, nil, now.Add(-24*time.Hour), now, discardLogger)

	if len(report.stale) != 1 || report.stale[0].id != "stale" {
		t.Errorf("report.stale = %+v, want only \"stale\" (fresh isn't stale, never has nothing to be stale)", report.stale)
	}
}

func TestRenderReportBody(t *testing.T) {
	t.Parallel()

	report := reportContent{
		start: time.Date(2026, 8, 27, 7, 0, 0, 0, time.UTC),
		end:   time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC),
		receivers: []receiverReportLine{
			{id: "recv-a", filesReceived: 3, bytesReceived: 1500, errors: 0},
		},
		errors: []store.ReceiverErrorEvent{
			{At: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "x.gpg", Error: "boom"},
		},
		stale: []staleReceiverLine{
			{id: "recv-b", staleAfter: 6 * time.Hour, lastSeen: time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)},
		},
	}

	body := renderReportBody(report)

	for _, want := range []string{"recv-a", "3 file(s)", "recv-b", "boom", "stale-after: 6h0m0s"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderReportBody() = %q, want it to contain %q", body, want)
		}
	}
}

func TestRenderReportBodyEmpty(t *testing.T) {
	t.Parallel()

	body := renderReportBody(reportContent{})

	for _, want := range []string{
		"No receivers configured", "No receivers currently stale", "No receiver errors recorded",
		"No jobs configured", "No job errors recorded",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderReportBody() = %q, want it to contain %q", body, want)
		}
	}
}

func TestRenderReportBodyIncludesJobs(t *testing.T) {
	t.Parallel()

	report := reportContent{
		jobs: []jobReportLine{
			{id: "job-a", runsCompleted: 2, bytesWritten: 4096, errors: 1},
		},
		jobErrors: []store.JobRunErrorEvent{
			{At: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), JobName: "job-a", Error: "boom"},
		},
	}

	body := renderReportBody(report)

	for _, want := range []string{"job-a", "2 run(s)", "boom"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderReportBody() = %q, want it to contain %q", body, want)
		}
	}
}

func TestRenderReportSubject(t *testing.T) {
	t.Parallel()

	report := reportContent{
		start:     time.Date(2026, 8, 27, 7, 0, 0, 0, time.UTC),
		end:       time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC),
		receivers: []receiverReportLine{{id: "recv-a"}},
		errors:    []store.ReceiverErrorEvent{{}, {}},
		stale:     []staleReceiverLine{{id: "recv-b"}},
		jobs:      []jobReportLine{{id: "job-a"}},
		jobErrors: []store.JobRunErrorEvent{{}},
	}

	got := renderReportSubject("{start} to {end}: {receivers} receiver(s), {errors} error(s), {stale} stale, {jobs} job(s), {job-errors} job error(s)", report)

	want := "2026-08-27 07:00 to 2026-08-28 07:00: 1 receiver(s), 2 error(s), 1 stale, 1 job(s), 1 job error(s)"
	if got != want {
		t.Errorf("renderReportSubject() = %q, want %q", got, want)
	}
}

func TestRenderReportSubjectDefaultTemplate(t *testing.T) {
	t.Parallel()

	report := reportContent{end: time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC)}

	// Mirrors report.defaultReportSubject: reproduces this feature's
	// original hardcoded subject line when report.subject: is unset.
	got := renderReportSubject("go-backup-tool report - {end}", report)

	want := "go-backup-tool report - 2026-08-28 07:00"
	if got != want {
		t.Errorf("renderReportSubject() = %q, want %q", got, want)
	}
}

func TestRenderReportSubjectServerName(t *testing.T) {
	t.Parallel()

	report := reportContent{serverName: "primary-backup-host"}

	got := renderReportSubject("[{server_name}] report", report)

	want := "[primary-backup-host] report"
	if got != want {
		t.Errorf("renderReportSubject() = %q, want %q", got, want)
	}
}

// TestRunReportLoopPicksUpSettingsChanges checks the loop idles while the
// report is disabled and starts sending as soon as it's enabled through
// report.Live — the web UI path — without a restart.
func TestRunReportLoopPicksUpSettingsChanges(t *testing.T) {
	t.Parallel()

	sent := make(chan struct{}, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent <- struct{}{}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	notifications := notify.NewRegistry(map[string]notify.Notification{"hook": {ID: "hook", Webhook: &notify.Webhook{URL: srv.URL, Method: http.MethodPost}}})
	live := reportpkg.NewLive(reportpkg.Settings{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go RunReportLoop(ctx, "", func() []string { return nil }, live, notifications, backup.NewReceiverRegistry(nil), nil, nil, discardLogger)

	select {
	case <-sent:
		t.Fatal("report sent while disabled")
	case <-time.After(1500 * time.Millisecond):
	}

	settings, err := reportpkg.ResolveSettings(reportpkg.FileReport{Enabled: true, Schedule: "@every 1s", Notifications: []string{"hook"}}, notifications)
	if err != nil {
		t.Fatal(err)
	}

	live.Set(settings)

	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("no report sent after enabling it")
	}
}

func jobNamesOf(rc *config.RunConfig) []string {
	names := make([]string, len(rc.Jobs))
	for i, j := range rc.Jobs {
		names[i] = j.Name
	}

	return names
}
