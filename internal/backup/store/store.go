// Package store is go-backup-tool's only home for database access: every
// query, statement, and schema definition against the shared state/retention
// database lives here, behind the Store interface, whose methods are
// Get*/List* lookups and Save*/Delete* writes (plus a handful of
// clearly-named one-off operations — ExpiredObjectPaths — whose behavior a
// plain Get/Save name would obscure).
//
// The database is either SQLite (OpenSQLite, the default: a file next to
// the config file) or PostgreSQL (OpenPostgres), chosen by the config
// file's database: section. Both share one gorm-based implementation; only
// opening, connection pooling, and driver settings differ per backend.
//
// Every other package reaches the database only through a Store, never
// through a *gorm.DB of its own.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store is the shared state/retention database go-backup-tool keeps: each
// start-time-anchored job's last successful run (so a restart can tell a
// genuinely missed run apart from an ordinary restart of a job that already
// ran on time), every file written to a local target or receiver with
// retention: set (so a later sweep knows what's eligible for automatic
// deletion), the settings managed in the web UI, and the web UI's audit
// logs. Nothing about web UI users is stored: every login is SSO and every
// permission comes from the access token's groups (see
// internal/backup/webui/auth.go). The only web UI credentials kept are the
// read-only API tokens' metadata and signing key (see tokens.go), plus, for
// a day after each OIDC back-channel logout, the subject and session it
// ended (see sso_logouts.go).
//
// Open one with OpenSQLite or OpenPostgres; the caller must Close it. Each
// method is documented on its implementation in this package's concern
// files (jobs.go, events.go, ...).
type Store interface {
	// Job and target runs, and uploads awaiting a retry (jobs.go).
	SaveJobRun(ctx context.Context, name, state string, success bool, startTime, endTime time.Time, bytesWritten int64, errText string) error
	GetLastJobSuccess(ctx context.Context, name string) (time.Time, bool, error)
	GetLastRun(ctx context.Context, name string) (LastRun, bool, error)
	ListJobRunEvents(ctx context.Context, limit int) ([]JobRunEvent, error)
	SummarizeJobRuns(ctx context.Context, start, end time.Time) ([]JobRunDaySummary, error)
	ListJobRunErrorEvents(ctx context.Context, start, end time.Time) ([]JobRunErrorEvent, error)
	SaveTargetRun(ctx context.Context, name string, success bool, target, state, errText string, at time.Time) error
	ListTargetRuns(ctx context.Context, name string) ([]TargetRun, error)
	ListTargetRunEvents(ctx context.Context, limit int) ([]TargetRunEvent, error)
	DeleteOldJobRuns(ctx context.Context, cutoff time.Time, maxPerJob int) (int64, error)
	DeleteOldTargetRuns(ctx context.Context, cutoff time.Time, maxPerTarget int) (int64, error)
	AddOutstandingTargetUpload(ctx context.Context, u OutstandingTargetUpload) error
	ListDueOutstandingTargetUploads(ctx context.Context, now time.Time) ([]OutstandingTargetUpload, error)
	RescheduleOutstandingTargetUpload(ctx context.Context, id uint, retryAt time.Time) error
	DeleteOutstandingTargetUpload(ctx context.Context, id uint) error
	CountOutstandingTargetUploads(ctx context.Context, jobName, target string) (int64, error)
	CountOutstandingTargetUploadsForFile(ctx context.Context, fileName string) (int64, error)

	// Retention tracking of written objects (objects.go).
	SaveObjectWrite(ctx context.Context, server, bucket, path string, writtenAt time.Time, retentionSeconds int64) error
	DeleteObjectWrite(ctx context.Context, path string) error
	ExpiredObjectPaths(ctx context.Context, server string, now time.Time, fallbackRetention time.Duration) ([]string, error)

	// What each on-change job's targets last received
	// (onchange_files.go).
	ListOnChangeFiles(ctx context.Context, jobName, target string) ([]OnChangeFile, error)
	SaveOnChangeFile(ctx context.Context, f OnChangeFile) error
	DeleteOnChangeFiles(ctx context.Context, jobName string) error

	// Login, download, and receiver event logs (events.go) and the audit
	// log (audit.go).
	SaveLoginEvent(ctx context.Context, ev LoginEvent) error
	ListLoginEvents(ctx context.Context, limit int) ([]LoginEvent, error)
	SaveDownloadEvent(ctx context.Context, ev DownloadEvent) error
	ListDownloadEvents(ctx context.Context, limit int) ([]DownloadEvent, error)
	SaveReceiverEvent(ctx context.Context, ev ReceiverEvent) error
	GetLastReceiverEvent(ctx context.Context, id string) (ReceiverEvent, bool, error)
	ListReceiverEvents(ctx context.Context, limit int) ([]ReceiverEvent, error)
	SummarizeReceiverEvents(ctx context.Context, start, end time.Time) ([]ReceiverDaySummary, error)
	ListReceiverErrorEvents(ctx context.Context, start, end time.Time) ([]ReceiverErrorEvent, error)
	PruneEvents(ctx context.Context, cutoff time.Time) (int64, error)
	SaveAuditEvent(ctx context.Context, ev AuditEvent) error
	ListAuditEvents(ctx context.Context, limit int) ([]AuditEvent, error)

	// Servers, commands, and jobs managed in the web UI (job_configs.go).
	ListServerConfigs(ctx context.Context) ([]ServerConfig, error)
	CreateServerConfig(ctx context.Context, c ServerConfig) error
	UpdateServerConfig(ctx context.Context, c ServerConfig) error
	DeleteServerConfig(ctx context.Context, name string) error
	ListCommandConfigs(ctx context.Context) ([]CommandConfig, error)
	CreateCommandConfig(ctx context.Context, c CommandConfig) error
	UpdateCommandConfig(ctx context.Context, c CommandConfig) error
	DeleteCommandConfig(ctx context.Context, id string) error
	ListJobConfigs(ctx context.Context) ([]JobConfig, error)
	GetJobConfig(ctx context.Context, name string) (JobConfig, bool, error)
	CreateJobConfig(ctx context.Context, c JobConfig) error
	UpdateJobConfig(ctx context.Context, c JobConfig) error
	DeleteJobConfig(ctx context.Context, name string) error

	// Notifications (notifications.go).
	ListNotificationConfigs(ctx context.Context) ([]NotificationConfig, error)
	GetNotificationConfig(ctx context.Context, id string) (NotificationConfig, bool, error)
	CreateNotificationConfig(ctx context.Context, n NotificationConfig) error
	UpdateNotificationConfig(ctx context.Context, n NotificationConfig) error
	DeleteNotificationConfig(ctx context.Context, id string) error

	// Receivers (receivers.go) and trusted servers (trusted_servers.go).
	ListReceiverConfigs(ctx context.Context) ([]ReceiverConfig, error)
	GetReceiverConfig(ctx context.Context, id string) (ReceiverConfig, bool, error)
	CreateReceiverConfig(ctx context.Context, r ReceiverConfig) error
	UpdateReceiverConfig(ctx context.Context, r ReceiverConfig) error
	DeleteReceiverConfig(ctx context.Context, id string) error
	ListReceiversRelyingOnPublicKey(ctx context.Context) ([]string, error)
	ListTrustedServers(ctx context.Context) ([]TrustedServerConfig, error)
	CreateTrustedServer(ctx context.Context, ts TrustedServerConfig) error
	UpdateTrustedServer(ctx context.Context, ts TrustedServerConfig) error
	DeleteTrustedServer(ctx context.Context, id string) error

	// Periodic report settings (report_settings.go) and generated reports
	// (reports.go).
	GetReportSettings(ctx context.Context) (ReportSettings, bool, error)
	SaveReportSettings(ctx context.Context, r ReportSettings) error
	SaveReport(ctx context.Context, r Report) (Report, error)
	ListReports(ctx context.Context, limit int) ([]ReportSummary, error)
	GetReport(ctx context.Context, id uint) (Report, error)
	DeleteReportsBefore(ctx context.Context, cutoff time.Time) (int64, error)

	// How long the job and target run logs keep their rows
	// (log_retention.go).
	GetLogRetentionSettings(ctx context.Context) (LogRetentionSettings, bool, error)
	SaveLogRetentionSettings(ctx context.Context, r LogRetentionSettings) error

	// Where this instance pushes its status (status_push_settings.go), and
	// the status other instances pushed to it (peer_statuses.go).
	GetStatusPushSettings(ctx context.Context) (StatusPushSettings, bool, error)
	SaveStatusPushSettings(ctx context.Context, p StatusPushSettings) error
	ListPeerStatuses(ctx context.Context) ([]PeerStatus, error)
	SavePeerStatus(ctx context.Context, p PeerStatus) error
	DeletePeerStatus(ctx context.Context, serverID string) error

	// API tokens and their signing key (tokens.go), and OIDC back-channel
	// logouts (sso_logouts.go).
	TokenSigningKey(ctx context.Context) ([]byte, error)
	SaveAPIToken(ctx context.Context, t APIToken) error
	GetAPIToken(ctx context.Context, id string) (APIToken, bool, error)
	ListAPITokens(ctx context.Context) ([]APIToken, error)
	RevokeAPIToken(ctx context.Context, id, by string, at time.Time) (bool, error)
	SaveSSOLogout(ctx context.Context, l SSOLogout) error
	ListSSOLogouts(ctx context.Context, issuer string, now time.Time) ([]SSOLogout, error)
	DeleteExpiredSSOLogouts(ctx context.Context, now time.Time) error

	// Close closes the underlying connection pools.
	Close() error
}

