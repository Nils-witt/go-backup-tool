package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"gorm.io/gorm"

	glebarezsqlite "github.com/glebarez/sqlite"
)

// readConns is how many connections a SQLite store's read pool holds. The
// dashboard's concurrent reads are few (a handful of open pages and API
// tokens), so a small pool is plenty.
const readConns = 4

// stateDBName is the SQLite database file go-backup-tool keeps, a sibling
// of the config file (see StateDBPath).
const stateDBName = ".go-backup-tool-state.db"

// StateDBPath returns the SQLite state db path for the config file at
// configPath: a sibling file in the same directory.
func StateDBPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), stateDBName)
}

// OpenSQLite opens (creating if needed) the SQLite state database at path,
// ensuring its schema exists. The caller must Close it.
func OpenSQLite(ctx context.Context, path string) (Store, error) {
	s, err := openSQLite(ctx, path)
	if err != nil {
		// A literal nil, never a nil *gormStore: callers compare the
		// Store against nil to tell an unavailable db apart.
		return nil, err
	}

	return s, nil
}

// openSQLite is OpenSQLite returning the concrete store, for this package's
// own tests.
//
// It holds two connection pools on the same file: db, a single connection
// every write (and every read inside a write's transaction) goes through,
// since SQLite allows one writer at a time anyway; and read, a few
// query_only connections the pure Get*/List* lookups use, so under WAL the
// dashboard's reads run alongside a job's writes instead of queuing behind
// them for the one connection.
func openSQLite(ctx context.Context, path string) (*gormStore, error) {
	gormConfig := newGormConfig()

	gdb, err := gorm.Open(glebarezsqlite.Open(withPragmas(path, sqlitePragmas)), gormConfig)
	if err != nil {
		return nil, fmt.Errorf("opening job state db %q: %w", path, err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("opening job state db %q: %w", path, err)
	}

	sqlDB.SetMaxOpenConns(1)

	if err := migrate(ctx, gdb); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initializing job state db %q: %w", path, err)
	}

	// Opened only once the schema exists: the read pool can't create it.
	read, err := openReadPool(path, gormConfig)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("opening job state db %q for reading: %w", path, err)
	}

	return &gormStore{db: gdb, read: read}, nil
}

// openReadPool opens a SQLite store's read pool: readConns connections to
// path, each query_only, so a write misrouted to it fails instead of
// bypassing the single writer connection.
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

// sqlitePragmas are applied to the writer connection: WAL so the
// dashboard's reads don't block behind a job's writes (and vice versa),
// a busy timeout instead of an immediate SQLITE_BUSY when another process
// (e.g. a one-off CLI run) holds the lock, and synchronous=NORMAL, which is
// durable under WAL short of an OS crash.
var sqlitePragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"synchronous(NORMAL)",
}

// readPragmas are applied to every connection of the read pool: the same
// busy timeout as sqlitePragmas, and query_only so it can never write. The
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
