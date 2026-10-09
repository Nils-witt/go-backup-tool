package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// trustedServerModel is trusted_servers: every remote go-backup-tool
// instance this one accepts receiver API requests from, keyed by that
// instance's persistent server UUID (the JWT issuer it signs requests with)
// and holding its PEM-encoded public key, managed in the web UI (see
// internal/backup/trust's Manager). Receivers reference rows here by id in
// their allowed_servers.
type trustedServerModel struct {
	ID        string    `gorm:"column:id;primaryKey"`
	Name      string    `gorm:"column:name;not null"`
	PublicKey string    `gorm:"column:public_key;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
	CreatedBy string    `gorm:"column:created_by;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
	UpdatedBy string    `gorm:"column:updated_by;not null"`
}

func (trustedServerModel) TableName() string { return "trusted_servers" }

// TrustedServerConfig is one stored trusted server (see trustedServerModel).
type TrustedServerConfig struct {
	ID        string
	Name      string
	PublicKey string
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
	UpdatedBy string
}

var (
	// ErrTrustedServerExists is returned by CreateTrustedServer for an id
	// that's already taken.
	ErrTrustedServerExists = errors.New("a trusted server with this id already exists")
	// ErrTrustedServerNotFound is returned by UpdateTrustedServer/
	// DeleteTrustedServer for an id with no stored trusted server.
	ErrTrustedServerNotFound = errors.New("trusted server not found")
)

// ListTrustedServers returns every stored trusted server, in id order.
func (s *Store) ListTrustedServers(ctx context.Context) ([]TrustedServerConfig, error) {
	var rows []trustedServerModel

	if err := s.db.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading trusted servers: %w", err)
	}

	out := make([]TrustedServerConfig, len(rows))
	for i, m := range rows {
		out[i] = TrustedServerConfig(m)
	}

	return out, nil
}

// CreateTrustedServer stores a new trusted server, returning
// ErrTrustedServerExists if its id is already taken.
func (s *Store) CreateTrustedServer(ctx context.Context, ts TrustedServerConfig) error {
	m := trustedServerModel(normalizeTrustedServerTimes(ts))

	res := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&m)
	if res.Error != nil {
		return fmt.Errorf("storing trusted server: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrTrustedServerExists
	}

	return nil
}

// UpdateTrustedServer replaces trusted server ts.ID's name and public key,
// keeping its CreatedAt/CreatedBy, and returns ErrTrustedServerNotFound if
// there is none.
func (s *Store) UpdateTrustedServer(ctx context.Context, ts TrustedServerConfig) error {
	ts = normalizeTrustedServerTimes(ts)

	res := s.db.WithContext(ctx).Model(&trustedServerModel{}).Where("id = ?", ts.ID).Updates(map[string]any{
		"name": ts.Name, "public_key": ts.PublicKey, "updated_at": ts.UpdatedAt, "updated_by": ts.UpdatedBy,
	})

	switch {
	case res.Error != nil:
		return fmt.Errorf("updating trusted server: %w", res.Error)
	case res.RowsAffected == 0:
		return ErrTrustedServerNotFound
	default:
		return nil
	}
}

// DeleteTrustedServer removes the stored trusted server with the given id,
// returning ErrTrustedServerNotFound if there is none.
func (s *Store) DeleteTrustedServer(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Delete(&trustedServerModel{}, "id = ?", id)
	if res.Error != nil {
		return fmt.Errorf("deleting trusted server: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return ErrTrustedServerNotFound
	}

	return nil
}

func normalizeTrustedServerTimes(ts TrustedServerConfig) TrustedServerConfig {
	ts.CreatedAt = ts.CreatedAt.UTC()
	ts.UpdatedAt = ts.UpdatedAt.UTC()

	return ts
}
