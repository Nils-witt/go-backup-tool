package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrReportNotFound is returned by GetReport for an id with no stored report.
var ErrReportNotFound = errors.New("report not found")

// Report triggers, stored in reports.trigger.
const (
	// ReportTriggerScheduled marks a report the report schedule produced.
	ReportTriggerScheduled = "scheduled"
	// ReportTriggerManual marks a report generated on demand in the web UI.
	ReportTriggerManual = "manual"
)

// reportModel is reports: every generated report (see ReportContent), kept
// until the report settings' history retention passes (see
// DeleteReportsBefore), so the web UI can show past reports. The counts are
// copied out of content so listing reports doesn't decode every one.
type reportModel struct {
	ID             uint          `gorm:"column:id;primaryKey;autoIncrement"`
	GeneratedAt    time.Time     `gorm:"column:generated_at;not null;index:idx_reports_generated_at"`
	PeriodStart    time.Time     `gorm:"column:period_start;not null"`
	PeriodEnd      time.Time     `gorm:"column:period_end;not null"`
	Trigger        string        `gorm:"column:trigger;not null"`
	TriggeredBy    string        `gorm:"column:triggered_by;not null;default:''"`
	Notifications  []string      `gorm:"column:notifications;serializer:json"`
	Receivers      int           `gorm:"column:receivers;not null;default:0"`
	ReceiverErrors int           `gorm:"column:receiver_errors;not null;default:0"`
	Stale          int           `gorm:"column:stale;not null;default:0"`
	Jobs           int           `gorm:"column:jobs;not null;default:0"`
	JobErrors      int           `gorm:"column:job_errors;not null;default:0"`
	Content        ReportContent `gorm:"column:content;serializer:json"`
}

func (reportModel) TableName() string { return "reports" }

// ReportContent is one report's computed sections, stored as JSON in
// reports.content. Its JSON shape is what's on disk: changing it needs a
// migration, not just a fixture update (see reports_test.go).
type ReportContent struct {
	Receivers      []ReportReceiverLine  `json:"receivers"`
	Stale          []ReportStaleReceiver `json:"stale"`
	ReceiverErrors []ReportReceiverError `json:"receiver_errors"`
	Jobs           []ReportJobLine       `json:"jobs"`
	JobErrors      []ReportJobError      `json:"job_errors"`
}

// ReportReceiverLine is one receiver's activity over a report's period.
type ReportReceiverLine struct {
	ID            string `json:"id"`
	FilesReceived int    `json:"files_received"`
	BytesReceived int64  `json:"bytes_received"`
	Errors        int    `json:"errors"`
}

// ReportStaleReceiver is one receiver found stale when a report was built.
type ReportStaleReceiver struct {
	ID                string     `json:"id"`
	StaleAfterSeconds int64      `json:"stale_after_seconds"`
	LastSeen          *time.Time `json:"last_seen"`
}

// ReportReceiverError is one failed receiver API request within a report's
// period.
type ReportReceiverError struct {
	At         time.Time `json:"at"`
	ReceiverID string    `json:"receiver_id"`
	Kind       string    `json:"kind"`
	Key        string    `json:"key"`
	Error      string    `json:"error"`
}

// ReportJobLine is one job's activity over a report's period.
type ReportJobLine struct {
	ID            string `json:"id"`
	RunsCompleted int    `json:"runs_completed"`
	BytesWritten  int64  `json:"bytes_written"`
	Errors        int    `json:"errors"`
}

// ReportJobError is one failed job run within a report's period.
type ReportJobError struct {
	At      time.Time `json:"at"`
	JobName string    `json:"job_name"`
	Error   string    `json:"error"`
}

// ReportSummary is a stored report without its content, as ListReports
// returns it.
type ReportSummary struct {
	ID             uint
	GeneratedAt    time.Time
	PeriodStart    time.Time
	PeriodEnd      time.Time
	Trigger        string
	TriggeredBy    string
	Notifications  []string
	Receivers      int
	ReceiverErrors int
	Stale          int
	Jobs           int
	JobErrors      int
}

// Report is one stored report (see reportModel).
type Report struct {
	ReportSummary

	Content ReportContent
}

// SaveReport stores r as a new report, ignoring r.ID and the counts (both
// derived here), and returns it as stored.
func (s *gormStore) SaveReport(ctx context.Context, r Report) (Report, error) {
	m := reportModel{
		GeneratedAt: r.GeneratedAt.UTC(), PeriodStart: r.PeriodStart.UTC(), PeriodEnd: r.PeriodEnd.UTC(),
		Trigger: r.Trigger, TriggeredBy: r.TriggeredBy, Notifications: r.Notifications,
		Receivers: len(r.Content.Receivers), ReceiverErrors: len(r.Content.ReceiverErrors), Stale: len(r.Content.Stale),
		Jobs: len(r.Content.Jobs), JobErrors: len(r.Content.JobErrors),
		Content: r.Content,
	}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		return Report{}, fmt.Errorf("saving report: %w", err)
	}

	return reportFromModel(m), nil
}

// ListReports returns up to limit stored reports, newest first, without
// their content.
func (s *gormStore) ListReports(ctx context.Context, limit int) ([]ReportSummary, error) {
	var rows []reportModel

	err := s.read.WithContext(ctx).Omit("content").Order("generated_at DESC, id DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading reports: %w", err)
	}

	out := make([]ReportSummary, len(rows))
	for i, m := range rows {
		out[i] = reportFromModel(m).ReportSummary
	}

	return out, nil
}

// GetReport returns the stored report id, or ErrReportNotFound.
func (s *gormStore) GetReport(ctx context.Context, id uint) (Report, error) {
	var m reportModel

	if err := s.read.WithContext(ctx).Take(&m, id).Error; err != nil {
		if isRecordNotFound(err) {
			return Report{}, ErrReportNotFound
		}

		return Report{}, fmt.Errorf("reading report %d: %w", id, err)
	}

	return reportFromModel(m), nil
}

// DeleteReportsBefore deletes every report generated before cutoff,
// returning how many it removed.
func (s *gormStore) DeleteReportsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res := s.db.WithContext(ctx).Where("generated_at < ?", cutoff.UTC()).Delete(&reportModel{})
	if res.Error != nil {
		return 0, fmt.Errorf("pruning reports: %w", res.Error)
	}

	return res.RowsAffected, nil
}

func reportFromModel(m reportModel) Report {
	return Report{
		ID: m.ID, GeneratedAt: m.GeneratedAt, PeriodStart: m.PeriodStart, PeriodEnd: m.PeriodEnd,
		Trigger: m.Trigger, TriggeredBy: m.TriggeredBy, Notifications: m.Notifications,
		Receivers: m.Receivers, ReceiverErrors: m.ReceiverErrors, Stale: m.Stale, Jobs: m.Jobs, JobErrors: m.JobErrors,
		Content: m.Content,
	}
}