var _ Store = (*gormStore)(nil)

// gormStore is Store's one implementation, shared by every backend. Every
// write (and every read inside a write's transaction) goes through db; the
// pure Get*/List* lookups use read. With SQLite they're separate pools on
// the same file (see openSQLite); with PostgreSQL, which handles concurrent
// readers and writers itself, both are the same pool.
type gormStore struct {
	db   *gorm.DB
	read *gorm.DB
}

// newGormConfig returns the gorm settings every backend opens with.
//
// Every method in this package already wraps and returns query errors
// (including the entirely expected "not found" case every Get* lookup can
// hit) to its caller, so GORM's own query logging — which would otherwise
// print every one of those as a Warn/Error-level log line — is silenced
// rather than duplicating that with noise database/sql never produced.
//
// Every write here is a single statement or already runs in an explicit
// Transaction, so GORM's implicit per-write transaction is skipped.
func newGormConfig() *gorm.Config {
	return &gorm.Config{
		Logger:                 logger.Default.LogMode(logger.Silent),
		SkipDefaultTransaction: true,
		PrepareStmt:            true,
	}
}

// migrate ensures gdb's schema exists and is current: it creates or extends
// every table, then applies the one-off data migrations, each a no-op once
// done (and on a fresh database).
func migrate(ctx context.Context, gdb *gorm.DB) error {
	if err := gdb.WithContext(ctx).AutoMigrate(
		&jobRunModel{}, &targetRunModel{}, &outstandingTargetUploadModel{},
		&objectModel{}, &onChangeFileModel{},
		&loginEventModel{}, &downloadEventModel{}, &receiverEventModel{}, &auditEventModel{},
		&apiTokenModel{}, &tokenSigningKeyModel{},
		&receiverModel{}, &notificationModel{}, &reportSettingsModel{}, &reportModel{}, &logRetentionSettingsModel{},
		&trustedServerModel{}, &ssoLogoutModel{},
		&statusPushSettingsModel{}, &peerStatusModel{},
		&serverModel{}, &commandModel{}, &jobModel{},
	); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}

	steps := []func(context.Context, *gorm.DB) error{
		removeObsoleteTables, removeObsoleteIndexes,
		migrateInlineJobCommands, migrateSingleJobCommands,
	}
	for _, step := range steps {
		if err := step(ctx, gdb); err != nil {
			return err
		}
	}

	return nil
}

// Close closes both underlying connection pools, or the one pool when db
// and read share it.
func (s *gormStore) Close() error {
	pools := []*gorm.DB{s.db}
	if s.read != s.db {
		pools = []*gorm.DB{s.read, s.db}
	}

	var errs []error

	for _, gdb := range pools {
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

// closeGormDB closes gdb's connection pool, for an Open* that fails after
// opening it; the close error is dropped in favor of the error being
// returned.
func closeGormDB(gdb *gorm.DB) {
	if sqlDB, err := gdb.DB(); err == nil {
		_ = sqlDB.Close()
	}
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
