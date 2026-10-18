package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// serverModel is servers: every upload destination a job's targets may name
// (see internal/backup/jobs' Manager), managed in the web UI. Fields are kept
// exactly as entered and validated (config.ResolveServer) on every load.
type serverModel struct {
	Name       string `gorm:"column:name;primaryKey"`
	Type       string `gorm:"column:type;not null"`
	Endpoint   string `gorm:"column:endpoint;not null;default:''"`
	ServerUUID string `gorm:"column:server_uuid;not null;default:''"`
	Path       string `gorm:"column:path;not null;default:''"`
	Retention  string `gorm:"column:retention;not null;default:''"`

	// TLS verification for a remote server's https endpoint (see
	// config.TargetTLS); all zero is Go's default verification.
	TLSInsecureSkipVerify bool   `gorm:"column:tls_insecure_skip_verify;not null;default:false"`
	TLSSkipHostnameVerify bool   `gorm:"column:tls_skip_hostname_verify;not null;default:false"`
	TLSCA                 string `gorm:"column:tls_ca;not null;default:''"`

	CreatedAt time.Time `gorm:"column:created_at;not null"`
	CreatedBy string    `gorm:"column:created_by;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
	UpdatedBy string    `gorm:"column:updated_by;not null"`
}

func (serverModel) TableName() string { return "servers" }

// ServerConfig is one stored server (see serverModel).
type ServerConfig struct {
	Name       string
	Type       string
	Endpoint   string
	ServerUUID string
	Path       string
	Retention  string

	TLSInsecureSkipVerify bool
	TLSSkipHostnameVerify bool
	TLSCA                 string

	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
	UpdatedBy string
}

// commandModel is commands: every named command — shell or tar — a job or a job target's
// on-error/on-recover may run (see internal/backup/jobs' Manager), managed in
// the web UI, kept as entered and validated (config.ResolveCommand) on every
// load.
type commandModel struct {
	ID            string    `gorm:"column:id;primaryKey"`
	Cmd           string    `gorm:"column:cmd;not null"`
	Timeout       string    `gorm:"column:timeout;not null;default:''"`
	Container     string    `gorm:"column:container;not null;default:''"`
	ContainerUser string    `gorm:"column:container_user;not null;default:''"`
	Type          string    `gorm:"column:type;not null;default:''"`
	Path          string    `gorm:"column:path;not null;default:''"`
	Exclude       []string  `gorm:"column:exclude;serializer:json"`
	Compress      string    `gorm:"column:compress;not null;default:''"`
	CreatedAt     time.Time `gorm:"column:created_at;not null"`
	CreatedBy     string    `gorm:"column:created_by;not null"`
	UpdatedAt     time.Time `gorm:"column:updated_at;not null"`
	UpdatedBy     string    `gorm:"column:updated_by;not null"`
}

func (commandModel) TableName() string { return "commands" }

// CommandConfig is one stored command (see commandModel). Type, Path,
// Exclude and Compress are config.FileCommand's; a row stored before they
// existed reads as an unset type, i.e. a shell command.
type CommandConfig struct {
	ID            string
	Cmd           string
	Timeout       string
	Container     string
	ContainerUser string
	Type          string
	Path          string
	Exclude       []string
	Compress      string
	CreatedAt     time.Time
	CreatedBy     string
	UpdatedAt     time.Time
	UpdatedBy     string
}

// JobTarget is one of a stored job's targets, as entered: a server name and
// bucket, plus an optional retention override and on-error/on-recover
// commands (by id). Stored as JSON inside its job's row.
type JobTarget struct {
	Server    string              `json:"server"`
	Bucket    string              `json:"bucket"`
	Retention string              `json:"retention,omitempty"`
	OnError   *JobTargetOnError   `json:"on_error,omitempty"`
	OnRecover *JobTargetOnRecover `json:"on_recover,omitempty"`
}

// JobTargetOnError is a JobTarget's on-error: block.
type JobTargetOnError struct {
	Command string `json:"command"`
	After   int    `json:"after"`
	Repeat  *bool  `json:"repeat,omitempty"`
}

// JobTargetOnRecover is a JobTarget's on-recover: block.
type JobTargetOnRecover struct {
	Command string `json:"command"`
}

// JobFile is one of a stored job's files, as entered: a command (by id)
// and an optional file name. Stored as JSON inside its job's row.
type JobFile struct {
	Command string `json:"command"`
	Name    string `json:"name,omitempty"`
}

// jobModel is jobs: every backup job this instance runs (see
// internal/backup/jobs' Manager), managed in the web UI. Kept as entered —
// durations, server names, and command/notification ids unparsed — and
// validated (config.ResolveJob) on every load. A job's run history
// (job_runs, target_runs) is keyed by its name, which is why it can't be
// renamed.
type jobModel struct {
	Name string `gorm:"column:name;primaryKey"`
	// Command is the job's single command (by id), from before a job
	// listed its files; Open moves any still set into Files (see
	// migrateSingleJobCommands), so it's always "" afterwards.
	Command string `gorm:"column:command;not null;default:''"`
	// Files are the backup's files, each a command (by id) and an optional
	// file name, in run order.
	Files []JobFile `gorm:"column:files;serializer:json"`
	// LegacyCmd is the job's own inline shell command, from before jobs
	// named a command; Open moves any still set into a command of its own
	// (see migrateInlineJobCommands), so it's always "" afterwards.
	LegacyCmd            string      `gorm:"column:cmd;not null"`
	Key                  string      `gorm:"column:key;not null;default:''"`
	Targets              []JobTarget `gorm:"column:targets;serializer:json"`
	Recipients           []string    `gorm:"column:recipients;serializer:json"`
	Armor                bool        `gorm:"column:armor;not null;default:false"`
	GPGBin               string      `gorm:"column:gpg_bin;not null;default:''"`
	GPGHomedir           string      `gorm:"column:gpg_homedir;not null;default:''"`
	Interval             string      `gorm:"column:interval;not null;default:''"`
	StartTime            string      `gorm:"column:start_time;not null;default:''"`
	StagingDir           string      `gorm:"column:staging_dir;not null;default:''"`
	FailureNotifications []string    `gorm:"column:failure_notifications;serializer:json"`
	CreatedAt            time.Time   `gorm:"column:created_at;not null"`
	CreatedBy            string      `gorm:"column:created_by;not null"`
	UpdatedAt            time.Time   `gorm:"column:updated_at;not null"`
	UpdatedBy            string      `gorm:"column:updated_by;not null"`
}

func (jobModel) TableName() string { return "jobs" }

// JobConfig is one stored job (see jobModel).
type JobConfig struct {
	Name                 string
	Command              string // always "" once Open has migrated it; see jobModel
	Files                []JobFile
	LegacyCmd            string // always "" once Open has migrated it; see jobModel
	Key                  string
	Targets              []JobTarget
	Recipients           []string
	Armor                bool
	GPGBin               string
	GPGHomedir           string
	Interval             string
	StartTime            string
	StagingDir           string
	FailureNotifications []string
	CreatedAt            time.Time
	CreatedBy            string
	UpdatedAt            time.Time
	UpdatedBy            string
}

var (
	// ErrServerExists is returned by CreateServerConfig for a taken name.
	ErrServerExists = errors.New("a server with this name already exists")
	// ErrServerNotFound is returned by UpdateServerConfig/
	// DeleteServerConfig for a name with no stored server.
	ErrServerNotFound = errors.New("server not found")
	// ErrCommandExists is returned by CreateCommandConfig for a taken id.
	ErrCommandExists = errors.New("a command with this id already exists")
	// ErrCommandNotFound is returned by UpdateCommandConfig/
	// DeleteCommandConfig for an id with no stored command.
	ErrCommandNotFound = errors.New("command not found")
	// ErrJobExists is returned by CreateJobConfig for a taken name.
	ErrJobExists = errors.New("a job with this name already exists")
	// ErrJobNotFound is returned by GetJobConfig's callers, UpdateJobConfig,
	// and DeleteJobConfig for a name with no stored job.
	ErrJobNotFound = errors.New("job not found")
)

// updatedColumns are the audit columns every config table update writes,
// keeping created_at/created_by.
var updatedColumns = []string{"updated_at", "updated_by"}

// ListServerConfigs returns every stored server, in name order.
func (s *gormStore) ListServerConfigs(ctx context.Context) ([]ServerConfig, error) {
	rows, err := listRows[serverModel](ctx, s.read, "name", "servers")
	if err != nil {
		return nil, err
	}

	out := make([]ServerConfig, len(rows))
	for i, m := range rows {
		out[i] = ServerConfig(m)
	}

	return out, nil
}

// CreateServerConfig stores a new server, returning ErrServerExists if its
// name is already taken.
func (s *gormStore) CreateServerConfig(ctx context.Context, c ServerConfig) error {
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	m := serverModel(c)

	return createRow(ctx, s.db, &m, "server", ErrServerExists)
}

// UpdateServerConfig replaces server c.Name's definition, keeping its
// CreatedAt/CreatedBy, and returns ErrServerNotFound if there is none.
func (s *gormStore) UpdateServerConfig(ctx context.Context, c ServerConfig) error {
	c.UpdatedAt = c.UpdatedAt.UTC()
	m := serverModel(c)

	return updateRow(ctx, s.db, &m, "name", c.Name, []string{
		"type", "endpoint", "server_uuid", "path", "retention",
		"tls_insecure_skip_verify", "tls_skip_hostname_verify", "tls_ca",
	}, "server", ErrServerNotFound)
}

// DeleteServerConfig removes the stored server name, returning
// ErrServerNotFound if there is none. Objects already written to it are
// kept, as is their retention tracking.
func (s *gormStore) DeleteServerConfig(ctx context.Context, name string) error {
	return deleteRow[serverModel](ctx, s.db, "name", name, "server", ErrServerNotFound)
}

// ListCommandConfigs returns every stored command, in id order.
func (s *gormStore) ListCommandConfigs(ctx context.Context) ([]CommandConfig, error) {
	rows, err := listRows[commandModel](ctx, s.read, "id", "commands")
	if err != nil {
		return nil, err
	}

	out := make([]CommandConfig, len(rows))
	for i, m := range rows {
		out[i] = CommandConfig(m)
	}

	return out, nil
}

// CreateCommandConfig stores a new command, returning ErrCommandExists if
// its id is already taken.
func (s *gormStore) CreateCommandConfig(ctx context.Context, c CommandConfig) error {
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	m := commandModel(c)

	return createRow(ctx, s.db, &m, "command", ErrCommandExists)
}

// UpdateCommandConfig replaces command c.ID's definition, keeping its
// CreatedAt/CreatedBy, and returns ErrCommandNotFound if there is none.
func (s *gormStore) UpdateCommandConfig(ctx context.Context, c CommandConfig) error {
	c.UpdatedAt = c.UpdatedAt.UTC()
	m := commandModel(c)

	return updateRow(ctx, s.db, &m, "id", c.ID, []string{
		"cmd", "timeout", "container", "container_user", "type", "path", "exclude", "compress",
	}, "command", ErrCommandNotFound)
}

// DeleteCommandConfig removes the stored command id, returning
// ErrCommandNotFound if there is none.
func (s *gormStore) DeleteCommandConfig(ctx context.Context, id string) error {
	return deleteRow[commandModel](ctx, s.db, "id", id, "command", ErrCommandNotFound)
}

// ListJobConfigs returns every stored job, in name order.
func (s *gormStore) ListJobConfigs(ctx context.Context) ([]JobConfig, error) {
	rows, err := listRows[jobModel](ctx, s.read, "name", "jobs")
	if err != nil {
		return nil, err
	}

	out := make([]JobConfig, len(rows))
	for i, m := range rows {
		out[i] = JobConfig(m)
	}

	return out, nil
}

// GetJobConfig looks up the stored job name, reporting false if there is
// none.
func (s *gormStore) GetJobConfig(ctx context.Context, name string) (JobConfig, bool, error) {
	var m jobModel

	if err := s.read.WithContext(ctx).Take(&m, "name = ?", name).Error; err != nil {
		if isRecordNotFound(err) {
			return JobConfig{}, false, nil
		}

		return JobConfig{}, false, fmt.Errorf("reading job: %w", err)
	}

	return JobConfig(m), true, nil
}

// CreateJobConfig stores a new job, returning ErrJobExists if its name is
// already taken.
func (s *gormStore) CreateJobConfig(ctx context.Context, c JobConfig) error {
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	m := jobModel(c)

	return createRow(ctx, s.db, &m, "job", ErrJobExists)
}

// UpdateJobConfig replaces job c.Name's definition, keeping its
// CreatedAt/CreatedBy, and returns ErrJobNotFound if there is none.
func (s *gormStore) UpdateJobConfig(ctx context.Context, c JobConfig) error {
	c.UpdatedAt = c.UpdatedAt.UTC()
	m := jobModel(c)

	return updateRow(ctx, s.db, &m, "name", c.Name, []string{
		"command", "files", "cmd", "key", "targets", "recipients", "armor", "gpg_bin", "gpg_homedir",
		"interval", "start_time", "staging_dir", "failure_notifications",
	}, "job", ErrJobNotFound)
}

// DeleteJobConfig removes the stored job name, returning ErrJobNotFound if
// there is none. Its run history and tracked objects are kept.
func (s *gormStore) DeleteJobConfig(ctx context.Context, name string) error {
	return deleteRow[jobModel](ctx, s.db, "name", name, "job", ErrJobNotFound)
}

// listRows returns every row of M's table ordered by orderBy.
func listRows[M any](ctx context.Context, db *gorm.DB, orderBy, what string) ([]M, error) {
	var rows []M

	if err := db.WithContext(ctx).Order(orderBy).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}

	return rows, nil
}

// createRow inserts m, returning exists if its primary key is already taken.
func createRow[M any](ctx context.Context, db *gorm.DB, m *M, what string, exists error) error {
	res := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(m)
	if res.Error != nil {
		return fmt.Errorf("storing %s: %w", what, res.Error)
	}

	if res.RowsAffected == 0 {
		return exists
	}

	return nil
}

// updateRow writes m's columns (plus updated_at/updated_by) onto the row
// whose keyCol is key — zero values included — returning notFound if there's
// no such row.
func updateRow[M any](ctx context.Context, db *gorm.DB, m *M, keyCol, key string, columns []string, what string, notFound error) error {
	res := db.WithContext(ctx).Model(new(M)).Where(keyCol+" = ?", key).
		Select(append(columns, updatedColumns...)).Updates(m)
	if res.Error != nil {
		return fmt.Errorf("updating %s: %w", what, res.Error)
	}

	if res.RowsAffected == 0 {
		return notFound
	}

	return nil
}

// deleteRow deletes the row whose keyCol is key, returning notFound if
// there's none.
func deleteRow[M any](ctx context.Context, db *gorm.DB, keyCol, key, what string, notFound error) error {
	res := db.WithContext(ctx).Delete(new(M), keyCol+" = ?", key)
	if res.Error != nil {
		return fmt.Errorf("deleting %s: %w", what, res.Error)
	}

	if res.RowsAffected == 0 {
		return notFound
	}

	return nil
}
