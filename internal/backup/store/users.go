package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"golang.org/x/crypto/bcrypt"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

// userModel is users: every dashboard account, whether managed through the
// web UI's "Users" admin section with a password, auto-provisioned by an
// SSO login (see GetOrProvisionOIDCUser), or both at once — a password
// account an admin has linked to an OIDC identity via SetUserOIDCUsername.
// Distinct from the single config-file admin (webui.username/password,
// resolved by the config package) that alone can manage rows here.
// PasswordHash is NULL for an OIDC-only row (nothing to verify a password
// login against). OIDCUsername is NULL/absent for a password-only row, and
// UNIQUE so at most one row can claim a given SSO identity (SQLite treats
// multiple NULLs in a UNIQUE column as non-conflicting, so any number of
// unlinked rows may coexist). Permissions is a permission.Permission
// bitmask (see the permission package).
type userModel struct {
	Username     string         `gorm:"column:username;primaryKey"`
	PasswordHash sql.NullString `gorm:"column:password_hash"`
	OIDCUsername sql.NullString `gorm:"column:oidc_username;uniqueIndex"`
	Permissions  int            `gorm:"column:permissions;not null;default:0"`
	CreatedAt    time.Time      `gorm:"column:created_at;not null"`
}

func (userModel) TableName() string { return "users" }

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
// the pure-Go sqlite driver GORM sits on doesn't export a typed error worth
// importing just for this, so this matches on the driver's own error text,
// which names the offending column, instead.
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

	m := userModel{
		Username:     username,
		PasswordHash: nullString(hash),
		OIDCUsername: nullString(oidcUsername),
		Permissions:  int(perm),
		CreatedAt:    time.Now().UTC(),
	}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
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

// checkRowsAffected reports ErrUserNotFound if result (from an UPDATE/
// DELETE keyed by username) touched no rows, meaning username didn't exist.
func checkRowsAffected(result *gorm.DB, verb, username string) error {
	if result.Error != nil {
		return fmt.Errorf("%s user %q: %w", verb, username, result.Error)
	}

	if result.RowsAffected == 0 {
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

	result := s.db.WithContext(ctx).Model(&userModel{}).Where("username = ?", username).Update("password_hash", hash)

	return checkRowsAffected(result, "updating", username)
}

// UpdateUserPermissions changes username's granted permissions to perm.
// Returns ErrUserNotFound if username doesn't exist.
func (s *Store) UpdateUserPermissions(ctx context.Context, username string, perm permission.Permission) error {
	result := s.db.WithContext(ctx).Model(&userModel{}).Where("username = ?", username).Update("permissions", int(perm))

	return checkRowsAffected(result, "updating", username)
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
	result := s.db.WithContext(ctx).Model(&userModel{}).Where("username = ?", username).Update("oidc_username", nullString(oidcUsername))
	if result.Error != nil {
		if isUniqueConstraintErrOn(result.Error, "users.oidc_username") {
			return ErrOIDCUsernameTaken
		}

		return fmt.Errorf("linking user %q to oidc identity %q: %w", username, oidcUsername, result.Error)
	}

	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}

	return nil
}

// DeleteUser removes username. Returns ErrUserNotFound if it doesn't exist.
func (s *Store) DeleteUser(ctx context.Context, username string) error {
	result := s.db.WithContext(ctx).Where("username = ?", username).Delete(&userModel{})
	if result.Error != nil {
		return fmt.Errorf("deleting user %q: %w", username, result.Error)
	}

	return checkRowsAffected(result, "deleting", username)
}

// toUser converts a userModel row into the exported User shape.
func toUser(m userModel) User {
	return User{
		Username:     m.Username,
		OIDCUsername: m.OIDCUsername.String,
		Permissions:  permission.Permission(m.Permissions),
		CreatedAt:    m.CreatedAt,
	}
}

