package store

import (
	"context"
	"testing"
	"time"
)

func TestSaveGetLastJobSuccessRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	start := time.Date(2026, 1, 1, 2, 59, 0, 0, time.UTC)
	want := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)

	if err := db.SaveJobRun(ctx, "job-a", "ok", true, start, want, 0, ""); err != nil {
		t.Fatalf("SaveJobRun() error: %v", err)
	}

	got, ok, err := db.GetLastJobSuccess(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastJobSuccess() error: %v", err)
	}

	if !ok {
		t.Fatal("GetLastJobSuccess() ok = false, want true")
	}

	if !got.Equal(want) {
		t.Errorf("GetLastJobSuccess() = %v, want %v", got, want)
	}
}

func TestGetLastJobSuccessUnknownJob(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)

	_, ok, err := db.GetLastJobSuccess(context.Background(), "no-such-job")
	if err != nil {
		t.Fatalf("GetLastJobSuccess() error: %v", err)
	}

	if ok {
		t.Fatal("GetLastJobSuccess() ok = true for unknown job, want false")
	}
}

func TestGetLastJobSuccessIgnoresFailedRuns(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	success := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if err := db.SaveJobRun(ctx, "job-a", "ok", true, success, success, 1024, ""); err != nil {
		t.Fatalf("SaveJobRun() success error: %v", err)
	}

	failed := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if err := db.SaveJobRun(ctx, "job-a", "failed", false, failed, failed, 0, "boom"); err != nil {
		t.Fatalf("SaveJobRun() failure error: %v", err)
	}

	got, ok, err := db.GetLastJobSuccess(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastJobSuccess() error: %v", err)
	}

	if !ok || !got.Equal(success) {
		t.Errorf("GetLastJobSuccess() = (%v, %v), want (%v, true) — a later failed run must not clobber the last success", got, ok, success)
	}
}

func TestSaveJobRunPrunesOldRunsPastLimit(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range maxJobRunsPerJob + 10 {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := db.SaveJobRun(ctx, "job-a", "ok", true, at, at, 0, ""); err != nil {
			t.Fatalf("SaveJobRun() run %d error: %v", i, err)
		}
	}

	events, err := db.ListJobRunEvents(ctx, maxJobRunsPerJob*2)
	if err != nil {
		t.Fatalf("ListJobRunEvents() error: %v", err)
	}

	if len(events) != maxJobRunsPerJob {
		t.Fatalf("ListJobRunEvents() returned %d runs, want %d after pruning", len(events), maxJobRunsPerJob)
	}

	want := base.Add(time.Duration(maxJobRunsPerJob+9) * time.Minute)
	if !events[0].Start.Equal(want) {
		t.Errorf("newest surviving run Start = %v, want %v (the most recent run)", events[0].Start, want)
	}
}

func TestSaveJobRunPruningIsPerJob(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range maxJobRunsPerJob + 5 {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := db.SaveJobRun(ctx, "job-a", "ok", true, at, at, 0, ""); err != nil {
			t.Fatalf("SaveJobRun() job-a run %d error: %v", i, err)
		}
	}

	if err := db.SaveJobRun(ctx, "job-b", "ok", true, base, base, 0, ""); err != nil {
		t.Fatalf("SaveJobRun() job-b error: %v", err)
	}

	events, err := db.ListJobRunEvents(ctx, maxJobRunsPerJob*2)
	if err != nil {
		t.Fatalf("ListJobRunEvents() error: %v", err)
	}

	if len(events) != maxJobRunsPerJob+1 {
		t.Fatalf("ListJobRunEvents() returned %d runs, want %d (job-a pruned to %d, job-b untouched)", len(events), maxJobRunsPerJob+1, maxJobRunsPerJob)
	}
}

func TestSaveGetLastRunRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	start := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 3, 0, 5, 0, time.UTC)

	if err := db.SaveJobRun(ctx, "job-a", "ok", true, start, end, 2048, ""); err != nil {
		t.Fatalf("SaveJobRun() error: %v", err)
	}

	got, ok, err := db.GetLastRun(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastRun() error: %v", err)
	}

	if !ok {
		t.Fatal("GetLastRun() ok = false, want true")
	}

	want := LastRun{Start: start, End: end, Success: true, Size: 2048}
	if !got.Start.Equal(want.Start) || !got.End.Equal(want.End) || got.Success != want.Success || got.Error != want.Error || got.Size != want.Size {
		t.Errorf("GetLastRun() = %+v, want %+v", got, want)
	}
}

