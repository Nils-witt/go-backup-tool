package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// loginEventsSchema is login_events: records every dashboard login attempt,
// password or SSO, win or lose, so an operator can review who's signed in
// (and who's tried and failed) from the web UI.
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
// when.
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
// of whether that receiver has retention: set (unlike objects, which only
// tracks writes for a receiver with retention: configured) — so a daily
// report can summarize how many files each receiver received, and any
// errors it hit, over a given day.
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

// LoginEvent is one recorded attempt to log into the dashboard, password or
// SSO, win or lose (see SaveLoginEvent/ListLoginEvents).
type LoginEvent struct {
	At         time.Time
	Username   string // best-effort identity: the submitted username, or the SSO claim used (email, falling back to subject); may be empty for a failed SSO attempt that never got that far
	Method     string // "password" or "oidc"
	Success    bool
	RemoteAddr string
	Detail     string // failure reason; empty on success
}

// SaveLoginEvent appends ev to the login log. Called for every login
// attempt a caller's handlers see, regardless of outcome.
func (s *Store) SaveLoginEvent(ctx context.Context, ev LoginEvent) error {
	const insert = `INSERT INTO login_events (at, username, method, success, remote_addr, detail) VALUES (?, ?, ?, ?, ?, ?)`

	if _, err := s.db.ExecContext(ctx, insert, ev.At.UTC(), ev.Username, ev.Method, ev.Success, ev.RemoteAddr, ev.Detail); err != nil {
		return fmt.Errorf("recording login event: %w", err)
	}

	return nil
}

// ListLoginEvents returns up to limit of the most recently recorded login
// events, newest first, for the dashboard's login log view.
func (s *Store) ListLoginEvents(ctx context.Context, limit int) ([]LoginEvent, error) {
	query := `SELECT at, username, method, success, remote_addr, detail FROM login_events ORDER BY id DESC LIMIT ?`

	return queryRows(ctx, s.db, "reading login events", query, []any{limit}, func(rows *sql.Rows) (LoginEvent, error) {
		var ev LoginEvent

		if err := rows.Scan(&ev.At, &ev.Username, &ev.Method, &ev.Success, &ev.RemoteAddr, &ev.Detail); err != nil {
			return LoginEvent{}, err
		}

		return ev, nil
	})
}

// DownloadEvent is one recorded attempt to download a file from the
// dashboard, win or lose (see SaveDownloadEvent/ListDownloadEvents).
type DownloadEvent struct {
	At         time.Time
	Username   string // best-effort identity of the logged-in dashboard session; empty when the web UI has no login configured
	ReceiverID string
	Key        string
	Success    bool
	RemoteAddr string
	Detail     string // failure reason; empty on success
}

// SaveDownloadEvent appends ev to the download log. Called for every
// download attempt a caller's handlers see, regardless of outcome.
func (s *Store) SaveDownloadEvent(ctx context.Context, ev DownloadEvent) error {
	const insert = `INSERT INTO download_events (at, username, receiver_id, key, success, remote_addr, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`

	if _, err := s.db.ExecContext(ctx, insert, ev.At.UTC(), ev.Username, ev.ReceiverID, ev.Key, ev.Success, ev.RemoteAddr, ev.Detail); err != nil {
		return fmt.Errorf("recording download event: %w", err)
	}

	return nil
}

// ListDownloadEvents returns up to limit of the most recently recorded
// download events, newest first, for the dashboard's download log view.
func (s *Store) ListDownloadEvents(ctx context.Context, limit int) ([]DownloadEvent, error) {
	query := `SELECT at, username, receiver_id, key, success, remote_addr, detail FROM download_events ORDER BY id DESC LIMIT ?`

	return queryRows(ctx, s.db, "reading download events", query, []any{limit}, func(rows *sql.Rows) (DownloadEvent, error) {
		var ev DownloadEvent

		if err := rows.Scan(&ev.At, &ev.Username, &ev.ReceiverID, &ev.Key, &ev.Success, &ev.RemoteAddr, &ev.Detail); err != nil {
			return DownloadEvent{}, err
		}

		return ev, nil
	})
}

// ReceiverEventReceive and ReceiverEventDelete are the kind values
// SaveReceiverEvent records, mirroring the two receiver API operations
// (receive/delete).
const (
	ReceiverEventReceive = "receive"
	ReceiverEventDelete  = "delete"
)

