package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// jobRunModel is job_runs: an append-only history of every completed job
// run, one row per run, so both GetLastJobSuccess (start-time-anchored jobs'
// catch-up logic, filtering on success) and GetLastRun (the web UI's
// restart-survives-last-run display, regardless of outcome) can be answered
// by querying this single history rather than maintaining two overlapping
// "current state" columns.
type jobRunModel struct {
	ID        uint           `gorm:"column:id;primaryKey;autoIncrement"`
	Name      string         `gorm:"column:name;not null"`
	Success   sql.NullBool   `gorm:"column:success"`
	StartTime sql.NullTime   `gorm:"column:startTime"`
	EndTime   sql.NullTime   `gorm:"column:endTime"`
	Error     sql.NullString `gorm:"column:error"`
	Size      sql.NullInt64  `gorm:"column:size"`
	State     string         `gorm:"column:state;not null"`
}

func (jobRunModel) TableName() string { return "job_runs" }

// targetRunModel is target_runs: an append-only history of every completed
// job target run, one level below job_runs, so a restart's caller can also
// show a target's last outcome instead of every target reverting to "idle"
// until it next runs. Target names the servers: entry the target came from
// (see config.Target.ServerName) rather than its index, so a historical run
// stays meaningful even after the config's targets: are edited or reordered
// later.
type targetRunModel struct {
	ID      uint           `gorm:"column:id;primaryKey;autoIncrement"`
	JobName string         `gorm:"column:job_name;not null"`
	Success bool           `gorm:"column:success;not null"`
	Target  string         `gorm:"column:target;not null"`
	RunAt   time.Time      `gorm:"column:run_at;not null"`
	State   string         `gorm:"column:state;not null"`
	Error   sql.NullString `gorm:"column:error"`
}

func (targetRunModel) TableName() string { return "target_runs" }

// outstandingTargetUploadModel is outstanding_target_uploads: an
// append-only record of a target upload that needs to be retried at a
// later time.
type outstandingTargetUploadModel struct {
	ID       uint      `gorm:"column:id;primaryKey;autoIncrement"`
	JobName  string    `gorm:"column:job_name;not null"`
	Target   string    `gorm:"column:target;not null"`
	RunAt    time.Time `gorm:"column:run_at;not null"`
	FileName string    `gorm:"column:fileName;not null"`
}

func (outstandingTargetUploadModel) TableName() string { return "outstanding_target_uploads" }

// maxJobRunsPerJob caps how many job_runs rows SaveJobRun retains per job
// name, so the table doesn't grow unbounded over a job's lifetime.
const maxJobRunsPerJob = 100

// SaveJobRun appends a job_runs row recording that job name's run starting
// at startTime and ending at endTime just completed, succeeding or failing
// with errText (empty on success) and having written bytesWritten bytes. It
// then prunes name's older runs beyond maxJobRunsPerJob. Insert and prune
// stay raw SQL — GORM has no "keep newest N per group, delete the rest"
// chain equivalent for the prune half.
func (s *Store) SaveJobRun(ctx context.Context, name, state string, success bool, startTime, endTime time.Time, bytesWritten int64, errText string) error {
	db := s.db.WithContext(ctx)

	const insert = `INSERT INTO job_runs (name, state, success, startTime, endTime, error, size) VALUES (?, ?, ?, ?, ?, ?, ?)`
	if err := db.Exec(insert, name, state, success, startTime.UTC(), endTime.UTC(), errText, bytesWritten).Error; err != nil {
		return fmt.Errorf("recording job %q run: %w", name, err)
	}

	const prune = `DELETE FROM job_runs WHERE name = ? AND id NOT IN (
		SELECT id FROM job_runs WHERE name = ? ORDER BY id DESC LIMIT ?
	)`
	if err := db.Exec(prune, name, name, maxJobRunsPerJob).Error; err != nil {
		return fmt.Errorf("pruning job %q run history: %w", name, err)
	}

	return nil
}

// GetLastJobSuccess returns job name's last recorded successful run, and false
// if none is recorded yet.
func (s *Store) GetLastJobSuccess(ctx context.Context, name string) (time.Time, bool, error) {
	var m jobRunModel

	err := s.db.WithContext(ctx).
		Where("name = ? AND success = 1", name).
		Order("endTime DESC").
		Take(&m).Error

	if isRecordNotFound(err) {
		return time.Time{}, false, nil
	}

	if err != nil {
		return time.Time{}, false, fmt.Errorf("reading job %q state: %w", name, err)
	}

	if !m.EndTime.Valid {
		return time.Time{}, false, nil
	}

	return m.EndTime.Time, true, nil
}

