// Package store is go-backup-tool's only home for database access: every
// query, statement, and schema definition against the shared
// state/retention sqlite database lives here, behind a *Store whose
// exported methods are Get*/List* lookups and Save*/Delete* writes (plus a
// handful of clearly-named one-off operations — ExpiredObjectPaths — whose
// behavior a plain Get/Save name would obscure).
// Every other package reaches the database only through a *Store, never
// through a *gorm.DB of its own.
package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	glebarezsqlite "github.com/glebarez/sqlite"
)

// Store wraps the shared state/retention sqlite database go-backup-tool
// keeps alongside the config file: each start-time-anchored job's last
// successful run (so a restart can tell a genuinely missed run apart from an
// ordinary restart of a job that already ran on time), every file written to
// a local target or receiver with retention: set (so a later sweep knows
// what's eligible for automatic deletion), and the web UI's audit logs.
// Nothing about web UI users is stored: every login is SSO and every
// permission comes from the access token's groups (see
// internal/backup/webui/auth.go). The only web UI credentials kept are the
// read-only API tokens' metadata and signing key (see tokens.go).
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
		&apiTokenModel{}, &tokenSigningKeyModel{},
	); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	if err := removeObsoleteTables(ctx, gdb); err != nil {
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

// isRecordNotFound reports whether err is GORM's "no row matched" sentinel,
// the equivalent of database/sql's sql.ErrNoRows — shared by every
// single-row optional lookup (First/Take) across this package.
func isRecordNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// obsoleteTables are the web UI's former local-account tables — users,
// groups, API tokens, and their pre-merge predecessors — left behind in a
// state db created before the web UI became SSO-only.
var obsoleteTables = []string{
	"user_groups", "groups", "webui_tokens", "users",
	"webui_users", "oidc_user_permissions",
}

// removeObsoleteTables drops every obsoleteTables entry still present. Safe
// to call on every startup: once they're gone it's a no-op.
func removeObsoleteTables(ctx context.Context, gdb *gorm.DB) error {
	for _, table := range obsoleteTables {
		if err := gdb.WithContext(ctx).Migrator().DropTable(table); err != nil {
			return fmt.Errorf("dropping obsolete table %s: %w", table, err)
		}
	}

	return nil
}
