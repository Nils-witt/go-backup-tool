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
	"strings"
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
// what's eligible for automatic deletion), and the web UI's audit logs.
// Nothing about web UI users is stored: every login is SSO and every
// permission comes from the access token's groups (see
// internal/backup/webui/auth.go). The only web UI credentials kept are the
// read-only API tokens' metadata and signing key (see tokens.go), plus, for
// a day after each OIDC back-channel logout, the subject and session it
// ended (see sso_logouts.go).
//
// It holds two connection pools on the same file: db, a single connection
// every write (and every read inside a write's transaction) goes through,
// since sqlite allows one writer at a time anyway; and read, a few
// query_only connections the pure Get*/List* lookups use, so under WAL the
// dashboard's reads run alongside a job's writes instead of queuing behind
// them for the one connection.
type Store struct {
	db   *gorm.DB
	read *gorm.DB
}

// readConns is how many connections Store.read pools. The dashboard's
// concurrent reads are few (a handful of open pages and API tokens), so a
// small pool is plenty.
const readConns = 4

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
// Writes go through a single connection (SetMaxOpenConns(1)): sqlite
// handles one writer at a time regardless, and the returned *Store is
// shared across every job's goroutine for the life of the run. Pure reads
// use a separate pool of readConns query_only connections (see Store).
func Open(ctx context.Context, path string) (*Store, error) {
	// Every method in this package already wraps and returns query errors
	// (including the entirely expected "not found" case every Get*
	// lookup can hit) to its caller, so GORM's own query logging — which
	// would otherwise print every one of those as a Warn/Error-level log
	// line — is silenced rather than duplicating that with noise
	// database/sql never produced.
	//
	// Every write here is a single statement or already runs in an explicit
	// Transaction, so GORM's implicit per-write transaction is skipped.
	gormConfig := &gorm.Config{
		Logger:                 logger.Default.LogMode(logger.Silent),
		SkipDefaultTransaction: true,
		PrepareStmt:            true,
	}

	gdb, err := gorm.Open(glebarezsqlite.Open(withPragmas(path, sqlitePragmas)), gormConfig)
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
		&loginEventModel{}, &downloadEventModel{}, &receiverEventModel{}, &auditEventModel{},
		&apiTokenModel{}, &tokenSigningKeyModel{},
		&receiverModel{}, &notificationModel{}, &reportSettingsModel{},
		&trustedServerModel{}, &ssoLogoutModel{},
		&serverModel{}, &commandModel{}, &jobModel{},
	); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	if err := removeObsoleteTables(ctx, gdb); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	if err := removeObsoleteIndexes(ctx, gdb); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	if err := migrateInlineJobCommands(ctx, gdb); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	if err := migrateSingleJobCommands(ctx, gdb); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	// Opened only once the schema exists: the read pool can't create it.
	read, err := openReadPool(path, gormConfig)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("opening job state db %q for reading: %w", path, err)
	}

	return &Store{db: gdb, read: read}, nil
}

// openReadPool opens Store.read: readConns connections to path, each
// query_only, so a write misrouted to it fails instead of bypassing the
// single writer connection.
func openReadPool(path string, gormConfig *gorm.Config) (*gorm.DB, error) {
	read, err := gorm.Open(glebarezsqlite.Open(withPragmas(path, readPragmas)), gormConfig)
	if err != nil {
		return nil, err
	}

	sqlDB, err := read.DB()
	if err != nil {
		return nil, err
	}

	sqlDB.SetMaxOpenConns(readConns)
	sqlDB.SetMaxIdleConns(readConns)

	return read, nil
}

// sqlitePragmas are applied to Store.db's connection: WAL so the
// dashboard's reads don't block behind a job's writes (and vice versa),
// a busy timeout instead of an immediate SQLITE_BUSY when another process
// (e.g. a one-off CLI run) holds the lock, and synchronous=NORMAL, which is
// durable under WAL short of an OS crash.
var sqlitePragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"synchronous(NORMAL)",
}

// readPragmas are applied to every connection of Store.read: the same busy
// timeout as sqlitePragmas, and query_only so it can never write. The
// database is already in WAL mode by the time it's opened (journal_mode is
// persistent), which is what lets these readers run alongside the writer.
var readPragmas = []string{
	"busy_timeout(5000)",
	"query_only(1)",
}

// withPragmas appends pragmas to path as glebarez/sqlite _pragma query
// parameters, preserving any query string path already carries.
func withPragmas(path string, pragmas []string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}

	var b strings.Builder

	b.WriteString(path)

	for _, p := range pragmas {
		b.WriteString(sep)
		b.WriteString("_pragma=")
		b.WriteString(p)

		sep = "&"
	}

	return b.String()
}

