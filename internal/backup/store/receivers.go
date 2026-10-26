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
	// PublicKey is the single sender key receivers used before trusted
	// servers, removed in v8.0.0. It's always written empty (an update
	// clears it) and only read by ListReceiversRelyingOnPublicKey.
	PublicKey string `gorm:"column:public_key;not null"`
	// Type is the receiver's kind as entered: "" or "backup", or
	// "directory". KeepVersions is a directory receiver's keep-versions:.
	Type                  string    `gorm:"column:type;not null;default:''"`
	KeepVersions          int       `gorm:"column:keep_versions;not null;default:0"`
	AllowedServers        []string  `gorm:"column:allowed_servers;serializer:json"`
	Path                  string    `gorm:"column:path;not null"`
	Retention             string    `gorm:"column:retention;not null;default:''"`
	StaleAfter            string    `gorm:"column:stale_after;not null;default:''"`
	StaleNotifications    []string  `gorm:"column:stale_notifications;serializer:json"`
	DownloadNotifications []string  `gorm:"column:download_notifications;serializer:json"`
	MaxObjectSize         string    `gorm:"column:max_object_size;not null;default:''"`
	Quota                 string    `gorm:"column:quota;not null;default:''"`
	Immutable             bool      `gorm:"column:immutable;not null;default:false"`
	CreatedAt             time.Time `gorm:"column:created_at;not null"`
	CreatedBy             string    `gorm:"column:created_by;not null"`
	UpdatedAt             time.Time `gorm:"column:updated_at;not null"`
	UpdatedBy             string    `gorm:"column:updated_by;not null"`
}

func (receiverModel) TableName() string { return "receivers" }

// ReceiverConfig is one stored receiver definition (see receiverModel).
type ReceiverConfig struct {
	ID                    string
	Type                  string
	KeepVersions          int
	AllowedServers        []string
	Path                  string
	Retention             string
	StaleAfter            string
	StaleNotifications    []string
	DownloadNotifications []string
	MaxObjectSize         string
	Quota                 string
	Immutable             bool
	CreatedAt             time.Time
	CreatedBy             string
	UpdatedAt             time.Time
	UpdatedBy             string
}

// newReceiverModel maps r onto its stored row, with no public key.
func newReceiverModel(r ReceiverConfig) receiverModel {
	return receiverModel{
		ID: r.ID, Type: r.Type, KeepVersions: r.KeepVersions, AllowedServers: r.AllowedServers, Path: r.Path, Retention: r.Retention, StaleAfter: r.StaleAfter,
		StaleNotifications: r.StaleNotifications, DownloadNotifications: r.DownloadNotifications,
		MaxObjectSize: r.MaxObjectSize, Quota: r.Quota, Immutable: r.Immutable,
		CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy,
	}
}

// config maps a stored row back onto the ReceiverConfig callers see.
func (m receiverModel) config() ReceiverConfig {
	return ReceiverConfig{
		ID: m.ID, Type: m.Type, KeepVersions: m.KeepVersions, AllowedServers: m.AllowedServers, Path: m.Path, Retention: m.Retention, StaleAfter: m.StaleAfter,
		StaleNotifications: m.StaleNotifications, DownloadNotifications: m.DownloadNotifications,
		MaxObjectSize: m.MaxObjectSize, Quota: m.Quota, Immutable: m.Immutable,
		CreatedAt: m.CreatedAt, CreatedBy: m.CreatedBy, UpdatedAt: m.UpdatedAt, UpdatedBy: m.UpdatedBy,
	}
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
func (s *gormStore) ListReceiverConfigs(ctx context.Context) ([]ReceiverConfig, error) {
	var rows []receiverModel

	if err := s.read.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading receivers: %w", err)
	}

	out := make([]ReceiverConfig, len(rows))
	for i, m := range rows {
		out[i] = m.config()
	}

	return out, nil
}

// GetReceiverConfig looks up the stored receiver with the given id,
// reporting false if there is none.
func (s *gormStore) GetReceiverConfig(ctx context.Context, id string) (ReceiverConfig, bool, error) {
	var m receiverModel

	if err := s.read.WithContext(ctx).Take(&m, "id = ?", id).Error; err != nil {
		if isRecordNotFound(err) {
			return ReceiverConfig{}, false, nil
		}

		return ReceiverConfig{}, false, fmt.Errorf("reading receiver: %w", err)
	}

	return m.config(), true, nil
}

// CreateReceiverConfig stores a new receiver, returning ErrReceiverExists if
// its id is already taken.
func (s *gormStore) CreateReceiverConfig(ctx context.Context, r ReceiverConfig) error {
	m := newReceiverModel(normalizeReceiverTimes(r))

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
func (s *gormStore) UpdateReceiverConfig(ctx context.Context, r ReceiverConfig) error {
	r = normalizeReceiverTimes(r)

	res := s.db.WithContext(ctx).Model(&receiverModel{}).Where("id = ?", r.ID).
		Select("public_key", "type", "keep_versions", "allowed_servers", "path", "retention", "stale_after", "stale_notifications", "download_notifications", "max_object_size", "quota", "immutable", "updated_at", "updated_by").
		Updates(&receiverModel{
			Type: r.Type, KeepVersions: r.KeepVersions,
			AllowedServers: r.AllowedServers, Path: r.Path, Retention: r.Retention, StaleAfter: r.StaleAfter,
			StaleNotifications: r.StaleNotifications, DownloadNotifications: r.DownloadNotifications,
			MaxObjectSize: r.MaxObjectSize, Quota: r.Quota, Immutable: r.Immutable,
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
func (s *gormStore) DeleteReceiverConfig(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Delete(&receiverModel{}, "id = ?", id)
	if res.Error != nil {
		return fmt.Errorf("deleting receiver: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrReceiverNotFound
	}

	return nil
}

func normalizeReceiverTimes(r ReceiverConfig) ReceiverConfig {
	r.CreatedAt = r.CreatedAt.UTC()
	r.UpdatedAt = r.UpdatedAt.UTC()

	return r
}

// ListReceiversRelyingOnPublicKey returns the ids of stored receivers that
// still authenticate senders only by a public key, which v8.0.0 removed: a
// public key and no allowed servers. They can no longer accept uploads
// until a sender is added as a trusted server and allowed on them.
func (s *gormStore) ListReceiversRelyingOnPublicKey(ctx context.Context) ([]string, error) {
	var rows []receiverModel

	if err := s.read.WithContext(ctx).Where("public_key <> ''").Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading receivers: %w", err)
	}

	var ids []string

	for _, m := range rows {
		if len(m.AllowedServers) == 0 {
			ids = append(ids, m.ID)
		}
	}

	return ids, nil
}
