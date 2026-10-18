package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm/clause"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// notificationModel is notifications: every named webhook/email
// destination jobs, receivers, and the report reference by id (see
// internal/backup/settings' Manager), managed in the web UI. The channels
// are kept exactly as entered, as JSON, and validated
// (notify.ResolveNotification) on every load rather than once on save, so
// e.g. an email notification turns inactive rather than broken when smtp:
// is later removed from the config file. A nil channel is stored as NULL.
type notificationModel struct {
	ID        string      `gorm:"column:id;primaryKey"`
	Webhook   *webhookRow `gorm:"column:webhook;serializer:json"`
	Email     *emailRow   `gorm:"column:email;serializer:json"`
	CreatedAt time.Time   `gorm:"column:created_at;not null"`
	CreatedBy string      `gorm:"column:created_by;not null"`
	UpdatedAt time.Time   `gorm:"column:updated_at;not null"`
	UpdatedBy string      `gorm:"column:updated_by;not null"`
}

func (notificationModel) TableName() string { return "notifications" }

// webhookRow, emailRow and emailEncryptRow are the JSON stored in the
// notifications table's webhook and email columns. They mirror
// notify.FileWebhook, notify.FileEmail and notify.FileEmailEncrypt field for
// field, with the same JSON names, but belong to this package, so a change
// to notify's types can't silently change the on-disk format of rows that
// are already stored (see storedNotificationFixtures and
// TestNotificationRowsMirrorNotifyTypes).
type webhookRow struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"`
}

type emailRow struct {
	To      []string         `json:"to"`
	From    string           `json:"from"`
	Subject string           `json:"subject"`
	Body    string           `json:"body"`
	Encrypt *emailEncryptRow `json:"encrypt,omitempty"`
}

type emailEncryptRow struct {
	Recipients []string `json:"recipients"`
}

// newNotificationModel maps n onto its stored row.
func newNotificationModel(n NotificationConfig) notificationModel {
	return notificationModel{
		ID: n.ID, Webhook: newWebhookRow(n.Webhook), Email: newEmailRow(n.Email),
		CreatedAt: n.CreatedAt, CreatedBy: n.CreatedBy, UpdatedAt: n.UpdatedAt, UpdatedBy: n.UpdatedBy,
	}
}

// config maps a stored row back onto the NotificationConfig callers see.
func (m notificationModel) config() NotificationConfig {
	return NotificationConfig{
		ID: m.ID, Webhook: m.Webhook.webhook(), Email: m.Email.email(),
		CreatedAt: m.CreatedAt, CreatedBy: m.CreatedBy, UpdatedAt: m.UpdatedAt, UpdatedBy: m.UpdatedBy,
	}
}

func newWebhookRow(w *notify.FileWebhook) *webhookRow {
	if w == nil {
		return nil
	}

	return &webhookRow{URL: w.URL, Method: w.Method, Headers: w.Headers, Body: w.Body}
}

func (r *webhookRow) webhook() *notify.FileWebhook {
	if r == nil {
		return nil
	}

	return &notify.FileWebhook{URL: r.URL, Method: r.Method, Headers: r.Headers, Body: r.Body}
}

func newEmailRow(e *notify.FileEmail) *emailRow {
	if e == nil {
		return nil
	}

	row := &emailRow{To: e.To, From: e.From, Subject: e.Subject, Body: e.Body}
	if e.Encrypt != nil {
		row.Encrypt = &emailEncryptRow{Recipients: e.Encrypt.Recipients}
	}

	return row
}

func (r *emailRow) email() *notify.FileEmail {
	if r == nil {
		return nil
	}

	e := &notify.FileEmail{To: r.To, From: r.From, Subject: r.Subject, Body: r.Body}
	if r.Encrypt != nil {
		e.Encrypt = &notify.FileEmailEncrypt{Recipients: r.Encrypt.Recipients}
	}

	return e
}

// NotificationConfig is one stored notification definition (see
// notificationModel).
type NotificationConfig struct {
	ID        string
	Webhook   *notify.FileWebhook
	Email     *notify.FileEmail
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
	UpdatedBy string
}

var (
	// ErrNotificationExists is returned by CreateNotificationConfig for an
	// id that's already taken.
	ErrNotificationExists = errors.New("a notification with this id already exists")
	// ErrNotificationNotFound is returned by UpdateNotificationConfig/
	// DeleteNotificationConfig for an id with no stored notification.
	ErrNotificationNotFound = errors.New("notification not found")
)

// ListNotificationConfigs returns every stored notification, in id order.
func (s *gormStore) ListNotificationConfigs(ctx context.Context) ([]NotificationConfig, error) {
	var rows []notificationModel

	if err := s.read.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading notifications: %w", err)
	}

	out := make([]NotificationConfig, len(rows))
	for i, m := range rows {
		out[i] = m.config()
	}

	return out, nil
}

// GetNotificationConfig looks up the stored notification with the given id,
// reporting false if there is none.
func (s *gormStore) GetNotificationConfig(ctx context.Context, id string) (NotificationConfig, bool, error) {
	var m notificationModel

	if err := s.read.WithContext(ctx).Take(&m, "id = ?", id).Error; err != nil {
		if isRecordNotFound(err) {
			return NotificationConfig{}, false, nil
		}

		return NotificationConfig{}, false, fmt.Errorf("reading notification: %w", err)
	}

	return m.config(), true, nil
}

// CreateNotificationConfig stores a new notification, returning
// ErrNotificationExists if its id is already taken.
func (s *gormStore) CreateNotificationConfig(ctx context.Context, n NotificationConfig) error {
	m := newNotificationModel(normalizeNotificationTimes(n))

	res := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&m)
	if res.Error != nil {
		return fmt.Errorf("storing notification: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrNotificationExists
	}

	return nil
}

// UpdateNotificationConfig replaces the stored notification n.ID's channels,
// keeping its CreatedAt/CreatedBy, and returns ErrNotificationNotFound if
// there is none.
func (s *gormStore) UpdateNotificationConfig(ctx context.Context, n NotificationConfig) error {
	n = normalizeNotificationTimes(n)

	res := s.db.WithContext(ctx).Model(&notificationModel{}).Where("id = ?", n.ID).
		Select("webhook", "email", "updated_at", "updated_by").
		Updates(&notificationModel{Webhook: newWebhookRow(n.Webhook), Email: newEmailRow(n.Email), UpdatedAt: n.UpdatedAt, UpdatedBy: n.UpdatedBy})
	if res.Error != nil {
		return fmt.Errorf("updating notification: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrNotificationNotFound
	}

	return nil
}

// DeleteNotificationConfig removes the stored notification with the given
// id, returning ErrNotificationNotFound if there is none.
func (s *gormStore) DeleteNotificationConfig(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Delete(&notificationModel{}, "id = ?", id)
	if res.Error != nil {
		return fmt.Errorf("deleting notification: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrNotificationNotFound
	}

	return nil
}

func normalizeNotificationTimes(n NotificationConfig) NotificationConfig {
	n.CreatedAt = n.CreatedAt.UTC()
	n.UpdatedAt = n.UpdatedAt.UTC()

	return n
}
