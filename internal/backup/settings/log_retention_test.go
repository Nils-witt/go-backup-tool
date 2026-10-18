package settings

import (
	"errors"
	"testing"
	"time"
)

func TestLogRetentionCutoffs(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	r := LogRetention{JobRunDays: 7}

	if got, want := r.JobRunCutoff(now), now.AddDate(0, 0, -7); !got.Equal(want) {
		t.Errorf("JobRunCutoff() = %v, want %v", got, want)
	}

	if got := r.TargetRunCutoff(now); !got.IsZero() {
		t.Errorf("TargetRunCutoff() with no day limit = %v, want the zero time", got)
	}
}

func TestManager_UpdateLogRetention(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	m := newTestManager(t, db)
	ctx := t.Context()

	if got := m.LogRetention(); got != DefaultLogRetention() {
		t.Errorf("LogRetention() before any save = %+v, want the defaults", got)
	}

	if err := m.UpdateLogRetention(ctx, "erin", LogRetention{JobRunDays: MaxLogRetentionDays + 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("UpdateLogRetention() with too many days error = %v, want ErrInvalid", err)
	}

	want := LogRetention{JobRunDays: 30, TargetRunDays: 7, TargetRunMaxRows: 20}
	if err := m.UpdateLogRetention(ctx, "erin", want); err != nil {
		t.Fatalf("UpdateLogRetention() error: %v", err)
	}

	if got := m.LogRetention(); got != want {
		t.Errorf("LogRetention() = %+v, want %+v", got, want)
	}

	select {
	case <-m.LogRetentionChanged():
	default:
		t.Error("LogRetentionChanged() didn't signal the update")
	}

	// A fresh Manager over the same db loads what was saved.
	reloaded := newTestManager(t, db)
	reloaded.Load(ctx)

	if got := reloaded.LogRetention(); got != want {
		t.Errorf("LogRetention() after Load = %+v, want %+v", got, want)
	}

	got, err := reloaded.GetLogRetention(ctx)
	if err != nil {
		t.Fatalf("GetLogRetention() error: %v", err)
	}

	if got.LogRetention != want || got.UpdatedBy != "erin" || got.UpdatedAt.IsZero() {
		t.Errorf("GetLogRetention() = %+v, want %+v by erin", got, want)
	}
}
