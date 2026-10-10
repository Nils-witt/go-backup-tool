package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// auditEventModel is audit_events: records every change a signed-in web UI
// user made (or tried to make) through the dashboard's API — creating,
// updating, or deleting a stored job, server, command, receiver, trusted
// server, notification, report setting, API token, or GPG key, and retrying
// a job's failed targets — so an operator can review who changed what, and
// when.
type auditEventModel struct {
	ID         uint      `gorm:"column:id;primaryKey;autoIncrement"`
	At         time.Time `gorm:"column:at;not null;index:idx_audit_events_at"`
	Username   string    `gorm:"column:username;not null"`
	Action     string    `gorm:"column:action;not null"`
	Resource   string    `gorm:"column:resource;not null"`
	Target     string    `gorm:"column:target;not null;default:''"`
	Method     string    `gorm:"column:method;not null"`
	Path       string    `gorm:"column:path;not null"`
	Status     int       `gorm:"column:status;not null"`
	Success    bool      `gorm:"column:success;not null"`
	RemoteAddr string    `gorm:"column:remote_addr;not null"`
	Detail     string    `gorm:"column:detail;not null;default:''"`
	Changes    string    `gorm:"column:changes;not null;default:''"` // JSON-encoded []AuditChange; empty for none
}

func (auditEventModel) TableName() string { return "audit_events" }

// AuditEvent is one recorded change request made through the web UI's API,
// win or lose (see SaveAuditEvent/ListAuditEvents). It never carries the
// request body itself, which may hold secrets (notification credentials,
// receiver keys).
type AuditEvent struct {
	At         time.Time
	Username   string // the signed-in principal that made the request
	Action     string // e.g. "create", "update", "delete", "retry"
	Resource   string // the API collection changed, e.g. "receiver-configs"
	Target     string // the changed item's id/name/key, when known; empty otherwise
	Method     string // the HTTP method
	Path       string // the request path
	Status     int    // the HTTP response status
	Success    bool   // Status was 2xx/3xx
	RemoteAddr string
	Detail     string // failure reason (the error response's text); empty on success
	Changes    []AuditChange
}

// AuditChange is one field an AuditEvent's request changed on its target:
// Field is its path within the item (e.g. "targets[0].bucket"), Old/New its
// JSON value before/after — nil when the field didn't exist before (a
// create) or doesn't any more (a delete). A secret field's values are
// replaced by a placeholder before they get here.
type AuditChange struct {
	Field string `json:"field"`
	Old   any    `json:"old,omitempty"`
	New   any    `json:"new,omitempty"`
}

// SaveAuditEvent appends ev to the audit log. Called for every change
// request a caller's handlers see, regardless of outcome.
func (s *Store) SaveAuditEvent(ctx context.Context, ev AuditEvent) error {
	var changes string

	if len(ev.Changes) > 0 {
		b, err := json.Marshal(ev.Changes)
		if err != nil {
			return fmt.Errorf("recording audit event: encoding changes: %w", err)
		}

		changes = string(b)
	}

	m := auditEventModel{
		At:         ev.At.UTC(),
		Username:   ev.Username,
		Action:     ev.Action,
		Resource:   ev.Resource,
		Target:     ev.Target,
		Method:     ev.Method,
		Path:       ev.Path,
		Status:     ev.Status,
		Success:    ev.Success,
		RemoteAddr: ev.RemoteAddr,
		Detail:     ev.Detail,
		Changes:    changes,
	}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("recording audit event: %w", err)
	}

	return nil
}

// ListAuditEvents returns up to limit of the most recently recorded audit
// events, newest first, for the dashboard's audit log view. Changes is
// never nil; a row whose changes don't decode lists none.
func (s *Store) ListAuditEvents(ctx context.Context, limit int) ([]AuditEvent, error) {
	return listRecentEvents(ctx, s.db, limit, func(m auditEventModel) AuditEvent {
		changes := []AuditChange{}
		if m.Changes != "" {
			_ = json.Unmarshal([]byte(m.Changes), &changes)
		}

		return AuditEvent{
			At: m.At, Username: m.Username, Action: m.Action, Resource: m.Resource, Target: m.Target,
			Method: m.Method, Path: m.Path, Status: m.Status, Success: m.Success, RemoteAddr: m.RemoteAddr, Detail: m.Detail,
			Changes: changes,
		}
	}, "reading audit events")
}