// Close closes both underlying connection pools.
func (s *Store) Close() error {
	var errs []error

	for _, gdb := range []*gorm.DB{s.read, s.db} {
		sqlDB, err := gdb.DB()
		if err != nil {
			errs = append(errs, err)
			continue
		}

		if err := sqlDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
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
// migrateInlineJobCommands moves every stored job's inline shell command
// (the cmd column, from before jobs named a command) into a commands row of
// its own — id "job-<name>", suffixed "-2", "-3", ... if taken — and points
// the job's command at it, all in one transaction. Once done, no job row
// has cmd set, so running it again changes nothing.
func migrateInlineJobCommands(ctx context.Context, gdb *gorm.DB) error {
	return gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var jobs []jobModel
		if err := tx.Where("cmd <> '' AND command = ''").Order("name").Find(&jobs).Error; err != nil {
			return fmt.Errorf("reading jobs with an inline cmd: %w", err)
		}

		if len(jobs) == 0 {
			return nil
		}

		var ids []string
		if err := tx.Model(&commandModel{}).Pluck("id", &ids).Error; err != nil {
			return fmt.Errorf("reading command ids: %w", err)
		}

		taken := make(map[string]bool, len(ids))
		for _, id := range ids {
			taken[id] = true
		}

		now := time.Now().UTC()

		for _, j := range jobs {
			id := "job-" + j.Name
			for n := 2; taken[id]; n++ {
				id = fmt.Sprintf("job-%s-%d", j.Name, n)
			}

			taken[id] = true

			c := commandModel{
				ID: id, Cmd: j.LegacyCmd,
				CreatedAt: j.CreatedAt, CreatedBy: j.CreatedBy, UpdatedAt: now, UpdatedBy: migrationUser,
			}
			if err := tx.Create(&c).Error; err != nil {
				return fmt.Errorf("creating command %q for job %q: %w", id, j.Name, err)
			}

			if err := tx.Model(&jobModel{}).Where("name = ?", j.Name).
				Updates(map[string]any{"command": id, "cmd": ""}).Error; err != nil {
				return fmt.Errorf("pointing job %q at command %q: %w", j.Name, id, err)
			}
		}

		return nil
	})
}

// migrateSingleJobCommands moves every stored job's single command (the
// command column, from before a job listed its files) into its files, as
// the only entry, in one transaction. Runs after migrateInlineJobCommands,
// which points a job's command at the command it creates. Once done, no job
// row has command set, so running it again changes nothing.
func migrateSingleJobCommands(ctx context.Context, gdb *gorm.DB) error {
	return gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var jobs []jobModel
		if err := tx.Where("command <> ''").Order("name").Find(&jobs).Error; err != nil {
			return fmt.Errorf("reading jobs with a single command: %w", err)
		}

		for _, j := range jobs {
			files := j.Files
			if len(files) == 0 {
				files = []JobFile{{Command: j.Command}}
			}

			if err := tx.Model(&jobModel{}).Where("name = ?", j.Name).
				Select("files", "command").Updates(&jobModel{Files: files}).Error; err != nil {
				return fmt.Errorf("moving job %q's command into its files: %w", j.Name, err)
			}
		}

		return nil
	})
}

// migrationUser is the updated_by recorded on rows a migration writes.
const migrationUser = "migration"

func removeObsoleteTables(ctx context.Context, gdb *gorm.DB) error {
	for _, table := range obsoleteTables {
		if err := gdb.WithContext(ctx).Migrator().DropTable(table); err != nil {
			return fmt.Errorf("dropping obsolete table %s: %w", table, err)
		}
	}

	return nil
}

// obsoleteIndexes are indexes a model no longer declares, which AutoMigrate
// never drops on its own, keyed by the model whose table holds them.
var obsoleteIndexes = []struct {
	model any
	name  string
}{
	// Superseded by idx_objects_expiry, whose leading column is the same.
	{&objectModel{}, "idx_objects_server"},
}

// removeObsoleteIndexes drops every obsoleteIndexes entry still present.
// Safe to call on every startup: once they're gone it's a no-op.
func removeObsoleteIndexes(ctx context.Context, gdb *gorm.DB) error {
	m := gdb.WithContext(ctx).Migrator()

	for _, idx := range obsoleteIndexes {
		if !m.HasIndex(idx.model, idx.name) {
			continue
		}

		if err := m.DropIndex(idx.model, idx.name); err != nil {
			return fmt.Errorf("dropping obsolete index %s: %w", idx.name, err)
		}
	}

	return nil
}
