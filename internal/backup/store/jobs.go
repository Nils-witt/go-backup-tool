package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// jobRunsSchema is job_runs: an append-only history of every completed job
// run, one row per run, so both GetLastJobSuccess (start-time-anchored jobs'
// catch-up logic, filtering on success) and GetLastRun (the web UI's
// restart-survives-last-run display, regardless of outcome) can be answered
// by querying this single history rather than maintaining two overlapping
// "current state" columns.
const jobRunsSchema = `CREATE TABLE IF NOT EXISTS job_runs (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	name      TEXT NOT NULL,
	success   BOOLEAN,
	startTime TIMESTAMP,
	endTime   TIMESTAMP,
	error     TEXT,
	size      INTEGER,
	state     TEXT NOT NULL
)`

// targetRunsSchema is target_runs: an append-only history of every
// completed job target run, one level below job_runs, so a restart's
// caller can also show a target's last outcome instead of every target
// reverting to "idle" until it next runs. target names the servers: entry
// the target came from (see config.Target.ServerName) rather than its
// index, so a historical run stays meaningful even after the config's
// targets: are edited or reordered later.
const targetRunsSchema = `CREATE TABLE IF NOT EXISTS target_runs (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	job_name TEXT NOT NULL,
	success  BOOLEAN NOT NULL,
	target   TEXT NOT NULL,
	run_at   TIMESTAMP NOT NULL,
	state    TEXT NOT NULL,
	error    TEXT
)`

const outstandingTargetUploads = `CREATE TABLE IF NOT EXISTS outstanding_target_uploads (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	job_name TEXT NOT NULL,
	target   TEXT NOT NULL,
	run_at   TIMESTAMP NOT NULL,
	fileName  TEXT NOT NULL
)`

// maxJobRunsPerJob caps how many job_runs rows SaveJobRun retains per job
// name, so the table doesn't grow unbounded over a job's lifetime.
const maxJobRunsPerJob = 100

// SaveJobRun appends a job_runs row recording that job name's run starting
// at startTime and ending at endTime just completed, succeeding or failing
// with errText (empty on success) and having written bytesWritten bytes. It
// then prunes name's older runs beyond maxJobRunsPerJob.
func (s *Store) SaveJobRun(ctx context.Context, name, state string, success bool, startTime, endTime time.Time, bytesWritten int64, errText string) error {
	const insert = `INSERT INTO job_runs (name, state, success, startTime, endTime, error, size) VALUES (?, ?, ?, ?, ?, ?, ?)`

	if _, err := s.db.ExecContext(ctx, insert, name, state, success, startTime.UTC(), endTime.UTC(), errText, bytesWritten); err != nil {
		return fmt.Errorf("recording job %q run: %w", name, err)
	}

	const prune = `DELETE FROM job_runs WHERE name = ? AND id NOT IN (
		SELECT id FROM job_runs WHERE name = ? ORDER BY id DESC LIMIT ?
	)`

	if _, err := s.db.ExecContext(ctx, prune, name, name, maxJobRunsPerJob); err != nil {
		return fmt.Errorf("pruning job %q run history: %w", name, err)
	}

	return nil
}

