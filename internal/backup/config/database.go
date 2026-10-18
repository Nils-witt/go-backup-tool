package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// DatabaseType is which database backend holds the state db (see
// DatabaseConfig).
type DatabaseType string

const (
	// DatabaseSQLite is the default backend: a SQLite file next to the
	// config file (see store.StateDBPath).
	DatabaseSQLite DatabaseType = "sqlite"
	// DatabasePostgres is a PostgreSQL server, reached through
	// DatabaseConfig.PostgresDSN.
	DatabasePostgres DatabaseType = "postgres"
)

// defaultPostgresPort and defaultPostgresSSLMode are what database.port and
// database.sslmode resolve to when unset.
const (
	defaultPostgresPort    = 5432
	defaultPostgresSSLMode = "require"
)

// postgresSSLModes are the sslmode values libpq (and pgx) accept.
var postgresSSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// fileDatabase is the config file's database: entry, choosing where the
// state db lives. Type is sqlite (the default, when unset) or postgres. A
// postgres database is given either as DSN — a postgres:// URL or libpq
// key=value string, passed through as written — or as the discrete
// host/port/user/password/dbname/sslmode fields, never both. A sqlite
// database takes none of the other fields.
type fileDatabase struct {
	Type     string `yaml:"type"`
	DSN      string `yaml:"dsn"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
}

// DatabaseConfig is the resolved database: entry.
type DatabaseConfig struct {
	Type DatabaseType

	// PostgresDSN is the connection string for DatabasePostgres: the
	// config file's dsn: as written, or a postgres:// URL built from its
	// discrete fields. Empty for DatabaseSQLite. It may carry a password,
	// so it must never be logged.
	PostgresDSN string
}

// hasDiscreteFields reports whether any of the discrete postgres connection
// fields is set.
func (d fileDatabase) hasDiscreteFields() bool {
	return d.Host != "" || d.Port != 0 || d.User != "" || d.Password != "" || d.DBName != "" || d.SSLMode != ""
}

// resolveDatabase validates cfg (the config file's database: entry) and
// resolves it into a DatabaseConfig.
func resolveDatabase(cfg fileDatabase) (DatabaseConfig, error) {
	switch DatabaseType(strings.TrimSpace(cfg.Type)) {
	case "", DatabaseSQLite:
		if cfg.DSN != "" || cfg.hasDiscreteFields() {
			return DatabaseConfig{}, errors.New("database: type sqlite takes no other settings (dsn, host, port, user, password, dbname, sslmode are for type postgres)")
		}

		return DatabaseConfig{Type: DatabaseSQLite}, nil
	case DatabasePostgres:
		dsn, err := resolvePostgresDSN(cfg)
		if err != nil {
			return DatabaseConfig{}, err
		}

		return DatabaseConfig{Type: DatabasePostgres, PostgresDSN: dsn}, nil
	default:
		return DatabaseConfig{}, fmt.Errorf("database.type %q: must be sqlite or postgres", cfg.Type)
	}
}

// resolvePostgresDSN returns a postgres database: entry's connection
// string: its dsn: as written, or one built from its discrete fields.
func resolvePostgresDSN(cfg fileDatabase) (string, error) {
	dsn := strings.TrimSpace(cfg.DSN)

	switch {
	case dsn != "" && cfg.hasDiscreteFields():
		return "", errors.New("database: set either dsn or host/port/user/password/dbname/sslmode, not both")
	case dsn != "":
		return dsn, nil
	case !cfg.hasDiscreteFields():
		return "", errors.New("database: type postgres needs either dsn or host, user, and dbname")
	}

	return buildPostgresURL(cfg)
}

// buildPostgresURL builds a postgres:// URL from cfg's discrete fields,
// applying defaultPostgresPort and defaultPostgresSSLMode. Every part is
// escaped, so a password may contain any character.
func buildPostgresURL(cfg fileDatabase) (string, error) {
	host, user, dbName := strings.TrimSpace(cfg.Host), strings.TrimSpace(cfg.User), strings.TrimSpace(cfg.DBName)

	var missing []string

	for _, f := range []struct{ name, value string }{{"host", host}, {"user", user}, {"dbname", dbName}} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}

	if len(missing) > 0 {
		return "", fmt.Errorf("database: type postgres needs %s", strings.Join(missing, ", "))
	}

	port := cfg.Port
	if port == 0 {
		port = defaultPostgresPort
	}

	if port < 1 || port > 65535 {
		return "", fmt.Errorf("database.port %d: must be between 1 and 65535", cfg.Port)
	}

	sslMode := strings.TrimSpace(cfg.SSLMode)
	if sslMode == "" {
		sslMode = defaultPostgresSSLMode
	}

	if !slices.Contains(postgresSSLModes, sslMode) {
		return "", fmt.Errorf("database.sslmode %q: must be one of %s", cfg.SSLMode, strings.Join(postgresSSLModes, ", "))
	}

	u := url.URL{
		Scheme:   "postgres",
		User:     url.User(user),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + dbName,
		RawQuery: url.Values{"sslmode": {sslMode}}.Encode(),
	}
	if cfg.Password != "" {
		u.User = url.UserPassword(user, cfg.Password)
	}

	return u.String(), nil
}
