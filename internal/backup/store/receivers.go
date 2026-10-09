package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// receiverModel is receivers: every receiver API entry this instance
// accepts objects into (see internal/backup/receiver's Manager), managed in
// the web UI. Fields are kept exactly as entered — durations and
// notification ids unparsed — so a row round-trips to the edit form, and is
// validated (config.ResolveReceiver) on every load rather than once on save.
type receiverModel struct {
	ID string `gorm:"column:id;primaryKey"`
	// PublicKey is the deprecated single sender key, "" once a receiver
	// only names AllowedServers.
	PublicKey             string    `gorm:"column:public_key;not null"`
	AllowedServers        []string  `gorm:"column:allowed_servers;serializer:json"`
	Path                  string    `gorm:"column:path;not null"`
	Retention             string    `gorm:"column:retention;not null;default:''"`
	StaleAfter            string    `gorm:"column:stale_after;not null;default:''"`
	StaleNotifications    []string  `gorm:"column:stale_notifications;serializer:json"`
	DownloadNotifications []string  `gorm:"column:download_notifications;serializer:json"`
	CreatedAt             time.Time `gorm:"column:created_at;not null"`
	CreatedBy             string    `gorm:"column:created_by;not null"`
	UpdatedAt             time.Time `gorm:"column:updated_at;not null"`
	UpdatedBy             string    `gorm:"column:updated_by;not null"`
}

func (receiverModel) TableName() string { return "receivers" }

// ReceiverConfig is one stored receiver definition (see receiverModel).
type ReceiverConfig struct {
	ID                    string
	PublicKey             string
	AllowedServers        []string
	Path                  string
	Retention             string
	StaleAfter            string
	StaleNotifications    []string
	DownloadNotifications []string
	CreatedAt             time.Time
	CreatedBy             string
	UpdatedAt             time.Time
	UpdatedBy             string
}

var (
	// ErrReceiverExists is returned by CreateReceiverConfig for an id
	// that's already taken.
	ErrReceiverExists = errors.New("a receiver with this id already exists")
	// ErrReceiverNotFound is returned by UpdateReceiverConfig/
	// DeleteReceiverConfig for an id with no stored receiver.
	ErrReceiverNotFound = errors.New("receiver not found")
)

// ListReceiverConfigs returns every stored receiver, in id order.
func (s *Store) ListReceiverConfigs(ctx context.Context) ([]ReceiverConfig, error) {
	var rows []receiverModel

	if err := s.db.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading receivers: %w", err)
	}

	out := make([]ReceiverConfig, len(rows))
	for i, m := range rows {
		out[i] = ReceiverConfig(m)
	}

	return out, nil
}

// GetReceiverConfig looks up the stored receiver with the given id,
// reporting false if there is none.
func (s *Store) GetReceiverConfig(ctx context.Context, id string) (ReceiverConfig, bool, error) {
	var m receiverModel

	if err := s.db.WithContext(ctx).Take(&m, "id = ?", id).Error; err != nil {
		if isRecordNotFound(err) {
			return ReceiverConfig{}, false, nil
		}

		return ReceiverConfig{}, false, fmt.Errorf("reading receiver: %w", err)
	}

	return ReceiverConfig(m), true, nil
}

// CreateReceiverConfig stores a new receiver, returning ErrReceiverExists if
// its id is already taken.
func (s *Store) CreateReceiverConfig(ctx context.Context, r ReceiverConfig) error {
	m := receiverModel(normalizeReceiverTimes(r))

	res := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&m)
	if res.Error != nil {
		return fmt.Errorf("storing receiver: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrReceiverExists
	}

	return nil
}

// UpdateReceiverConfig replaces the stored receiver r.ID's definition,
// keeping its CreatedAt/CreatedBy, and returns ErrReceiverNotFound if there
// is none.
func (s *Store) UpdateReceiverConfig(ctx context.Context, r ReceiverConfig) error {
	r = normalizeReceiverTimes(r)

	res := s.db.WithContext(ctx).Model(&receiverModel{}).Where("id = ?", r.ID).
		Select("public_key", "allowed_servers", "path", "retention", "stale_after", "stale_notifications", "download_notifications", "updated_at", "updated_by").
		Updates(&receiverModel{
			PublicKey: r.PublicKey, AllowedServers: r.AllowedServers, Path: r.Path, Retention: r.Retention, StaleAfter: r.StaleAfter,
			StaleNotifications: r.StaleNotifications, DownloadNotifications: r.DownloadNotifications,
			UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy,
		})
	if res.Error != nil {
		return fmt.Errorf("updating receiver: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrReceiverNotFound
	}

	return nil
}

// DeleteReceiverConfig removes the stored receiver with the given id,
// returning ErrReceiverNotFound if there is none. Its receiver_events and
// tracked objects are kept, as are its files on disk.
func (s *Store) DeleteReceiverConfig(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Delete(&receiverModel{}, "id = ?", id)
	if res.Error != nil {
		return fmt.Errorf("deleting receiver: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrReceiverNotFound
	}

	return nil
}

// ImportReceiverConfigs stores every entry of rs whose id isn't stored yet,
// leaving existing rows (possibly edited in the web UI since) untouched, and
// returns the ids it inserted. Used once per startup to carry the config
// file's deprecated receivers: entries over into the state db.
func (s *Store) ImportReceiverConfigs(ctx context.Context, rs []ReceiverConfig) ([]string, error) {
	rows := make([]receiverModel, len(rs))
	for i, r := range rs {
		rows[i] = receiverModel(normalizeReceiverTimes(r))
	}

	imported, err := insertMissing(ctx, s.db, rows, func(m receiverModel) string { return m.ID })
	if err != nil {
		return nil, fmt.Errorf("importing receivers: %w", err)
	}

	return imported, nil
}

func normalizeReceiverTimes(r ReceiverConfig) ReceiverConfig {
	r.CreatedAt = r.CreatedAt.UTC()
	r.UpdatedAt = r.UpdatedAt.UTC()

	return r
}
