package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// logRetentionSettingsModel is log_retention_settings: the single row (id
// 1) holding how long the job run and target run logs keep their rows (see
// internal/backup/settings' Manager), managed in the web UI. A 0 means no
// limit of that kind.
type logRetentionSettingsModel struct {
	ID               uint      `gorm:"column:id;primaryKey"`
	JobRunDays       int       `gorm:"column:job_run_days;not null;default:0"`
	JobRunMaxRows    int       `gorm:"column:job_run_max_rows;not null;default:0"`
	TargetRunDays    int       `gorm:"column:target_run_days;not null;default:0"`
	TargetRunMaxRows int       `gorm:"column:target_run_max_rows;not null;default:0"`
	UpdatedAt        time.Time `gorm:"column:updated_at;not null"`
	UpdatedBy        string    `gorm:"column:updated_by;not null"`
}

func (logRetentionSettingsModel) TableName() string { return "log_retention_settings" }

// logRetentionSettingsID is log_retention_settings' only row.
const logRetentionSettingsID = 1

// LogRetentionSettings is the stored log retention settings (see
// logRetentionSettingsModel).
type LogRetentionSettings struct {
	JobRunDays       int
	JobRunMaxRows    int
	TargetRunDays    int
	TargetRunMaxRows int
	UpdatedAt        time.Time
	UpdatedBy        string
}

// GetLogRetentionSettings returns the stored log retention settings,
// reporting false if none have been saved yet.
func (s *gormStore) GetLogRetentionSettings(ctx context.Context) (LogRetentionSettings, bool, error) {
	m, ok, err := takeSettingsRow[logRetentionSettingsModel](ctx, s, logRetentionSettingsID, "log retention settings")
	if !ok || err != nil {
		return LogRetentionSettings{}, ok, err
	}

	return LogRetentionSettings{
		JobRunDays: m.JobRunDays, JobRunMaxRows: m.JobRunMaxRows,
		TargetRunDays: m.TargetRunDays, TargetRunMaxRows: m.TargetRunMaxRows,
		UpdatedAt: m.UpdatedAt, UpdatedBy: m.UpdatedBy,
	}, true, nil
}

// SaveLogRetentionSettings stores r as the log retention settings,
// replacing any saved before.
func (s *gormStore) SaveLogRetentionSettings(ctx context.Context, r LogRetentionSettings) error {
	m := logRetentionSettingsModel{
		ID:         logRetentionSettingsID,
		JobRunDays: r.JobRunDays, JobRunMaxRows: r.JobRunMaxRows,
		TargetRunDays: r.TargetRunDays, TargetRunMaxRows: r.TargetRunMaxRows,
		UpdatedAt: r.UpdatedAt.UTC(), UpdatedBy: r.UpdatedBy,
	}

	if err := s.db.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("saving log retention settings: %w", err)
	}

	return nil
}

// DeleteOldJobRuns deletes the job_runs rows that ended before cutoff, and
// those beyond each job's maxPerJob newest, returning how many it removed.
// A zero cutoff or a maxPerJob of 0 skips that limit. It never deletes a
// job's newest run or its newest successful run, which GetLastRun and
// GetLastJobSuccess (a start-time-anchored job's catch-up) depend on. Stays
// raw SQL: GORM has no "keep newest N per group" chain equivalent.
func (s *gormStore) DeleteOldJobRuns(ctx context.Context, cutoff time.Time, maxPerJob int) (int64, error) {
	const keep = `id NOT IN (SELECT MAX(id) FROM job_runs GROUP BY name)
		AND id NOT IN (SELECT MAX(id) FROM job_runs WHERE success GROUP BY name)`

	n, err := s.pruneRuns(ctx, pruneQueries{
		byAge: `DELETE FROM job_runs WHERE "endTime" < ? AND ` + keep,
		byCount: `DELETE FROM job_runs WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY name ORDER BY id DESC) AS rn FROM job_runs
			) ranked WHERE rn > ?
		) AND ` + keep,
	}, cutoff, maxPerJob)
	if err != nil {
		return 0, fmt.Errorf("pruning job run history: %w", err)
	}

	return n, nil
}

// DeleteOldTargetRuns deletes the target_runs rows recorded before cutoff, and
// those beyond each job/target pair's maxPerTarget newest, returning how
// many it removed. A zero cutoff or a maxPerTarget of 0 skips that limit.
// It never deletes a pair's newest run, which ListTargetRuns depends on.
func (s *gormStore) DeleteOldTargetRuns(ctx context.Context, cutoff time.Time, maxPerTarget int) (int64, error) {
	const keep = `id NOT IN (SELECT MAX(id) FROM target_runs GROUP BY job_name, target)`

	n, err := s.pruneRuns(ctx, pruneQueries{
		byAge: `DELETE FROM target_runs WHERE run_at < ? AND ` + keep,
		byCount: `DELETE FROM target_runs WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY job_name, target ORDER BY id DESC) AS rn FROM target_runs
			) ranked WHERE rn > ?
		) AND ` + keep,
	}, cutoff, maxPerTarget)
	if err != nil {
		return 0, fmt.Errorf("pruning target run history: %w", err)
	}

	return n, nil
}

// pruneQueries are one run history table's two prune statements: byAge
// takes the cutoff, byCount the number of rows to keep per group.
type pruneQueries struct {
	byAge, byCount string
}

// pruneRuns runs q's statements whose limit is set, in one transaction,
// returning how many rows they removed in total.
func (s *gormStore) pruneRuns(ctx context.Context, q pruneQueries, cutoff time.Time, maxRows int) (int64, error) {
	var total int64

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !cutoff.IsZero() {
			res := tx.Exec(q.byAge, cutoff.UTC())
			if res.Error != nil {
				return res.Error
			}

			total += res.RowsAffected
		}

		if maxRows > 0 {
			res := tx.Exec(q.byCount, maxRows)
			if res.Error != nil {
				return res.Error
			}

			total += res.RowsAffected
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	return total, nil
}
