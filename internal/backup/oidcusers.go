package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// oidcUserPermissionsSchema is oidc_user_permissions: a per-identity
// permission record for an OIDC/SSO login (see handleOIDCCallback in
// oidc.go), keyed by the same identity string recorded in the login log
// (see oidcIdentity in oidc.go — an idToken's "email" claim, or its subject
// if the provider didn't send one). OIDC has no notion of an account
// managed here the way a web UI "Users" admin-managed account is (see
// webUIUsersSchema/webusers.go) — there's no password to store — so a row
// simply grants webui.oidc.default-permissions: (or, once an admin has
// edited it, whatever they've since set) to that one identity.
// handleOIDCCallback provisions a row automatically at an identity's first
// login (rather than requiring an admin to add one before it appears in the
// "Users" admin section's OIDC listing), granting it the currently
// configured default; every login thereafter honors whatever's currently
// stored, including an admin's own edit (see OIDCUserPermissions).
const oidcUserPermissionsSchema = `CREATE TABLE IF NOT EXISTS oidc_user_permissions (
	identity    TEXT NOT NULL PRIMARY KEY,
	permissions INTEGER NOT NULL,
	updated_at  TIMESTAMP NOT NULL
)`

// OIDCUserPermission is one identity's stored permission override, as
// returned by ListOIDCUserPermissions for the "Users" admin section's OIDC
// listing.
type OIDCUserPermission struct {
	Identity    string
	Permissions Permission
	UpdatedAt   time.Time
}

// ErrOIDCUserPermissionsNotFound is returned by DeleteOIDCUserPermissions
// when identity has no stored override.
var ErrOIDCUserPermissionsNotFound = errors.New("oidc user permission override not found")

// SetOIDCUserPermissions stores perm as identity's permission record,
// replacing any existing one (an upsert, since — unlike a web UI "Users"
// admin-managed account — there's no separate create step: this both
// provisions an identity's very first record, automatically at its first
// login (see handleOIDCCallback in oidc.go), and applies an admin's later
// edit to an existing one).
func SetOIDCUserPermissions(ctx context.Context, db *sql.DB, identity string, perm Permission) error {
	const upsert = `INSERT INTO oidc_user_permissions (identity, permissions, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(identity) DO UPDATE SET permissions = excluded.permissions, updated_at = excluded.updated_at`

	if _, err := db.ExecContext(ctx, upsert, identity, int(perm), time.Now().UTC()); err != nil {
		return fmt.Errorf("setting oidc user %q permissions: %w", identity, err)
	}

	return nil
}

// DeleteOIDCUserPermissions removes identity's stored permission override,
// reverting it to webui.oidc.default-permissions:. Returns
// ErrOIDCUserPermissionsNotFound if identity has no override.
func DeleteOIDCUserPermissions(ctx context.Context, db *sql.DB, identity string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM oidc_user_permissions WHERE identity = ?`, identity)
	if err != nil {
		return fmt.Errorf("deleting oidc user %q permissions: %w", identity, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("deleting oidc user %q permissions: %w", identity, err)
	}

	if n == 0 {
		return ErrOIDCUserPermissionsNotFound
	}

	return nil
}

// ListOIDCUserPermissions returns every stored OIDC permission override, in
// identity order, for the "Users" admin section's OIDC listing.
func ListOIDCUserPermissions(ctx context.Context, db *sql.DB) ([]OIDCUserPermission, error) {
	query := `SELECT identity, permissions, updated_at FROM oidc_user_permissions ORDER BY identity`

	return queryRows(ctx, db, "listing oidc user permissions", query, nil, func(rows *sql.Rows) (OIDCUserPermission, error) {
		var (
			p    OIDCUserPermission
			perm int
		)

		if err := rows.Scan(&p.Identity, &perm, &p.UpdatedAt); err != nil {
			return OIDCUserPermission{}, err
		}

		p.Permissions = Permission(perm)

		return p, nil
	})
}

// OIDCUserPermissions looks up identity's stored permission override,
// reporting ok=false if it has none — the caller (handleOIDCCallback) then
// falls back to webui.oidc.default-permissions:.
func OIDCUserPermissions(ctx context.Context, db *sql.DB, identity string) (Permission, bool, error) {
	perm, ok, err := queryRowOptional(ctx, db, fmt.Sprintf("looking up oidc user %q permissions", identity),
		`SELECT permissions FROM oidc_user_permissions WHERE identity = ?`, []any{identity},
		func(row *sql.Row) (int, error) {
			var perm int
			if err := row.Scan(&perm); err != nil {
				return 0, err
			}

			return perm, nil
		})
	if err != nil {
		return 0, false, err
	}

	return Permission(perm), ok, nil
}
