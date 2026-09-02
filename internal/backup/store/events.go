package store

import (
	"context"
	"fmt"
	"time"
)

// loginEventModel is login_events: records every dashboard login attempt,
// password or SSO, win or lose, so an operator can review who's signed in
// (and who's tried and failed) from the web UI.
type loginEventModel struct {
	ID         uint      `gorm:"column:id;primaryKey;autoIncrement"`
	At         time.Time `gorm:"column:at;not null"`
	Username   string    `gorm:"column:username;not null"`
	Method     string    `gorm:"column:method;not null"`
	Success    bool      `gorm:"column:success;not null"`
	RemoteAddr string    `gorm:"column:remote_addr;not null"`
	Detail     string    `gorm:"column:detail;not null;default:''"`
}

func (loginEventModel) TableName() string { return "login_events" }

// downloadEventModel is download_events: records every dashboard file
// download, win or lose, so an operator can see who pulled which file and
// when.
type downloadEventModel struct {
	ID         uint      `gorm:"column:id;primaryKey;autoIncrement"`
	At         time.Time `gorm:"column:at;not null"`
	Username   string    `gorm:"column:username;not null"`
	ReceiverID string    `gorm:"column:receiver_id;not null"`
	Key        string    `gorm:"column:key;not null"`
	Success    bool      `gorm:"column:success;not null"`
	RemoteAddr string    `gorm:"column:remote_addr;not null"`
	Detail     string    `gorm:"column:detail;not null;default:''"`
}

func (downloadEventModel) TableName() string { return "download_events" }

// receiverEventModel is receiver_events: records every receiver API
// request (PUT or DELETE) this instance has served, win or lose, regardless
// of whether that receiver has retention: set (unlike objects, which only
// tracks writes for a receiver with retention: configured) — so a daily
// report can summarize how many files each receiver received, and any
// errors it hit, over a given day.
type receiverEventModel struct {
	ID         uint      `gorm:"column:id;primaryKey;autoIncrement"`
	At         time.Time `gorm:"column:at;not null"`
	ReceiverID string    `gorm:"column:receiver_id;not null"`
	Kind       string    `gorm:"column:kind;not null"`
	Key        string    `gorm:"column:key;not null"`
	Size       int64     `gorm:"column:size;not null;default:0"`
	Success    bool      `gorm:"column:success;not null"`
	Error      string    `gorm:"column:error;not null;default:''"`
}

func (receiverEventModel) TableName() string { return "receiver_events" }

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
	m := loginEventModel{
		At:         ev.At.UTC(),
		Username:   ev.Username,
		Method:     ev.Method,
		Success:    ev.Success,
		RemoteAddr: ev.RemoteAddr,
		Detail:     ev.Detail,
	}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("recording login event: %w", err)
	}

	return nil
}

// ListLoginEvents returns up to limit of the most recently recorded login
// events, newest first, for the dashboard's login log view.
func (s *Store) ListLoginEvents(ctx context.Context, limit int) ([]LoginEvent, error) {
	var rows []loginEventModel

	if err := s.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading login events: %w", err)
	}

	events := make([]LoginEvent, len(rows))
	for i, m := range rows {
		events[i] = LoginEvent{At: m.At, Username: m.Username, Method: m.Method, Success: m.Success, RemoteAddr: m.RemoteAddr, Detail: m.Detail}
	}

	return events, nil
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
	m := downloadEventModel{
		At:         ev.At.UTC(),
		Username:   ev.Username,
		ReceiverID: ev.ReceiverID,
		Key:        ev.Key,
		Success:    ev.Success,
		RemoteAddr: ev.RemoteAddr,
		Detail:     ev.Detail,
	}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("recording download event: %w", err)
	}

	return nil
}

// ListDownloadEvents returns up to limit of the most recently recorded
// download events, newest first, for the dashboard's download log view.
func (s *Store) ListDownloadEvents(ctx context.Context, limit int) ([]DownloadEvent, error) {
	var rows []downloadEventModel

	if err := s.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading download events: %w", err)
	}

	events := make([]DownloadEvent, len(rows))
	for i, m := range rows {
		events[i] = DownloadEvent{At: m.At, Username: m.Username, ReceiverID: m.ReceiverID, Key: m.Key, Success: m.Success, RemoteAddr: m.RemoteAddr, Detail: m.Detail}
	}

	return events, nil
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
	m := receiverEventModel{
		At:         ev.At.UTC(),
		ReceiverID: ev.ReceiverID,
		Kind:       ev.Kind,
		Key:        ev.Key,
		Size:       ev.Size,
		Success:    ev.Success,
		Error:      ev.Error,
	}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
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
	var m receiverEventModel

	err := s.db.WithContext(ctx).Where("receiver_id = ?", id).Order("id DESC").Take(&m).Error

	switch {
	case isRecordNotFound(err):
		return ReceiverEvent{}, false, nil
	case err != nil:
		return ReceiverEvent{}, false, fmt.Errorf("reading receiver %q last event: %w", id, err)
	default:
		return ReceiverEvent{At: m.At, ReceiverID: m.ReceiverID, Kind: m.Kind, Key: m.Key, Size: m.Size, Success: m.Success, Error: m.Error}, true, nil
	}
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
// id's ReceiverDaySummary for the events recorded in [start, end). Stays
// raw SQL: the conditional-aggregation columns (SUM(CASE WHEN ...)) have no
// GORM chain equivalent.
func (s *Store) SummarizeReceiverEvents(ctx context.Context, start, end time.Time) ([]ReceiverDaySummary, error) {
	const query = `SELECT receiver_id AS receiver_id,
		SUM(CASE WHEN kind = ? AND success = 1 THEN 1 ELSE 0 END) AS files_received,
		SUM(CASE WHEN kind = ? AND success = 1 THEN size ELSE 0 END) AS bytes_received,
		SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END) AS errors
		FROM receiver_events
		WHERE at >= ? AND at < ?
		GROUP BY receiver_id
		ORDER BY receiver_id`

	var out []ReceiverDaySummary

	err := s.db.WithContext(ctx).Raw(query, ReceiverEventReceive, ReceiverEventReceive, start.UTC(), end.UTC()).Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("summarizing receiver events: %w", err)
	}

	return out, nil
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
	var rows []receiverEventModel

	err := s.db.WithContext(ctx).
		Where("success = 0 AND at >= ? AND at < ?", start.UTC(), end.UTC()).
		Order("at ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading receiver error events: %w", err)
	}

	events := make([]ReceiverErrorEvent, len(rows))
	for i, m := range rows {
		events[i] = ReceiverErrorEvent{At: m.At, ReceiverID: m.ReceiverID, Kind: m.Kind, Key: m.Key, Error: m.Error}
	}

	return events, nil
}
