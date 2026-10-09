package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// reportSettingsModel is report_settings: the single row (id 1) holding the
// periodic report's settings (see internal/backup/settings' Manager),
// managed in the web UI. Kept as entered and validated
// (report.ResolveSettings) on every load.
type reportSettingsModel struct {
	ID            uint      `gorm:"column:id;primaryKey"`
	Enabled       bool      `gorm:"column:enabled;not null"`
	Schedule      string    `gorm:"column:schedule;not null;default:''"`
	Notifications []string  `gorm:"column:notifications;serializer:json"`
	UpdatedAt     time.Time `gorm:"column:updated_at;not null"`
	UpdatedBy     string    `gorm:"column:updated_by;not null"`
}

func (reportSettingsModel) TableName() string { return "report_settings" }

// reportSettingsID is report_settings' only row.
const reportSettingsID = 1

// ReportSettings is the stored report settings (see reportSettingsModel).
type ReportSettings struct {
	Enabled       bool
	Schedule      string
	Notifications []string
	UpdatedAt     time.Time
	UpdatedBy     string
}

// GetReportSettings returns the stored report settings, reporting false if
// none have been saved yet (the report is then disabled).
func (s *Store) GetReportSettings(ctx context.Context) (ReportSettings, bool, error) {
	var m reportSettingsModel

	if err := s.db.WithContext(ctx).Take(&m, reportSettingsID).Error; err != nil {
		if isRecordNotFound(err) {
			return ReportSettings{}, false, nil
		}

		return ReportSettings{}, false, fmt.Errorf("reading report settings: %w", err)
	}

	return ReportSettings{Enabled: m.Enabled, Schedule: m.Schedule, Notifications: m.Notifications, UpdatedAt: m.UpdatedAt, UpdatedBy: m.UpdatedBy}, true, nil
}

// SaveReportSettings stores r as the report settings, replacing any saved
// before.
func (s *Store) SaveReportSettings(ctx context.Context, r ReportSettings) error {
	m := reportSettingsModelFrom(r)

	if err := s.db.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("saving report settings: %w", err)
	}

	return nil
}

// ImportReportSettings stores r only if no report settings are saved yet,
// reporting whether it did. Used once per startup to carry the config
// file's deprecated report: over into the state db.
func (s *Store) ImportReportSettings(ctx context.Context, r ReportSettings) (bool, error) {
	m := reportSettingsModelFrom(r)

	res := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&m)
	if res.Error != nil {
		return false, fmt.Errorf("importing report settings: %w", res.Error)
	}

	return res.RowsAffected > 0, nil
}

func reportSettingsModelFrom(r ReportSettings) reportSettingsModel {
	return reportSettingsModel{
		ID: reportSettingsID, Enabled: r.Enabled, Schedule: r.Schedule, Notifications: r.Notifications,
		UpdatedAt: r.UpdatedAt.UTC(), UpdatedBy: r.UpdatedBy,
	}
}
