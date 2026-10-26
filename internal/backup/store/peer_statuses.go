package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// peerStatusModel is peer_statuses: the status every other instance last
// pushed to this one (see internal/backup/peerstatus), keyed by its server
// UUID (the trusted server it authenticated as), so the dashboard still
// shows it after a restart. Payload is the push's body exactly as received
// and verified: its format belongs to peerstatus, which decodes it, so it's
// stored as opaque bytes rather than as a JSON column of store's own types.
type peerStatusModel struct {
	ServerID   string    `gorm:"column:server_id;primaryKey"`
	ReceivedAt time.Time `gorm:"column:received_at;not null"`
	Payload    []byte    `gorm:"column:payload;not null"`
}

func (peerStatusModel) TableName() string { return "peer_statuses" }

// PeerStatus is one instance's last pushed status (see peerStatusModel).
type PeerStatus struct {
	ServerID   string
	ReceivedAt time.Time
	Payload    []byte
}

// ListPeerStatuses returns every stored instance status, in server id
// order.
func (s *gormStore) ListPeerStatuses(ctx context.Context) ([]PeerStatus, error) {
	var rows []peerStatusModel

	if err := s.read.WithContext(ctx).Order("server_id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading peer statuses: %w", err)
	}

	out := make([]PeerStatus, len(rows))
	for i, m := range rows {
		out[i] = PeerStatus(m)
	}

	return out, nil
}

// SavePeerStatus stores p as instance p.ServerID's status, replacing the
// one it pushed before.
func (s *gormStore) SavePeerStatus(ctx context.Context, p PeerStatus) error {
	m := peerStatusModel{ServerID: p.ServerID, ReceivedAt: p.ReceivedAt.UTC(), Payload: p.Payload}

	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error; err != nil {
		return fmt.Errorf("storing peer status: %w", err)
	}

	return nil
}

// DeletePeerStatus deletes instance serverID's stored status, a no-op if
// there is none.
func (s *gormStore) DeletePeerStatus(ctx context.Context, serverID string) error {
	if err := s.db.WithContext(ctx).Delete(&peerStatusModel{}, "server_id = ?", serverID).Error; err != nil {
		return fmt.Errorf("deleting peer status: %w", err)
	}

	return nil
}
