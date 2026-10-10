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
	"gorm.io/gorm/clause"
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
	//
	// Every write here is a single statement or already runs in an explicit
	// Transaction, so GORM's implicit per-write transaction is skipped.
	gormConfig := &gorm.Config{
		Logger:                 logger.Default.LogMode(logger.Silent),
		SkipDefaultTransaction: true,
		PrepareStmt:            true,
	}

	gdb, err := gorm.Open(glebarezsqlite.Open(withPragmas(path)), gormConfig)
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
		&receiverModel{}, &notificationModel{}, &reportSettingsModel{},
		&trustedServerModel{},
		&serverModel{}, &commandModel{}, &jobModel{},
	); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	if err := removeObsoleteTables(ctx, gdb); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	if err := migrateInlineJobCommands(ctx, gdb); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrating job state db %q: %w", path, err)
	}

	return &Store{db: gdb}, nil
}

// sqlitePragmas are applied to every connection Open makes: WAL so the
// dashboard's reads don't block behind a job's writes (and vice versa),
// a busy timeout instead of an immediate SQLITE_BUSY when another process
// (e.g. a one-off CLI run) holds the lock, and synchronous=NORMAL, which is
// durable under WAL short of an OS crash.
var sqlitePragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"synchronous(NORMAL)",
}

// withPragmas appends sqlitePragmas to path as glebarez/sqlite _pragma
// query parameters, preserving any query string path already carries.
func withPragmas(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}

	var b strings.Builder

	b.WriteString(path)

	for _, p := range sqlitePragmas {
		b.WriteString(sep)
		b.WriteString("_pragma=")
		b.WriteString(p)

		sep = "&"
	}

	return b.String()
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}

// insertMissing inserts every one of rows whose primary key isn't taken yet,
// in one transaction, leaving existing rows untouched, and returns the id
// (as id reports it) of each row it inserted. Shared by the Import* methods that carry
// the config file's deprecated sections over into the state db.
func insertMissing[M any](ctx context.Context, db *gorm.DB, rows []M, id func(M) string) ([]string, error) {
	var inserted []string

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, m := range rows {
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&m)
			if res.Error != nil {
				return res.Error
			}

			if res.RowsAffected > 0 {
				inserted = append(inserted, id(m))
			}
		}

		return nil
	})

	return inserted, err
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
