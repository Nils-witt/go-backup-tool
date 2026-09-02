// Package store is go-backup-tool's only home for database access: every
// query, statement, and schema definition against the shared
// state/retention sqlite database lives here, behind a *Store whose
// exported methods are Get*/List* lookups and Save*/Delete* writes (plus a
// handful of clearly-named one-off operations — VerifyUser, RevokeAPIToken,
// ExpiredObjectPaths — whose behavior a plain Get/Save name would obscure).
// Every other package reaches the database only through a *Store, never
// through a *gorm.DB of its own.
package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	glebarezsqlite "github.com/glebarez/sqlite"
)

// Store wraps the shared state/retention sqlite database go-backup-tool
// keeps alongside the config file: each start-time-anchored job's last
// successful run (so a restart can tell a genuinely missed run apart from an
// ordinary restart of a job that already ran on time), every file written to
// a local target or receiver with retention: set (so a later sweep knows
// what's eligible for automatic deletion), and the web UI's own users, API
// tokens, OIDC permission overrides, and audit logs.
type Store struct {
	db *gorm.DB
}

// stateDBName is the sqlite database file Open opens, a sibling of the
// config file (see StateDBPath).
const stateDBName = ".go-backup-tool-state.db"

// StateDBPath returns the state db path for the config file at configPath: a
// sibling file in the same directory.
func StateDBPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), stateDBName)
}

// Open opens (creating if needed) the state-tracking sqlite database at
// path, ensuring its schema exists. The caller must Close it.
//
// SetMaxOpenConns(1) serializes every access through a single connection:
// sqlite handles one writer at a time regardless, and the returned *Store is
// shared across every job's goroutine for the life of the run.
func Open(ctx context.Context, path string) (*Store, error) {
	// Every method in this package already wraps and returns query errors
	// (including the entirely expected "not found" case every Get*
	// lookup can hit) to its caller, so GORM's own query logging — which
	// would otherwise print every one of those as a Warn/Error-level log
	// line — is silenced rather than duplicating that with noise
	// database/sql never produced.
	gormConfig := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}

	gdb, err := gorm.Open(glebarezsqlite.Open(path), gormConfig)
	if err != nil {
		return nil, fmt.Errorf("opening job state db %q: %w", path, err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("opening job state db %q: %w", path, err)
	}

	sqlDB.SetMaxOpenConns(1)

	if err := gdb.WithContext(ctx).AutoMigrate(
		&jobRunModel{}, &targetRunModel{}, &outstandingTargetUploadModel{},
		&objectModel{},
		&loginEventModel{}, &downloadEventModel{}, &receiverEventModel{},
		&userModel{}, &apiTokenModel{},
	); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	if err := ensureUsersMigratedFromLegacyTables(ctx, gdb); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	return &Store{db: gdb}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}

// tableExists reports whether a table named name exists in the schema, as
// queried through db (either Store's own handle at runtime, or a
// migration's own transaction).
func tableExists(ctx context.Context, db *gorm.DB, name string) (bool, error) {
	return db.WithContext(ctx).Migrator().HasTable(name), nil
}

// isRecordNotFound reports whether err is GORM's "no row matched" sentinel,
// the equivalent of database/sql's sql.ErrNoRows — shared by every
// single-row optional lookup (First/Take) across this package.
func isRecordNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// ensureUsersMigratedFromLegacyTables copies every row still sitting in the
// pre-merge webui_users / oidc_user_permissions tables — present only in a
// state db created before those two were folded into the unified users
// table (see userModel) — into users, then drops both. Safe to call on
// every startup: once neither legacy table exists (the case on every run
// after the first upgraded one) it's a no-op. Runs inside a transaction
// since it copies rows across two tables and then drops them — the db must
// not end up half-migrated if the process is killed mid-way. Must be
// called after AutoMigrate has created users.
func ensureUsersMigratedFromLegacyTables(ctx context.Context, gdb *gorm.DB) error {
	return gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		webuiExists, err := tableExists(ctx, tx, "webui_users")
		if err != nil {
			return err
		}

		oidcExists, err := tableExists(ctx, tx, "oidc_user_permissions")
		if err != nil {
			return err
		}

		if !webuiExists && !oidcExists {
			return nil
		}

		// webui_users first, and into an otherwise-empty users table: no
		// collision is possible for it. oidc_user_permissions second, so its
		// own collision check (see migrateOIDCUserPermissions) sees every
		// webui_users-derived row already in place.
		if webuiExists {
			if err := migrateWebUIUsers(ctx, tx); err != nil {
				return err
			}
		}

		if oidcExists {
			if err := migrateOIDCUserPermissions(ctx, tx); err != nil {
				return err
			}
		}

		return nil
	})
}

