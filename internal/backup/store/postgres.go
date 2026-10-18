package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// postgresConns is how many connections a PostgreSQL store pools, shared by
// reads and writes. A run's writes and the dashboard's reads are few at a
// time, so a small pool is plenty.
const postgresConns = 10

// postgresConnectTimeout bounds OpenPostgres's first connection, so an
// unreachable server fails startup instead of hanging it.
const postgresConnectTimeout = 30 * time.Second

// OpenPostgres connects to the PostgreSQL database dsn names (a
// postgres:// URL or a libpq key=value string), ensuring its schema exists.
// It fails within postgresConnectTimeout if the server can't be reached.
// The caller must Close it.
//
// Errors never include dsn, which may carry a password.
func OpenPostgres(ctx context.Context, dsn string) (Store, error) {
	s, err := openPostgres(ctx, dsn)
	if err != nil {
		// A literal nil, never a nil *gormStore: callers compare the
		// Store against nil to tell an unavailable db apart.
		return nil, err
	}

	return s, nil
}

// openPostgres is OpenPostgres returning the concrete store, for this
// package's own tests.
func openPostgres(ctx context.Context, dsn string) (*gormStore, error) {
	// The automatic ping gorm.Open does has no deadline; the ping below
	// replaces it with one bounded by ctx and postgresConnectTimeout.
	gormConfig := newGormConfig()
	gormConfig.DisableAutomaticPing = true

	gdb, err := gorm.Open(postgres.Open(dsn), gormConfig)
	if err != nil {
		return nil, fmt.Errorf("opening postgres state db: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("opening postgres state db: %w", err)
	}

	sqlDB.SetMaxOpenConns(postgresConns)
	sqlDB.SetMaxIdleConns(postgresConns)

	pingCtx, cancel := context.WithTimeout(ctx, postgresConnectTimeout)
	defer cancel()

	if err := sqlDB.PingContext(pingCtx); err != nil {
		closeGormDB(gdb)
		return nil, fmt.Errorf("connecting to postgres state db: %w", err)
	}

	if err := migrate(ctx, gdb); err != nil {
		closeGormDB(gdb)
		return nil, fmt.Errorf("initializing postgres state db: %w", err)
	}

	return &gormStore{db: gdb, read: gdb}, nil
}
