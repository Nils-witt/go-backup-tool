package backup

import (
	"context"
	"database/sql"
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

// ScheduleStateDBPath returns the state db path for the config file at
// configPath: a sibling file in the same directory.
func ScheduleStateDBPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), scheduleStateDBName)
}

// OpenScheduleStateDB opens (creating if needed) the state-tracking sqlite
// database at path, ensuring its schema exists. The caller must Close it.
//
// SetMaxOpenConns(1) serializes every access through a single connection:
// sqlite handles one writer at a time regardless, and this *sql.DB is shared
// across every job's goroutine for the life of the run.
func OpenScheduleStateDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening job state db %q: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	if err := execSchemaOrClose(ctx, db, path, jobRunsSchema); err != nil {
		return nil, err
	}

	if err := execSchemaOrClose(ctx, db, path, targetRunsSchema); err != nil {
		return nil, err
	}

	if err := execSchemaOrClose(ctx, db, path, objectsSchema); err != nil {
		return nil, err
	}

	if err := ensureRetentionSecondsColumn(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	if err := execSchemaOrClose(ctx, db, path, outstandingUploadsSchema); err != nil {
		return nil, err
	}

	if err := execSchemaOrClose(ctx, db, path, loginEventsSchema); err != nil {
		return nil, err
	}

	if err := execSchemaOrClose(ctx, db, path, downloadEventsSchema); err != nil {
		return nil, err
	}

	if err := execSchemaOrClose(ctx, db, path, receiverEventsSchema); err != nil {
		return nil, err
	}

	return db, nil
}

// execSchemaOrClose runs schema against db, closing db and wrapping the
// error with path if it fails, so OpenScheduleStateDB doesn't repeat that
// close-and-wrap boilerplate for each of its CREATE TABLE statements.
func execSchemaOrClose(ctx context.Context, db *sql.DB, path, schema string) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	return nil
}

// jobRunsSchema is job_runs: last_success is used only by start-time-
// anchored jobs' catch-up logic (see runner.lastJobSuccess) and is nullable
// since a job may have persisted last-run info (below) without ever having
// succeeded yet. The last_run_* columns record the most recent run
// regardless of outcome, for every job, purely so the web UI can still show
// it across a restart (see runner.recordLastRun / statusStore.seedLastRun)
// — a distinct concept from last_success, which must keep pointing at the
// last *successful* run even after a later run fails.
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
// completed success/failure, one level below job_runs, so a restart's web
// UI can also show a target's last outcome instead of every target
// reverting to "idle" until it next runs (see runner.persistTargetRun /
// statusStore.seedTargetRun). target_idx is index-aligned with the job's
// targets: as configured, matching statusStore.targetDone's existing
// index-alignment assumption.
const targetRunsSchema = `CREATE TABLE IF NOT EXISTS target_runs (
	job_name   TEXT NOT NULL,
	target_idx INTEGER NOT NULL,
	run_at     TIMESTAMP NOT NULL,
	state      TEXT NOT NULL,
	error      TEXT,
	PRIMARY KEY (job_name, target_idx)
)`

// objectsSchema is objects: records every file go-backup-tool has written to
// a local target or receiver with retention: set, so a later sweep (see
// retention.go) knows what's eligible for automatic deletion.
// retention_seconds records the retention duration in effect when each
// object was written, so a later config change to a server's retention:
// doesn't retroactively change how long already-written objects are kept
// (see sweepRetention). It defaults to 0 ("unknown") so rows from before
// this column existed keep sweeping under the target's current retention,
// exactly as they did before it was added.
const objectsSchema = `CREATE TABLE IF NOT EXISTS objects (
	server            TEXT NOT NULL,
	bucket            TEXT NOT NULL,
	path              TEXT NOT NULL PRIMARY KEY,
	written_at        TIMESTAMP NOT NULL,
	retention_seconds INTEGER NOT NULL DEFAULT 0
)`

// outstandingUploadsSchema is outstanding_uploads: records a target upload
// that failed and still has attempts remaining (see config.retries), so
// monitorOutstandingUploads (uploadretry.go) can retry it roughly once a
// minute instead of the old in-run, sleep-based retry loop. id is the
// primary key (rather than job_name+target_idx, as target_runs uses)
// because a repeating job can queue a new failure for the same target
// against a different staging path while an earlier one is still
// outstanding. key is the run's already-resolved object key (post {time}
// substitution) and must never be recomputed on retry.
const outstandingUploadsSchema = `CREATE TABLE IF NOT EXISTS outstanding_uploads (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	job_name        TEXT NOT NULL,
	target_idx      INTEGER NOT NULL,
	staging_path    TEXT NOT NULL,
	key             TEXT NOT NULL,
	queued_at       TIMESTAMP NOT NULL,
	attempts        INTEGER NOT NULL,
	last_attempt_at TIMESTAMP,
	last_error      TEXT,
	UNIQUE (job_name, target_idx, staging_path)
)`

