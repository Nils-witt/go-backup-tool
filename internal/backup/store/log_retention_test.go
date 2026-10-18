package store

import (
	"context"
	"testing"
	"time"
)

func TestLogRetentionSettingsRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if _, ok, err := db.GetLogRetentionSettings(ctx); err != nil || ok {
		t.Fatalf("GetLogRetentionSettings() on an empty db = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	want := LogRetentionSettings{
		JobRunDays:       30,
		JobRunMaxRows:    0,
		TargetRunDays:    7,
		TargetRunMaxRows: 50,
		UpdatedAt:        time.Now(),
		UpdatedBy:        "alice",
	}

	if err := db.SaveLogRetentionSettings(ctx, want); err != nil {
		t.Fatalf("SaveLogRetentionSettings() error: %v", err)
	}

	got, ok, err := db.GetLogRetentionSettings(ctx)
	if err != nil || !ok {
		t.Fatalf("GetLogRetentionSettings() = (_, %v, %v), want (_, true, nil)", ok, err)
	}

	if got.JobRunDays != want.JobRunDays || got.JobRunMaxRows != want.JobRunMaxRows ||
		got.TargetRunDays != want.TargetRunDays || got.TargetRunMaxRows != want.TargetRunMaxRows ||
		got.UpdatedAt.IsZero() || got.UpdatedBy != want.UpdatedBy {
		t.Errorf("GetLogRetentionSettings() = %+v, want %+v", got, want)
	}
}

func TestDeleteOldJobRuns(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		cutoff    time.Time
		maxPerJob int
		// wantRemoved is how many rows go, of job-a's 10 runs (one a minute
		// from base, only the first one successful) and job-b's single run.
		wantRemoved int64
	}{
		{name: "no limits", wantRemoved: 0},
		{name: "max rows per job", maxPerJob: 3, wantRemoved: 6},
		{name: "cutoff", cutoff: base.Add(5 * time.Minute), wantRemoved: 4},
		{name: "both limits", cutoff: base.Add(2 * time.Minute), maxPerJob: 5, wantRemoved: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := openTestStore(t)
			ctx := context.Background()

			for i := range 10 {
				at := base.Add(time.Duration(i) * time.Minute)
				if err := db.SaveJobRun(ctx, "job-a", "ok", i == 0, at, at, 0, ""); err != nil {
					t.Fatalf("SaveJobRun() run %d error: %v", i, err)
				}
			}

			if err := db.SaveJobRun(ctx, "job-b", "ok", true, base, base, 0, ""); err != nil {
				t.Fatalf("SaveJobRun() job-b error: %v", err)
			}

			n, err := db.DeleteOldJobRuns(ctx, tt.cutoff, tt.maxPerJob)
			if err != nil {
				t.Fatalf("DeleteOldJobRuns() error: %v", err)
			}

			if n != tt.wantRemoved {
				t.Errorf("DeleteOldJobRuns() removed %d rows, want %d", n, tt.wantRemoved)
			}

			assertJobRunsKept(t, db, base)
		})
	}
}

// assertJobRunsKept checks that, whatever the limits, TestDeleteOldJobRuns'
// jobs keep their newest run and their newest successful one.
func assertJobRunsKept(t *testing.T, db *gormStore, base time.Time) {
	t.Helper()

	ctx := context.Background()

	if last, ok, err := db.GetLastRun(ctx, "job-a"); err != nil || !ok || !last.End.Equal(base.Add(9*time.Minute)) {
		t.Errorf("GetLastRun(job-a) = (%v, %v, %v), want the run at +9m", last.End, ok, err)
	}

	if success, ok, err := db.GetLastJobSuccess(ctx, "job-a"); err != nil || !ok || !success.Equal(base) {
		t.Errorf("GetLastJobSuccess(job-a) = (%v, %v, %v), want %v", success, ok, err, base)
	}

	if _, ok, err := db.GetLastRun(ctx, "job-b"); err != nil || !ok {
		t.Errorf("GetLastRun(job-b) = (_, %v, %v), want job-b's only run kept", ok, err)
	}
}

func TestDeleteOldTargetRuns(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		cutoff       time.Time
		maxPerTarget int
		// wantRemoved is how many rows go, of job-a/server-a's 10 runs (one
		// a minute from base) and job-a/server-b's and job-b/server-a's
		// single runs.
		wantRemoved int64
	}{
		{name: "no limits", wantRemoved: 0},
		{name: "max rows per target", maxPerTarget: 4, wantRemoved: 6},
		{name: "cutoff", cutoff: base.Add(3 * time.Minute), wantRemoved: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := openTestStore(t)
			ctx := context.Background()

			for i := range 10 {
				at := base.Add(time.Duration(i) * time.Minute)
				if err := db.SaveTargetRun(ctx, "job-a", true, "server-a", "ok", "", at); err != nil {
					t.Fatalf("SaveTargetRun() run %d error: %v", i, err)
				}
			}

			if err := db.SaveTargetRun(ctx, "job-a", true, "server-b", "ok", "", base); err != nil {
				t.Fatalf("SaveTargetRun() job-a/server-b error: %v", err)
			}

			if err := db.SaveTargetRun(ctx, "job-b", true, "server-a", "ok", "", base); err != nil {
				t.Fatalf("SaveTargetRun() job-b/server-a error: %v", err)
			}

			n, err := db.DeleteOldTargetRuns(ctx, tt.cutoff, tt.maxPerTarget)
			if err != nil {
				t.Fatalf("DeleteOldTargetRuns() error: %v", err)
			}

			if n != tt.wantRemoved {
				t.Errorf("DeleteOldTargetRuns() removed %d rows, want %d", n, tt.wantRemoved)
			}

			// Every job/target pair keeps its newest run, so the old single
			// runs survive any cutoff.
			for _, job := range []string{"job-a", "job-b"} {
				runs, err := db.ListTargetRuns(ctx, job)
				if err != nil {
					t.Fatalf("ListTargetRuns(%s) error: %v", job, err)
				}

				want := map[string]int{"job-a": 2, "job-b": 1}[job]
				if len(runs) != want {
					t.Errorf("ListTargetRuns(%s) returned %d targets, want %d", job, len(runs), want)
				}
			}
		})
	}
}
