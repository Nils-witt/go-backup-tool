package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

// usersSchema is users: every dashboard account, whether managed through
// the web UI's "Users" admin section with a password, auto-provisioned by
// an SSO login (see GetOrProvisionOIDCUser), or both at once — a password
// account an admin has linked to an OIDC identity via
// SetUserOIDCUsername. Distinct from the single config-file admin
// (webui.username/password, resolved by the config package) that alone can
// manage rows here. password_hash is NULL for an OIDC-only row (nothing to
// verify a password login against). oidc_username is NULL/absent for a
// password-only row, and UNIQUE so at most one row can claim a given SSO
// identity (SQLite treats multiple NULLs in a UNIQUE column as
// non-conflicting, so any number of unlinked rows may coexist).
// permissions is a permission.Permission bitmask (see the permission
// package).
const usersSchema = `CREATE TABLE IF NOT EXISTS users (
	username      TEXT NOT NULL PRIMARY KEY,
	password_hash TEXT,
	oidc_username TEXT UNIQUE,
	permissions   INTEGER NOT NULL DEFAULT 0,
	created_at    TIMESTAMP NOT NULL
)`

// User is one users row, as returned by ListUsers/GetUser. It never carries
// the password hash — nothing outside VerifyUser/UpdateUserPassword needs
// it. OIDCUsername is "" if this account has no linked or auto-provisioned
// OIDC identity.
type User struct {
	Username     string
	OIDCUsername string
	Permissions  permission.Permission
	CreatedAt    time.Time
}

// ErrUserExists is returned by SaveUser when username is already taken.
var ErrUserExists = errors.New("user already exists")

// ErrUserNotFound is returned by UpdateUserPassword/UpdateUserPermissions/
// SetUserOIDCUsername/DeleteUser when username doesn't name an existing
// account.
var ErrUserNotFound = errors.New("user not found")

// ErrOIDCUsernameTaken is returned by SaveUser/SetUserOIDCUsername when the
// given OIDC identity is already linked to a different row.
var ErrOIDCUsernameTaken = errors.New("oidc identity is already linked to another user")

// HashPassword bcrypt-hashes password for storage (see SaveUser/
// UpdateUserPassword). bcrypt.DefaultCost balances hashing time against
// resistance to offline brute-forcing if the state db ever leaks — the
// standard trade-off for a password an operator chooses, unlike e.g. a
// high-entropy generated token, which wouldn't need hashing's
// slow-by-design property at all.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}

	return string(hash), nil
}

// isUniqueConstraintErrOn reports whether err is a sqlite UNIQUE constraint
// violation on column (e.g. "users.username" or "users.oidc_username") —
// modernc.org/sqlite doesn't export a typed error worth importing just for
// this, so this matches on the driver's own error text, which names the
// offending column, instead.
func isUniqueConstraintErrOn(err error, column string) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed: "+column)
}

// nullString returns s as a sql.NullString, valid only if s is non-empty —
// the wire format for an optional TEXT column (password_hash,
// oidc_username) that's absent rather than an empty string.
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// SaveUser adds a new account granted perm, hashing password for storage
// (see HashPassword) and linking it to oidcUsername if non-empty. Returns
// ErrUserExists if username is already taken, or ErrOIDCUsernameTaken if
// oidcUsername already names another row's link.
func (s *Store) SaveUser(ctx context.Context, username, password, oidcUsername string, perm permission.Permission) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}

	const insert = `INSERT INTO users (username, password_hash, oidc_username, permissions, created_at) VALUES (?, ?, ?, ?, ?)`

	if _, err := s.db.ExecContext(ctx, insert, username, hash, nullString(oidcUsername), int(perm), time.Now().UTC()); err != nil {
		switch {
		case isUniqueConstraintErrOn(err, "users.username"):
			return ErrUserExists
		case isUniqueConstraintErrOn(err, "users.oidc_username"):
			return ErrOIDCUsernameTaken
		default:
			return fmt.Errorf("creating user %q: %w", username, err)
		}
	}

	return nil
}

