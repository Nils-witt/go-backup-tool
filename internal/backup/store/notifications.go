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
// is later removed from the config file.
type notificationModel struct {
	ID        string              `gorm:"column:id;primaryKey"`
	Webhook   *notify.FileWebhook `gorm:"column:webhook;serializer:json"`
	Email     *notify.FileEmail   `gorm:"column:email;serializer:json"`
	CreatedAt time.Time           `gorm:"column:created_at;not null"`
	CreatedBy string              `gorm:"column:created_by;not null"`
	UpdatedAt time.Time           `gorm:"column:updated_at;not null"`
	UpdatedBy string              `gorm:"column:updated_by;not null"`
}

func (notificationModel) TableName() string { return "notifications" }

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
func (s *Store) ListNotificationConfigs(ctx context.Context) ([]NotificationConfig, error) {
	var rows []notificationModel

	if err := s.db.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading notifications: %w", err)
	}

	out := make([]NotificationConfig, len(rows))
	for i, m := range rows {
		out[i] = NotificationConfig(m)
	}

	return out, nil
}

// GetNotificationConfig looks up the stored notification with the given id,
// reporting false if there is none.
func (s *Store) GetNotificationConfig(ctx context.Context, id string) (NotificationConfig, bool, error) {
	var m notificationModel

	if err := s.db.WithContext(ctx).Take(&m, "id = ?", id).Error; err != nil {
		if isRecordNotFound(err) {
			return NotificationConfig{}, false, nil
		}

		return NotificationConfig{}, false, fmt.Errorf("reading notification: %w", err)
	}

	return NotificationConfig(m), true, nil
}

// CreateNotificationConfig stores a new notification, returning
// ErrNotificationExists if its id is already taken.
func (s *Store) CreateNotificationConfig(ctx context.Context, n NotificationConfig) error {
	m := notificationModel(normalizeNotificationTimes(n))

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
func (s *Store) UpdateNotificationConfig(ctx context.Context, n NotificationConfig) error {
	n = normalizeNotificationTimes(n)

	res := s.db.WithContext(ctx).Model(&notificationModel{}).Where("id = ?", n.ID).
		Select("webhook", "email", "updated_at", "updated_by").
		Updates(&notificationModel{Webhook: n.Webhook, Email: n.Email, UpdatedAt: n.UpdatedAt, UpdatedBy: n.UpdatedBy})
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
func (s *Store) DeleteNotificationConfig(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Delete(&notificationModel{}, "id = ?", id)
	if res.Error != nil {
		return fmt.Errorf("deleting notification: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrNotificationNotFound
	}

	return nil
}

// ImportNotificationConfigs stores every entry of ns whose id isn't stored
// yet, leaving existing rows (possibly edited in the web UI since)
// untouched, and returns the ids it inserted. Used once per startup to
// carry the config file's deprecated notifications: over into the state db.
func (s *Store) ImportNotificationConfigs(ctx context.Context, ns []NotificationConfig) ([]string, error) {
	rows := make([]notificationModel, len(ns))
	for i, n := range ns {
		rows[i] = notificationModel(normalizeNotificationTimes(n))
	}

	imported, err := insertMissing(ctx, s.db, rows, func(m notificationModel) string { return m.ID })
	if err != nil {
		return nil, fmt.Errorf("importing notifications: %w", err)
	}

	return imported, nil
}

func normalizeNotificationTimes(n NotificationConfig) NotificationConfig {
	n.CreatedAt = n.CreatedAt.UTC()
	n.UpdatedAt = n.UpdatedAt.UTC()

	return n
}
