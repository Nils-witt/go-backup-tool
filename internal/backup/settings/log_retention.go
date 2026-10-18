package settings

import (
	"context"
	"fmt"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

const (
	// DefaultLogRetentionMaxRows is how many rows each job keeps in the job
	// run log, and each job/target pair in the target run log, until log
	// retention settings are saved.
	DefaultLogRetentionMaxRows = 100
	// MaxLogRetentionDays bounds LogRetention's day limits.
	MaxLogRetentionDays = 3650
	// MaxLogRetentionRows bounds LogRetention's row limits.
	MaxLogRetentionRows = 100_000
)

// LogRetention is how long the job run and target run logs keep their
// rows: anything older than the Days limit goes, and so does anything
// beyond the MaxRows newest rows per job (job runs) or per job/target pair
// (target runs). A 0 means no limit of that kind. Whatever the limits, each
// job's newest run and newest successful run, and each job/target pair's
// newest run, are kept, since the dashboard and catch-up scheduling read
// them.
type LogRetention struct {
	JobRunDays       int `yaml:"job-run-days" json:"job_run_days"`
	JobRunMaxRows    int `yaml:"job-run-max-rows" json:"job_run_max_rows"`
	TargetRunDays    int `yaml:"target-run-days" json:"target_run_days"`
	TargetRunMaxRows int `yaml:"target-run-max-rows" json:"target_run_max_rows"`
}

// DefaultLogRetention is the log retention in effect until settings are
// saved: no age limit, and DefaultLogRetentionMaxRows rows.
func DefaultLogRetention() LogRetention {
	return LogRetention{JobRunMaxRows: DefaultLogRetentionMaxRows, TargetRunMaxRows: DefaultLogRetentionMaxRows}
}

// JobRunCutoff returns the time before which job runs are deleted, or the
// zero time if they have no age limit.
func (r LogRetention) JobRunCutoff(now time.Time) time.Time { return cutoff(now, r.JobRunDays) }

// TargetRunCutoff returns the time before which target runs are deleted,
// or the zero time if they have no age limit.
func (r LogRetention) TargetRunCutoff(now time.Time) time.Time { return cutoff(now, r.TargetRunDays) }

func cutoff(now time.Time, days int) time.Time {
	if days <= 0 {
		return time.Time{}
	}

	return now.AddDate(0, 0, -days)
}

// validate reports the first of r's limits that is out of range.
func (r LogRetention) validate() error {
	for _, f := range []struct {
		name       string
		value, max int
	}{
		{"job-run-days", r.JobRunDays, MaxLogRetentionDays},
		{"job-run-max-rows", r.JobRunMaxRows, MaxLogRetentionRows},
		{"target-run-days", r.TargetRunDays, MaxLogRetentionDays},
		{"target-run-max-rows", r.TargetRunMaxRows, MaxLogRetentionRows},
	} {
		if f.value < 0 || f.value > f.max {
			return fmt.Errorf("%s must be between 0 (no limit) and %d, got %d", f.name, f.max, f.value)
		}
	}

	return nil
}

func logRetentionFrom(s store.LogRetentionSettings) LogRetention {
	return LogRetention{
		JobRunDays:       s.JobRunDays,
		JobRunMaxRows:    s.JobRunMaxRows,
		TargetRunDays:    s.TargetRunDays,
		TargetRunMaxRows: s.TargetRunMaxRows,
	}
}

// loadLogRetentionLocked applies the stored log retention settings, if
// any. m.mu must be held.
func (m *Manager) loadLogRetentionLocked(ctx context.Context) {
	stored, ok, err := m.db.GetLogRetentionSettings(ctx)
	if err != nil {
		m.log.Error("reading log retention settings from the state db", "err", err)
		return
	}

	if !ok {
		return
	}

	r := logRetentionFrom(stored)
	if err := r.validate(); err != nil {
		m.log.Error("log retention settings are invalid; the defaults apply until fixed in the web UI", "err", err)
		return
	}

	m.logRetention.Store(&r)
}

// LogRetention returns the log retention currently in effect.
func (m *Manager) LogRetention() LogRetention { return *m.logRetention.Load() }

// LogRetentionChanged receives a value after each UpdateLogRetention, so a
// pruning loop can apply the new limits right away instead of at its next
// tick.
func (m *Manager) LogRetentionChanged() <-chan struct{} { return m.logRetentionChanged }

// ManagedLogRetention is the log retention settings as the web UI shows
// them. UpdatedAt is zero, and UpdatedBy empty, while none were ever saved
// (the defaults then apply).
type ManagedLogRetention struct {
	LogRetention

	UpdatedAt time.Time
	UpdatedBy string
}

// GetLogRetention returns the stored log retention settings, or
// DefaultLogRetention if none were ever saved.
func (m *Manager) GetLogRetention(ctx context.Context) (ManagedLogRetention, error) {
	if m.db == nil {
		return ManagedLogRetention{}, ErrStoreUnavailable
	}

	stored, ok, err := m.db.GetLogRetentionSettings(ctx)
	if err != nil {
		return ManagedLogRetention{}, err
	}

	if !ok {
		return ManagedLogRetention{LogRetention: DefaultLogRetention()}, nil
	}

	return ManagedLogRetention{LogRetention: logRetentionFrom(stored), UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy}, nil
}

// UpdateLogRetention validates and stores r as the log retention settings
// and applies them; rows past the new limits are deleted shortly after (see
// LogRetentionChanged). Validation failures wrap ErrInvalid.
func (m *Manager) UpdateLogRetention(ctx context.Context, user string, r LogRetention) error {
	if m.db == nil {
		return ErrStoreUnavailable
	}

	if err := r.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.db.SaveLogRetentionSettings(ctx, store.LogRetentionSettings{
		JobRunDays:       r.JobRunDays,
		JobRunMaxRows:    r.JobRunMaxRows,
		TargetRunDays:    r.TargetRunDays,
		TargetRunMaxRows: r.TargetRunMaxRows,
		UpdatedAt:        time.Now(),
		UpdatedBy:        user,
	}); err != nil {
		return err
	}

	m.logRetention.Store(&r)

	select {
	case m.logRetentionChanged <- struct{}{}:
	default: // a change is already pending; the pruning loop reads the latest
	}

	m.log.Info("log retention settings updated",
		"job_run_days", r.JobRunDays, "job_run_max_rows", r.JobRunMaxRows,
		"target_run_days", r.TargetRunDays, "target_run_max_rows", r.TargetRunMaxRows, "by", user)

	return nil
}