// LastRun is a job's most recently completed run (success or failure), as
// persisted by SaveJobRun and returned by GetLastRun so a restart's caller
// can still show when a job last ran instead of reverting to "never" until
// it next runs.
type LastRun struct {
	Start   time.Time
	End     time.Time
	Success bool
	Error   string
	Size    int64
}

// GetLastRun returns job name's most recently persisted run, and false if
// none is recorded yet.
func (s *Store) GetLastRun(ctx context.Context, name string) (LastRun, bool, error) {
	var m jobRunModel

	err := s.db.WithContext(ctx).
		Where("name = ?", name).
		Order("endTime DESC").
		Take(&m).Error

	if isRecordNotFound(err) {
		return LastRun{}, false, nil
	}

	if err != nil {
		return LastRun{}, false, fmt.Errorf("reading job %q last run: %w", name, err)
	}

	if !m.Success.Valid {
		return LastRun{}, false, nil
	}

	return LastRun{
		Start:   m.StartTime.Time,
		End:     m.EndTime.Time,
		Success: m.Success.Bool,
		Error:   m.Error.String,
		Size:    m.Size.Int64,
	}, true, nil
}

// JobRunEvent is one historical job run, as appended by SaveJobRun and
// returned by ListJobRunEvents — unlike GetLastRun, which only returns a
// job's most recently completed run, this preserves every run across every
// job's history, for the dashboard's job run log view.
type JobRunEvent struct {
	JobName string
	Start   time.Time
	End     time.Time
	Success bool
	Size    int64
	Error   string
}

// ListJobRunEvents returns up to limit of the most recently recorded job
// runs across every job, newest first, for the dashboard's job run log
// view.
func (s *Store) ListJobRunEvents(ctx context.Context, limit int) ([]JobRunEvent, error) {
	var rows []jobRunModel

	if err := s.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading job run events: %w", err)
	}

	events := make([]JobRunEvent, len(rows))
	for i, m := range rows {
		events[i] = JobRunEvent{
			JobName: m.Name,
			Start:   m.StartTime.Time,
			End:     m.EndTime.Time,
			Success: m.Success.Bool,
			Size:    m.Size.Int64,
			Error:   m.Error.String,
		}
	}

	return events, nil
}

// JobRunDaySummary is one job's activity over a time window, as returned by
// SummarizeJobRuns for a daily report: how many runs completed successfully,
// their total bytes written, and how many runs (of either outcome) failed.
type JobRunDaySummary struct {
	JobName       string
	RunsCompleted int
	BytesWritten  int64
	Errors        int
}

// SummarizeJobRuns returns, in job name order, every job name's
// JobRunDaySummary for the runs completed (endTime) in [start, end). Stays
// raw SQL: the conditional-aggregation columns (SUM(CASE WHEN ...)) have no
// GORM chain equivalent, mirroring SummarizeReceiverEvents.
func (s *Store) SummarizeJobRuns(ctx context.Context, start, end time.Time) ([]JobRunDaySummary, error) {
	const query = `SELECT name AS job_name,
		SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END) AS runs_completed,
		SUM(CASE WHEN success = 1 THEN size ELSE 0 END) AS bytes_written,
		SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END) AS errors
		FROM job_runs
		WHERE endTime >= ? AND endTime < ?
		GROUP BY name
		ORDER BY name`

	var out []JobRunDaySummary

	if err := s.db.WithContext(ctx).Raw(query, start.UTC(), end.UTC()).Scan(&out).Error; err != nil {
		return nil, fmt.Errorf("summarizing job runs: %w", err)
	}

	return out, nil
}

// JobRunErrorEvent is one failed job run in a time window, as returned by
// ListJobRunErrorEvents for a daily report's error listing.
type JobRunErrorEvent struct {
	At      time.Time
	JobName string
	Error   string
}