// GetLastJobSuccess returns job name's last recorded successful run, and false
// if none is recorded yet.
func (s *Store) GetLastJobSuccess(ctx context.Context, name string) (time.Time, bool, error) {
	errMsg := fmt.Sprintf("reading job %q state", name)

	return queryRowOptional(ctx, s.db, errMsg, `SELECT endTime FROM job_runs WHERE name = ? AND success = 1 ORDER BY endTime DESC LIMIT 1`, []any{name}, func(row *sql.Row) (time.Time, error) {
		var t sql.NullTime
		if err := row.Scan(&t); err != nil {
			return time.Time{}, err
		}

		if !t.Valid {
			return time.Time{}, sql.ErrNoRows
		}

		return t.Time, nil
	})
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
	errMsg := fmt.Sprintf("reading job %q last run", name)
	query := `SELECT success, "startTime", "endTime", error, size FROM job_runs WHERE name = ? ORDER BY endTime DESC LIMIT 1`

	return queryRowOptional(ctx, s.db, errMsg, query, []any{name}, func(row *sql.Row) (LastRun, error) {
		var (
			start, end sql.NullTime
			errText    sql.NullString
			size       sql.NullInt64
			success    sql.NullBool
		)

		if err := row.Scan(&success, &start, &end, &errText, &size); err != nil {
			return LastRun{}, err
		}

		if !success.Valid {
			return LastRun{}, sql.ErrNoRows
		}

		return LastRun{
			Start:   start.Time,
			End:     end.Time,
			Success: success.Bool,
			Error:   errText.String,
			Size:    size.Int64,
		}, nil
	})
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
	query := `SELECT name, "startTime", "endTime", success, size, error FROM job_runs ORDER BY id DESC LIMIT ?`

	return queryRows(ctx, s.db, "reading job run events", query, []any{limit}, func(rows *sql.Rows) (JobRunEvent, error) {
		var (
			ev      JobRunEvent
			start   sql.NullTime
			end     sql.NullTime
			success sql.NullBool
			size    sql.NullInt64
			errText sql.NullString
		)

		if err := rows.Scan(&ev.JobName, &start, &end, &success, &size, &errText); err != nil {
			return JobRunEvent{}, err
		}

		ev.Start = start.Time
		ev.End = end.Time
		ev.Success = success.Bool
		ev.Size = size.Int64
		ev.Error = errText.String

		return ev, nil
	})
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
	const insert = `INSERT INTO target_runs (job_name, success, target, run_at, state, error) VALUES (?, ?, ?, ?, ?, ?)`

	if _, err := s.db.ExecContext(ctx, insert, name, success, target, at.UTC(), state, errText); err != nil {
		return fmt.Errorf("recording job %q target %q run: %w", name, target, err)
	}

	const prune = `DELETE FROM target_runs WHERE job_name = ? AND target = ? AND id NOT IN (
		SELECT id FROM target_runs WHERE job_name = ? AND target = ? ORDER BY id DESC LIMIT ?
	)`

	if _, err := s.db.ExecContext(ctx, prune, name, target, name, target, maxTargetRunsPerTarget); err != nil {
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
func (s *Store) ListTargetRuns(ctx context.Context, name string) ([]TargetRun, error) {
	errMsg := fmt.Sprintf("reading job %q target runs", name)
	query := `SELECT target, state, error FROM target_runs
		WHERE job_name = ? AND id IN (
			SELECT MAX(id) FROM target_runs WHERE job_name = ? GROUP BY target
		)`

	return queryRows(ctx, s.db, errMsg, query, []any{name, name}, func(rows *sql.Rows) (TargetRun, error) {
		var (
			target  string
			state   string
			errText sql.NullString
		)

		if err := rows.Scan(&target, &state, &errText); err != nil {
			return TargetRun{}, err
		}

		return TargetRun{Target: target, State: state, Error: errText.String}, nil
	})
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
	query := `SELECT run_at, job_name, target, success, state, error FROM target_runs ORDER BY id DESC LIMIT ?`

	return queryRows(ctx, s.db, "reading target run events", query, []any{limit}, func(rows *sql.Rows) (TargetRunEvent, error) {
		var (
			ev      TargetRunEvent
			errText sql.NullString
		)

		if err := rows.Scan(&ev.At, &ev.JobName, &ev.Target, &ev.Success, &ev.State, &errText); err != nil {
			return TargetRunEvent{}, err
		}

		ev.Error = errText.String

		return ev, nil
	})
}

// AddOutstandingTargetUpload records a target upload that needs to be retried at a later time.
func (s *Store) AddOutstandingTargetUpload(ctx context.Context, jobName, targetName, fileName string, retryAt time.Time) error {
	const insert = `INSERT INTO outstanding_target_uploads (job_name, target, run_at, fileName) VALUES (?, ?, ?, ?)`

	if _, err := s.db.ExecContext(ctx, insert, jobName, targetName, retryAt, fileName); err != nil {
		return fmt.Errorf("recording outstanding target upload for job %q target %q: %w", jobName, targetName, err)
	}

	return nil
}
