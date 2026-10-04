package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"
)

// apiTokenModel is api_tokens: every long-lived read-only API token the web
// UI has issued (see internal/backup/webui/tokens.go). Only the token's
// metadata is kept — never the signed token itself, which is shown once at
// creation — and a revoked token's row stays behind (RevokedAt set) so the
// token list still shows what it was and who revoked it.
type apiTokenModel struct {
	ID        string     `gorm:"column:id;primaryKey"`
	Name      string     `gorm:"column:name;not null"`
	CreatedBy string     `gorm:"column:created_by;not null"`
	CreatedAt time.Time  `gorm:"column:created_at;not null"`
	ExpiresAt time.Time  `gorm:"column:expires_at;not null"`
	RevokedAt *time.Time `gorm:"column:revoked_at"`
	RevokedBy string     `gorm:"column:revoked_by;not null;default:''"`
}

func (apiTokenModel) TableName() string { return "api_tokens" }

// tokenSigningKeyModel is token_signing_keys: the single HMAC key API tokens
// are signed with, generated on first use (see TokenSigningKey).
type tokenSigningKeyModel struct {
	ID  uint   `gorm:"column:id;primaryKey"`
	Key []byte `gorm:"column:key;not null"`
}

func (tokenSigningKeyModel) TableName() string { return "token_signing_keys" }

// tokenSigningKeyBytes is the HMAC-SHA256 key length TokenSigningKey
// generates: the hash's own output size.
const tokenSigningKeyBytes = 32

// APIToken is one issued API token's metadata (see SaveAPIToken).
type APIToken struct {
	ID        string
	Name      string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
	RevokedBy string
}

// TokenSigningKey returns the API token signing key, generating and storing
// a random one on first call.
func (s *Store) TokenSigningKey(ctx context.Context) ([]byte, error) {
	var m tokenSigningKeyModel

	err := s.db.WithContext(ctx).Take(&m, 1).Error
	if err == nil {
		return m.Key, nil
	}

	if !isRecordNotFound(err) {
		return nil, fmt.Errorf("reading token signing key: %w", err)
	}

	key := make([]byte, tokenSigningKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating token signing key: %w", err)
	}

	m = tokenSigningKeyModel{ID: 1, Key: key}
	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		return nil, fmt.Errorf("storing token signing key: %w", err)
	}

	return key, nil
}

// SaveAPIToken records a newly issued API token.
func (s *Store) SaveAPIToken(ctx context.Context, t APIToken) error {
	m := apiTokenModel{
		ID: t.ID, Name: t.Name, CreatedBy: t.CreatedBy,
		CreatedAt: t.CreatedAt.UTC(), ExpiresAt: t.ExpiresAt.UTC(),
	}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("recording api token: %w", err)
	}

	return nil
}

// GetAPIToken looks up the API token with the given id, reporting false if
// there is none.
func (s *Store) GetAPIToken(ctx context.Context, id string) (APIToken, bool, error) {
	var m apiTokenModel

	if err := s.db.WithContext(ctx).Take(&m, "id = ?", id).Error; err != nil {
		if isRecordNotFound(err) {
			return APIToken{}, false, nil
		}

		return APIToken{}, false, fmt.Errorf("reading api token: %w", err)
	}

	return APIToken(m), true, nil
}

// ListAPITokens returns every issued API token, revoked and expired ones
// included, newest first.
func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	var rows []apiTokenModel

	if err := s.db.WithContext(ctx).Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading api tokens: %w", err)
	}

	tokens := make([]APIToken, len(rows))
	for i, m := range rows {
		tokens[i] = APIToken(m)
	}

	return tokens, nil
}

// RevokeAPIToken marks the API token with the given id revoked by `by` at
// `at`, reporting false if no such token exists or it was already revoked.
func (s *Store) RevokeAPIToken(ctx context.Context, id, by string, at time.Time) (bool, error) {
	at = at.UTC()

	res := s.db.WithContext(ctx).Model(&apiTokenModel{}).
		Where("id = ? AND revoked_at IS NULL", id).
		Updates(map[string]any{"revoked_at": &at, "revoked_by": by})
	if res.Error != nil {
		return false, fmt.Errorf("revoking api token: %w", res.Error)
	}

	return res.RowsAffected > 0, nil
}
