package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// apiTokensSchema is webui_tokens: every long-lived API token an admin has
// minted for a web UI "Users" admin-managed account (see
// handleIssueWebUIUserToken in webui.go), recorded so the "Users" admin
// section can list a user's outstanding tokens and revoke one by jti — the
// admin who mints a token is never shown it again after issuance (see
// loginResponseJSON), so without this record there'd be no way to revoke a
// leaked or no-longer-needed token without also holding it. revoked_at is
// set once by RevokeAPIToken and left alone after; an interactive login's
// own short-lived session token (see sessionStore.create in webui.go) is
// never recorded here, only tokens minted with a caller-chosen TTL. jti
// matches the token's own JWT "jti" claim, letting sessionStore's revocation
// check (a jti blocklist) and this table agree on the same identifier.
const apiTokensSchema = `CREATE TABLE IF NOT EXISTS webui_tokens (
	jti         TEXT NOT NULL PRIMARY KEY,
	username    TEXT NOT NULL,
	permissions INTEGER NOT NULL,
	created_at  TIMESTAMP NOT NULL,
	expires_at  TIMESTAMP NOT NULL,
	revoked_at  TIMESTAMP
)`

// APIToken is one long-lived API token minted for a web UI "Users"
// admin-managed account (see RecordAPIToken), as returned by
// ListAPITokensForUser/ListRevokedAPITokens/RevokeAPIToken. It never carries
// the token's own signed JWT value — only jti, the identifier that was
// embedded in it — since the raw token itself is never stored (see
// apiTokensSchema).
type APIToken struct {
	JTI         string
	Username    string
	Permissions Permission
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time // nil if not revoked
}

// ErrAPITokenNotFound is returned by RevokeAPIToken when jti doesn't name a
// recorded token.
var ErrAPITokenNotFound = errors.New("token not found")

// RecordAPIToken records that a long-lived API token identified by jti was
// just minted for username, granting perm, valid from createdAt until
// expiresAt. Called once by handleIssueWebUIUserToken right after
// sessionStore.createWithTTL mints the token itself.
func RecordAPIToken(ctx context.Context, db *sql.DB, jti, username string, perm Permission, createdAt, expiresAt time.Time) error {
	const insert = `INSERT INTO webui_tokens (jti, username, permissions, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`

	if _, err := db.ExecContext(ctx, insert, jti, username, int(perm), createdAt.UTC(), expiresAt.UTC()); err != nil {
		return fmt.Errorf("recording API token for %q: %w", username, err)
	}

	return nil
}

// scanAPIToken scans one webui_tokens row (jti, username, permissions,
// created_at, expires_at, revoked_at, in that order) into an APIToken.
// Shared by ListAPITokensForUser, ListRevokedAPITokens, and RevokeAPIToken.
func scanAPIToken(row interface {
	Scan(dest ...any) error
},
) (APIToken, error) {
	var (
		t         APIToken
		perm      int
		revokedAt sql.NullTime
	)

	if err := row.Scan(&t.JTI, &t.Username, &perm, &t.CreatedAt, &t.ExpiresAt, &revokedAt); err != nil {
		return APIToken{}, err
	}

	t.Permissions = Permission(perm)

	if revokedAt.Valid {
		t.RevokedAt = &revokedAt.Time
	}

	return t, nil
}

// ListAPITokensForUser returns every long-lived API token recorded for
// username, most recently issued first, for the "Users" admin section's
// per-user token listing.
func ListAPITokensForUser(ctx context.Context, db *sql.DB, username string) ([]APIToken, error) {
	query := `SELECT jti, username, permissions, created_at, expires_at, revoked_at FROM webui_tokens WHERE username = ? ORDER BY created_at DESC`

	return queryRows(ctx, db, fmt.Sprintf("listing API tokens for %q", username), query, []any{username}, func(rows *sql.Rows) (APIToken, error) {
		return scanAPIToken(rows)
	})
}

// ListRevokedAPITokens returns every recorded API token that's been revoked
// (see RevokeAPIToken) and hasn't yet reached its own expiry, for
// newSessionStore to preload sessionStore's in-memory revocation blocklist
// at startup — otherwise a restart would forget every revocation made
// against a token that's still outstanding, since a JWT's signature alone
// can't be un-signed and the blocklist itself only ever lived in memory
// before this.
func ListRevokedAPITokens(ctx context.Context, db *sql.DB, now time.Time) ([]APIToken, error) {
	query := `SELECT jti, username, permissions, created_at, expires_at, revoked_at FROM webui_tokens WHERE revoked_at IS NOT NULL AND expires_at > ?`

	return queryRows(ctx, db, "listing revoked API tokens", query, []any{now.UTC()}, func(rows *sql.Rows) (APIToken, error) {
		return scanAPIToken(rows)
	})
}

// GetAPIToken returns the recorded API token named by jti, reporting
// ok=false if it doesn't name a recorded token. Used by
// handleRevokeWebUIUserToken to check a token's owner *before* revoking it
// — RevokeAPIToken itself doesn't take a username, so skipping this check
// would let a request naming the wrong {username} in its URL still revoke
// another user's token while reporting 404, as if nothing had happened.
func GetAPIToken(ctx context.Context, db *sql.DB, jti string) (APIToken, bool, error) {
	query := `SELECT jti, username, permissions, created_at, expires_at, revoked_at FROM webui_tokens WHERE jti = ?`

	return queryRowOptional(ctx, db, fmt.Sprintf("looking up API token %q", jti), query, []any{jti}, func(row *sql.Row) (APIToken, error) {
		return scanAPIToken(row)
	})
}

// RevokeAPIToken marks the recorded API token named by jti revoked (a no-op,
// not an error, if it was already revoked) and returns its up-to-date row so
// the caller can also blocklist it in sessionStore's in-memory revocation
// check (see sessionStore.revokeJTI in webui.go) without a second lookup.
// Returns ErrAPITokenNotFound if jti doesn't name a recorded token —
// including an interactive login's own session token, which is never
// recorded here in the first place (see apiTokensSchema). Callers that also
// need to check the token's owner before revoking it (see
// handleRevokeWebUIUserToken) must do so themselves first, via GetAPIToken
// — this always revokes jti regardless of who it belongs to.
func RevokeAPIToken(ctx context.Context, db *sql.DB, jti string, at time.Time) (APIToken, error) {
	if _, err := db.ExecContext(ctx, `UPDATE webui_tokens SET revoked_at = ? WHERE jti = ? AND revoked_at IS NULL`, at.UTC(), jti); err != nil {
		return APIToken{}, fmt.Errorf("revoking API token %q: %w", jti, err)
	}

	query := `SELECT jti, username, permissions, created_at, expires_at, revoked_at FROM webui_tokens WHERE jti = ?`

	t, ok, err := queryRowOptional(ctx, db, fmt.Sprintf("looking up API token %q", jti), query, []any{jti}, func(row *sql.Row) (APIToken, error) {
		return scanAPIToken(row)
	})
	if err != nil {
		return APIToken{}, err
	}

	if !ok {
		return APIToken{}, ErrAPITokenNotFound
	}

	return t, nil
}
