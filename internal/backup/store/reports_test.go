package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestReportsSaveListGet(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC)

	older, err := db.SaveReport(ctx, Report{
		GeneratedAt: now.Add(-24 * time.Hour), PeriodStart: now.Add(-48 * time.Hour), PeriodEnd: now.Add(-24 * time.Hour),
		Trigger: ReportTriggerScheduled, Notifications: []string{"ops"},
	})
	if err != nil {
		t.Fatalf("SaveReport() error: %v", err)
	}

	content := ReportContent{
		Receivers: []ReportReceiverLine{{ID: "recv", FilesReceived: 2, BytesReceived: 300}},
		JobErrors: []ReportJobError{{At: now.Add(-time.Hour), JobName: "nightly", Error: "boom"}},
	}

	newer, err := db.SaveReport(ctx, Report{
		GeneratedAt: now, PeriodStart: now.Add(-time.Hour), PeriodEnd: now, Trigger: ReportTriggerManual, TriggeredBy: "erin",
		Content: content,
	})
	if err != nil {
		t.Fatalf("SaveReport() error: %v", err)
	}

	if got := [3]int{newer.Receivers, newer.JobErrors, newer.Jobs}; got != [3]int{1, 1, 0} {
		t.Errorf("SaveReport() receivers, job errors, jobs = %v, want [1 1 0]", got)
	}

	list, err := db.ListReports(ctx, 10)
	if err != nil {
		t.Fatalf("ListReports() error: %v", err)
	}

	if len(list) != 2 || list[0].ID != newer.ID || list[1].ID != older.ID || !reflect.DeepEqual(list[1].Notifications, []string{"ops"}) {
		t.Fatalf("ListReports() = %+v, want newest first, the older one sent to ops", list)
	}

	got, err := db.GetReport(ctx, newer.ID)
	if err != nil {
		t.Fatalf("GetReport() error: %v", err)
	}

	if !reflect.DeepEqual(got.Content, content) || got.TriggeredBy != "erin" {
		t.Errorf("GetReport() = %+v, want content %+v by erin", got, content)
	}
}

func TestDeleteReportsBefore(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC)

	older, err := db.SaveReport(ctx, Report{GeneratedAt: now.Add(-24 * time.Hour), Trigger: ReportTriggerScheduled})
	if err != nil {
		t.Fatalf("SaveReport() error: %v", err)
	}

	newer, err := db.SaveReport(ctx, Report{GeneratedAt: now, Trigger: ReportTriggerManual})
	if err != nil {
		t.Fatalf("SaveReport() error: %v", err)
	}

	n, err := db.DeleteReportsBefore(ctx, now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("DeleteReportsBefore() = %d, %v, want 1 deleted", n, err)
	}

	if _, err := db.GetReport(ctx, older.ID); !errors.Is(err, ErrReportNotFound) {
		t.Errorf("GetReport(pruned) error = %v, want ErrReportNotFound", err)
	}

	if _, err := db.GetReport(ctx, newer.ID); err != nil {
		t.Errorf("GetReport(kept) error = %v", err)
	}

	if _, err := db.GetReport(ctx, 9999); !errors.Is(err, ErrReportNotFound) {
		t.Errorf("GetReport(missing) error = %v, want ErrReportNotFound", err)
	}
}

// storedReportContent pins the exact JSON reports.content holds. Rows like
// this sit in users' state dbs, so a failing test here means a change needs
// a migration, not a fixture update.
const storedReportContent = `{"receivers":[{"id":"recv","files_received":2,"bytes_received":300,"errors":1}],` +
	`"stale":[{"id":"recv","stale_after_seconds":3600,"last_seen":"2026-10-17T06:00:00Z"}],` +
	`"receiver_errors":[{"at":"2026-10-18T06:00:00Z","receiver_id":"recv","kind":"receive","key":"a.gpg","error":"disk full"}],` +
	`"jobs":[{"id":"nightly","runs_completed":1,"bytes_written":42,"errors":1}],` +
	`"job_errors":[{"at":"2026-10-18T05:00:00Z","job_name":"nightly","error":"boom"}]}`

func TestReportContentStoredFormat(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	lastSeen := time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)

	saved, err := db.SaveReport(t.Context(), Report{
		GeneratedAt: time.Now(), Trigger: ReportTriggerManual,
		Content: ReportContent{
			Receivers:      []ReportReceiverLine{{ID: "recv", FilesReceived: 2, BytesReceived: 300, Errors: 1}},
			Stale:          []ReportStaleReceiver{{ID: "recv", StaleAfterSeconds: 3600, LastSeen: &lastSeen}},
			ReceiverErrors: []ReportReceiverError{{At: time.Date(2026, 10, 18, 6, 0, 0, 0, time.UTC), ReceiverID: "recv", Kind: "receive", Key: "a.gpg", Error: "disk full"}},
			Jobs:           []ReportJobLine{{ID: "nightly", RunsCompleted: 1, BytesWritten: 42, Errors: 1}},
			JobErrors:      []ReportJobError{{At: time.Date(2026, 10, 18, 5, 0, 0, 0, time.UTC), JobName: "nightly", Error: "boom"}},
		},
	})
	if err != nil {
		t.Fatalf("SaveReport() error: %v", err)
	}

	var raw string
	if err := db.read.Raw("SELECT content FROM reports WHERE id = ?", saved.ID).Scan(&raw).Error; err != nil {
		t.Fatalf("reading raw report row: %v", err)
	}

	if raw != storedReportContent {
		t.Errorf("content column =\n%s\nwant\n%s", raw, storedReportContent)
	}
}
