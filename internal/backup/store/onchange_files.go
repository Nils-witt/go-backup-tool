package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// onChangeFileModel is onchange_files: for every on-change job, target, and
// file under the job's source, the content that target last confirmed
// having (see pipeline.runOnChange), so an unchanged file isn't uploaded
// again. Size and ModTime let a scan skip rehashing a file whose metadata
// hasn't changed; SHA256 is the plaintext's hash, compared once it has.
type onChangeFileModel struct {
	JobName    string    `gorm:"column:job_name;primaryKey"`
	Target     string    `gorm:"column:target;primaryKey"`
	Path       string    `gorm:"column:path;primaryKey"`
	Size       int64     `gorm:"column:size;not null"`
	ModTime    time.Time `gorm:"column:mod_time;not null"`
	SHA256     string    `gorm:"column:sha256;not null"`
	UploadedAt time.Time `gorm:"column:uploaded_at;not null"`
}

func (onChangeFileModel) TableName() string { return "onchange_files" }

// onChangePathColumn is onchange_files' path column.
const onChangePathColumn = "path"

// OnChangeFile is one onchange_files row (see onChangeFileModel). Path is
// slash-separated, relative to the job's source path.
type OnChangeFile struct {
	JobName    string
	Target     string
	Path       string
	Size       int64
	ModTime    time.Time
	SHA256     string
	UploadedAt time.Time
}

// ListOnChangeFiles returns every file target is recorded as having for
// the on-change job jobName, in path order.
func (s *gormStore) ListOnChangeFiles(ctx context.Context, jobName, target string) ([]OnChangeFile, error) {
	var rows []onChangeFileModel

	err := s.read.WithContext(ctx).
		Where("job_name = ? AND target = ?", jobName, target).
		Order(onChangePathColumn).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading on-change files: %w", err)
	}

	out := make([]OnChangeFile, len(rows))
	for i, m := range rows {
		out[i] = OnChangeFile(m)
	}

	return out, nil
}

// SaveOnChangeFile records (an upsert) that f.Target has f.Path's content
// as of f's hash and metadata.
func (s *gormStore) SaveOnChangeFile(ctx context.Context, f OnChangeFile) error {
	f.ModTime, f.UploadedAt = f.ModTime.UTC(), f.UploadedAt.UTC()
	m := onChangeFileModel(f)

	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "job_name"}, {Name: "target"}, {Name: onChangePathColumn}},
		DoUpdates: clause.AssignmentColumns([]string{"size", "mod_time", "sha256", "uploaded_at"}),
	}).Create(&m).Error
	if err != nil {
		return fmt.Errorf("recording on-change file: %w", err)
	}

	return nil
}

// DeleteOnChangeFiles forgets every file recorded for the on-change job
// jobName, on every target — once the job is deleted, or so a re-created
// job of the same name starts by uploading everything again.
func (s *gormStore) DeleteOnChangeFiles(ctx context.Context, jobName string) error {
	if err := s.db.WithContext(ctx).Where("job_name = ?", jobName).Delete(&onChangeFileModel{}).Error; err != nil {
		return fmt.Errorf("deleting on-change files: %w", err)
	}

	return nil
}