// loginEventsSchema is login_events: records every dashboard login attempt,
// password or SSO, win or lose, so an operator can review who's signed in
// (and who's tried and failed) from the web UI (see recordLoginEvent/
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

// downloadEventsSchema is download_events: records every dashboard file
// download, win or lose, so an operator can see who pulled which file and
// when (see recordDownloadEvent/readDownloadEvents and the dashboard's
// "Download log" section).
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

// receiverEventsSchema is receiver_events: records every receiver API
// request (PUT or DELETE) this instance has served, win or lose, regardless
// of whether that receiver has retention: set (unlike objects above, which
// only tracks writes for a receiver with retention: configured) — so the
// daily report (see report.go) can summarize how many files each receiver
// received, and any errors it hit, over a given day.
const receiverEventsSchema = `CREATE TABLE IF NOT EXISTS receiver_events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	at          TIMESTAMP NOT NULL,
	receiver_id TEXT NOT NULL,
	kind        TEXT NOT NULL,
	key         TEXT NOT NULL,
	size        INTEGER NOT NULL DEFAULT 0,
	success     INTEGER NOT NULL,
	error       TEXT NOT NULL DEFAULT ''
)`

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

// WriteLastSuccess records that job name last completed successfully at at.
func WriteLastSuccess(ctx context.Context, db *sql.DB, name string, at time.Time) error {
	const upsert = `INSERT INTO job_runs (name, last_success) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET last_success = excluded.last_success`

	if _, err := db.ExecContext(ctx, upsert, name, at.UTC()); err != nil {
		return fmt.Errorf("recording job %q success: %w", name, err)
	}

	return nil
}