// migrateWebUIUsers copies every row of the pre-merge webui_users table
// into users (oidc_username left NULL), then drops webui_users. Rows are
// read into memory before any INSERT is issued, rather than inserting
// while the SELECT's rows are still open, since this shares db's single
// connection (see Store.SetMaxOpenConns) with the transaction's writes.
func migrateWebUIUsers(ctx context.Context, tx *gorm.DB) error {
	type legacyUser struct {
		Username     string
		PasswordHash string
		Permissions  int
		CreatedAt    time.Time
	}

	var legacyUsers []legacyUser

	if err := tx.WithContext(ctx).Raw(`SELECT username, password_hash, permissions, created_at FROM webui_users`).Scan(&legacyUsers).Error; err != nil {
		return fmt.Errorf("reading webui_users for migration: %w", err)
	}

	for _, u := range legacyUsers {
		const insert = `INSERT INTO users (username, password_hash, oidc_username, permissions, created_at) VALUES (?, ?, NULL, ?, ?)`
		if err := tx.WithContext(ctx).Exec(insert, u.Username, u.PasswordHash, u.Permissions, u.CreatedAt).Error; err != nil {
			return fmt.Errorf("migrating webui_users row %q: %w", u.Username, err)
		}
	}

	if err := tx.WithContext(ctx).Exec(`DROP TABLE webui_users`).Error; err != nil {
		return fmt.Errorf("dropping webui_users: %w", err)
	}

	return nil
}

// migrateOIDCUserPermissions copies every row of the pre-merge
// oidc_user_permissions table into users as a passwordless row linked via
// oidc_username, then drops oidc_user_permissions. If an identity string
// happens to equal an existing users.username (from migrateWebUIUsers,
// which must run first), that unrelated row is never adopted — an OIDC
// login only ever matches by oidc_username, never by username (see
// GetOrProvisionOIDCUser) — so the new row's username is disambiguated
// instead (see uniqueUsernameFor) while oidc_username keeps the original
// identity string.
func migrateOIDCUserPermissions(ctx context.Context, tx *gorm.DB) error {
	type legacyOIDCUser struct {
		Identity    string
		Permissions int
		UpdatedAt   time.Time
	}

	var legacyUsers []legacyOIDCUser

	if err := tx.WithContext(ctx).Raw(`SELECT identity, permissions, updated_at FROM oidc_user_permissions`).Scan(&legacyUsers).Error; err != nil {
		return fmt.Errorf("reading oidc_user_permissions for migration: %w", err)
	}

	for _, u := range legacyUsers {
		username, err := uniqueUsernameFor(ctx, tx, u.Identity)
		if err != nil {
			return err
		}

		const insert = `INSERT INTO users (username, password_hash, oidc_username, permissions, created_at) VALUES (?, NULL, ?, ?, ?)`
		if err := tx.WithContext(ctx).Exec(insert, username, u.Identity, u.Permissions, u.UpdatedAt).Error; err != nil {
			return fmt.Errorf("migrating oidc_user_permissions row %q: %w", u.Identity, err)
		}
	}

	if err := tx.WithContext(ctx).Exec(`DROP TABLE oidc_user_permissions`).Error; err != nil {
		return fmt.Errorf("dropping oidc_user_permissions: %w", err)
	}

	return nil
}
