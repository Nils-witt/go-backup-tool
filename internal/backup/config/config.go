// Package config reads go-backup-tool's configuration: CLI flags (-config,
// -job, -log-level) and the YAML config file they name, resolved into a
// RunConfig — the jobs to run, the overall run timeout, and the optional web
// UI/receiver API settings — ready for the rest of the program to consume.
package config

import (
	"log/slog"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// Config holds one backup job's parameters.
type Config struct {
	Name string // job name, from its jobs: entry; always set

	// Kind is the job's type:. A JobKindOnChange job has Source set and no
	// Sources; it uploads changed files one by one (see
	// pipeline.runOnChange) instead of running commands.
	Kind JobKind

	// Source is an on-change job's directory tree; zero for a backup job.
	Source OnChangeSource

	// Sources are the job's files: entries, resolved, in run order: each
	// one's stdout, encrypted, becomes one file of the backup, named Name
	// under the backup's Key (see pipeline.runPipeline). A source's Timeout
	// bounds it only when TimeoutSet; otherwise only the run's overall
	// timeout does.
	Sources []JobSource

	// fileRefs are the job's files: entries as written, resolved into
	// Sources by resolveJobSources.
	fileRefs []FileJobFile

	// DockerHost is the Docker daemon address (see dockerexec.New), set on
	// each run's own copy of its job's config by the runner. Only used by a
	// source with a Container.
	DockerHost string

	// Key names the backup: a folder holding one file per source. It may
	// still contain the {time} placeholder; resolved fresh per run.
	Key string

	// File is the file under Key a single object read or write addresses
	// (see objectstore.LocalObjectPath and remoteclient.Upload): one source's
	// Name, set on a per-file copy of a run's config. Empty addresses Key
	// itself, as a flat object — how backups were stored before a job could
	// have more than one command, and how older senders still upload to a
	// receiver.
	File string

	// CreatedAt is when this run's backup content was actually produced, set
	// once per run by runner.runOnce/RetryFailedTargets from the same
	// instant used to resolve the {time} key placeholder. Zero means "use
	// upload time" — see RecordObjectWrite and remoteclient.Upload.
	CreatedAt time.Time

	targetRefs []jobTargetRef // raw targets: entries, resolved against servers by resolveJobTargets
	Targets    []Target       // resolved destinations; empty until resolveJobTargets runs
	Recipients stringSlice
	Armor      bool
	GPGBin     string
	GPGHomedir string
	Interval   time.Duration // repeat every interval; 0 runs the job once
	StartTime  time.Time     // anchors the interval grid; zero means "run immediately, then every interval"

	// StagingDir is the directory the encrypted backup is written to before
	// any target upload starts (see stageBackup); empty means the OS default
	// temp directory (os.CreateTemp's behavior when given "").
	StagingDir string

	// ServerName is fileConfig.ServerName, this instance's own {server_name}
	// notification placeholder, copied onto every job at build time (see
	// buildJobsFromFile) the same way it's copied onto every
	// ResolvedReceiver (see ResolvedReceiver.ServerName), so
	// pipeline.notifyJobFailure can substitute it without a separate
	// parameter. "" when server-name: is unset.
	ServerName string

	// FailureNotifications names notifications (by id) to fire whenever
	// this job's run ends in an error on any target — whether every target
	// failed or just some (see pipeline.notifyJobFailure). They're looked up
	// in the live notify.Registry when the job fails, so notifications
	// managed in the web UI apply without a restart; an unknown id is logged
	// and skipped. Empty means no live notification on failure; the
	// periodic report (see report.go) still aggregates job errors
	// regardless of this field.
	FailureNotifications []string
}

// jobTargetRef is one targets: entry as written in a job: a server name
// (looked up in the top-level servers: list) plus the bucket to use on it.
type jobTargetRef struct {
	server string
	bucket string

	// retention overrides, for this job's writes to this target only, the
	// retention its server (a local server only — see resolveJobTargets)
	// would otherwise apply. Zero means no override: the server's
	// retention: applies unchanged. There's no way to override retention:
	// to "keep forever" for one job while the server keeps a retention: of
	// its own; that's not expected to be a common need.
	retention time.Duration

	// onErrorCommandID/onErrorAfter/onErrorOnce carry a targets: entry's
	// on-error: block through to resolveJobTargets, which resolves
	// onErrorCommandID against the top-level commands: map into
	// Target.OnErrorCommand. An empty onErrorCommandID means no on-error: was
	// configured for this target. onErrorOnce is on-error.repeat: false.
	onErrorCommandID string
	onErrorAfter     int
	onErrorOnce      bool

	// onRecoverCommandID carries a targets: entry's on-recover: block through
	// to resolveJobTargets, the same way as onErrorCommandID, into
	// Target.OnRecoverCommand. Empty means no on-recover: was configured.
	onRecoverCommandID string
}

// Target is one upload destination for a job, fully resolved from a
// jobTargetRef against its named server. A job uploads the same encrypted
// object to every one of its targets. Its kind determines which of the
// fields below apply: local uses only bucket (as a subdirectory of
// localPath) and localPath itself; remote uses bucket (as the id sent to
// the destination instance) and endpoint. A remote Target authenticates
// with this instance's identity (see remoteclient.Upload), not a field on
// Target itself.
type Target struct {
	ServerName string // the servers: entry this came from, for diagnostics
	Kind       ServerKind
	Bucket     string
	Endpoint   string

	// ServerUUID is a remote server's server-uuid: the destination
	// instance's own server UUID, "" when not configured (or for a local
	// server). Informational: it identifies the destination in status
	// snapshots, not part of authentication.
	ServerUUID string

	// TLS is a remote server's certificate verification settings; the
	// zero value is Go's default verification.
	TLS TargetTLS

	// LocalPath is the local server's root directory (only set when
	// kind == serverKindLocal). The object is written to
	// LocalPath/bucket/key.
	LocalPath string

	// Retention is how long a local target's written objects are kept
	// before they're deleted automatically (only set when
	// kind == serverKindLocal). Zero means no automatic expiry. Normally
	// the server's Retention:, but a job's targets: entry may override it
	// for that job's own writes (see resolveJobTargets); either way, this
	// resolved value is what RecordLocalWrite stamps on each write. See
	// Retention.go.
	Retention time.Duration

	// OnErrorCommand, if non-nil, is fired (see pipeline.runTargetCommand)
	// once this target's in-memory consecutive-failure streak reaches
	// OnErrorAfter, and again on every consecutive failure after that unless
	// OnErrorOnce (on-error.repeat: false) is set — see
	// Runner.handleTargetOutcome in the pipeline package. nil (the common
	// case) means no on-error: was configured for this target.
	OnErrorCommand *Command
	OnErrorAfter   int
	OnErrorOnce    bool

	// OnRecoverCommand, if non-nil, is fired (see pipeline.runTargetCommand)
	// once, on this target's first success after one or more consecutive
	// failures — see Runner.handleTargetOutcome in the pipeline package. nil
	// (the common case) means no on-recover: was configured for this target.
	OnRecoverCommand *Command
}

// JobSource is one of a job's files: entries, resolved: the command to
// run and the file its encrypted output is stored as under the backup's key.
type JobSource struct {
	Command

	// Name is the file name under the backup's key: the entry's name:, or
	// the command's id plus ".gpg" (".asc" with armor) when that's unset.
	// Always a valid single path segment (see ValidateFileName).
	Name string
}

// RunConfig is the result of ParseFlags: one or more jobs to run, plus the
// overall run timeout and the optional web UI listen address.
type RunConfig struct {
	// Jobs are the config file's (deprecated) jobs: entries, resolved
	// against its servers:/commands: and narrowed by -job. Every job lives
	// in the state db from startup on (see jobs.Manager, which imports
	// FileJobs/FileServers/FileCommands); Jobs is only what the config file
	// itself defines.
	Jobs []*Config

	// FileJobs, FileServers and FileCommands are the config file's
	// (deprecated) jobs:/servers:/commands: entries as written, already
	// validated, kept raw so jobs.Manager can import them into the state db,
	// where they live from then on (managed in the web UI). Each FileJobs
	// entry already has the config file's top-level job defaults layered
	// in (see flattenFileJob), except gpg-bin/gpg-homedir: those stay
	// top-level settings (see GPG) every job falls back to.
	FileJobs     []FileJob
	FileServers  []FileServer
	FileCommands []FileCommand

	// JobFilter is -job: when set, only the job of that name is scheduled.
	// It may name a job stored only in the state db, so an unknown name is
	// reported once jobs are loaded (see jobs.Manager), not here.
	JobFilter string

	// JobEditing mirrors fileWebUI.JobEditing: whether the web UI may
	// create, change, or delete jobs, servers, and commands. Off, they're
	// shown read-only.
	JobEditing bool

	Timeout time.Duration
	Listen  string // empty disables the web UI; see resolveWebUIListen

	// DockerSocket is the config file's docker-socket:, the Docker daemon
	// that a job's or command's container: runs through. Empty falls back
	// to DOCKER_HOST, then the default socket (see dockerexec.ResolveHost).
	DockerSocket string
	ConfigPath   string // where the config file was loaded from; a SQLite state db lives alongside it
	LogLevel     slog.Level
	LogFile      string // empty disables file logging; otherwise log output is also appended here

	// Database is the config file's resolved database: entry: which
	// backend holds the state db (see resolveDatabase).
	Database DatabaseConfig

	// Sentry is the config file's resolved sentry: entry: where errors and
	// panics are reported (see resolveSentry). The zero value disables it.
	Sentry SentrySettings

	// ExportSettings and ImportSettings (-export-settings/-import-settings)
	// name a settings file (see the settingsfile package) to write or apply
	// instead of running jobs; the process exits once that's done. At most
	// one is set.
	ExportSettings string
	ImportSettings string

	// SMTP is the config file's resolved smtp: entry, and GPG its
	// gpg-bin/gpg-homedir: what every email notification — including ones
	// created in the web UI — sends and encrypts with (see
	// notify.ResolveNotification). SMTP stays configured in the config file.
	SMTP notify.SMTPSettings
	GPG  notify.GPGSettings

	// ReceiversBaseDir is fileWebUI.ReceiversBaseDir, cleaned: every path a
	// receiver is given from the web UI must lie inside it (see
	// ValidateReceiverPath). Empty means the web UI can't set receiver paths.
	ReceiversBaseDir string

	// ServerName is fileConfig.ServerName, this instance's own {server_name}
	// notification placeholder (see fileConfig.ServerName).
	ServerName string

	// KeysDir is where this instance's persistent identity (its RSA key pair
	// and UUID — see identity.LoadServerIdentityAtStartup) is stored.
	// Defaults to DefaultKeysDir when the config file's top-level keys-dir:
	// is unset.
	KeysDir string

	// LogViewer enables the web UI's live log viewer (served over
	// /api/logs, see handleLogs/newRunLogger). Off by default: this
	// process's raw log output may include operator detail (paths, error
	// text) an operator might not want shown to every signed-in user with
	// the "view" permission.
	LogViewer bool

	// EventLogLimit is how many of the most recent entries each web UI
	// event log serves (see fileWebUI.EventLogLimit). Always positive: an
	// unset webui.event-log-limit: resolves to
	// DefaultEventLogLimit.
	EventLogLimit int

	// EventLogRetention is how long the event logs keep an entry (see
	// fileWebUI.EventLogRetention); 0 keeps them forever.
	EventLogRetention time.Duration

	// TrustProxyHeaders, when set, makes the web UI derive the client
	// address it records (login/download logs, access log) from
	// proxy-supplied headers rather than the raw TCP connection — see
	// fileWebUI.TrustProxyHeaders and clientAddr in webui.go — and lets the
	// live status WebSocket accept the proxy-reported host as its origin.
	TrustProxyHeaders bool

	// DevMode mirrors fileWebUI.DevMode: when set, the web UI adds
	// permissive CORS response headers so a frontend dev server on its own
	// origin can call this instance's API directly — see fileWebUI.DevMode.
	DevMode bool

	// CORSGetOrigins mirrors fileWebUI.CORSGetOrigins: the origins (or
	// "*") allowed to read the web UI's GET /api/... endpoints cross-origin.
	// Empty adds no such CORS headers.
	CORSGetOrigins []string

	// RateLimit is webui.rate-limit: resolved, with its defaults filled in
	// (see fileWebUIRateLimit).
	RateLimit RateLimitSettings

	// OIDC is the web UI's only login method: the SPA runs the OpenID
	// Connect login itself as a public client, and every /api/... request
	// carries the provider's access token — see auth.go in
	// internal/backup/webui. Disabled leaves the dashboard locked (see
	// resolveOIDCSettings).
	OIDC OIDCSettings
}
