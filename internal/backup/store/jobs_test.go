package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSaveGetLastSuccessRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	want := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if err := db.SaveLastSuccess(ctx, "job-a", want); err != nil {
		t.Fatalf("SaveLastSuccess() error: %v", err)
	}

	got, ok, err := db.GetLastSuccess(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastSuccess() error: %v", err)
	}

	if !ok {
		t.Fatal("GetLastSuccess() ok = false, want true")
	}

	if !got.Equal(want) {
		t.Errorf("GetLastSuccess() = %v, want %v", got, want)
	}
}

func TestGetLastSuccessUnknownJob(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)

	_, ok, err := db.GetLastSuccess(context.Background(), "no-such-job")
	if err != nil {
		t.Fatalf("GetLastSuccess() error: %v", err)
	}

	if ok {
		t.Fatal("GetLastSuccess() ok = true for unknown job, want false")
	}
}

func TestSaveLastSuccessUpsert(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	first := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	second := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	if err := db.SaveLastSuccess(ctx, "job-a", first); err != nil {
		t.Fatalf("SaveLastSuccess() first error: %v", err)
	}

	if err := db.SaveLastSuccess(ctx, "job-a", second); err != nil {
		t.Fatalf("SaveLastSuccess() second error: %v", err)
	}

	got, ok, err := db.GetLastSuccess(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastSuccess() error: %v", err)
	}

	if !ok {
		t.Fatal("GetLastSuccess() ok = false, want true")
	}

	if !got.Equal(second) {
		t.Errorf("GetLastSuccess() = %v, want %v (second write should overwrite first)", got, second)
	}
}

func TestSaveGetLastRunRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	want := LastRun{
		Start: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 1, 3, 0, 5, 0, time.UTC),
		State: "ok",
		Size:  2048,
	}

	if err := db.SaveLastRun(ctx, "job-a", want); err != nil {
		t.Fatalf("SaveLastRun() error: %v", err)
	}

	got, ok, err := db.GetLastRun(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastRun() error: %v", err)
	}

	if !ok {
		t.Fatal("GetLastRun() ok = false, want true")
	}

	if !got.Start.Equal(want.Start) || !got.End.Equal(want.End) || got.State != want.State || got.Error != want.Error || got.Size != want.Size {
		t.Errorf("GetLastRun() = %+v, want %+v", got, want)
	}
}

func TestSaveGetTargetRunRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)

	if err := db.SaveTargetRun(ctx, "job-a", 0, "ok", "", at); err != nil {
		t.Fatalf("SaveTargetRun() target 0 error: %v", err)
	}

	if err := db.SaveTargetRun(ctx, "job-a", 1, "failed", "boom", at); err != nil {
		t.Fatalf("SaveTargetRun() target 1 error: %v", err)
	}

	got, err := db.ListTargetRuns(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListTargetRuns() error: %v", err)
	}

	want := map[int]TargetRun{
		0: {Index: 0, State: "ok", Error: ""},
		1: {Index: 1, State: "failed", Error: "boom"},
	}

	if len(got) != len(want) {
		t.Fatalf("ListTargetRuns() = %+v, want %d entries", got, len(want))
	}

	for _, tr := range got {
		if w, ok := want[tr.Index]; !ok || tr != w {
			t.Errorf("ListTargetRuns() entry %+v, want %+v", tr, want[tr.Index])
		}
	}
}

func TestListTargetRunsUnknownJob(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)

	got, err := db.ListTargetRuns(context.Background(), "no-such-job")
	if err != nil {
		t.Fatalf("ListTargetRuns() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ListTargetRuns() = %+v, want none for unknown job", got)
	}
}

func TestSaveTargetRunUpsert(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	first := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	second := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	if err := db.SaveTargetRun(ctx, "job-a", 0, "ok", "", first); err != nil {
		t.Fatalf("SaveTargetRun() first error: %v", err)
	}

	if err := db.SaveTargetRun(ctx, "job-a", 0, "failed", "boom", second); err != nil {
		t.Fatalf("SaveTargetRun() second error: %v", err)
	}

	got, err := db.ListTargetRuns(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListTargetRuns() error: %v", err)
	}

	if len(got) != 1 || got[0].State != "failed" || got[0].Error != "boom" {
		t.Errorf("ListTargetRuns() = %+v, want a single failed entry (second write should overwrite first)", got)
	}
}