// ReadLastSuccess returns job name's last recorded successful run, and false
// if none is recorded yet.
func ReadLastSuccess(ctx context.Context, db *sql.DB, name string) (time.Time, bool, error) {
	errMsg := fmt.Sprintf("reading job %q state", name)

	return queryRowOptional(ctx, db, errMsg, `SELECT last_success FROM job_runs WHERE name = ?`, []any{name}, func(row *sql.Row) (time.Time, error) {
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

// LastRun is a job's most recently completed run (success or failure),
// persisted so a restart's web UI can still show when a job last ran
// instead of reverting to "never" until it next runs (see
// runner.recordLastRun / statusStore.seedLastRun).
type LastRun struct {
	Start time.Time
	End   time.Time
	State RunState
	Error string
	Size  int64
}

// WriteLastRun records job name's most recently completed run, overwriting
// whatever was recorded for its previous run. Called for every job after
// every run, regardless of outcome or whether the job uses start-time.
func WriteLastRun(ctx context.Context, db *sql.DB, name string, run LastRun) error {
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

// ReadLastRun returns job name's most recently persisted run, and false if
// none is recorded yet (including when a row exists for name only because
// writeLastSuccess wrote it, without any last_run_* data alongside it).
func ReadLastRun(ctx context.Context, db *sql.DB, name string) (LastRun, bool, error) {
	errMsg := fmt.Sprintf("reading job %q last run", name)
	query := `SELECT last_run_start, last_run_end, last_run_state, last_run_error, last_run_size FROM job_runs WHERE name = ?`

	return queryRowOptional(ctx, db, errMsg, query, []any{name}, func(row *sql.Row) (LastRun, error) {
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
			State: RunState(state.String),
			Error: errText.String,
			Size:  size.Int64,
		}, nil
	})
}

// WriteTargetRun records job name's target at index's just-finished
// success/failure, overwriting whatever was recorded for that target's
// previous run. Called for every target of every run, mirroring
// writeLastRun's per-job persistence one level down.
func WriteTargetRun(ctx context.Context, db *sql.DB, name string, index int, state RunState, errText string, at time.Time) error {
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

// TargetRun is one job target's most recently persisted success/failure, as
// returned by readTargetRuns.
type TargetRun struct {
	Index int
	State RunState
	Error string
}

// ReadTargetRuns returns every target run persisted for job name, one entry
// per target index that has completed at least once.
func ReadTargetRuns(ctx context.Context, db *sql.DB, name string) ([]TargetRun, error) {
	errMsg := fmt.Sprintf("reading job %q target runs", name)

	return queryRows(ctx, db, errMsg, `SELECT target_idx, state, error FROM target_runs WHERE job_name = ?`, []any{name}, func(rows *sql.Rows) (TargetRun, error) {
		var (
			index   int
			state   string
			errText sql.NullString
		)

		if err := rows.Scan(&index, &state, &errText); err != nil {
			return TargetRun{}, err
		}

		return TargetRun{Index: index, State: RunState(state), Error: errText.String}, nil
	})
}

// OutstandingUpload is one target upload that failed and still has retry
// attempts remaining, as persisted by queueOutstandingUpload and retried by
// monitorOutstandingUploads (uploadretry.go).
type OutstandingUpload struct {
	ID          int64
	JobName     string
	TargetIdx   int
	StagingPath string
	Key         string
	QueuedAt    time.Time
	Attempts    int
	LastError   string
}

// QueueOutstandingUpload records that job name's target at index failed its
// first upload attempt for stagingPath (as key), so monitorOutstandingUploads
// retries it going forward instead of the old in-run retry loop. Idempotent
// for the same (job, target, stagingPath) triple: a duplicate call (which
// shouldn't happen in practice) leaves the existing row untouched rather than
// erroring or inserting a second row.
func QueueOutstandingUpload(ctx context.Context, db *sql.DB, jobName string, targetIdx int, stagingPath, key string, at time.Time, uploadErr error) error {
	const insert = `INSERT INTO outstanding_uploads (job_name, target_idx, staging_path, key, queued_at, attempts, last_attempt_at, last_error)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(job_name, target_idx, staging_path) DO NOTHING`

	errText := ""
	if uploadErr != nil {
		errText = uploadErr.Error()
	}

	if _, err := db.ExecContext(ctx, insert, jobName, targetIdx, stagingPath, key, at.UTC(), at.UTC(), errText); err != nil {
		return fmt.Errorf("queuing outstanding upload for job %q target %d: %w", jobName, targetIdx, err)
	}

	return nil
}

// ListOutstandingUploads returns every queued outstanding upload, oldest
// first, so monitorOutstandingUploads processes failures in the order they
// occurred.
func ListOutstandingUploads(ctx context.Context, db *sql.DB) ([]OutstandingUpload, error) {
	query := `SELECT id, job_name, target_idx, staging_path, key, queued_at, attempts, last_error FROM outstanding_uploads ORDER BY queued_at ASC`

	return queryRows(ctx, db, "reading outstanding uploads", query, nil, func(rows *sql.Rows) (OutstandingUpload, error) {
		var (
			u         OutstandingUpload
			lastError sql.NullString
		)

		if err := rows.Scan(&u.ID, &u.JobName, &u.TargetIdx, &u.StagingPath, &u.Key, &u.QueuedAt, &u.Attempts, &lastError); err != nil {
			return OutstandingUpload{}, err
		}

		u.LastError = lastError.String

		return u, nil
	})
}

// RecordOutstandingUploadAttempt increments id's attempts and records the
// error from its latest failed retry, called when a retry has failed again
// but hasn't yet hit the job's max attempts (see uploadretry.go).
func RecordOutstandingUploadAttempt(ctx context.Context, db *sql.DB, id int64, at time.Time, attemptErr error) error {
	const update = `UPDATE outstanding_uploads SET attempts = attempts + 1, last_attempt_at = ?, last_error = ? WHERE id = ?`

	errText := ""
	if attemptErr != nil {
		errText = attemptErr.Error()
	}

	if _, err := db.ExecContext(ctx, update, at.UTC(), errText, id); err != nil {
		return fmt.Errorf("recording outstanding upload %d attempt: %w", id, err)
	}

	return nil
}

// DeleteOutstandingUpload removes outstanding upload id — called once its
// upload finally succeeds, once it's been retried the job's maximum number of
// times, or once its staging file is confirmed permanently gone.
func DeleteOutstandingUpload(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM outstanding_uploads WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting outstanding upload %d: %w", id, err)
	}

	return nil
}

// CountOutstandingUploadsForPath reports how many outstanding uploads (across
// every job and target) still reference stagingPath, so a caller knows
// whether it's finally safe to delete that staged file.
func CountOutstandingUploadsForPath(ctx context.Context, db *sql.DB, stagingPath string) (int, error) {
	var n int

	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outstanding_uploads WHERE staging_path = ?`, stagingPath).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting outstanding uploads for %q: %w", stagingPath, err)
	}

	return n, nil
}

// LoginEvent is one recorded attempt to log into the dashboard, password or
// SSO, win or lose (see recordLoginEvent/readLoginEvents).
type LoginEvent struct {
	At         time.Time
	Username   string // best-effort identity: the submitted username, or the SSO claim used (email, falling back to subject); may be empty for a failed SSO attempt that never got that far
	Method     string // "password" or "oidc"
	Success    bool
	RemoteAddr string
	Detail     string // failure reason; empty on success
}

// RecordLoginEvent appends ev to the login log. Called for every login
// attempt the dashboard's handlers see, regardless of outcome.
func RecordLoginEvent(ctx context.Context, db *sql.DB, ev LoginEvent) error {
	const insert = `INSERT INTO login_events (at, username, method, success, remote_addr, detail) VALUES (?, ?, ?, ?, ?, ?)`

	if _, err := db.ExecContext(ctx, insert, ev.At.UTC(), ev.Username, ev.Method, ev.Success, ev.RemoteAddr, ev.Detail); err != nil {
		return fmt.Errorf("recording login event: %w", err)
	}

	return nil
}

// ReadLoginEvents returns up to limit of the most recently recorded login
// events, newest first, for the dashboard's login log view.
func ReadLoginEvents(ctx context.Context, db *sql.DB, limit int) ([]LoginEvent, error) {
	query := `SELECT at, username, method, success, remote_addr, detail FROM login_events ORDER BY id DESC LIMIT ?`

	return queryRows(ctx, db, "reading login events", query, []any{limit}, func(rows *sql.Rows) (LoginEvent, error) {
		var ev LoginEvent

		if err := rows.Scan(&ev.At, &ev.Username, &ev.Method, &ev.Success, &ev.RemoteAddr, &ev.Detail); err != nil {
			return LoginEvent{}, err
		}

		return ev, nil
	})
}

// DownloadEvent is one recorded attempt to download a file from the
// dashboard, win or lose (see recordDownloadEvent/readDownloadEvents).
type DownloadEvent struct {
	At         time.Time
	Username   string // best-effort identity of the logged-in dashboard session; empty when the web UI has no login configured
	ReceiverID string
	Key        string
	Success    bool
	RemoteAddr string
	Detail     string // failure reason; empty on success
}

// RecordDownloadEvent appends ev to the download log. Called for every
// download attempt handleDownloadFile sees, regardless of outcome.
func RecordDownloadEvent(ctx context.Context, db *sql.DB, ev DownloadEvent) error {
	const insert = `INSERT INTO download_events (at, username, receiver_id, key, success, remote_addr, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`

	if _, err := db.ExecContext(ctx, insert, ev.At.UTC(), ev.Username, ev.ReceiverID, ev.Key, ev.Success, ev.RemoteAddr, ev.Detail); err != nil {
		return fmt.Errorf("recording download event: %w", err)
	}

	return nil
}

// ReadDownloadEvents returns up to limit of the most recently recorded
// download events, newest first, for the dashboard's download log view.
func ReadDownloadEvents(ctx context.Context, db *sql.DB, limit int) ([]DownloadEvent, error) {
	query := `SELECT at, username, receiver_id, key, success, remote_addr, detail FROM download_events ORDER BY id DESC LIMIT ?`

	return queryRows(ctx, db, "reading download events", query, []any{limit}, func(rows *sql.Rows) (DownloadEvent, error) {
		var ev DownloadEvent

		if err := rows.Scan(&ev.At, &ev.Username, &ev.ReceiverID, &ev.Key, &ev.Success, &ev.RemoteAddr, &ev.Detail); err != nil {
			return DownloadEvent{}, err
		}

		return ev, nil
	})
}

// ReceiverEventReceive and ReceiverEventDelete are the kind values
// recordReceiverEvent records, mirroring the two receiver API operations
// (handleReceiveObject/handleDeleteObject in webui.go).
const (
	ReceiverEventReceive = "receive"
	ReceiverEventDelete  = "delete"
)

// ReceiverEvent is one recorded receiver API request, win or lose, as
// persisted by recordReceiverEvent and summarized by report.go for the daily
// report.
type ReceiverEvent struct {
	At         time.Time
	ReceiverID string
	Kind       string // ReceiverEventReceive or ReceiverEventDelete
	Key        string
	Size       int64 // bytes written; 0 for a delete or a failed receive
	Success    bool
	Error      string // failure reason; empty on success
}

// RecordReceiverEvent appends ev to the receiver event log. Called for every
// receiver API request handleReceiveObject/handleDeleteObject serve,
// regardless of outcome.
func RecordReceiverEvent(ctx context.Context, db *sql.DB, ev ReceiverEvent) error {
	const insert = `INSERT INTO receiver_events (at, receiver_id, kind, key, size, success, error) VALUES (?, ?, ?, ?, ?, ?, ?)`

	if _, err := db.ExecContext(ctx, insert, ev.At.UTC(), ev.ReceiverID, ev.Kind, ev.Key, ev.Size, ev.Success, ev.Error); err != nil {
		return fmt.Errorf("recording receiver event: %w", err)
	}

	return nil
}

// ReadLastReceiverEvent returns receiver id's most recently recorded
// receiver_events row (receive or delete, win or lose), and false if none is
// recorded yet — used to seed a restarted process's in-memory
// receiverStatusStore (see seedReceiverStatusFromState in receiver.go) with
// what its last request actually did, mirroring readLastRun's reasoning for
// job status.
func ReadLastReceiverEvent(ctx context.Context, db *sql.DB, id string) (ReceiverEvent, bool, error) {
	errMsg := fmt.Sprintf("reading receiver %q last event", id)
	query := `SELECT at, receiver_id, kind, key, size, success, error FROM receiver_events WHERE receiver_id = ? ORDER BY id DESC LIMIT 1`

	return queryRowOptional(ctx, db, errMsg, query, []any{id}, func(row *sql.Row) (ReceiverEvent, error) {
		var ev ReceiverEvent
		if err := row.Scan(&ev.At, &ev.ReceiverID, &ev.Kind, &ev.Key, &ev.Size, &ev.Success, &ev.Error); err != nil {
			return ReceiverEvent{}, err
		}

		return ev, nil
	})
}

// ReceiverDaySummary is one receiver's activity over a time window, as
// returned by summarizeReceiverEvents for the daily report: how many files
// it successfully received, their total size, and how many requests (of
// either kind) failed.
type ReceiverDaySummary struct {
	ReceiverID    string
	FilesReceived int
	BytesReceived int64
	Errors        int
}

// SummarizeReceiverEvents returns, in receiver id order, every receiver
// id's receiverDaySummary for the events recorded in [start, end).
func SummarizeReceiverEvents(ctx context.Context, db *sql.DB, start, end time.Time) ([]ReceiverDaySummary, error) {
	const query = `SELECT receiver_id,
		SUM(CASE WHEN kind = ? AND success = 1 THEN 1 ELSE 0 END),
		SUM(CASE WHEN kind = ? AND success = 1 THEN size ELSE 0 END),
		SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END)
		FROM receiver_events
		WHERE at >= ? AND at < ?
		GROUP BY receiver_id
		ORDER BY receiver_id`

	args := []any{ReceiverEventReceive, ReceiverEventReceive, start.UTC(), end.UTC()}

	return queryRows(ctx, db, "summarizing receiver events", query, args, func(rows *sql.Rows) (ReceiverDaySummary, error) {
		var s ReceiverDaySummary

		if err := rows.Scan(&s.ReceiverID, &s.FilesReceived, &s.BytesReceived, &s.Errors); err != nil {
			return ReceiverDaySummary{}, err
		}

		return s, nil
	})
}

// ReceiverErrorEvent is one failed receiver API request in a time window, as
// returned by readReceiverErrorEvents for the daily report's error listing.
type ReceiverErrorEvent struct {
	At         time.Time
	ReceiverID string
	Kind       string
	Key        string
	Error      string
}

// ReadReceiverErrorEvents returns every failed receiver API request
// recorded in [start, end), oldest first.
func ReadReceiverErrorEvents(ctx context.Context, db *sql.DB, start, end time.Time) ([]ReceiverErrorEvent, error) {
	query := `SELECT at, receiver_id, kind, key, error FROM receiver_events WHERE success = 0 AND at >= ? AND at < ? ORDER BY at ASC`

	return queryRows(ctx, db, "reading receiver error events", query, []any{start.UTC(), end.UTC()}, func(rows *sql.Rows) (ReceiverErrorEvent, error) {
		var e ReceiverErrorEvent

		if err := rows.Scan(&e.At, &e.ReceiverID, &e.Kind, &e.Key, &e.Error); err != nil {
			return ReceiverErrorEvent{}, err
		}

		return e, nil
	})
}