// ReceiverEvent is one recorded receiver API request, win or lose, as
// persisted by SaveReceiverEvent and summarized by SummarizeReceiverEvents
// for a daily report.
type ReceiverEvent struct {
	At         time.Time
	ReceiverID string
	Kind       string // ReceiverEventReceive or ReceiverEventDelete
	Key        string
	Size       int64 // bytes written; 0 for a delete or a failed receive
	Success    bool
	Error      string // failure reason; empty on success
}

// SaveReceiverEvent appends ev to the receiver event log. Called for every
// receiver API request a caller's handlers serve, regardless of outcome.
func (s *Store) SaveReceiverEvent(ctx context.Context, ev ReceiverEvent) error {
	const insert = `INSERT INTO receiver_events (at, receiver_id, kind, key, size, success, error) VALUES (?, ?, ?, ?, ?, ?, ?)`

	if _, err := s.db.ExecContext(ctx, insert, ev.At.UTC(), ev.ReceiverID, ev.Kind, ev.Key, ev.Size, ev.Success, ev.Error); err != nil {
		return fmt.Errorf("recording receiver event: %w", err)
	}

	return nil
}

// GetLastReceiverEvent returns receiver id's most recently recorded
// receiver_events row (receive or delete, win or lose), and false if none
// is recorded yet — used to seed a restarted process's in-memory receiver
// status with what its last request actually did, mirroring GetLastRun's
// reasoning for job status.
func (s *Store) GetLastReceiverEvent(ctx context.Context, id string) (ReceiverEvent, bool, error) {
	errMsg := fmt.Sprintf("reading receiver %q last event", id)
	query := `SELECT at, receiver_id, kind, key, size, success, error FROM receiver_events WHERE receiver_id = ? ORDER BY id DESC LIMIT 1`

	return queryRowOptional(ctx, s.db, errMsg, query, []any{id}, func(row *sql.Row) (ReceiverEvent, error) {
		var ev ReceiverEvent
		if err := row.Scan(&ev.At, &ev.ReceiverID, &ev.Kind, &ev.Key, &ev.Size, &ev.Success, &ev.Error); err != nil {
			return ReceiverEvent{}, err
		}

		return ev, nil
	})
}

// ReceiverDaySummary is one receiver's activity over a time window, as
// returned by SummarizeReceiverEvents for a daily report: how many files it
// successfully received, their total size, and how many requests (of
// either kind) failed.
type ReceiverDaySummary struct {
	ReceiverID    string
	FilesReceived int
	BytesReceived int64
	Errors        int
}

// SummarizeReceiverEvents returns, in receiver id order, every receiver
// id's ReceiverDaySummary for the events recorded in [start, end).
func (s *Store) SummarizeReceiverEvents(ctx context.Context, start, end time.Time) ([]ReceiverDaySummary, error) {
	const query = `SELECT receiver_id,
		SUM(CASE WHEN kind = ? AND success = 1 THEN 1 ELSE 0 END),
		SUM(CASE WHEN kind = ? AND success = 1 THEN size ELSE 0 END),
		SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END)
		FROM receiver_events
		WHERE at >= ? AND at < ?
		GROUP BY receiver_id
		ORDER BY receiver_id`

	args := []any{ReceiverEventReceive, ReceiverEventReceive, start.UTC(), end.UTC()}

	return queryRows(ctx, s.db, "summarizing receiver events", query, args, func(rows *sql.Rows) (ReceiverDaySummary, error) {
		var sum ReceiverDaySummary

		if err := rows.Scan(&sum.ReceiverID, &sum.FilesReceived, &sum.BytesReceived, &sum.Errors); err != nil {
			return ReceiverDaySummary{}, err
		}

		return sum, nil
	})
}

// ReceiverErrorEvent is one failed receiver API request in a time window, as
// returned by ListReceiverErrorEvents for a daily report's error listing.
type ReceiverErrorEvent struct {
	At         time.Time
	ReceiverID string
	Kind       string
	Key        string
	Error      string
}

// ListReceiverErrorEvents returns every failed receiver API request
// recorded in [start, end), oldest first.
func (s *Store) ListReceiverErrorEvents(ctx context.Context, start, end time.Time) ([]ReceiverErrorEvent, error) {
	query := `SELECT at, receiver_id, kind, key, error FROM receiver_events WHERE success = 0 AND at >= ? AND at < ? ORDER BY at ASC`

	return queryRows(ctx, s.db, "reading receiver error events", query, []any{start.UTC(), end.UTC()}, func(rows *sql.Rows) (ReceiverErrorEvent, error) {
		var e ReceiverErrorEvent

		if err := rows.Scan(&e.At, &e.ReceiverID, &e.Kind, &e.Key, &e.Error); err != nil {
			return ReceiverErrorEvent{}, err
		}

		return e, nil
	})
}
