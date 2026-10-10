package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// objectModel is objects: records every file go-backup-tool has written to
// a local target or receiver with retention: set, so a later sweep knows
// what's eligible for automatic deletion. RetentionSeconds records the
// retention duration in effect when each object was written, so a later
// config change to a server's retention: doesn't retroactively change how
// long already-written objects are kept (see ExpiredObjectPaths). It
// defaults to 0 ("unknown") so rows from before this column existed keep
// sweeping under the caller's current retention, exactly as they did before
// it was added.
type objectModel struct {
	Server           string    `gorm:"column:server;not null;index:idx_objects_server"`
	Bucket           string    `gorm:"column:bucket;not null"`
	Path             string    `gorm:"column:path;primaryKey"`
	WrittenAt        time.Time `gorm:"column:written_at;not null"`
	RetentionSeconds int64     `gorm:"column:retention_seconds;not null;default:0"`
}

func (objectModel) TableName() string { return "objects" }

// SaveObjectWrite records that a file was written to path on server/bucket
// at writtenAt, tracked for retentionSeconds — an upsert, so a later write
// to the same path (e.g. a repeating job overwriting its own object)
// refreshes both the write time and the retention window in effect for it.
func (s *Store) SaveObjectWrite(ctx context.Context, server, bucket, path string, writtenAt time.Time, retentionSeconds int64) error {
	m := objectModel{
		Server:           server,
		Bucket:           bucket,
		Path:             path,
		WrittenAt:        writtenAt.UTC(),
		RetentionSeconds: retentionSeconds,
	}

	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "path"}},
		DoUpdates: clause.AssignmentColumns([]string{"written_at", "retention_seconds"}),
	}).Create(&m).Error
	if err != nil {
		return fmt.Errorf("recording write to state db: %w", err)
	}

	return nil
}

// DeleteObjectWrite removes any tracked record for path, used both when a
// swept file is removed from disk and when a write is rolled back, so the
// database doesn't go on tracking a file that no longer exists.
func (s *Store) DeleteObjectWrite(ctx context.Context, path string) error {
	if err := s.db.WithContext(ctx).Where("path = ?", path).Delete(&objectModel{}).Error; err != nil {
		return fmt.Errorf("removing retention record: %w", err)
	}

	return nil
}

// ExpiredObjectPaths returns the tracked paths for server that are past
// their retention window as of now. Each row's own retention_seconds is
// used when set (> 0); rows recorded before that column existed have it as
// 0 and fall back to fallbackRetention (the caller's current retention for
// server).
//
// The filter runs in sqlite rather than loading every row for server: one
// indexed written_at range query per distinct retention_seconds value
// (normally just one or two per server), each with its own cutoff.
func (s *Store) ExpiredObjectPaths(ctx context.Context, server string, now time.Time, fallbackRetention time.Duration) ([]string, error) {
	db := s.db.WithContext(ctx)

	var retentions []int64

	if err := db.Model(&objectModel{}).Where("server = ?", server).
		Distinct().Pluck("retention_seconds", &retentions).Error; err != nil {
		return nil, fmt.Errorf("reading retention rows: %w", err)
	}

	var paths []string

	for _, secs := range retentions {
		retention := time.Duration(secs) * time.Second
		if retention <= 0 {
			retention = fallbackRetention
		}

		var batch []string

		err := db.Model(&objectModel{}).
			Where("server = ? AND retention_seconds = ? AND written_at < ?", server, secs, now.Add(-retention).UTC()).
			Order("written_at").
			Pluck("path", &batch).Error
		if err != nil {
			return nil, fmt.Errorf("reading retention rows: %w", err)
		}

		paths = append(paths, batch...)
	}

	return paths, nil
}
