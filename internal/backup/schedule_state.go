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
// already ran on time. It also holds the objects table (see retention.go)
// tracking every file written to a local target or receiver with retention:
// set, so a later sweep knows what's eligible for automatic deletion — one
// shared database rather than a separate retention db per local root,
// disambiguated by the per-row server/path columns below.
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

	// objects records every file go-backup-tool has written to a local
	// target or receiver with retention: set, so a later sweep (see
	// retention.go) knows what's eligible for automatic deletion.
	// retention_seconds records the retention duration in effect when each
	// object was written, so a later config change to a server's retention:
	// doesn't retroactively change how long already-written objects are
	// kept (see sweepRetention). It defaults to 0 ("unknown") so rows from
	// before this column existed keep sweeping under the target's current
	// retention, exactly as they did before it was added.
	const objectsSchema = `CREATE TABLE IF NOT EXISTS objects (
		server            TEXT NOT NULL,
		bucket            TEXT NOT NULL,
		path              TEXT NOT NULL PRIMARY KEY,
		written_at        TIMESTAMP NOT NULL,
		retention_seconds INTEGER NOT NULL DEFAULT 0
	)`

	if _, err := db.ExecContext(ctx, objectsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	if err := ensureRetentionSecondsColumn(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	// login_events records every dashboard login attempt, password or SSO,
	// win or lose, so an operator can review who's signed in (and who's
	// tried and failed) from the web UI (see recordLoginEvent/
	// readLoginEvents and the dashboard's "Login log" section).
	const loginEventsSchema = `CREATE TABLE IF NOT EXISTS login_events (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		at          TIMESTAMP NOT NULL,
		username    TEXT NOT NULL,
		method      TEXT NOT NULL,
		success     INTEGER NOT NULL,
		remote_addr TEXT NOT NULL,
		detail      TEXT NOT NULL DEFAULT ''
	)`

	if _, err := db.ExecContext(ctx, loginEventsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	// download_events records every dashboard file download, win or lose, so
	// an operator can see who pulled which file and when (see
	// recordDownloadEvent/readDownloadEvents and the dashboard's "Download
	// log" section).
	const downloadEventsSchema = `CREATE TABLE IF NOT EXISTS download_events (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		at          TIMESTAMP NOT NULL,
		username    TEXT NOT NULL,
		receiver_id TEXT NOT NULL,
		key         TEXT NOT NULL,
		success     INTEGER NOT NULL,
		remote_addr TEXT NOT NULL,
		detail      TEXT NOT NULL DEFAULT ''
	)`

	if _, err := db.ExecContext(ctx, downloadEventsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	return db, nil
}

// ensureRetentionSecondsColumn adds the retention_seconds column to objects
// if it's missing, for a state db created before that column existed.
// CREATE TABLE IF NOT EXISTS above is a no-op against such a db, so the
// column has to be added explicitly.
func ensureRetentionSecondsColumn(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(objects)`)
	if err != nil {
		return fmt.Errorf("reading objects table schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var found bool

	for rows.Next() {
		var (
			cid           int
			name, colType string
			notNull, pk   int
			defaultValue  sql.NullString
		)

		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("reading objects table schema: %w", err)
		}

		if name == "retention_seconds" {
			found = true
			break
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading objects table schema: %w", err)
	}

	if found {
		return nil
	}

	if _, err := db.ExecContext(ctx, `ALTER TABLE objects ADD COLUMN retention_seconds INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("adding retention_seconds column: %w", err)
	}

	return nil
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

// loginEvent is one recorded attempt to log into the dashboard, password or
// SSO, win or lose (see recordLoginEvent/readLoginEvents).
type loginEvent struct {
	At         time.Time
	Username   string // best-effort identity: the submitted username, or the SSO claim used (email, falling back to subject); may be empty for a failed SSO attempt that never got that far
	Method     string // "password" or "oidc"
	Success    bool
	RemoteAddr string
	Detail     string // failure reason; empty on success
}

// recordLoginEvent appends ev to the login log. Called for every login
// attempt the dashboard's handlers see, regardless of outcome.
func recordLoginEvent(ctx context.Context, db *sql.DB, ev loginEvent) error {
	const insert = `INSERT INTO login_events (at, username, method, success, remote_addr, detail) VALUES (?, ?, ?, ?, ?, ?)`

	if _, err := db.ExecContext(ctx, insert, ev.At.UTC(), ev.Username, ev.Method, ev.Success, ev.RemoteAddr, ev.Detail); err != nil {
		return fmt.Errorf("recording login event: %w", err)
	}

	return nil
}

// readLoginEvents returns up to limit of the most recently recorded login
// events, newest first, for the dashboard's login log view.
func readLoginEvents(ctx context.Context, db *sql.DB, limit int) ([]loginEvent, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT at, username, method, success, remote_addr, detail FROM login_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading login events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []loginEvent

	for rows.Next() {
		var ev loginEvent

		if err := rows.Scan(&ev.At, &ev.Username, &ev.Method, &ev.Success, &ev.RemoteAddr, &ev.Detail); err != nil {
			return nil, fmt.Errorf("reading login events: %w", err)
		}

		out = append(out, ev)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading login events: %w", err)
	}

	return out, nil
}

// downloadEvent is one recorded attempt to download a file from the
// dashboard, win or lose (see recordDownloadEvent/readDownloadEvents).
type downloadEvent struct {
	At         time.Time
	Username   string // best-effort identity of the logged-in dashboard session; empty when the web UI has no login configured
	ReceiverID string
	Key        string
	Success    bool
	RemoteAddr string
	Detail     string // failure reason; empty on success
}

// recordDownloadEvent appends ev to the download log. Called for every
// download attempt handleDownloadFile sees, regardless of outcome.
func recordDownloadEvent(ctx context.Context, db *sql.DB, ev downloadEvent) error {
	const insert = `INSERT INTO download_events (at, username, receiver_id, key, success, remote_addr, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`

	if _, err := db.ExecContext(ctx, insert, ev.At.UTC(), ev.Username, ev.ReceiverID, ev.Key, ev.Success, ev.RemoteAddr, ev.Detail); err != nil {
		return fmt.Errorf("recording download event: %w", err)
	}

	return nil
}

// readDownloadEvents returns up to limit of the most recently recorded
// download events, newest first, for the dashboard's download log view.
func readDownloadEvents(ctx context.Context, db *sql.DB, limit int) ([]downloadEvent, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT at, username, receiver_id, key, success, remote_addr, detail FROM download_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading download events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []downloadEvent

	for rows.Next() {
		var ev downloadEvent

		if err := rows.Scan(&ev.At, &ev.Username, &ev.ReceiverID, &ev.Key, &ev.Success, &ev.RemoteAddr, &ev.Detail); err != nil {
			return nil, fmt.Errorf("reading download events: %w", err)
		}

		out = append(out, ev)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading download events: %w", err)
	}

	return out, nil
}
