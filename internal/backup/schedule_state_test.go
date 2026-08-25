package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func openTestStateDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.db")

	db, err := openScheduleStateDB(context.Background(), path)
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
	if err := writeLastSuccess(ctx, db, "job-a", want); err != nil {
		t.Fatalf("writeLastSuccess() error: %v", err)
	}

	got, ok, err := readLastSuccess(ctx, db, "job-a")
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

	_, ok, err := readLastSuccess(context.Background(), db, "no-such-job")
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

	if err := writeLastSuccess(ctx, db, "job-a", first); err != nil {
		t.Fatalf("writeLastSuccess() first error: %v", err)
	}

	if err := writeLastSuccess(ctx, db, "job-a", second); err != nil {
		t.Fatalf("writeLastSuccess() second error: %v", err)
	}

	got, ok, err := readLastSuccess(ctx, db, "job-a")
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

	want := lastRun{
		Start: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 1, 3, 0, 5, 0, time.UTC),
		State: stateOK,
		Size:  2048,
	}

	if err := writeLastRun(ctx, db, "job-a", want); err != nil {
		t.Fatalf("writeLastRun() error: %v", err)
	}

	got, ok, err := readLastRun(ctx, db, "job-a")
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

	if err := writeTargetRun(ctx, db, "job-a", 0, stateOK, "", at); err != nil {
		t.Fatalf("writeTargetRun() target 0 error: %v", err)
	}

	if err := writeTargetRun(ctx, db, "job-a", 1, stateFailed, "boom", at); err != nil {
		t.Fatalf("writeTargetRun() target 1 error: %v", err)
	}

	got, err := readTargetRuns(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readTargetRuns() error: %v", err)
	}

	want := map[int]targetRun{
		0: {Index: 0, State: stateOK, Error: ""},
		1: {Index: 1, State: stateFailed, Error: "boom"},
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

	got, err := readTargetRuns(context.Background(), db, "no-such-job")
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

	if err := writeTargetRun(ctx, db, "job-a", 0, stateOK, "", first); err != nil {
		t.Fatalf("writeTargetRun() first error: %v", err)
	}

	if err := writeTargetRun(ctx, db, "job-a", 0, stateFailed, "boom", second); err != nil {
		t.Fatalf("writeTargetRun() second error: %v", err)
	}

	got, err := readTargetRuns(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readTargetRuns() error: %v", err)
	}

	if len(got) != 1 || got[0].State != stateFailed || got[0].Error != "boom" {
		t.Errorf("readTargetRuns() = %+v, want a single failed entry (second write should overwrite first)", got)
	}
}

func TestReadLastRunUnknownJob(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)

	_, ok, err := readLastRun(context.Background(), db, "no-such-job")
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
	if err := writeLastSuccess(ctx, db, "job-a", time.Now()); err != nil {
		t.Fatalf("writeLastSuccess() error: %v", err)
	}

	_, ok, err := readLastRun(ctx, db, "job-a")
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
	if err := writeLastSuccess(ctx, db, "job-a", success); err != nil {
		t.Fatalf("writeLastSuccess() error: %v", err)
	}

	failedRun := lastRun{
		Start: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 1, 9, 0, 1, 0, time.UTC),
		State: stateFailed,
		Error: "boom",
	}

	if err := writeLastRun(ctx, db, "job-a", failedRun); err != nil {
		t.Fatalf("writeLastRun() error: %v", err)
	}

	gotSuccess, ok, err := readLastSuccess(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readLastSuccess() error: %v", err)
	}

	if !ok || !gotSuccess.Equal(success) {
		t.Errorf("readLastSuccess() = (%v, %v), want (%v, true) — a later failed run must not clobber last_success", gotSuccess, ok, success)
	}

	gotRun, ok, err := readLastRun(ctx, db, "job-a")
	if err != nil {
		t.Fatalf("readLastRun() error: %v", err)
	}

	if !ok || gotRun.State != stateFailed || gotRun.Error != "boom" {
		t.Errorf("readLastRun() = (%+v, %v), want a failed run with error %q", gotRun, ok, "boom")
	}
}

func TestScheduleStateDBPath(t *testing.T) {
	t.Parallel()

	got := scheduleStateDBPath("/etc/go-backup-tool/config.yaml")
	want := filepath.Join("/etc/go-backup-tool", scheduleStateDBName)

	if got != want {
		t.Errorf("scheduleStateDBPath() = %q, want %q", got, want)
	}
}