// checkRowsAffected reports ErrUserNotFound if res (from an UPDATE/DELETE
// keyed by username) touched no rows, meaning username didn't exist.
func checkRowsAffected(res sql.Result, verb, username string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s user %q: %w", verb, username, err)
	}

	if n == 0 {
		return ErrUserNotFound
	}

	return nil
}

// UpdateUserPassword changes username's stored password hash to password's,
// hashing it for storage (see HashPassword). Returns ErrUserNotFound if
// username doesn't exist.
func (s *Store) UpdateUserPassword(ctx context.Context, username, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE username = ?`, hash, username)
	if err != nil {
		return fmt.Errorf("updating user %q password: %w", username, err)
	}

	return checkRowsAffected(res, "updating", username)
}

// UpdateUserPermissions changes username's granted permissions to perm.
// Returns ErrUserNotFound if username doesn't exist.
func (s *Store) UpdateUserPermissions(ctx context.Context, username string, perm permission.Permission) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET permissions = ? WHERE username = ?`, int(perm), username)
	if err != nil {
		return fmt.Errorf("updating user %q permissions: %w", username, err)
	}

	return checkRowsAffected(res, "updating", username)
}

// SetUserOIDCUsername links username's row to oidcUsername, or clears any
// existing link when oidcUsername is "" — a deliberate admin action (see
// the "Users" admin section), never inferred from a login: an SSO login
// only ever matches an existing row by this column (see
// GetOrProvisionOIDCUser), never by comparing the identity string to an
// unrelated row's username. Returns ErrUserNotFound if username doesn't
// exist, or ErrOIDCUsernameTaken if oidcUsername already links a different
// row.
func (s *Store) SetUserOIDCUsername(ctx context.Context, username, oidcUsername string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET oidc_username = ? WHERE username = ?`, nullString(oidcUsername), username)
	if err != nil {
		if isUniqueConstraintErrOn(err, "users.oidc_username") {
			return ErrOIDCUsernameTaken
		}

		return fmt.Errorf("linking user %q to oidc identity %q: %w", username, oidcUsername, err)
	}

	return checkRowsAffected(res, "linking", username)
}

// DeleteUser removes username. Returns ErrUserNotFound if it doesn't exist.
func (s *Store) DeleteUser(ctx context.Context, username string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return fmt.Errorf("deleting user %q: %w", username, err)
	}

	return checkRowsAffected(res, "deleting", username)
}

// scanUser scans a users row (username, oidc_username, permissions,
// created_at — in that column order) into a User, shared by ListUsers'
// and GetUser's scan functions.
func scanUser(scan func(...any) error) (User, error) {
	var (
		u            User
		oidcUsername sql.NullString
		perm         int
	)

	if err := scan(&u.Username, &oidcUsername, &perm, &u.CreatedAt); err != nil {
		return User{}, err
	}

	u.OIDCUsername = oidcUsername.String
	u.Permissions = permission.Permission(perm)

	return u, nil
}

// ListUsers returns every account, in username order, for the "Users"
// admin section's listing.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	query := `SELECT username, oidc_username, permissions, created_at FROM users ORDER BY username`

	return queryRows(ctx, s.db, "listing users", query, nil, func(rows *sql.Rows) (User, error) {
		return scanUser(rows.Scan)
	})
}

// GetUser returns the named account, reporting ok=false if username doesn't
// exist.
func (s *Store) GetUser(ctx context.Context, username string) (User, bool, error) {
	return queryRowOptional(ctx, s.db, fmt.Sprintf("looking up user %q", username),
		`SELECT username, oidc_username, permissions, created_at FROM users WHERE username = ?`, []any{username},
		func(sqlRow *sql.Row) (User, error) {
			return scanUser(sqlRow.Scan)
		})
}

// VerifyUser checks username/password against the stored account (see
// SaveUser), returning its granted permissions and ok=true only if
// username exists, has a password set, and password matches its stored
// hash. An OIDC-only row (NULL password_hash) always fails verification
// rather than being compared against. bcrypt.CompareHashAndPassword's own
// constant-time comparison (of the re-derived hash, not the raw password)
// is what keeps a real comparison safe from a timing attack.
func (s *Store) VerifyUser(ctx context.Context, username, password string) (permission.Permission, bool, error) {
	type row struct {
		hash sql.NullString
		perm int
	}

	r, ok, err := queryRowOptional(ctx, s.db, fmt.Sprintf("looking up user %q", username),
		`SELECT password_hash, permissions FROM users WHERE username = ?`, []any{username},
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

	if !ok || !r.hash.Valid || bcrypt.CompareHashAndPassword([]byte(r.hash.String), []byte(password)) != nil {
		// A missing user, a passwordless (OIDC-only) row, or a hash mismatch
		// is a failed verification, not an error the caller needs to handle
		// separately.
		return 0, false, nil //nolint:nilerr // failed verification, not a caller-facing error
	}

	return permission.Permission(r.perm), true, nil
}

// uniqueUsernameFor returns candidate if no row already has it as its
// username, otherwise a disambiguated variant (candidate + "-oidc", then
// "-oidc-2", "-oidc-3", ...). Used only for the pathological case where a
// brand new OIDC identity's string collides with an unrelated row's
// existing username — GetOrProvisionOIDCUser and the legacy-table migration
// must never adopt that row (an SSO login only ever matches by
// oidc_username), so the new row instead gets a disambiguated username
// while still recording the original identity as its oidc_username.
func uniqueUsernameFor(ctx context.Context, q queryRower, candidate string) (string, error) {
	name := candidate

	for i := 2; ; i++ {
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE username = ?)`, name).Scan(&exists); err != nil {
			return "", fmt.Errorf("checking username %q availability: %w", name, err)
		}

		if !exists {
			return name, nil
		}

		name = candidate + "-oidc"
		if i > 2 {
			name = fmt.Sprintf("%s-oidc-%d", candidate, i)
		}
	}
}

