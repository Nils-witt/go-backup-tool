package backup

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStateDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.db")

	db, err := OpenScheduleStateDB(context.Background(), path)
	if err != nil {
		t.Fatalf("openScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

func TestWriteReadLastSuccessRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	want := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if err := WriteLastSuccess(ctx, db, "job-a", want); err != nil {
		t.Fatalf("writeLastSuccess() error: %v", err)
	}

	got, ok, err := ReadLastSuccess(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readLastSuccess() error: %v", err)
	}

	if !ok {
		t.Fatal("readLastSuccess() ok = false, want true")
	}

	if !got.Equal(want) {
		t.Errorf("readLastSuccess() = %v, want %v", got, want)
	}
}

func TestReadLastSuccessUnknownJob(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	_, ok, err := ReadLastSuccess(context.Background(), db, "no-such-job")
	if err != nil {
		t.Fatalf("readLastSuccess() error: %v", err)
	}

	if ok {
		t.Fatal("readLastSuccess() ok = true for unknown job, want false")
	}
}

func TestWriteLastSuccessUpsert(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	first := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	second := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	if err := WriteLastSuccess(ctx, db, "job-a", first); err != nil {
		t.Fatalf("writeLastSuccess() first error: %v", err)
	}

	if err := WriteLastSuccess(ctx, db, "job-a", second); err != nil {
		t.Fatalf("writeLastSuccess() second error: %v", err)
	}

	got, ok, err := ReadLastSuccess(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readLastSuccess() error: %v", err)
	}

	if !ok {
		t.Fatal("readLastSuccess() ok = false, want true")
	}

	if !got.Equal(second) {
		t.Errorf("readLastSuccess() = %v, want %v (second write should overwrite first)", got, second)
	}
}

func TestWriteReadLastRunRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	want := LastRun{
		Start: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 1, 3, 0, 5, 0, time.UTC),
		State: StateOK,
		Size:  2048,
	}

	if err := WriteLastRun(ctx, db, "job-a", want); err != nil {
		t.Fatalf("writeLastRun() error: %v", err)
	}

	got, ok, err := ReadLastRun(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readLastRun() error: %v", err)
	}

	if !ok {
		t.Fatal("readLastRun() ok = false, want true")
	}

	if !got.Start.Equal(want.Start) || !got.End.Equal(want.End) || got.State != want.State || got.Error != want.Error || got.Size != want.Size {
		t.Errorf("readLastRun() = %+v, want %+v", got, want)
	}
}

func TestWriteReadTargetRunRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)

	if err := WriteTargetRun(ctx, db, "job-a", 0, StateOK, "", at); err != nil {
		t.Fatalf("writeTargetRun() target 0 error: %v", err)
	}

	if err := WriteTargetRun(ctx, db, "job-a", 1, StateFailed, "boom", at); err != nil {
		t.Fatalf("writeTargetRun() target 1 error: %v", err)
	}

	got, err := ReadTargetRuns(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readTargetRuns() error: %v", err)
	}

	want := map[int]TargetRun{
		0: {Index: 0, State: StateOK, Error: ""},
		1: {Index: 1, State: StateFailed, Error: "boom"},
	}

	if len(got) != len(want) {
		t.Fatalf("readTargetRuns() = %+v, want %d entries", got, len(want))
	}

	for _, tr := range got {
		if w, ok := want[tr.Index]; !ok || tr != w {
			t.Errorf("readTargetRuns() entry %+v, want %+v", tr, want[tr.Index])
		}
	}
}

