package store

import (
	"context"
	"fmt"
	"time"
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
	HistoryDays   int       `gorm:"column:history_days;not null;default:0"`
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
	HistoryDays   int
	UpdatedAt     time.Time
	UpdatedBy     string
}

// GetReportSettings returns the stored report settings, reporting false if
// none have been saved yet (the report is then disabled).
func (s *gormStore) GetReportSettings(ctx context.Context) (ReportSettings, bool, error) {
	var m reportSettingsModel

	if err := s.read.WithContext(ctx).Take(&m, reportSettingsID).Error; err != nil {
		if isRecordNotFound(err) {
			return ReportSettings{}, false, nil
		}

		return ReportSettings{}, false, fmt.Errorf("reading report settings: %w", err)
	}

	return ReportSettings{Enabled: m.Enabled, Schedule: m.Schedule, Notifications: m.Notifications, HistoryDays: m.HistoryDays, UpdatedAt: m.UpdatedAt, UpdatedBy: m.UpdatedBy}, true, nil
}

// SaveReportSettings stores r as the report settings, replacing any saved
// before.
func (s *gormStore) SaveReportSettings(ctx context.Context, r ReportSettings) error {
	m := reportSettingsModelFrom(r)

	if err := s.db.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("saving report settings: %w", err)
	}

	return nil
}

func reportSettingsModelFrom(r ReportSettings) reportSettingsModel {
	return reportSettingsModel{
		ID: reportSettingsID, Enabled: r.Enabled, Schedule: r.Schedule, Notifications: r.Notifications, HistoryDays: r.HistoryDays,
		UpdatedAt: r.UpdatedAt.UTC(), UpdatedBy: r.UpdatedBy,
	}
}