// GetOrProvisionOIDCUser looks up the row linked to oidcUsername and
// returns its stored permissions. If none exists yet — this identity's
// very first login — it provisions one: username set to oidcUsername
// itself (disambiguated via uniqueUsernameFor if that string collides with
// an unrelated existing username), password_hash left NULL,
// oidc_username set to oidcUsername, permissions set to defaultPerm — and
// returns defaultPerm. This is the sole lookup an OIDC login uses, and the
// mechanism that guarantees it never adopts a row matched by username
// alone: only an oidc_username match counts as "found," so a pre-existing
// password account whose username happens to equal oidcUsername is never
// touched (see SetUserOIDCUsername for the deliberate, admin-only way to
// link one).
//
// The lookup-then-insert here isn't wrapped in its own transaction: it
// relies on Store's existing single-connection guarantee
// (db.SetMaxOpenConns(1), see Store) to serialize it against any concurrent
// write, the same guarantee SaveOIDCUserPermissions's upsert used to rely
// on before this method replaced it.
func (s *Store) GetOrProvisionOIDCUser(ctx context.Context, oidcUsername string, defaultPerm permission.Permission) (permission.Permission, error) {
	perm, ok, err := queryRowOptional(ctx, s.db, fmt.Sprintf("looking up oidc user %q", oidcUsername),
		`SELECT permissions FROM users WHERE oidc_username = ?`, []any{oidcUsername},
		func(row *sql.Row) (int, error) {
			var perm int
			if err := row.Scan(&perm); err != nil {
				return 0, err
			}

			return perm, nil
		})
	if err != nil {
		return 0, err
	}

	if ok {
		return permission.Permission(perm), nil
	}

	username, err := uniqueUsernameFor(ctx, s.db, oidcUsername)
	if err != nil {
		return 0, err
	}

	const insert = `INSERT INTO users (username, password_hash, oidc_username, permissions, created_at) VALUES (?, NULL, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, insert, username, oidcUsername, int(defaultPerm), time.Now().UTC()); err != nil {
		return 0, fmt.Errorf("provisioning oidc user %q: %w", oidcUsername, err)
	}

	return defaultPerm, nil
}