func TestReadTargetRunsUnknownJob(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	got, err := ReadTargetRuns(context.Background(), db, "no-such-job")
	if err != nil {
		t.Fatalf("readTargetRuns() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("readTargetRuns() = %+v, want none for unknown job", got)
	}
}

func TestWriteTargetRunUpsert(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	first := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	second := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	if err := WriteTargetRun(ctx, db, "job-a", 0, StateOK, "", first); err != nil {
		t.Fatalf("writeTargetRun() first error: %v", err)
	}

	if err := WriteTargetRun(ctx, db, "job-a", 0, StateFailed, "boom", second); err != nil {
		t.Fatalf("writeTargetRun() second error: %v", err)
	}

	got, err := ReadTargetRuns(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readTargetRuns() error: %v", err)
	}

	if len(got) != 1 || got[0].State != StateFailed || got[0].Error != "boom" {
		t.Errorf("readTargetRuns() = %+v, want a single failed entry (second write should overwrite first)", got)
	}
}

func TestReadLastRunUnknownJob(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	_, ok, err := ReadLastRun(context.Background(), db, "no-such-job")
	if err != nil {
		t.Fatalf("readLastRun() error: %v", err)
	}

	if ok {
		t.Fatal("readLastRun() ok = true for unknown job, want false")
	}
}

func TestReadLastRunRowFromSuccessOnlyIsNotConfused(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	// writeLastSuccess alone creates a row with no last_run_* data; that
	// must not be misread as a persisted run.
	if err := WriteLastSuccess(ctx, db, "job-a", time.Now()); err != nil {
		t.Fatalf("writeLastSuccess() error: %v", err)
	}

	_, ok, err := ReadLastRun(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readLastRun() error: %v", err)
	}

	if ok {
		t.Fatal("readLastRun() ok = true for a success-only row, want false")
	}
}

func TestWriteLastRunUpsertPreservesLastSuccess(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	success := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if err := WriteLastSuccess(ctx, db, "job-a", success); err != nil {
		t.Fatalf("writeLastSuccess() error: %v", err)
	}

	failedRun := LastRun{
		Start: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 1, 9, 0, 1, 0, time.UTC),
		State: StateFailed,
		Error: "boom",
	}

	if err := WriteLastRun(ctx, db, "job-a", failedRun); err != nil {
		t.Fatalf("writeLastRun() error: %v", err)
	}

	gotSuccess, ok, err := ReadLastSuccess(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readLastSuccess() error: %v", err)
	}

	if !ok || !gotSuccess.Equal(success) {
		t.Errorf("readLastSuccess() = (%v, %v), want (%v, true) — a later failed run must not clobber last_success", gotSuccess, ok, success)
	}

	gotRun, ok, err := ReadLastRun(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readLastRun() error: %v", err)
	}

	if !ok || gotRun.State != StateFailed || gotRun.Error != "boom" {
		t.Errorf("readLastRun() = (%+v, %v), want a failed run with error %q", gotRun, ok, "boom")
	}
}