func TestGetLastRunReturnsMostRecentRun(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	first := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	second := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	if err := db.SaveJobRun(ctx, "job-a", "ok", true, first, first, 0, ""); err != nil {
		t.Fatalf("SaveJobRun() first error: %v", err)
	}

	if err := db.SaveJobRun(ctx, "job-a", "failed", false, second, second, 0, "boom"); err != nil {
		t.Fatalf("SaveJobRun() second error: %v", err)
	}

	got, ok, err := db.GetLastRun(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetLastRun() error: %v", err)
	}

	if !ok || got.Success || got.Error != "boom" {
		t.Errorf("GetLastRun() = (%+v, %v), want the most recently recorded (failed) run", got, ok)
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

func TestSaveGetTargetRunRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)

	if err := db.SaveTargetRun(ctx, "job-a", true, "server-a", "ok", "", at); err != nil {
		t.Fatalf("SaveTargetRun() target server-a error: %v", err)
	}

	if err := db.SaveTargetRun(ctx, "job-a", false, "server-b", "failed", "boom", at); err != nil {
		t.Fatalf("SaveTargetRun() target server-b error: %v", err)
	}

	got, err := db.ListTargetRuns(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListTargetRuns() error: %v", err)
	}

	want := map[string]TargetRun{
		"server-a": {Target: "server-a", State: "ok", Error: ""},
		"server-b": {Target: "server-b", State: "failed", Error: "boom"},
	}

	if len(got) != len(want) {
		t.Fatalf("ListTargetRuns() = %+v, want %d entries", got, len(want))
	}

	for _, tr := range got {
		if w, ok := want[tr.Target]; !ok || tr != w {
			t.Errorf("ListTargetRuns() entry %+v, want %+v", tr, want[tr.Target])
		}
	}
}

func TestSaveTargetRunPrunesOldRunsPastLimit(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range maxTargetRunsPerTarget + 10 {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := db.SaveTargetRun(ctx, "job-a", true, "server-a", "ok", "", at); err != nil {
			t.Fatalf("SaveTargetRun() run %d error: %v", i, err)
		}
	}

	events, err := db.ListTargetRunEvents(ctx, maxTargetRunsPerTarget*2)
	if err != nil {
		t.Fatalf("ListTargetRunEvents() error: %v", err)
	}

	if len(events) != maxTargetRunsPerTarget {
		t.Fatalf("ListTargetRunEvents() returned %d runs, want %d after pruning", len(events), maxTargetRunsPerTarget)
	}

	want := base.Add(time.Duration(maxTargetRunsPerTarget+9) * time.Minute)
	if !events[0].At.Equal(want) {
		t.Errorf("newest surviving run At = %v, want %v (the most recent run)", events[0].At, want)
	}
}

func TestSaveTargetRunPruningIsPerJobAndTarget(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range maxTargetRunsPerTarget + 5 {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := db.SaveTargetRun(ctx, "job-a", true, "server-a", "ok", "", at); err != nil {
			t.Fatalf("SaveTargetRun() job-a/server-a run %d error: %v", i, err)
		}
	}

	if err := db.SaveTargetRun(ctx, "job-a", true, "server-b", "ok", "", base); err != nil {
		t.Fatalf("SaveTargetRun() job-a/server-b error: %v", err)
	}

	if err := db.SaveTargetRun(ctx, "job-b", true, "server-a", "ok", "", base); err != nil {
		t.Fatalf("SaveTargetRun() job-b/server-a error: %v", err)
	}

	events, err := db.ListTargetRunEvents(ctx, maxTargetRunsPerTarget*2)
	if err != nil {
		t.Fatalf("ListTargetRunEvents() error: %v", err)
	}

	want := maxTargetRunsPerTarget + 2
	if len(events) != want {
		t.Fatalf("ListTargetRunEvents() returned %d runs, want %d (job-a/server-a pruned to %d, other pairs untouched)", len(events), want, maxTargetRunsPerTarget)
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

func TestListTargetRunsReturnsMostRecentPerTarget(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	first := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	second := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	if err := db.SaveTargetRun(ctx, "job-a", true, "server-a", "ok", "", first); err != nil {
		t.Fatalf("SaveTargetRun() first error: %v", err)
	}

	if err := db.SaveTargetRun(ctx, "job-a", false, "server-a", "failed", "boom", second); err != nil {
		t.Fatalf("SaveTargetRun() second error: %v", err)
	}

	got, err := db.ListTargetRuns(ctx, "job-a")
	if err != nil {
		t.Fatalf("ListTargetRuns() error: %v", err)
	}

	if len(got) != 1 || got[0].State != "failed" || got[0].Error != "boom" {
		t.Errorf("ListTargetRuns() = %+v, want a single failed entry (most recent run should win)", got)
	}
}