// ListJobRunErrorEvents returns every failed job run completed (endTime) in
// [start, end), oldest first, mirroring ListReceiverErrorEvents.
func (s *Store) ListJobRunErrorEvents(ctx context.Context, start, end time.Time) ([]JobRunErrorEvent, error) {
	var rows []jobRunModel

	err := s.db.WithContext(ctx).
		Where("success = 0 AND endTime >= ? AND endTime < ?", start.UTC(), end.UTC()).
		Order("endTime ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading job run error events: %w", err)
	}

	events := make([]JobRunErrorEvent, len(rows))
	for i, m := range rows {
		events[i] = JobRunErrorEvent{At: m.EndTime.Time, JobName: m.Name, Error: m.Error.String}
	}

	return events, nil
}

// maxTargetRunsPerTarget caps how many target_runs rows SaveTargetRun
// retains per job name/target pair, so the table doesn't grow unbounded
// over a target's lifetime.
const maxTargetRunsPerTarget = 100

// SaveTargetRun appends a target_runs row recording job name's target
// (target names the servers: entry it came from, see config.Target.
// ServerName) just-finished success/failure. Called for every target of
// every run, mirroring SaveJobRun's per-job persistence one level down. It
// then prunes that job/target pair's older runs beyond
// maxTargetRunsPerTarget.
func (s *Store) SaveTargetRun(ctx context.Context, name string, success bool, target, state, errText string, at time.Time) error {
	db := s.db.WithContext(ctx)

	const insert = `INSERT INTO target_runs (job_name, success, target, run_at, state, error) VALUES (?, ?, ?, ?, ?, ?)`
	if err := db.Exec(insert, name, success, target, at.UTC(), state, errText).Error; err != nil {
		return fmt.Errorf("recording job %q target %q run: %w", name, target, err)
	}

	const prune = `DELETE FROM target_runs WHERE job_name = ? AND target = ? AND id NOT IN (
		SELECT id FROM target_runs WHERE job_name = ? AND target = ? ORDER BY id DESC LIMIT ?
	)`
	if err := db.Exec(prune, name, target, name, target, maxTargetRunsPerTarget).Error; err != nil {
		return fmt.Errorf("pruning job %q target %q run history: %w", name, target, err)
	}

	return nil
}

// TargetRun is one job target's most recently persisted success/failure, as
// returned by ListTargetRuns.
type TargetRun struct {
	Target string
	State  string
	Error  string
}

// ListTargetRuns returns each of job name's targets' most recently
// persisted run, one entry per target that has completed at least once.
// Stays raw SQL: the correlated "latest row per target" subquery has no
// GORM greatest-n-per-group builder equivalent.
func (s *Store) ListTargetRuns(ctx context.Context, name string) ([]TargetRun, error) {
	query := `SELECT target AS target, state AS state, COALESCE(error, '') AS error FROM target_runs
		WHERE job_name = ? AND id IN (
			SELECT MAX(id) FROM target_runs WHERE job_name = ? GROUP BY target
		)`

	var out []TargetRun

	if err := s.db.WithContext(ctx).Raw(query, name, name).Scan(&out).Error; err != nil {
		return nil, fmt.Errorf("reading job %q target runs: %w", name, err)
	}

	return out, nil
}

// TargetRunEvent is one historical job target run, as appended by
// SaveTargetRun and returned by ListTargetRunEvents — unlike ListTargetRuns,
// which only returns each target's most recently completed outcome, this
// preserves every run across every job's history, for the dashboard's
// target run log view.
type TargetRunEvent struct {
	At      time.Time
	JobName string
	Target  string
	Success bool
	State   string
	Error   string
}

// ListTargetRunEvents returns up to limit of the most recently recorded
// target runs across every job, newest first, for the dashboard's target
// run log view.
func (s *Store) ListTargetRunEvents(ctx context.Context, limit int) ([]TargetRunEvent, error) {
	var rows []targetRunModel

	if err := s.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading target run events: %w", err)
	}

	events := make([]TargetRunEvent, len(rows))
	for i, m := range rows {
		events[i] = TargetRunEvent{At: m.RunAt, JobName: m.JobName, Target: m.Target, Success: m.Success, State: m.State, Error: m.Error.String}
	}

	return events, nil
}

// AddOutstandingTargetUpload records a target upload that needs to be retried at a later time.
func (s *Store) AddOutstandingTargetUpload(ctx context.Context, jobName, targetName, fileName string, retryAt time.Time) error {
	m := outstandingTargetUploadModel{JobName: jobName, Target: targetName, RunAt: retryAt, FileName: fileName}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("recording outstanding target upload for job %q target %q: %w", jobName, targetName, err)
	}

	return nil
}
