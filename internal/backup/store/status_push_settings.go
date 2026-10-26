package store

import (
	"context"
	"fmt"
	"time"
)

// statusPushSettingsModel is status_push_settings: the single row (id 1)
// holding where this instance pushes its status to (see
// internal/backup/settings' Manager and internal/backup/peerstatus),
// managed in the web UI. Server names a type: remote server; it's checked
// on save, and again at every push, since the server may change since.
type statusPushSettingsModel struct {
	ID              uint      `gorm:"column:id;primaryKey"`
	Enabled         bool      `gorm:"column:enabled;not null"`
	Server          string    `gorm:"column:server;not null;default:''"`
	IntervalSeconds int       `gorm:"column:interval_seconds;not null;default:0"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null"`
	UpdatedBy       string    `gorm:"column:updated_by;not null"`
}

func (statusPushSettingsModel) TableName() string { return "status_push_settings" }

// statusPushSettingsID is status_push_settings' only row.
const statusPushSettingsID = 1

// StatusPushSettings is the stored status push settings (see
// statusPushSettingsModel).
type StatusPushSettings struct {
	Enabled         bool
	Server          string
	IntervalSeconds int
	UpdatedAt       time.Time
	UpdatedBy       string
}

// GetStatusPushSettings returns the stored status push settings, reporting
// false if none have been saved yet (the push is then disabled).
func (s *gormStore) GetStatusPushSettings(ctx context.Context) (StatusPushSettings, bool, error) {
	m, ok, err := takeSettingsRow[statusPushSettingsModel](ctx, s, statusPushSettingsID, "status push settings")
	if !ok || err != nil {
		return StatusPushSettings{}, ok, err
	}

	return StatusPushSettings{
		Enabled: m.Enabled, Server: m.Server, IntervalSeconds: m.IntervalSeconds,
		UpdatedAt: m.UpdatedAt, UpdatedBy: m.UpdatedBy,
	}, true, nil
}

// SaveStatusPushSettings stores p as the status push settings, replacing
// any saved before.
func (s *gormStore) SaveStatusPushSettings(ctx context.Context, p StatusPushSettings) error {
	m := statusPushSettingsModel{
		ID:      statusPushSettingsID,
		Enabled: p.Enabled, Server: p.Server, IntervalSeconds: p.IntervalSeconds,
		UpdatedAt: p.UpdatedAt.UTC(), UpdatedBy: p.UpdatedBy,
	}

	if err := s.db.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("saving status push settings: %w", err)
	}

	return nil
}