// ListUsers returns every account, in username order, for the "Users"
// admin section's listing.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	var rows []userModel

	if err := s.db.WithContext(ctx).Order("username").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}

	users := make([]User, len(rows))
	for i, m := range rows {
		users[i] = toUser(m)
	}

	return users, nil
}

// GetUser returns the named account, reporting ok=false if username doesn't
// exist.
func (s *Store) GetUser(ctx context.Context, username string) (User, bool, error) {
	var m userModel

	err := s.db.WithContext(ctx).Where("username = ?", username).Take(&m).Error

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return User{}, false, nil
	case err != nil:
		return User{}, false, fmt.Errorf("looking up user %q: %w", username, err)
	default:
		return toUser(m), true, nil
	}
}

// VerifyUser checks username/password against the stored account (see
// SaveUser), returning its granted permissions and ok=true only if
// username exists, has a password set, and password matches its stored
// hash. An OIDC-only row (NULL password_hash) always fails verification
// rather than being compared against. bcrypt.CompareHashAndPassword's own
// constant-time comparison (of the re-derived hash, not the raw password)
// is what keeps a real comparison safe from a timing attack.
func (s *Store) VerifyUser(ctx context.Context, username, password string) (permission.Permission, bool, error) {
	var m userModel

	err := s.db.WithContext(ctx).Select("password_hash", "permissions").Where("username = ?", username).Take(&m).Error

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("looking up user %q: %w", username, err)
	}

	if !m.PasswordHash.Valid || bcrypt.CompareHashAndPassword([]byte(m.PasswordHash.String), []byte(password)) != nil {
		// A passwordless (OIDC-only) row or a hash mismatch is a failed
		// verification, not an error the caller needs to handle separately.
		return 0, false, nil //nolint:nilerr // failed verification, not a caller-facing error
	}

	return permission.Permission(m.Permissions), true, nil
}

// uniqueUsernameFor returns candidate if no row already has it as its
// username, otherwise a disambiguated variant (candidate + "-oidc", then
// "-oidc-2", "-oidc-3", ...). Used only for the pathological case where a
// brand new OIDC identity's string collides with an unrelated row's
// existing username — GetOrProvisionOIDCUser and the legacy-table migration
// must never adopt that row (an SSO login only ever matches by
// oidc_username), so the new row instead gets a disambiguated username
// while still recording the original identity as its oidc_username.
func uniqueUsernameFor(ctx context.Context, db *gorm.DB, candidate string) (string, error) {
	name := candidate

	for i := 2; ; i++ {
		var exists bool
		if err := db.WithContext(ctx).Raw(`SELECT EXISTS(SELECT 1 FROM users WHERE username = ?)`, name).Scan(&exists).Error; err != nil {
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
// (db.SetMaxOpenConns(1), see Open) to serialize it against any concurrent
// write, the same guarantee SaveOIDCUserPermissions's upsert used to rely
// on before this method replaced it.
func (s *Store) GetOrProvisionOIDCUser(ctx context.Context, oidcUsername string, defaultPerm permission.Permission) (permission.Permission, error) {
	var m userModel

	err := s.db.WithContext(ctx).Select("permissions").Where("oidc_username = ?", oidcUsername).Take(&m).Error

	switch {
	case err == nil:
		return permission.Permission(m.Permissions), nil
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return 0, fmt.Errorf("looking up oidc user %q: %w", oidcUsername, err)
	}

	username, err := uniqueUsernameFor(ctx, s.db, oidcUsername)
	if err != nil {
		return 0, err
	}

	provisioned := userModel{
		Username:     username,
		OIDCUsername: nullString(oidcUsername),
		Permissions:  int(defaultPerm),
		CreatedAt:    time.Now().UTC(),
	}

	if err := s.db.WithContext(ctx).Create(&provisioned).Error; err != nil {
		return 0, fmt.Errorf("provisioning oidc user %q: %w", oidcUsername, err)
	}

	return defaultPerm, nil
}
