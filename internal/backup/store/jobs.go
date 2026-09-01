package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// jobRunsSchema is job_runs: last_success is used only by start-time-
// anchored jobs' catch-up logic and is nullable since a job may have
// persisted last-run info (below) without ever having succeeded yet. The
// last_run_* columns record the most recent run regardless of outcome, for
// every job, purely so a caller (e.g. the web UI) can still show it across a
// restart — a distinct concept from last_success, which must keep pointing
// at the last *successful* run even after a later run fails.
const jobRunsSchema = `CREATE TABLE IF NOT EXISTS job_runs (
	name           TEXT NOT NULL PRIMARY KEY,
	last_success   TIMESTAMP,
	last_run_start TIMESTAMP,
	last_run_end   TIMESTAMP,
	last_run_state TEXT,
	last_run_error TEXT,
	last_run_size  INTEGER
)`

// targetRunsSchema is target_runs: records each job target's most recently
// completed success/failure, one level below job_runs, so a restart's
// caller can also show a target's last outcome instead of every target
// reverting to "idle" until it next runs. target_idx is index-aligned with
// the job's targets: as configured.
const targetRunsSchema = `CREATE TABLE IF NOT EXISTS target_runs (
	job_name   TEXT NOT NULL,
	target_idx INTEGER NOT NULL,
	run_at     TIMESTAMP NOT NULL,
	state      TEXT NOT NULL,
	error      TEXT,
	PRIMARY KEY (job_name, target_idx)
)`

// targetErrorsSchema is target_errors: appends one row for every occurrence
// of a job target failing to upload, unlike target_runs above which only
// keeps each target's most recently completed outcome (a later failure or
// success overwrites it). server/bucket are recorded alongside job_name/
// target_idx (rather than relying on a join against the job's current
// config) so a historical error stays meaningful even after the config's
// targets: are edited or reordered later.
const targetErrorsSchema = `CREATE TABLE IF NOT EXISTS target_errors (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	at         TIMESTAMP NOT NULL,
	job_name   TEXT NOT NULL,
	target_idx INTEGER NOT NULL,
	server     TEXT NOT NULL,
	bucket     TEXT NOT NULL,
	error      TEXT NOT NULL
)`

// SaveLastSuccess records that job name last completed successfully at at.
func (s *Store) SaveLastSuccess(ctx context.Context, name string, at time.Time) error {
	const upsert = `INSERT INTO job_runs (name, last_success) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET last_success = excluded.last_success`

	if _, err := s.db.ExecContext(ctx, upsert, name, at.UTC()); err != nil {
		return fmt.Errorf("recording job %q success: %w", name, err)
	}

	return nil
}

