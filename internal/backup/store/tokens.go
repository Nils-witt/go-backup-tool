package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

// apiTokenModel is webui_tokens: every long-lived API token an admin has
// minted for a web UI "Users" admin-managed account, recorded so the "Users"
// admin section can list a user's outstanding tokens and revoke one by jti —
// the admin who mints a token is never shown it again after issuance, so
// without this record there'd be no way to revoke a leaked or
// no-longer-needed token without also holding it. RevokedAt is set once by
// RevokeAPIToken and left alone after; an interactive login's own
// short-lived session token is never recorded here, only tokens minted with
// a caller-chosen TTL. JTI matches the token's own JWT "jti" claim, letting
// the caller's own revocation blocklist and this table agree on the same
// identifier.
type apiTokenModel struct {
	JTI         string       `gorm:"column:jti;primaryKey"`
	Username    string       `gorm:"column:username;not null"`
	Permissions int          `gorm:"column:permissions;not null"`
	CreatedAt   time.Time    `gorm:"column:created_at;not null"`
	ExpiresAt   time.Time    `gorm:"column:expires_at;not null"`
	RevokedAt   sql.NullTime `gorm:"column:revoked_at"`
}

func (apiTokenModel) TableName() string { return "webui_tokens" }

// APIToken is one long-lived API token minted for a web UI "Users"
// admin-managed account (see SaveAPIToken), as returned by
// ListAPITokensForUser/ListRevokedAPITokens/RevokeAPIToken. It never carries
// the token's own signed JWT value — only jti, the identifier that was
// embedded in it — since the raw token itself is never stored (see
// apiTokenModel).
type APIToken struct {
	JTI         string
	Username    string
	Permissions permission.Permission
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time // nil if not revoked
}

// ErrAPITokenNotFound is returned by RevokeAPIToken when jti doesn't name a
// recorded token.
var ErrAPITokenNotFound = errors.New("token not found")

// SaveAPIToken records that a long-lived API token identified by jti was
// just minted for username, granting perm, valid from createdAt until
// expiresAt.
func (s *Store) SaveAPIToken(ctx context.Context, jti, username string, perm permission.Permission, createdAt, expiresAt time.Time) error {
	m := apiTokenModel{
		JTI:         jti,
		Username:    username,
		Permissions: int(perm),
		CreatedAt:   createdAt.UTC(),
		ExpiresAt:   expiresAt.UTC(),
	}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("recording API token for %q: %w", username, err)
	}

	return nil
}

// toAPIToken converts an apiTokenModel row into the exported APIToken
// shape.
func toAPIToken(m apiTokenModel) APIToken {
	t := APIToken{
		JTI:         m.JTI,
		Username:    m.Username,
		Permissions: permission.Permission(m.Permissions),
		CreatedAt:   m.CreatedAt,
		ExpiresAt:   m.ExpiresAt,
	}

	if m.RevokedAt.Valid {
		t.RevokedAt = &m.RevokedAt.Time
	}

	return t
}

// ListAPITokensForUser returns every long-lived API token recorded for
// username, most recently issued first, for the "Users" admin section's
// per-user token listing.
func (s *Store) ListAPITokensForUser(ctx context.Context, username string) ([]APIToken, error) {
	var rows []apiTokenModel

	if err := s.db.WithContext(ctx).Where("username = ?", username).Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing API tokens for %q: %w", username, err)
	}

	tokens := make([]APIToken, len(rows))
	for i, m := range rows {
		tokens[i] = toAPIToken(m)
	}

	return tokens, nil
}

// ListRevokedAPITokens returns every recorded API token that's been revoked
// (see RevokeAPIToken) and hasn't yet reached its own expiry, for a caller
// (e.g. the web UI's session store) to preload an in-memory revocation
// blocklist at startup — otherwise a restart would forget every revocation
// made against a token that's still outstanding, since a JWT's signature
// alone can't be un-signed and such a blocklist itself only ever lives in
// memory before this.
func (s *Store) ListRevokedAPITokens(ctx context.Context, now time.Time) ([]APIToken, error) {
	var rows []apiTokenModel

	if err := s.db.WithContext(ctx).Where("revoked_at IS NOT NULL AND expires_at > ?", now.UTC()).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing revoked API tokens: %w", err)
	}

	tokens := make([]APIToken, len(rows))
	for i, m := range rows {
		tokens[i] = toAPIToken(m)
	}

	return tokens, nil
}

// GetAPIToken returns the recorded API token named by jti, reporting
// ok=false if it doesn't name a recorded token.
func (s *Store) GetAPIToken(ctx context.Context, jti string) (APIToken, bool, error) {
	var m apiTokenModel

	err := s.db.WithContext(ctx).Where("jti = ?", jti).Take(&m).Error

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return APIToken{}, false, nil
	case err != nil:
		return APIToken{}, false, fmt.Errorf("looking up API token %q: %w", jti, err)
	default:
		return toAPIToken(m), true, nil
	}
}

// RevokeAPIToken marks the recorded API token named by jti revoked (a no-op,
// not an error, if it was already revoked) and returns its up-to-date row so
// the caller can also blocklist it in its own in-memory revocation check
// without a second lookup. Returns ErrAPITokenNotFound if jti doesn't name a
// recorded token — including an interactive login's own session token,
// which is never recorded here in the first place (see apiTokenModel).
func (s *Store) RevokeAPIToken(ctx context.Context, jti string, at time.Time) (APIToken, error) {
	if err := s.db.WithContext(ctx).Model(&apiTokenModel{}).
		Where("jti = ? AND revoked_at IS NULL", jti).
		Update("revoked_at", at.UTC()).Error; err != nil {
		return APIToken{}, fmt.Errorf("revoking API token %q: %w", jti, err)
	}

	t, ok, err := s.GetAPIToken(ctx, jti)
	if err != nil {
		return APIToken{}, err
	}

	if !ok {
		return APIToken{}, ErrAPITokenNotFound
	}

	return t, nil
}