func TestGetLastRunUnknownJob(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)

	_, ok, err := db.GetLastRun(context.Background(), "no-such-job")
	if err != nil {
		t.Fatalf("GetLastRun() error: %v", err)
	}

	if ok {
		t.Fatal("GetLastRun() ok = true for unknown job, want false")
	}
}

func TestGetLastRunRowFromSuccessOnlyIsNotConfused(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	// SaveLastSuccess alone creates a row with no last_run_* data; that
	// must not be misread as a persisted run.
	if err := db.SaveLastSuccess(ctx, "job-a", time.Now()); err != nil {
		t.Fatalf("SaveLastSuccess() error: %v", err)
	}

	_, ok, err := db.GetLastRun(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastRun() error: %v", err)
	}

	if ok {
		t.Fatal("GetLastRun() ok = true for a success-only row, want false")
	}
}

func TestSaveLastRunUpsertPreservesLastSuccess(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	success := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if err := db.SaveLastSuccess(ctx, "job-a", success); err != nil {
		t.Fatalf("SaveLastSuccess() error: %v", err)
	}

	failedRun := LastRun{
		Start: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 1, 9, 0, 1, 0, time.UTC),
		State: "failed",
		Error: "boom",
	}

	if err := db.SaveLastRun(ctx, "job-a", failedRun); err != nil {
		t.Fatalf("SaveLastRun() error: %v", err)
	}

	gotSuccess, ok, err := db.GetLastSuccess(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastSuccess() error: %v", err)
	}

	if !ok || !gotSuccess.Equal(success) {
		t.Errorf("GetLastSuccess() = (%v, %v), want (%v, true) — a later failed run must not clobber last_success", gotSuccess, ok, success)
	}

	gotRun, ok, err := db.GetLastRun(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastRun() error: %v", err)
	}

	if !ok || gotRun.State != "failed" || gotRun.Error != "boom" {
		t.Errorf("GetLastRun() = (%+v, %v), want a failed run with error %q", gotRun, ok, "boom")
	}
}

func TestSaveListTargetErrorsNewestFirst(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	errs := []error{
		errors.New("connection refused"),
		errors.New("access denied"),
	}

	if err := db.SaveTargetError(ctx, "job-a", 0, "s3-server", "bucket-a", time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), errs[0]); err != nil {
		t.Fatalf("SaveTargetError() error: %v", err)
	}

	if err := db.SaveTargetError(ctx, "job-a", 1, "local-server", "bucket-b", time.Date(2026, 1, 1, 3, 1, 0, 0, time.UTC), errs[1]); err != nil {
		t.Fatalf("SaveTargetError() error: %v", err)
	}

	got, err := db.ListTargetErrors(ctx, 10)
	if err != nil {
		t.Fatalf("ListTargetErrors() error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("ListTargetErrors() returned %d errors, want 2", len(got))
	}

	if got[0].JobName != "job-a" || got[0].TargetIdx != 1 || got[0].Server != "local-server" || got[0].Bucket != "bucket-b" || got[0].Error != "access denied" {
		t.Errorf("ListTargetErrors()[0] = %+v, want the most recently recorded error first", got[0])
	}

	if got[1].Server != "s3-server" || got[1].Error != "connection refused" {
		t.Errorf("ListTargetErrors()[1] = %+v, want the earlier error", got[1])
	}
}

func TestListTargetErrorsRespectsLimit(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	for i := range 5 {
		at := time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)
		if err := db.SaveTargetError(ctx, "job-a", 0, "s3-server", "bucket-a", at, errors.New("boom")); err != nil {
			t.Fatalf("SaveTargetError() error: %v", err)
		}
	}

	got, err := db.ListTargetErrors(ctx, 2)
	if err != nil {
		t.Fatalf("ListTargetErrors() error: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("ListTargetErrors(limit=2) returned %d errors, want 2", len(got))
	}
}

func TestListTargetErrorsEmpty(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)

	got, err := db.ListTargetErrors(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListTargetErrors() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ListTargetErrors() on an empty log = %+v, want none", got)
	}
}