// GetLastSuccess returns job name's last recorded successful run, and false
// if none is recorded yet.
func (s *Store) GetLastSuccess(ctx context.Context, name string) (time.Time, bool, error) {
	errMsg := fmt.Sprintf("reading job %q state", name)

	return queryRowOptional(ctx, s.db, errMsg, `SELECT last_success FROM job_runs WHERE name = ?`, []any{name}, func(row *sql.Row) (time.Time, error) {
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
// persisted by SaveLastRun/returned by GetLastRun so a restart's caller can
// still show when a job last ran instead of reverting to "never" until it
// next runs. State is the caller's own run-state value, stored and returned
// verbatim (this package has no opinion on what values it takes).
type LastRun struct {
	Start time.Time
	End   time.Time
	State string
	Error string
	Size  int64
}

// SaveLastRun records job name's most recently completed run, overwriting
// whatever was recorded for its previous run. Called for every job after
// every run, regardless of outcome or whether the job uses start-time.
func (s *Store) SaveLastRun(ctx context.Context, name string, run LastRun) error {
	const upsert = `INSERT INTO job_runs (name, last_run_start, last_run_end, last_run_state, last_run_error, last_run_size)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			last_run_start = excluded.last_run_start,
			last_run_end   = excluded.last_run_end,
			last_run_state = excluded.last_run_state,
			last_run_error = excluded.last_run_error,
			last_run_size  = excluded.last_run_size`

	if _, err := s.db.ExecContext(ctx, upsert, name, run.Start.UTC(), run.End.UTC(), run.State, run.Error, run.Size); err != nil {
		return fmt.Errorf("recording job %q last run: %w", name, err)
	}

	return nil
}

// GetLastRun returns job name's most recently persisted run, and false if
// none is recorded yet (including when a row exists for name only because
// SaveLastSuccess wrote it, without any last_run_* data alongside it).
func (s *Store) GetLastRun(ctx context.Context, name string) (LastRun, bool, error) {
	errMsg := fmt.Sprintf("reading job %q last run", name)
	query := `SELECT last_run_start, last_run_end, last_run_state, last_run_error, last_run_size FROM job_runs WHERE name = ?`

	return queryRowOptional(ctx, s.db, errMsg, query, []any{name}, func(row *sql.Row) (LastRun, error) {
		var (
			start, end     sql.NullTime
			state, errText sql.NullString
			size           sql.NullInt64
		)

		if err := row.Scan(&start, &end, &state, &errText, &size); err != nil {
			return LastRun{}, err
		}

		if !state.Valid {
			return LastRun{}, sql.ErrNoRows
		}

		return LastRun{
			Start: start.Time,
			End:   end.Time,
			State: state.String,
			Error: errText.String,
			Size:  size.Int64,
		}, nil
	})
}

// SaveTargetRun records job name's target at index's just-finished
// success/failure, overwriting whatever was recorded for that target's
// previous run. Called for every target of every run, mirroring
// SaveLastRun's per-job persistence one level down.
func (s *Store) SaveTargetRun(ctx context.Context, name string, index int, state, errText string, at time.Time) error {
	const upsert = `INSERT INTO target_runs (job_name, target_idx, run_at, state, error) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(job_name, target_idx) DO UPDATE SET
			run_at = excluded.run_at,
			state  = excluded.state,
			error  = excluded.error`

	if _, err := s.db.ExecContext(ctx, upsert, name, index, at.UTC(), state, errText); err != nil {
		return fmt.Errorf("recording job %q target %d run: %w", name, index, err)
	}

	return nil
}

// TargetRun is one job target's most recently persisted success/failure, as
// returned by ListTargetRuns.
type TargetRun struct {
	Index int
	State string
	Error string
}

// ListTargetRuns returns every target run persisted for job name, one entry
// per target index that has completed at least once.
func (s *Store) ListTargetRuns(ctx context.Context, name string) ([]TargetRun, error) {
	errMsg := fmt.Sprintf("reading job %q target runs", name)

	return queryRows(ctx, s.db, errMsg, `SELECT target_idx, state, error FROM target_runs WHERE job_name = ?`, []any{name}, func(rows *sql.Rows) (TargetRun, error) {
		var (
			index   int
			state   string
			errText sql.NullString
		)

		if err := rows.Scan(&index, &state, &errText); err != nil {
			return TargetRun{}, err
		}

		return TargetRun{Index: index, State: state, Error: errText.String}, nil
	})
}

// TargetError is one recorded target upload failure, as appended by
// SaveTargetError and returned by ListTargetErrors.
type TargetError struct {
	At        time.Time
	JobName   string
	TargetIdx int
	Server    string
	Bucket    string
	Error     string
}

// SaveTargetError appends a target_errors row recording that job name's
// target at index (uploading to server/bucket) failed, at the given time,
// with targetErr. Called for every failed upload attempt — so, unlike
// target_runs, which only keeps each target's most recently completed
// outcome, this table preserves every occurrence across a repeating job's
// runs.
func (s *Store) SaveTargetError(ctx context.Context, jobName string, index int, server, bucket string, at time.Time, targetErr error) error {
	const insert = `INSERT INTO target_errors (at, job_name, target_idx, server, bucket, error) VALUES (?, ?, ?, ?, ?, ?)`

	errText := ""
	if targetErr != nil {
		errText = targetErr.Error()
	}

	if _, err := s.db.ExecContext(ctx, insert, at.UTC(), jobName, index, server, bucket, errText); err != nil {
		return fmt.Errorf("recording target error for job %q target %d: %w", jobName, index, err)
	}

	return nil
}

// ListTargetErrors returns up to limit of the most recently recorded target
// errors, newest first.
//
//nolint:dupl // same query/scan shape as ListLoginEvents by coincidence of field count; not worth a shared abstraction over queryRows for one more caller
func (s *Store) ListTargetErrors(ctx context.Context, limit int) ([]TargetError, error) {
	query := `SELECT at, job_name, target_idx, server, bucket, error FROM target_errors ORDER BY id DESC LIMIT ?`

	return queryRows(ctx, s.db, "reading target errors", query, []any{limit}, func(rows *sql.Rows) (TargetError, error) {
		var e TargetError

		if err := rows.Scan(&e.At, &e.JobName, &e.TargetIdx, &e.Server, &e.Bucket, &e.Error); err != nil {
			return TargetError{}, err
		}

		return e, nil
	})
}