func TestQueueAndListOutstandingUploads(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	earlier := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	later := time.Date(2026, 1, 1, 3, 5, 0, 0, time.UTC)

	if err := QueueOutstandingUpload(ctx, db, "job-a", 1, "/tmp/a.staged", "backup-a.gpg", later, errors.New("boom b")); err != nil {
		t.Fatalf("queueOutstandingUpload() second error: %v", err)
	}

	if err := QueueOutstandingUpload(ctx, db, "job-a", 0, "/tmp/a.staged", "backup-a.gpg", earlier, errors.New("boom a")); err != nil {
		t.Fatalf("queueOutstandingUpload() first error: %v", err)
	}

	got, err := ListOutstandingUploads(ctx, db)
	if err != nil {
		t.Fatalf("listOutstandingUploads() error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("listOutstandingUploads() = %+v, want 2 rows", got)
	}

	if got[0].TargetIdx != 0 || got[1].TargetIdx != 1 {
		t.Errorf("listOutstandingUploads() order = [target %d, target %d], want oldest (queued_at) first: [0, 1]", got[0].TargetIdx, got[1].TargetIdx)
	}

	if got[0].JobName != "job-a" || got[0].StagingPath != "/tmp/a.staged" || got[0].Key != "backup-a.gpg" || got[0].Attempts != 1 || got[0].LastError != "boom a" {
		t.Errorf("listOutstandingUploads()[0] = %+v, want job-a/target 0 with attempts=1", got[0])
	}
}

func TestQueueOutstandingUploadIdempotent(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)

	if err := QueueOutstandingUpload(ctx, db, "job-a", 0, "/tmp/a.staged", "backup-a.gpg", at, errors.New("boom")); err != nil {
		t.Fatalf("queueOutstandingUpload() first error: %v", err)
	}

	if err := QueueOutstandingUpload(ctx, db, "job-a", 0, "/tmp/a.staged", "backup-a.gpg", at, errors.New("boom again")); err != nil {
		t.Fatalf("queueOutstandingUpload() second error: %v", err)
	}

	got, err := ListOutstandingUploads(ctx, db)
	if err != nil {
		t.Fatalf("listOutstandingUploads() error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("listOutstandingUploads() = %+v, want exactly 1 row for a duplicate (job, target, path)", got)
	}
}

func TestRecordOutstandingUploadAttempt(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()
	queuedAt := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	retriedAt := time.Date(2026, 1, 1, 3, 1, 0, 0, time.UTC)

	if err := QueueOutstandingUpload(ctx, db, "job-a", 0, "/tmp/a.staged", "backup-a.gpg", queuedAt, errors.New("first failure")); err != nil {
		t.Fatalf("queueOutstandingUpload() error: %v", err)
	}

	rows, err := ListOutstandingUploads(ctx, db)
	if err != nil || len(rows) != 1 {
		t.Fatalf("listOutstandingUploads() = %+v, %v, want exactly 1 row", rows, err)
	}

	if err := RecordOutstandingUploadAttempt(ctx, db, rows[0].ID, retriedAt, errors.New("second failure")); err != nil {
		t.Fatalf("recordOutstandingUploadAttempt() error: %v", err)
	}

	got, err := ListOutstandingUploads(ctx, db)
	if err != nil || len(got) != 1 {
		t.Fatalf("listOutstandingUploads() after attempt = %+v, %v, want exactly 1 row", got, err)
	}

	if got[0].Attempts != 2 || got[0].LastError != "second failure" {
		t.Errorf("listOutstandingUploads()[0] = %+v, want attempts=2, last_error=%q", got[0], "second failure")
	}
}

func TestDeleteOutstandingUpload(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := QueueOutstandingUpload(ctx, db, "job-a", 0, "/tmp/a.staged", "backup-a.gpg", time.Now(), errors.New("boom")); err != nil {
		t.Fatalf("queueOutstandingUpload() error: %v", err)
	}

	rows, err := ListOutstandingUploads(ctx, db)
	if err != nil || len(rows) != 1 {
		t.Fatalf("listOutstandingUploads() = %+v, %v, want exactly 1 row", rows, err)
	}

	if err := DeleteOutstandingUpload(ctx, db, rows[0].ID); err != nil {
		t.Fatalf("deleteOutstandingUpload() error: %v", err)
	}

	got, err := ListOutstandingUploads(ctx, db)
	if err != nil {
		t.Fatalf("listOutstandingUploads() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("listOutstandingUploads() after delete = %+v, want none", got)
	}
}

func TestCountOutstandingUploadsForPath(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()
	at := time.Now()

	if err := QueueOutstandingUpload(ctx, db, "job-a", 0, "/tmp/shared.staged", "backup-a.gpg", at, errors.New("boom")); err != nil {
		t.Fatalf("queueOutstandingUpload() target 0 error: %v", err)
	}

	if err := QueueOutstandingUpload(ctx, db, "job-a", 1, "/tmp/shared.staged", "backup-a.gpg", at, errors.New("boom")); err != nil {
		t.Fatalf("queueOutstandingUpload() target 1 error: %v", err)
	}

	if err := QueueOutstandingUpload(ctx, db, "job-b", 0, "/tmp/other.staged", "backup-b.gpg", at, errors.New("boom")); err != nil {
		t.Fatalf("queueOutstandingUpload() other path error: %v", err)
	}

	got, err := CountOutstandingUploadsForPath(ctx, db, "/tmp/shared.staged")
	if err != nil {
		t.Fatalf("countOutstandingUploadsForPath() error: %v", err)
	}

	if got != 2 {
		t.Errorf("countOutstandingUploadsForPath(shared.staged) = %d, want 2", got)
	}

	got, err = CountOutstandingUploadsForPath(ctx, db, "/tmp/other.staged")
	if err != nil {
		t.Fatalf("countOutstandingUploadsForPath() error: %v", err)
	}

	if got != 1 {
		t.Errorf("countOutstandingUploadsForPath(other.staged) = %d, want 1", got)
	}

	got, err = CountOutstandingUploadsForPath(ctx, db, "/tmp/nonexistent.staged")
	if err != nil {
		t.Fatalf("countOutstandingUploadsForPath() error: %v", err)
	}

	if got != 0 {
		t.Errorf("countOutstandingUploadsForPath(nonexistent.staged) = %d, want 0", got)
	}
}

func TestScheduleStateDBPath(t *testing.T) {
	t.Parallel()

	got := ScheduleStateDBPath("/etc/go-backup-tool/config.yaml")
	want := filepath.Join("/etc/go-backup-tool", scheduleStateDBName)

	if got != want {
		t.Errorf("scheduleStateDBPath() = %q, want %q", got, want)
	}
}

func TestRecordReadLoginEventsNewestFirst(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	events := []LoginEvent{
		{At: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), Username: "admin", Method: "password", Success: true, RemoteAddr: "10.0.0.1:1"},
		{At: time.Date(2026, 1, 1, 3, 1, 0, 0, time.UTC), Username: "admin", Method: "password", Success: false, RemoteAddr: "10.0.0.2:1", Detail: "incorrect username or password"},
		{At: time.Date(2026, 1, 1, 3, 2, 0, 0, time.UTC), Username: "person@example.com", Method: "oidc", Success: true, RemoteAddr: "10.0.0.3:1"},
	}

	for _, ev := range events {
		if err := RecordLoginEvent(ctx, db, ev); err != nil {
			t.Fatalf("recordLoginEvent() error: %v", err)
		}
	}

	got, err := ReadLoginEvents(ctx, db, 10)
	if err != nil {
		t.Fatalf("readLoginEvents() error: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("readLoginEvents() returned %d events, want 3", len(got))
	}

	if !got[0].At.Equal(events[2].At) || got[0].Username != "person@example.com" || got[0].Method != "oidc" || !got[0].Success {
		t.Errorf("readLoginEvents()[0] = %+v, want the most recently recorded event first", got[0])
	}

	if got[1].Success || got[1].Detail != "incorrect username or password" {
		t.Errorf("readLoginEvents()[1] = %+v, want the failed password attempt with its detail", got[1])
	}
}

func TestReadLoginEventsRespectsLimit(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	for i := range 5 {
		ev := LoginEvent{At: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC), Username: "admin", Method: "password", Success: true, RemoteAddr: "10.0.0.1:1"}
		if err := RecordLoginEvent(ctx, db, ev); err != nil {
			t.Fatalf("recordLoginEvent() error: %v", err)
		}
	}

	got, err := ReadLoginEvents(ctx, db, 2)
	if err != nil {
		t.Fatalf("readLoginEvents() error: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("readLoginEvents(limit=2) returned %d events, want 2", len(got))
	}
}

func TestReadLoginEventsEmpty(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	got, err := ReadLoginEvents(context.Background(), db, 10)
	if err != nil {
		t.Fatalf("readLoginEvents() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("readLoginEvents() on an empty log = %+v, want none", got)
	}
}

func TestRecordReadDownloadEventsNewestFirst(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	events := []DownloadEvent{
		{At: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), Username: "admin", ReceiverID: "a", Key: "backup.gpg", Success: true, RemoteAddr: "10.0.0.1:1"},
		{At: time.Date(2026, 1, 1, 3, 1, 0, 0, time.UTC), Username: "admin", ReceiverID: "a", Key: "missing.gpg", Success: false, RemoteAddr: "10.0.0.2:1", Detail: "not found"},
	}

	for _, ev := range events {
		if err := RecordDownloadEvent(ctx, db, ev); err != nil {
			t.Fatalf("recordDownloadEvent() error: %v", err)
		}
	}

	got, err := ReadDownloadEvents(ctx, db, 10)
	if err != nil {
		t.Fatalf("readDownloadEvents() error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("readDownloadEvents() returned %d events, want 2", len(got))
	}

	if got[0].Success || got[0].Key != "missing.gpg" || got[0].Detail != "not found" {
		t.Errorf("readDownloadEvents()[0] = %+v, want the most recently recorded (failed) attempt first", got[0])
	}

	if !got[1].At.Equal(events[0].At) || got[1].Username != "admin" || got[1].ReceiverID != "a" || !got[1].Success {
		t.Errorf("readDownloadEvents()[1] = %+v, want the earlier successful download", got[1])
	}
}

func TestReadDownloadEventsRespectsLimit(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	for i := range 5 {
		ev := DownloadEvent{At: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC), Username: "admin", ReceiverID: "a", Key: "backup.gpg", Success: true, RemoteAddr: "10.0.0.1:1"}
		if err := RecordDownloadEvent(ctx, db, ev); err != nil {
			t.Fatalf("recordDownloadEvent() error: %v", err)
		}
	}

	got, err := ReadDownloadEvents(ctx, db, 2)
	if err != nil {
		t.Fatalf("readDownloadEvents() error: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("readDownloadEvents(limit=2) returned %d events, want 2", len(got))
	}
}

func TestReadDownloadEventsEmpty(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	got, err := ReadDownloadEvents(context.Background(), db, 10)
	if err != nil {
		t.Fatalf("readDownloadEvents() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("readDownloadEvents() on an empty log = %+v, want none", got)
	}
}

func TestReadLastReceiverEventReturnsMostRecent(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	older := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	events := []ReceiverEvent{
		{At: older, ReceiverID: "recv-a", Kind: ReceiverEventReceive, Key: "a1.gpg", Size: 100, Success: true},
		{At: newer, ReceiverID: "recv-a", Kind: ReceiverEventDelete, Key: "a2.gpg", Success: false, Error: "disk full"},
		{At: newer, ReceiverID: "recv-b", Kind: ReceiverEventReceive, Key: "b1.gpg", Size: 50, Success: true},
	}

	for _, ev := range events {
		if err := RecordReceiverEvent(ctx, db, ev); err != nil {
			t.Fatalf("recordReceiverEvent() error: %v", err)
		}
	}

	got, ok, err := ReadLastReceiverEvent(ctx, db, "recv-a")
	if err != nil {
		t.Fatalf("readLastReceiverEvent() error: %v", err)
	}

	if !ok {
		t.Fatal("readLastReceiverEvent() ok = false, want true")
	}

	want := events[1]
	if !got.At.Equal(want.At) || got.Kind != want.Kind || got.Key != want.Key || got.Success != want.Success || got.Error != want.Error {
		t.Errorf("readLastReceiverEvent() = %+v, want %+v", got, want)
	}
}

func TestReadLastReceiverEventUnknownReceiver(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	_, ok, err := ReadLastReceiverEvent(context.Background(), db, "does-not-exist")
	if err != nil {
		t.Fatalf("readLastReceiverEvent() error: %v", err)
	}

	if ok {
		t.Error("readLastReceiverEvent() ok = true, want false for a receiver with no events")
	}
}
