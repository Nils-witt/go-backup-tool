package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// scheduleStateDBName is the sqlite database go-backup-tool keeps alongside
// the config file, tracking each start-time-anchored job's last successful
// run so a restart can tell a genuinely missed run (nothing recorded for the
// most recent due grid slot) apart from an ordinary restart of a job that
// already ran on time.
const scheduleStateDBName = ".go-backup-tool-state.db"

// scheduleStateDBPath returns the state db path for the config file at
// configPath: a sibling file in the same directory.
func scheduleStateDBPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), scheduleStateDBName)
}

// openScheduleStateDB opens (creating if needed) the state-tracking sqlite
// database at path, ensuring its schema exists. The caller must Close it.
//
// SetMaxOpenConns(1) serializes every access through a single connection:
// sqlite handles one writer at a time regardless, and this *sql.DB is shared
// across every job's goroutine for the life of the run.
func openScheduleStateDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening job state db %q: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	// last_success is used only by start-time-anchored jobs' catch-up logic
	// (see runner.lastJobSuccess) and is nullable since a job may have
	// persisted last-run info (below) without ever having succeeded yet.
	// The last_run_* columns record the most recent run regardless of
	// outcome, for every job, purely so the web UI can still show it across
	// a restart (see runner.recordLastRun / statusStore.seedLastRun) — a
	// distinct concept from last_success, which must keep pointing at the
	// last *successful* run even after a later run fails.
	const schema = `CREATE TABLE IF NOT EXISTS job_runs (
		name           TEXT NOT NULL PRIMARY KEY,
		last_success   TIMESTAMP,
		last_run_start TIMESTAMP,
		last_run_end   TIMESTAMP,
		last_run_state TEXT,
		last_run_error TEXT,
		last_run_size  INTEGER
	)`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	// target_runs records each job target's most recently completed
	// success/failure, one level below job_runs, so a restart's web UI can
	// also show a target's last outcome instead of every target reverting
	// to "idle" until it next runs (see runner.persistTargetRun /
	// statusStore.seedTargetRun). target_idx is index-aligned with the
	// job's targets: as configured, matching statusStore.targetDone's
	// existing index-alignment assumption.
	const targetSchema = `CREATE TABLE IF NOT EXISTS target_runs (
		job_name   TEXT NOT NULL,
		target_idx INTEGER NOT NULL,
		run_at     TIMESTAMP NOT NULL,
		state      TEXT NOT NULL,
		error      TEXT,
		PRIMARY KEY (job_name, target_idx)
	)`

	if _, err := db.ExecContext(ctx, targetSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	return db, nil
}

// writeLastSuccess records that job name last completed successfully at at.
func writeLastSuccess(ctx context.Context, db *sql.DB, name string, at time.Time) error {
	const upsert = `INSERT INTO job_runs (name, last_success) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET last_success = excluded.last_success`

	if _, err := db.ExecContext(ctx, upsert, name, at.UTC()); err != nil {
		return fmt.Errorf("recording job %q success: %w", name, err)
	}

	return nil
}

// readLastSuccess returns job name's last recorded successful run, and false
// if none is recorded yet.
func readLastSuccess(ctx context.Context, db *sql.DB, name string) (time.Time, bool, error) {
	var t sql.NullTime

	err := db.QueryRowContext(ctx, `SELECT last_success FROM job_runs WHERE name = ?`, name).Scan(&t)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("reading job %q state: %w", name, err)
	case !t.Valid:
		return time.Time{}, false, nil
	default:
		return t.Time, true, nil
	}
}

// lastRun is a job's most recently completed run (success or failure),
// persisted so a restart's web UI can still show when a job last ran
// instead of reverting to "never" until it next runs (see
// runner.recordLastRun / statusStore.seedLastRun).
type lastRun struct {
	Start time.Time
	End   time.Time
	State runState
	Error string
	Size  int64
}

// writeLastRun records job name's most recently completed run, overwriting
// whatever was recorded for its previous run. Called for every job after
// every run, regardless of outcome or whether the job uses start-time.
func writeLastRun(ctx context.Context, db *sql.DB, name string, run lastRun) error {
	const upsert = `INSERT INTO job_runs (name, last_run_start, last_run_end, last_run_state, last_run_error, last_run_size)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			last_run_start = excluded.last_run_start,
			last_run_end   = excluded.last_run_end,
			last_run_state = excluded.last_run_state,
			last_run_error = excluded.last_run_error,
			last_run_size  = excluded.last_run_size`

	if _, err := db.ExecContext(ctx, upsert, name, run.Start.UTC(), run.End.UTC(), string(run.State), run.Error, run.Size); err != nil {
		return fmt.Errorf("recording job %q last run: %w", name, err)
	}

	return nil
}

// readLastRun returns job name's most recently persisted run, and false if
// none is recorded yet (including when a row exists for name only because
// writeLastSuccess wrote it, without any last_run_* data alongside it).
func readLastRun(ctx context.Context, db *sql.DB, name string) (lastRun, bool, error) {
	var (
		start, end     sql.NullTime
		state, errText sql.NullString
		size           sql.NullInt64
	)

	err := db.QueryRowContext(ctx,
		`SELECT last_run_start, last_run_end, last_run_state, last_run_error, last_run_size FROM job_runs WHERE name = ?`,
		name,
	).Scan(&start, &end, &state, &errText, &size)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return lastRun{}, false, nil
	case err != nil:
		return lastRun{}, false, fmt.Errorf("reading job %q last run: %w", name, err)
	case !state.Valid:
		return lastRun{}, false, nil
	}

	return lastRun{
		Start: start.Time,
		End:   end.Time,
		State: runState(state.String),
		Error: errText.String,
		Size:  size.Int64,
	}, true, nil
}

// writeTargetRun records job name's target at index's just-finished
// success/failure, overwriting whatever was recorded for that target's
// previous run. Called for every target of every run, mirroring
// writeLastRun's per-job persistence one level down.
func writeTargetRun(ctx context.Context, db *sql.DB, name string, index int, state runState, errText string, at time.Time) error {
	const upsert = `INSERT INTO target_runs (job_name, target_idx, run_at, state, error) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(job_name, target_idx) DO UPDATE SET
			run_at = excluded.run_at,
			state  = excluded.state,
			error  = excluded.error`

	if _, err := db.ExecContext(ctx, upsert, name, index, at.UTC(), string(state), errText); err != nil {
		return fmt.Errorf("recording job %q target %d run: %w", name, index, err)
	}

	return nil
}

// targetRun is one job target's most recently persisted success/failure, as
// returned by readTargetRuns.
type targetRun struct {
	Index int
	State runState
	Error string
}

// readTargetRuns returns every target run persisted for job name, one entry
// per target index that has completed at least once.
func readTargetRuns(ctx context.Context, db *sql.DB, name string) ([]targetRun, error) {
	rows, err := db.QueryContext(ctx, `SELECT target_idx, state, error FROM target_runs WHERE job_name = ?`, name)
	if err != nil {
		return nil, fmt.Errorf("reading job %q target runs: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	var out []targetRun

	for rows.Next() {
		var (
			index   int
			state   string
			errText sql.NullString
		)

		if err := rows.Scan(&index, &state, &errText); err != nil {
			return nil, fmt.Errorf("reading job %q target runs: %w", name, err)
		}

		out = append(out, targetRun{Index: index, State: runState(state), Error: errText.String})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading job %q target runs: %w", name, err)
	}

	return out, nil
}
