package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// webUIUsersSchema is webui_users: dashboard users managed through the web
// UI's "Users" admin section (see CreateWebUIUser and friends below),
// distinct from the single config-file admin (webui.username/password in
// config.go) that alone can manage them (see requireAdmin in webui.go).
// permissions is a Permission bitmask (see permission.go).
const webUIUsersSchema = `CREATE TABLE IF NOT EXISTS webui_users (
	username      TEXT NOT NULL PRIMARY KEY,
	password_hash TEXT NOT NULL,
	permissions   INTEGER NOT NULL DEFAULT 0,
	created_at    TIMESTAMP NOT NULL
)`

// WebUIUser is one dashboard user managed through the web UI's "Users"
// admin section, as returned by ListWebUIUsers/GetWebUIUser. It never
// carries the password hash — nothing outside VerifyWebUIUser/
// UpdateWebUIUserPassword needs it.
type WebUIUser struct {
	Username    string
	Permissions Permission
	CreatedAt   time.Time
}

// ErrWebUIUserExists is returned by CreateWebUIUser when username is
// already taken.
var ErrWebUIUserExists = errors.New("user already exists")

// ErrWebUIUserNotFound is returned by UpdateWebUIUserPassword/
// UpdateWebUIUserPermissions/DeleteWebUIUser when username doesn't name an
// existing dashboard user.
var ErrWebUIUserNotFound = errors.New("user not found")

// HashPassword bcrypt-hashes password for storage (see CreateWebUIUser/
// UpdateWebUIUserPassword). bcrypt.DefaultCost balances hashing time
// against resistance to offline brute-forcing if the state db ever leaks —
// the standard trade-off for a password an operator chooses, unlike e.g. a
// high-entropy generated token, which wouldn't need hashing's
// slow-by-design property at all.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}

	return string(hash), nil
}

// isUniqueConstraintErr reports whether err is a sqlite UNIQUE constraint
// violation — modernc.org/sqlite doesn't export a typed error for this
// worth importing just for CreateWebUIUser's one check, so this matches on
// the driver's own error text instead.
func isUniqueConstraintErr(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// CreateWebUIUser adds a new dashboard user granted perm, hashing password
// for storage (see HashPassword). Returns ErrWebUIUserExists if username is
// already taken.
func CreateWebUIUser(ctx context.Context, db *sql.DB, username, password string, perm Permission) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}

	const insert = `INSERT INTO webui_users (username, password_hash, permissions, created_at) VALUES (?, ?, ?, ?)`

	if _, err := db.ExecContext(ctx, insert, username, hash, int(perm), time.Now().UTC()); err != nil {
		if isUniqueConstraintErr(err) {
			return ErrWebUIUserExists
		}

		return fmt.Errorf("creating web UI user %q: %w", username, err)
	}

	return nil
}

// checkRowsAffected reports ErrWebUIUserNotFound if res (from an UPDATE/
// DELETE keyed by username) touched no rows, meaning username didn't exist.
func checkRowsAffected(res sql.Result, verb, username string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s web UI user %q: %w", verb, username, err)
	}

	if n == 0 {
		return ErrWebUIUserNotFound
	}

	return nil
}

// UpdateWebUIUserPassword changes username's stored password hash to
// password's, hashing it for storage (see HashPassword). Returns
// ErrWebUIUserNotFound if username doesn't exist.
func UpdateWebUIUserPassword(ctx context.Context, db *sql.DB, username, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}

	res, err := db.ExecContext(ctx, `UPDATE webui_users SET password_hash = ? WHERE username = ?`, hash, username)
	if err != nil {
		return fmt.Errorf("updating web UI user %q password: %w", username, err)
	}

	return checkRowsAffected(res, "updating", username)
}

// UpdateWebUIUserPermissions changes username's granted permissions to
// perm. Returns ErrWebUIUserNotFound if username doesn't exist.
func UpdateWebUIUserPermissions(ctx context.Context, db *sql.DB, username string, perm Permission) error {
	res, err := db.ExecContext(ctx, `UPDATE webui_users SET permissions = ? WHERE username = ?`, int(perm), username)
	if err != nil {
		return fmt.Errorf("updating web UI user %q permissions: %w", username, err)
	}

	return checkRowsAffected(res, "updating", username)
}

// DeleteWebUIUser removes username. Returns ErrWebUIUserNotFound if it
// doesn't exist.
func DeleteWebUIUser(ctx context.Context, db *sql.DB, username string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM webui_users WHERE username = ?`, username)
	if err != nil {
		return fmt.Errorf("deleting web UI user %q: %w", username, err)
	}

	return checkRowsAffected(res, "deleting", username)
}

// ListWebUIUsers returns every dashboard user, in username order, for the
// "Users" admin section's listing.
func ListWebUIUsers(ctx context.Context, db *sql.DB) ([]WebUIUser, error) {
	query := `SELECT username, permissions, created_at FROM webui_users ORDER BY username`

	return queryRows(ctx, db, "listing web UI users", query, nil, func(rows *sql.Rows) (WebUIUser, error) {
		var (
			u    WebUIUser
			perm int
		)

		if err := rows.Scan(&u.Username, &perm, &u.CreatedAt); err != nil {
			return WebUIUser{}, err
		}

		u.Permissions = Permission(perm)

		return u, nil
	})
}

// GetWebUIUser returns the named web UI "Users" admin-managed account (see
// CreateWebUIUser), reporting ok=false if username doesn't exist. Used by
// handleIssueWebUIUserToken (webui.go) to look up an account's currently
// granted permissions before minting it a long-lived API token.
func GetWebUIUser(ctx context.Context, db *sql.DB, username string) (WebUIUser, bool, error) {
	return queryRowOptional(ctx, db, fmt.Sprintf("looking up web UI user %q", username),
		`SELECT username, permissions, created_at FROM webui_users WHERE username = ?`, []any{username},
		func(sqlRow *sql.Row) (WebUIUser, error) {
			var (
				u    WebUIUser
				perm int
			)

			if err := sqlRow.Scan(&u.Username, &perm, &u.CreatedAt); err != nil {
				return WebUIUser{}, err
			}

			u.Permissions = Permission(perm)

			return u, nil
		})
}

// VerifyWebUIUser checks username/password against the stored dashboard
// user (see CreateWebUIUser), returning its granted permissions and
// ok=true only if username exists and password matches its stored hash.
// bcrypt.CompareHashAndPassword's own constant-time comparison (of the
// re-derived hash, not the raw password) is what keeps this safe from a
// timing attack — unlike the config-file admin's plain-string comparison in
// handleWebUILogin, this needs no separate subtle.ConstantTimeCompare.
func VerifyWebUIUser(ctx context.Context, db *sql.DB, username, password string) (Permission, bool, error) {
	type row struct {
		hash string
		perm int
	}

	r, ok, err := queryRowOptional(ctx, db, fmt.Sprintf("looking up web UI user %q", username),
		`SELECT password_hash, permissions FROM webui_users WHERE username = ?`, []any{username},
		func(sqlRow *sql.Row) (row, error) {
			var r row
			if err := sqlRow.Scan(&r.hash, &r.perm); err != nil {
				return row{}, err
			}

			return r, nil
		})
	if err != nil {
		return 0, false, err
	}

	if !ok || bcrypt.CompareHashAndPassword([]byte(r.hash), []byte(password)) != nil {
		// A missing user or a hash mismatch is a failed verification, not an
		// error the caller needs to handle separately.
		return 0, false, nil //nolint:nilerr // failed verification, not a caller-facing error
	}

	return Permission(r.perm), true, nil
}
