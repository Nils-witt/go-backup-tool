// Package config reads go-backup-tool's configuration: CLI flags (-config,
// -job, -log-level) and the YAML config file they name, resolved into a
// RunConfig — the jobs to run, the overall run timeout, and the optional web
// UI/receiver API settings — ready for the rest of the program to consume.
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	appconfig "nilswitt.dev/go-backup-tool/internal/backup/app/config"
	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// Config holds one backup job's parameters.
type Config struct {
	Name string // job name, from its jobs: entry; always set
	Cmd  string
	Key  string // may still contain the {time} placeholder; resolved fresh per run

	// CreatedAt is when this run's backup content was actually produced, set
	// once per run by runner.runOnce/RetryFailedTargets from the same
	// instant used to resolve the {time} key placeholder. Zero means "use
	// upload time" — see RecordObjectWrite and pipeline.UploadToRemote.
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

	// StateDB is the shared state/retention sqlite database (see the store
	// package and retention.go), set on each run's own copy of its job's
	// config by runner.runOnce so RecordLocalWrite/RemoveRetentionRecord
	// reach it without every function in the upload call chain needing its
	// own db parameter. Nil disables retention tracking for this run (e.g.
	// the db couldn't be opened at startup) — see RecordLocalWrite.
	StateDB *store.Store

	// Identity is this instance's own persistent Identity (see
	// loadServerIdentity), set on each run's own copy of its job's config by
	// runner.runOnce the same way stateDB is. uploadToRemote/
	// deleteRemoteObject (pipeline.go) use it to sign a type: remote
	// target's requests. Nil means loadServerIdentity failed at startup (see
	// its own doc comment); any job with a remote target then fails that
	// target's uploads until a later run's Identity loads successfully.
	Identity *identity.ServerIdentity

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

// ServerKind distinguishes a servers: entry's destination type. There is no
// default: every servers: entry must set an explicit type:.
type ServerKind string

// The ServerKind values a servers: entry's type: can resolve to.
const (
	ServerKindLocal  ServerKind = "local"  // type: local
	ServerKindRemote ServerKind = "remote" // type: remote
)

// parseServerKind validates a FileServer's Type field.
func parseServerKind(t string) (ServerKind, error) {
	switch strings.TrimSpace(t) {
	case string(ServerKindLocal):
		return ServerKindLocal, nil
	case string(ServerKindRemote):
		return ServerKindRemote, nil
	case "":
		return "", fmt.Errorf("type is required (want %q or %q)", ServerKindLocal, ServerKindRemote)
	default:
		return "", fmt.Errorf("unknown type %q (want %q or %q)", t, ServerKindLocal, ServerKindRemote)
	}
}

// Target is one upload destination for a job, fully resolved from a
// jobTargetRef against its named server. A job uploads the same encrypted
// object to every one of its targets. Its kind determines which of the
// fields below apply: local uses only bucket (as a subdirectory of
// localPath) and localPath itself; remote uses bucket (as the id sent to
// the destination instance) and endpoint. A remote Target authenticates
// with the run's own cfg.identity (see uploadToRemote/deleteRemoteObject in
// pipeline.go), not a field on Target itself.
type Target struct {
	ServerName string // the servers: entry this came from, for diagnostics
	Kind       ServerKind
	Bucket     string
	Endpoint   string

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

// Command is one top-level commands: entry after validation, ready to be
// run by the pipeline package (see pipeline.runTargetCommand) once a
// target's consecutive-failure streak reaches its on-error.after threshold,
// or once a target recovers from a failure streak (on-recover:).
type Command struct {
	ID      string
	Cmd     string
	Timeout time.Duration
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

	Timeout    time.Duration
	Listen     string // empty disables the web UI; see resolveWebUIListen
	ConfigPath string // where the config file was loaded from; state db lives alongside it
	LogLevel   slog.Level
	LogFile    string                      // empty disables file logging; otherwise log output is also appended here
	Receivers  map[string]ResolvedReceiver // the config file's (deprecated) receivers: entries, keyed by id; imported into the state db at startup, see receiver.Manager

	// FileReceivers are the config file's receivers: entries as written,
	// already validated into Receivers above, kept raw so receiver.Manager
	// can import them into the state db's receivers table, which is where
	// every receiver lives from then on (managed in the web UI).
	FileReceivers []FileReceiver

	// FileNotifications are the config file's (deprecated) notifications:
	// entries as written, already validated, kept raw so settings.Manager
	// can import them into the state db, where every notification lives
	// from then on (managed in the web UI).
	FileNotifications []notify.FileNotification

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
	// and UUID — see loadServerIdentity) is stored. Defaults to
	// defaultServerKeyDir when the config file's top-level keys-dir: is
	// unset.
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
	// appconfig.DefaultEventLogLimit.
	EventLogLimit int

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

	// OIDC is the web UI's only login method: the SPA runs the OpenID
	// Connect login itself as a public client, and every /api/... request
	// carries the provider's access token — see auth.go in
	// internal/backup/webui. Disabled leaves the dashboard locked (see
	// resolveOIDCSettings).
	OIDC OIDCSettings

	// FileReport is the config file's (deprecated) report: entry as
	// written, its schedule already validated, kept raw so settings.Manager
	// can import it into the state db, where the report's settings live
	// from then on (managed in the web UI). The report itself — a periodic
	// summary of receiver and job activity, see pipeline.RunReportLoop — is
	// independent of the web UI.
	FileReport report.FileReport
}

// OIDCSettings is runConfig's resolved form of the config file's
// webui.oidc: entry (see fileWebUIOIDC). The web UI's SPA logs in as the
// public client ClientID (authorization code + PKCE, no client secret), and
// the backend verifies every request's access token against Issuer (see
// internal/backup/webui/auth.go). Nothing about users is stored: a
// request's permissions are DefaultPermissions plus whatever its token's
// groups (read from GroupsClaim) are mapped to in GroupPermissions, worked
// out afresh on every request.
type OIDCSettings struct {
	Enabled     bool
	Issuer      string
	ClientID    string
	Scopes      []string
	ButtonLabel string
	GroupsClaim string

	// DefaultPermissions is granted to every signed-in user, on top of
	// whatever their groups grant. Defaults to
	// permission.PermissionView|permission.PermissionDownload when
	// webui.oidc.default-permissions: is unset.
	DefaultPermissions permission.Permission

	// GroupPermissions maps a group name (as it appears in the token's
	// GroupsClaim claim) to the permissions its members get on top of
	// DefaultPermissions.
	GroupPermissions map[string]permission.Permission
}

const (
	// defaultOIDCButtonLabel is used when webui.oidc.button-label is unset.
	defaultOIDCButtonLabel = "Sign in with SSO"
	// defaultOIDCGroupsClaim is used when webui.oidc.groups-claim is unset.
	defaultOIDCGroupsClaim = "groups"
)

// FileJob mirrors config's per-job fields for YAML unmarshaling, used both
// for the top-level shared defaults and for each entry under jobs:. Any
// field left unset falls through to the built-in default (top-level) or the
// top-level value (a jobs: entry).
//
// A job names its upload destination(s) via targets:, each entry a
// {server, bucket} pair referencing a servers: entry defined at the top
// level (see FileServer) — server connection details (endpoint) live there,
// not on the job. A targets: entry may also set its own retention: (local
// servers only), overriding the server's for that job's writes to that
// target — see FileJobTarget.
type FileJob struct {
	Name       string          `yaml:"name" json:"name"`
	Cmd        string          `yaml:"cmd" json:"cmd"`
	Key        string          `yaml:"key" json:"key"`
	Targets    []FileJobTarget `yaml:"targets" json:"targets"`
	Recipients []string        `yaml:"recipients" json:"recipients"`
	Armor      bool            `yaml:"armor" json:"armor"`
	GPGBin     string          `yaml:"gpg-bin" json:"gpg_bin"`
	GPGHomedir string          `yaml:"gpg-homedir" json:"gpg_homedir"`
	Interval   string          `yaml:"interval" json:"interval"`
	StartTime  string          `yaml:"start-time" json:"start_time"`
	StagingDir string          `yaml:"staging-dir" json:"staging_dir"`

	// FailureNotifications names top-level notifications: entries (see
	// notify.Build) to fire whenever this job's run ends in an error on any
	// target — whether every target failed or just some. Optional.
	FailureNotifications []string `yaml:"failure-notifications" json:"failure_notifications"`
}

// FileJobTarget mirrors jobTargetRef for YAML unmarshaling. Retention (local
// servers only) overrides, for this job's writes to this target, the
// retention its server otherwise applies — same duration syntax as a
// server's own retention: (see FileServer). Unset keeps the server's
// retention: unchanged; it's an error to set it against a target whose
// server isn't type: local.
type FileJobTarget struct {
	Server    string               `yaml:"server" json:"server"`
	Bucket    string               `yaml:"bucket" json:"bucket"`
	Retention string               `yaml:"retention" json:"retention"`
	OnError   *FileTargetOnError   `yaml:"on-error" json:"on_error"`
	OnRecover *FileTargetOnRecover `yaml:"on-recover" json:"on_recover"`
}

// FileTargetOnError mirrors a targets: entry's on-error: block for YAML
// unmarshaling. Command references a top-level commands: entry's id
// (required whenever on-error: is present at all — see applyFileJob).
// After is how many consecutive times this target must fail, in a row,
// before Command first fires. Must be a positive integer — validated in
// resolveJobTargets, once the target has been resolved against servers:.
// Repeat (default true) makes Command fire again on every subsequent
// consecutive failure (see pipeline.Runner.handleTargetOutcome); false fires
// it only once per streak, when the streak reaches After.
type FileTargetOnError struct {
	Command string `yaml:"command" json:"command"`
	After   int    `yaml:"after" json:"after"`
	Repeat  *bool  `yaml:"repeat" json:"repeat"`
}

// FileTargetOnRecover mirrors a targets: entry's on-recover: block for YAML
// unmarshaling. Command references a top-level commands: entry's id
// (required whenever on-recover: is present at all — see applyFileJob). It
// fires once, on the first success after one or more consecutive failures
// (see pipeline.Runner.handleTargetOutcome), independent of on-error:.
type FileTargetOnRecover struct {
	Command string `yaml:"command" json:"command"`
}

// FileServer is one top-level servers: entry, defined once and referenced by
// name from any job's targets: list. type: selects the destination kind:
// "local" for a directory on the local filesystem, using only path; or
// "remote" for another go-backup-tool instance's receiver API, using only
// endpoint — auth is this instance's own identity (see loadServerIdentity),
// not a config field. type: is required; there is no default.
// Retention (local only) is a duration string (e.g. "7d" or "168h" for 7
// days, "30m" for 30 minutes) — like time.ParseDuration but with "d" also
// accepted for days (parsed by parseDayDuration, since the standard library
// has no day unit; "m" is already minutes in time.ParseDuration, not
// months); when set, any object this tool writes under path is deleted once
// it's older than that, tracked in the shared state sqlite database kept
// alongside the config file (see retention.go and schedule_state.go). Unset
// or "0"
// disables automatic cleanup. A remote server needs no auth field of its
// own: it authenticates to the destination instance's receiver API with
// this instance's own persistent identity (see loadServerIdentity), which
// the destination instance verifies against the public key configured on
// its matching receivers: entry's public-key: (see fileReceiver).
type FileServer struct {
	Name      string `yaml:"name" json:"name"`
	Type      string `yaml:"type" json:"type"`
	Endpoint  string `yaml:"endpoint" json:"endpoint"`
	Path      string `yaml:"path" json:"path"`           // local only: root directory backups are written under
	Retention string `yaml:"retention" json:"retention"` // local only: e.g. "7d" or "168h"; unset/"0" keeps objects forever
}

// FileCommand is one top-level commands: entry, defined once and referenced
// by id from a target's on-error.command or on-recover.command — the same "define once, reference
// by id" shape as notify.FileNotification. Cmd is run through the platform
// shell, the same way a job's own cmd: is (see pipeline.newSourceCommand).
// Timeout (optional) bounds how long one firing may run; defaults to
// defaultOnErrorCommandTimeout when unset.
type FileCommand struct {
	ID      string `yaml:"id" json:"id"`
	Cmd     string `yaml:"cmd" json:"cmd"`
	Timeout string `yaml:"timeout" json:"timeout"`
}

// fileConfig is the top-level shape of the YAML config file. Its embedded
// FileJob holds shared defaults applied to every entry in Jobs before that
// entry's own fields override them.
type fileConfig struct {
	FileJob `yaml:",inline"`

	Timeout  string       `yaml:"timeout"`
	LogLevel string       `yaml:"log-level"` // debug, info, warn, or error; overridden by -log-level when that flag is explicitly given
	KeysDir  string       `yaml:"keys-dir"`  // where this instance's persistent identity (RSA key pair + UUID) is stored; defaults to defaultServerKeyDir
	LogFile  string       `yaml:"log-file"`  // if set, log output is also appended to this file (in addition to stderr)
	Servers  []FileServer `yaml:"servers"`
	Jobs     []FileJob    `yaml:"jobs"`

	// ServerName identifies this instance in notifications: a {server_name}
	// placeholder, substituted the same way as a stale/download
	// notification's other placeholders (see renderStaleWebhookPayload/
	// renderDownloadWebhookPayload) or a report notification's (see
	// pipeline.renderReportSubject), is available in every notification
	// webhook.body/email.subject/email.body — handy for telling which
	// go-backup-tool instance a notification came from when several share
	// the same webhook/inbox. Unset (the default) substitutes as "". Also
	// the web UI's displayed instance name, which falls back to the
	// INSTANCE_NAME environment variable when this is unset (see
	// webui.resolveInstanceName).
	ServerName string `yaml:"server-name"`

	// SMTP is the outgoing mail server used by any notifications: entry with
	// an email: block (see notify.ResolveSMTP). Unset unless at least one
	// notification uses email.
	SMTP notify.FileSMTP `yaml:"smtp"`

	// Notifications are the named webhook/email destinations referenced by
	// id from Report.Notifications and a receiver's
	// StaleNotifications/DownloadNotifications (see notify.Build).
	Notifications []notify.FileNotification `yaml:"notifications"`

	// Commands are named, reusable shell commands, referenced by id from a
	// target's on-error.command or on-recover.command (see buildCommands) —
	// the same "define
	// once, reference by id" shape as Notifications.
	Commands []FileCommand `yaml:"commands"`

	Receivers []FileReceiver    `yaml:"receivers"`
	WebUI     fileWebUI         `yaml:"webui"`
	Report    report.FileReport `yaml:"report"`
}

// fileWebUI is the top-level webui: entry, grouping every setting that
// controls the optional web UI dashboard (and, since it's served by the same
// HTTP server, the receiver API — see fileConfig's Receivers).
type fileWebUI struct {
	// Enabled turns the web UI on; Listen (e.g. ":8080") is the address it
	// binds. Unset/false (the default) disables the web UI entirely,
	// regardless of Listen. It's an error to set Enabled: true without also
	// giving Listen a value — see resolveWebUIListen. When enabled, the
	// process stays alive to keep serving the dashboard even after every
	// job has finished its (possibly one-shot) run, until stopped
	// (Ctrl-C/SIGTERM) or the config file's timeout elapses.
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`

	// Username/Password are no longer supported (SSO is the only login
	// method, see fileWebUIOIDC). They're still decoded only so
	// resolveWebUISettings can reject a config that sets them, rather than
	// silently ignoring what used to be its login.
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	// LogViewer turns on the web UI's live log viewer (a "Logs" section on
	// the dashboard, polling /api/logs). Unset/false (the default) keeps it
	// off.
	LogViewer bool `yaml:"log-viewer"`

	// EventLogLimit caps how many of the most recent entries each of the
	// web UI's event logs (job runs, target runs, logins, downloads,
	// receiver events) shows. Every entry stays in the state database
	// regardless; this only limits how many are served. Unset (0) defaults
	// to appconfig.DefaultEventLogLimit. Must be between 1 and
	// maxEventLogLimit when set.
	EventLogLimit int `yaml:"event-log-limit"`

	// TrustProxyHeaders makes the web UI take the client address recorded
	// in the login/download logs and access log (see clientAddr in
	// webui.go) from the Forwarded/X-Forwarded-For/X-Real-Ip request
	// headers instead of the raw TCP connection's address, when present.
	// Only enable this when the web UI sits behind a reverse proxy that
	// itself sets these headers and strips any client-supplied copies
	// first — otherwise any client can spoof its own logged address by
	// sending these headers itself. Unset/false (the default) always uses
	// the TCP connection's own address. It also makes the live status
	// WebSocket check the browser's Origin against the host a proxy reports
	// in Forwarded host=/X-Forwarded-Host (see forwardedHost in webui.go),
	// for a proxy that rewrites the Host header.
	TrustProxyHeaders bool `yaml:"trust-proxy-headers"`

	// OIDC configures Single Sign-On via an OpenID Connect provider, the
	// web UI's only login method — see fileWebUIOIDC.
	OIDC fileWebUIOIDC `yaml:"oidc"`

	// DevMode adds permissive CORS response headers (Access-Control-Allow-
	// Origin: *, plus the methods/headers the dashboard's own fetch() calls
	// use) to every response, so a frontend dev server running on its own
	// origin (e.g. `npm run dev`, http://localhost:5173) can call this
	// instance's /api/... endpoints directly without Vite's dev-only proxy
	// (see frontend/vite.config.ts) in the way. Unset/false (the default)
	// adds no CORS headers at all, matching the dashboard's normal
	// same-origin deployment. Never enable this against a production
	// instance: it lets any website's JavaScript read API responses from a
	// browser that also holds a valid bearer token for this instance.
	DevMode bool `yaml:"dev-mode"`

	// CORSGetOrigins enables CORS for the web UI's GET /api/... endpoints
	// only: a request from one of these origins (scheme://host[:port], e.g.
	// "https://status.example.com"), or from any origin if the list holds
	// "*", gets an Access-Control-Allow-Origin response header, and its
	// preflight is answered allowing GET/HEAD with an Authorization header.
	// Every other method stays same-origin only, so another site can read
	// (with its own bearer token) but never change anything. Unset/empty
	// (the default) adds no CORS headers. webui.dev-mode:, when also set,
	// takes precedence with its permissive all-method CORS.
	CORSGetOrigins []string `yaml:"cors-get-origins"`

	// ReceiversBaseDir is the directory every receiver path set from the
	// web UI must lie inside (see ValidateReceiverPath), so an admin can't
	// point a receiver at an arbitrary location on this machine. Must be
	// absolute. Unset leaves the web UI unable to create receivers or change
	// their path.
	ReceiversBaseDir string `yaml:"receivers-base-dir"`

	// JobEditing lets web UI admins create, change, and delete jobs,
	// servers, and commands. Off by default: a job's cmd: and a command's
	// cmd: run as shell commands on this machine, and a local server's
	// path: is written to, so editing them from the web UI amounts to
	// running arbitrary commands as this process's user. Off, the web UI
	// shows them read-only.
	JobEditing bool `yaml:"job-editing"`
}

// fileWebUIOIDC is the webui.oidc: entry, configuring Single Sign-On for the
// web UI dashboard via an OpenID Connect provider (Keycloak, Authentik,
// Okta, ...) — the dashboard's only login method. The SPA itself is the OIDC client: a public client
// running authorization code + PKCE in the browser, with
// <origin>/login/sso/callback as its redirect URI, so the provider must
// register it as a public/SPA client and allow this origin for CORS. There
// is no client secret. Left disabled, nobody can sign in to the dashboard,
// though the receiver API (same HTTP server) is unaffected.
type fileWebUIOIDC struct {
	Enabled bool `yaml:"enabled"`

	// Issuer is the provider's issuer URL (e.g.
	// "https://auth.example.com/realms/main") — the base for OpenID Connect
	// Discovery, and what every access token's "iss" must match.
	Issuer string `yaml:"issuer"`

	// ClientID is the public client the SPA logs in as; an access token
	// must name it as its "azp" or in its "aud".
	ClientID string `yaml:"client-id"`

	// ClientSecret/RedirectURL belonged to the old server-side login flow
	// and are no longer supported; decoded only so resolveOIDCSettings can
	// reject a config that still sets them.
	ClientSecret string `yaml:"client-secret"`
	RedirectURL  string `yaml:"redirect-url"`

	// Scopes are the OAuth2 scopes the SPA requests. Unset defaults to
	// {"openid", "profile", "email"}. Add "offline_access" to get a refresh
	// token: without one, a session ends when its access token expires.
	Scopes []string `yaml:"scopes"`

	// ButtonLabel is the login page's SSO button text. Defaults to
	// defaultOIDCButtonLabel.
	ButtonLabel string `yaml:"button-label"`

	// GroupsClaim names the access-token claim listing the user's groups (a
	// string array, or a single string). Defaults to defaultOIDCGroupsClaim.
	GroupsClaim string `yaml:"groups-claim"`

	// DefaultPermissions lists the dashboard permissions ("view", "download",
	// "admin", "login-log", "download-log", "job-run-log",
	// "target-run-log", and/or "receiver-log") granted to every signed-in
	// user. Unset defaults to "view" and "download".
	DefaultPermissions []string `yaml:"default-permissions"`

	// GroupPermissions maps a group name (as it appears in GroupsClaim) to
	// the permission names its members get on top of DefaultPermissions.
	GroupPermissions map[string][]string `yaml:"group-permissions"`
}

// ParseFlags parses args (typically os.Args[1:]) into a runConfig, writing
// usage output to out on error or -h/-help. It takes an explicit argument
// list and a fresh FlagSet (rather than the package-level flag.CommandLine)
// so it can be called repeatedly and in isolation from tests.
//
// All job parameters come from the YAML config file (-config, defaulting to
// config.yaml); there are no CLI flags to set them individually. Every job
// is defined under the config file's jobs: list; -job selects a single one
// to run, or every job runs (in order) when -job isn't given. -log-level
// (debug, info, warn, or error; default info) controls diagnostic log
// verbosity; it can also be set via the config file's top-level log-level:,
// which -log-level overrides when explicitly given on the command line.
//
// Each job's key keeps any {time} placeholder unresolved: it's substituted
// fresh by the caller immediately before every run (see substituteKeyTime),
// not here, so a job with a nonzero interval doesn't overwrite the same
// object on every repeat.
func ParseFlags(args []string, out io.Writer) (*RunConfig, error) {
	fs := flag.NewFlagSet("go-backup-tool", flag.ContinueOnError)
	fs.SetOutput(out)

	var (
		configPath string
		jobFilter  string
		logLevel   string
	)

	fs.StringVar(&configPath, "config", appconfig.DefaultConfigPath, "path to the YAML config file")
	fs.StringVar(&jobFilter, "job", "", "run only the named job from the config file's jobs: list")
	fs.StringVar(&logLevel, "log-level", "info", "log verbosity: debug, info, warn, or error (overrides the config file's log-level:)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	configExplicit, logLevelExplicit := explicitFlags(fs)

	fileCfg, err := loadFileConfig(configPath, configExplicit)
	if err != nil {
		return nil, err
	}

	if fileCfg == nil {
		return nil, fmt.Errorf("no config file found at %q; create one (see config.example.yaml) or pass -config <path>", configPath)
	}

	level, err := parseLogLevel(effectiveLogLevel(logLevel, fileCfg.LogLevel, logLevelExplicit))
	if err != nil {
		return nil, err
	}

	webUI, err := resolveWebUISettings(fileCfg.WebUI)
	if err != nil {
		return nil, err
	}

	listen := webUI.listen

	notifications, commands, err := resolveNotificationsAndCommands(fileCfg)
	if err != nil {
		return nil, err
	}

	jobs, err := resolveJobs(fileCfg, commands)
	if err != nil {
		return nil, err
	}

	jobs, err = prepareJobs(jobs, jobFilter)
	if err != nil {
		return nil, err
	}

	// Notification ids are checked against the live registry at startup
	// (see settings.Manager/receiver.Manager), not here: the config file's
	// notifications: is deprecated and may already be gone.
	receivers, err := buildReceivers(fileCfg.Receivers, nil, fileCfg.ServerName)
	if err != nil {
		return nil, err
	}

	timeout, err := parseConfigTimeout(fileCfg.Timeout)
	if err != nil {
		return nil, err
	}

	keysDir := strings.TrimSpace(fileCfg.KeysDir)
	if keysDir == "" {
		keysDir = identity.DefaultServerKeyDir
	}

	if _, err := report.ResolveSettings(fileCfg.Report, nil); err != nil {
		return nil, err
	}

	return &RunConfig{
		Jobs:              jobs,
		FileJobs:          flattenFileJobs(fileCfg),
		FileServers:       fileCfg.Servers,
		FileCommands:      fileCfg.Commands,
		JobFilter:         jobFilter,
		JobEditing:        fileCfg.WebUI.JobEditing,
		Timeout:           timeout,
		Listen:            listen,
		ConfigPath:        configPath,
		LogLevel:          level,
		LogFile:           strings.TrimSpace(fileCfg.LogFile),
		Receivers:         receivers,
		FileReceivers:     fileCfg.Receivers,
		FileNotifications: fileCfg.Notifications,
		SMTP:              notifications.smtp,
		GPG:               notifications.gpg,
		ReceiversBaseDir:  webUI.receiversBaseDir,
		KeysDir:           keysDir,
		LogViewer:         fileCfg.WebUI.LogViewer,
		EventLogLimit:     webUI.eventLogLimit,
		TrustProxyHeaders: fileCfg.WebUI.TrustProxyHeaders,
		DevMode:           fileCfg.WebUI.DevMode,
		CORSGetOrigins:    webUI.corsGetOrigins,
		OIDC:              webUI.oidc,
		FileReport:        fileCfg.Report,
		ServerName:        fileCfg.ServerName,
	}, nil
}

// resolvedNotifications is what resolveNotifications derives from the
// config file: the resolved smtp:/gpg settings every email notification
// uses, and its (deprecated) notifications: entries, validated.
type resolvedNotifications struct {
	smtp notify.SMTPSettings
	gpg  notify.GPGSettings
	byID map[string]notify.Notification
}

// resolveNotifications resolves fileCfg's top-level smtp:/notifications:
// entries (see notify.ResolveSMTP/notify.Build) — split out of ParseFlags to
// keep its own cyclomatic complexity down. Encrypted notification emails use
// the top-level gpg-bin/gpg-homedir, the same gpg and keyring as backups.
func resolveNotifications(fileCfg *fileConfig) (resolvedNotifications, error) {
	smtp, err := notify.ResolveSMTP(fileCfg.SMTP)
	if err != nil {
		return resolvedNotifications{}, err
	}

	gpg := notify.GPGSettings{Bin: appconfig.DefaultGPGBin}
	applyString(&gpg.Bin, fileCfg.GPGBin)
	applyString(&gpg.Homedir, fileCfg.GPGHomedir)

	byID, err := notify.Build(fileCfg.Notifications, smtp, gpg)
	if err != nil {
		return resolvedNotifications{}, err
	}

	return resolvedNotifications{smtp: smtp, gpg: gpg, byID: byID}, nil
}

// resolveNotificationsAndCommands resolves fileCfg's top-level
// notifications: and commands: entries together, so ParseFlags only needs
// one error check for both (keeping its own cyclomatic complexity down)
// instead of one each.
func resolveNotificationsAndCommands(fileCfg *fileConfig) (resolvedNotifications, map[string]Command, error) {
	notifications, err := resolveNotifications(fileCfg)
	if err != nil {
		return resolvedNotifications{}, nil, err
	}

	commands, err := buildCommands(fileCfg.Commands)
	if err != nil {
		return resolvedNotifications{}, nil, err
	}

	return notifications, commands, nil
}

// defaultOnErrorCommandTimeout bounds how long firing a commands: entry may
// take when it sets no timeout: of its own.
const defaultOnErrorCommandTimeout = 30 * time.Second

// buildCommands resolves fileCfg's top-level commands: entries into an
// id -> Command map, used to resolve a target's on-error.command and
// on-recover.command. Validates
// that every entry has a non-empty, unique id and a non-empty cmd — the
// same validation shape as notify.Build for notifications:.
func buildCommands(fileCommands []FileCommand) (map[string]Command, error) {
	commands := make(map[string]Command, len(fileCommands))

	for i, fc := range fileCommands {
		id := strings.TrimSpace(fc.ID)
		if id == "" {
			return nil, fmt.Errorf("commands[%d]: id is required", i)
		}

		if _, exists := commands[id]; exists {
			return nil, fmt.Errorf("commands[%d]: duplicate command id %q", i, id)
		}

		command, err := ResolveCommand(fc)
		if err != nil {
			return nil, err
		}

		commands[id] = command
	}

	return commands, nil
}

// ResolveCommand validates one commands: entry — from the config file or
// stored in the state db (see jobs.Manager) — into a Command: a non-empty id
// and cmd, and an optional positive timeout (default
// defaultOnErrorCommandTimeout).
func ResolveCommand(fc FileCommand) (Command, error) {
	id := strings.TrimSpace(fc.ID)
	if id == "" {
		return Command{}, errors.New("id is required")
	}

	cmd := strings.TrimSpace(fc.Cmd)
	if cmd == "" {
		return Command{}, fmt.Errorf("command %q: cmd is required", id)
	}

	timeout, err := parseOnErrorCommandTimeout(fc.Timeout)
	if err != nil {
		return Command{}, fmt.Errorf("command %q: %w", id, err)
	}

	return Command{ID: id, Cmd: cmd, Timeout: timeout}, nil
}

// parseOnErrorCommandTimeout parses a commands: entry's timeout: string,
// defaulting to defaultOnErrorCommandTimeout when unset. A non-positive
// timeout is rejected: it would leave no time to actually run the command.
func parseOnErrorCommandTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultOnErrorCommandTimeout, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parsing timeout %q: %w", s, err)
	}

	if d <= 0 {
		return 0, fmt.Errorf("timeout must be positive, got %q", s)
	}

	return d, nil
}

// webUISettings is what ParseFlags derives from the config file's webui:
// entry beyond a plain field copy (see resolveWebUISettings).
type webUISettings struct {
	listen           string
	oidc             OIDCSettings
	receiversBaseDir string
	eventLogLimit    int
	corsGetOrigins   []string
}

// resolveWebUISettings resolves cfg (the config file's webui: entry) into
// its listen address (see resolveWebUIListen), its SSO settings (see
// resolveOIDCSettings), and its receivers base dir (see
// resolveReceiversBaseDir) — the pieces of runConfig ParseFlags derives
// from webui: that need validation beyond a plain field copy.
func resolveWebUISettings(cfg fileWebUI) (webUISettings, error) {
	listen, err := resolveWebUIListen(cfg)
	if err != nil {
		return webUISettings{}, err
	}

	if cfg.Username != "" || cfg.Password != "" {
		return webUISettings{}, errors.New("webui.username/webui.password are no longer supported: the web UI only supports SSO login, configure webui.oidc instead")
	}

	oidc, err := resolveOIDCSettings(cfg.OIDC, listen)
	if err != nil {
		return webUISettings{}, err
	}

	baseDir, err := resolveReceiversBaseDir(cfg.ReceiversBaseDir)
	if err != nil {
		return webUISettings{}, err
	}

	limit, err := resolveEventLogLimit(cfg.EventLogLimit)
	if err != nil {
		return webUISettings{}, err
	}

	corsGetOrigins, err := resolveCORSGetOrigins(cfg.CORSGetOrigins)
	if err != nil {
		return webUISettings{}, err
	}

	return webUISettings{listen: listen, oidc: oidc, receiversBaseDir: baseDir, eventLogLimit: limit, corsGetOrigins: corsGetOrigins}, nil
}

// resolveCORSGetOrigins validates webui.cors-get-origins: (see
// fileWebUI.CORSGetOrigins): each entry must be "*" or a bare origin —
// http(s)://host[:port] with no path, query, fragment or userinfo, since a
// browser's Origin header never carries any of those and so could never
// match. Entries are trimmed, a trailing "/" is dropped, and the scheme and
// host lowercased to match how browsers send Origin.
func resolveCORSGetOrigins(origins []string) ([]string, error) {
	if len(origins) == 0 {
		return nil, nil
	}

	resolved := make([]string, 0, len(origins))

	for _, o := range origins {
		o = strings.TrimSuffix(strings.TrimSpace(o), "/")
		if o == "*" {
			resolved = append(resolved, o)
			continue
		}

		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return nil, fmt.Errorf("webui.cors-get-origins: %q is not \"*\" or an origin like https://example.com[:port]", o)
		}

		resolved = append(resolved, strings.ToLower(u.Scheme)+"://"+strings.ToLower(u.Host))
	}

	return resolved, nil
}

// maxEventLogLimit bounds webui.event-log-limit:, since every entry it
// allows is read, sent, and rendered by the dashboard on each poll.
const maxEventLogLimit = 10000

// resolveEventLogLimit validates webui.event-log-limit: (see
// fileWebUI.EventLogLimit), returning appconfig.DefaultEventLogLimit when
// it's unset.
func resolveEventLogLimit(n int) (int, error) {
	if n == 0 {
		return appconfig.DefaultEventLogLimit, nil
	}

	if n < 1 || n > maxEventLogLimit {
		return 0, fmt.Errorf("webui.event-log-limit %d must be between 1 and %d", n, maxEventLogLimit)
	}

	return n, nil
}

// resolveReceiversBaseDir validates webui.receivers-base-dir: (see
// fileWebUI.ReceiversBaseDir), requiring an absolute path when set, and
// returns it cleaned. Unset returns "".
func resolveReceiversBaseDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}

	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("webui.receivers-base-dir %q must be an absolute path", dir)
	}

	return filepath.Clean(dir), nil
}

// resolveOIDCSettings validates cfg (the config file's webui.oidc: entry)
// against listen (the web UI's resolved listen address, empty if the web UI
// itself is disabled — see resolveWebUIListen) and returns runConfig's
// resolved OIDCSettings. An unset/false cfg.Enabled returns the zero value:
// since SSO is the web UI's only login method, that leaves the dashboard
// locked (nobody can sign in) while the receiver API, served by the same
// HTTP server, keeps working. It's an error to enable OIDC without the web
// UI itself, or without issuer and client-id set.
func resolveOIDCSettings(cfg fileWebUIOIDC, listen string) (OIDCSettings, error) {
	if cfg.ClientSecret != "" || cfg.RedirectURL != "" {
		return OIDCSettings{}, errors.New("webui.oidc.client-secret/webui.oidc.redirect-url are no longer supported: the web UI logs in as a public client (redirect URI <origin>/login/sso/callback), remove them")
	}

	if !cfg.Enabled {
		return OIDCSettings{}, nil
	}

	if listen == "" {
		return OIDCSettings{}, errors.New("webui.oidc.enabled is true but webui.enabled is not")
	}

	issuer := strings.TrimSpace(cfg.Issuer)
	clientID := strings.TrimSpace(cfg.ClientID)

	required := []struct{ name, val string }{
		{"issuer", issuer},
		{"client-id", clientID},
	}

	for _, r := range required {
		if r.val == "" {
			return OIDCSettings{}, fmt.Errorf("webui.oidc.enabled is true but webui.oidc.%s is not set", r.name)
		}
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}

	defaultPerm := permission.PermissionView | permission.PermissionDownload

	if len(cfg.DefaultPermissions) > 0 {
		parsed, err := permission.ParsePermissions(cfg.DefaultPermissions)
		if err != nil {
			return OIDCSettings{}, fmt.Errorf("webui.oidc.default-permissions: %w", err)
		}

		defaultPerm = parsed
	}

	groupPerms, err := parseOIDCGroupPermissions(cfg.GroupPermissions)
	if err != nil {
		return OIDCSettings{}, err
	}

	buttonLabel := strings.TrimSpace(cfg.ButtonLabel)
	if buttonLabel == "" {
		buttonLabel = defaultOIDCButtonLabel
	}

	groupsClaim := strings.TrimSpace(cfg.GroupsClaim)
	if groupsClaim == "" {
		groupsClaim = defaultOIDCGroupsClaim
	}

	return OIDCSettings{
		Enabled:            true,
		Issuer:             issuer,
		ClientID:           clientID,
		Scopes:             scopes,
		ButtonLabel:        buttonLabel,
		GroupsClaim:        groupsClaim,
		DefaultPermissions: defaultPerm,
		GroupPermissions:   groupPerms,
	}, nil
}

// parseOIDCGroupPermissions parses webui.oidc.group-permissions: (group
// name -> permission names) into group name -> permission bitmask,
// rejecting an empty group name or an unknown permission name.
func parseOIDCGroupPermissions(cfg map[string][]string) (map[string]permission.Permission, error) {
	groupPerms := make(map[string]permission.Permission, len(cfg))

	for group, names := range cfg {
		if strings.TrimSpace(group) == "" {
			return nil, errors.New("webui.oidc.group-permissions: group name must not be empty")
		}

		parsed, err := permission.ParsePermissions(names)
		if err != nil {
			return nil, fmt.Errorf("webui.oidc.group-permissions[%q]: %w", group, err)
		}

		groupPerms[group] = parsed
	}

	return groupPerms, nil
}

// resolveWebUIListen returns the effective web UI listen address from the
// config file's webui: entry: empty disables the web UI entirely, whether
// because webui.enabled is false/unset or webui: wasn't given at all. It's
// an error to set webui.enabled: true without also giving webui.listen a
// value, since there would then be no address to bind.
func resolveWebUIListen(cfg fileWebUI) (string, error) {
	if !cfg.Enabled {
		return "", nil
	}

	listen := strings.TrimSpace(cfg.Listen)
	if listen == "" {
		return "", errors.New("webui.enabled is true but webui.listen is not set")
	}

	return listen, nil
}

// parseConfigTimeout parses the config file's top-level timeout: string, if
// any, into a time.Duration; an empty string (unset) means no overall run
// timeout.
func parseConfigTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parsing config file timeout %q: %w", s, err)
	}

	return d, nil
}

// explicitFlags reports whether -config and -log-level were explicitly
// given on the command line (as opposed to holding their default values),
// so callers can tell a deliberate override from an unset flag.
func explicitFlags(fs *flag.FlagSet) (configExplicit, logLevelExplicit bool) {
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "config":
			configExplicit = true
		case "log-level":
			logLevelExplicit = true
		}
	})

	return configExplicit, logLevelExplicit
}

// effectiveLogLevel resolves the log level string to parse: the config
// file's log-level: (fileLevel), unless flagLevel was explicitly given on
// the command line, which always wins.
func effectiveLogLevel(flagLevel, fileLevel string, flagExplicit bool) string {
	if !flagExplicit && strings.TrimSpace(fileLevel) != "" {
		return fileLevel
	}

	return flagLevel
}

// parseLogLevel parses a log level string (from the -log-level flag or the
// config file's log-level:) into a slog.Level, accepting the same
// case-insensitive names slog itself recognizes (debug, info, warn, error).
func parseLogLevel(s string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("parsing log level %q: %w", s, err)
	}

	return level, nil
}

// resolveJobs builds the list of jobs to run from fileCfg's jobs: list,
// layering fileCfg's top-level fields as shared defaults under each entry's
// own fields, and resolving each job's targets: against fileCfg's servers:.
// commands is the config file's already-resolved
// top-level commands: map (see buildCommands), used to resolve a target's
// on-error.command and on-recover.command.
//
// An empty jobs: list is fine here: jobs may be stored in the state db
// instead (see jobs.Manager), which is where the "nothing to run" check
// happens.
func resolveJobs(fileCfg *fileConfig, commands map[string]Command) ([]*Config, error) {
	return buildJobsFromFile(fileCfg, commands)
}

// buildJobsFromFile builds one *config per entry in fileCfg.Jobs, layering
// fileCfg's top-level fields as defaults under each entry's own fields and
// resolving each job's targets: against fileCfg.Servers/commands.
func buildJobsFromFile(fileCfg *fileConfig, commands map[string]Command) ([]*Config, error) {
	servers, err := buildServers(fileCfg.Servers)
	if err != nil {
		return nil, err
	}

	jobs := make([]*Config, 0, len(fileCfg.Jobs))
	seen := make(map[string]bool, len(fileCfg.Jobs))

	for i, fj := range fileCfg.Jobs {
		name := strings.TrimSpace(fj.Name)
		if name == "" {
			return nil, fmt.Errorf("jobs[%d]: name is required", i)
		}

		if seen[name] {
			return nil, fmt.Errorf("jobs[%d]: duplicate job name %q", i, name)
		}

		seen[name] = true

		cfg := newConfigDefaults()
		cfg.Name = name
		cfg.ServerName = fileCfg.ServerName

		if err := applyFileJob(cfg, &fileCfg.FileJob); err != nil {
			return nil, fmt.Errorf("job %q: %w", name, err)
		}

		if err := applyFileJob(cfg, &fj); err != nil {
			return nil, fmt.Errorf("job %q: %w", name, err)
		}

		if err := resolveJobTargets(cfg, servers, commands); err != nil {
			return nil, fmt.Errorf("job %q: %w", name, err)
		}

		jobs = append(jobs, cfg)
	}

	return jobs, nil
}

// buildServers resolves fileServers into a name -> ResolvedServer map,
// validating that every entry has a non-empty, unique name and an explicit,
// valid type:.
func buildServers(fileServers []FileServer) (map[string]ResolvedServer, error) {
	servers := make(map[string]ResolvedServer, len(fileServers))

	for i, fs := range fileServers {
		name := strings.TrimSpace(fs.Name)
		if name == "" {
			return nil, fmt.Errorf("servers[%d]: name is required", i)
		}

		if _, exists := servers[name]; exists {
			return nil, fmt.Errorf("servers[%d]: duplicate server name %q", i, name)
		}

		server, err := ResolveServer(fs)
		if err != nil {
			return nil, err
		}

		servers[name] = server
	}

	return servers, nil
}

// ResolveServer validates one servers: entry — from the config file or
// stored in the state db (see jobs.Manager) — into a ResolvedServer jobs'
// targets can be resolved against: a non-empty name and an explicit, valid
// type: with only the fields that type uses.
func ResolveServer(fs FileServer) (ResolvedServer, error) {
	name := strings.TrimSpace(fs.Name)
	if name == "" {
		return ResolvedServer{}, errors.New("name is required")
	}

	kind, err := parseServerKind(fs.Type)
	if err != nil {
		return ResolvedServer{}, fmt.Errorf("server %q: %w", name, err)
	}

	if kind == ServerKindLocal {
		return buildLocalServer(name, &fs)
	}

	return buildRemoteServer(name, &fs)
}

// buildLocalServer validates and builds a ResolvedServer for a type: local
// servers: entry, which uses only path and retention.
func buildLocalServer(name string, fs *FileServer) (ResolvedServer, error) {
	if strings.TrimSpace(fs.Path) == "" {
		return ResolvedServer{}, fmt.Errorf("server %q: path is required for type: local", name)
	}

	if fs.Endpoint != "" {
		return ResolvedServer{}, fmt.Errorf("server %q: endpoint is not valid for type: local", name)
	}

	retention, err := parseRetention(fs.Retention)
	if err != nil {
		return ResolvedServer{}, fmt.Errorf("server %q: %w", name, err)
	}

	return ResolvedServer{name: name, kind: ServerKindLocal, path: fs.Path, retention: retention}, nil
}

// buildRemoteServer validates and builds a ResolvedServer for a type: remote
// servers: entry, which uses only endpoint — auth is this instance's own
// identity (see loadServerIdentity), not a config field.
func buildRemoteServer(name string, fs *FileServer) (ResolvedServer, error) {
	if strings.TrimSpace(fs.Endpoint) == "" {
		return ResolvedServer{}, fmt.Errorf("server %q: endpoint is required for type: remote", name)
	}

	if fs.Path != "" || fs.Retention != "" {
		return ResolvedServer{}, fmt.Errorf("server %q: path/retention are not valid for type: remote", name)
	}

	return ResolvedServer{name: name, kind: ServerKindRemote, endpoint: fs.Endpoint}, nil
}

// parseRetention parses a local server's retention: string into a
// time.Duration. An empty string means no automatic expiry (the zero
// value); a negative duration is rejected since "delete files from the
// future" isn't meaningful.
func parseRetention(s string) (time.Duration, error) {
	return parseOptionalDayDuration("retention", s, true)
}

// parseOptionalDayDuration parses field's duration string s via
// parseDayDuration; an empty s means unset, returning the zero duration.
// allowZero permits a zero duration (rejecting only negative values, for
// retention:, where "delete from the future" isn't meaningful); when false,
// zero is rejected too (for stale-after:, where "stale after zero time" is
// always true and so isn't a meaningful setting). Shared by parseRetention
// and parseStaleAfter (receiver.go), which otherwise duplicate this trim/
// empty/parse/sign-check shape with only the field name and boundary
// differing.
func parseOptionalDayDuration(field, s string, allowZero bool) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	d, err := parseDayDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parsing %s %q: %w", field, s, err)
	}

	if allowZero && d < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %q", field, s)
	}

	if !allowZero && d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", field, s)
	}

	return d, nil
}

// dayUnitRE matches a leading "<number>d" component (e.g. "7d" or "1.5d") of
// a duration string handled by parseDayDuration.
var dayUnitRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)d`)

// parseDayDuration is time.ParseDuration extended with a "d" (day) unit,
// which the standard library doesn't support: e.g. "30d" or "1d12h" for
// go-backup-tool's retention: (time.ParseDuration's "m" already means
// minutes, so that unit needs no extra support). A day component, if
// present, must come first, mirroring time.ParseDuration's largest-to-
// smallest unit ordering; whatever follows it (if anything) is parsed by
// time.ParseDuration as usual and added on.
func parseDayDuration(s string) (time.Duration, error) {
	rest := s

	neg := false

	switch {
	case strings.HasPrefix(rest, "-"):
		neg, rest = true, rest[1:]
	case strings.HasPrefix(rest, "+"):
		rest = rest[1:]
	}

	if rest == "" {
		return 0, fmt.Errorf("invalid duration %q", s)
	}

	var days float64

	if m := dayUnitRE.FindStringSubmatch(rest); m != nil {
		var err error

		days, err = strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}

		rest = rest[len(m[0]):]
	}

	var rem time.Duration

	if rest != "" {
		var err error

		rem, err = time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
	}

	total := time.Duration(days*24*float64(time.Hour)) + rem
	if neg {
		total = -total
	}

	return total, nil
}

// ResolvedServer is one servers: entry, ready to be combined with a job's
// targetRef bucket into a target.
type ResolvedServer struct {
	name      string
	kind      ServerKind
	endpoint  string
	path      string        // local only: root directory backups are written under
	retention time.Duration // local only: 0 means no automatic expiry
}

// Kind is s's type:.
func (s ResolvedServer) Kind() ServerKind { return s.kind }

// resolveJobTargets resolves cfg's raw target references (targetRefs, from
// targets:) against servers, building cfg.targets, and resolves each ref's
// on-error.command and on-recover.command (if any) against commands (see
// buildCommands) into that target's OnErrorCommand/OnRecoverCommand. A job with no target references at all is left
// with an empty cfg.targets; validateJob reports that as an error.
func resolveJobTargets(cfg *Config, servers map[string]ResolvedServer, commands map[string]Command) error {
	if len(cfg.targetRefs) == 0 {
		return nil
	}

	cfg.Targets = make([]Target, len(cfg.targetRefs))

	for i, ref := range cfg.targetRefs {
		if strings.TrimSpace(ref.server) == "" {
			return fmt.Errorf("targets[%d]: server is required", i)
		}

		if strings.TrimSpace(ref.bucket) == "" {
			return fmt.Errorf("targets[%d]: bucket is required", i)
		}

		server, ok := servers[ref.server]
		if !ok {
			return fmt.Errorf("targets[%d]: no server named %q defined under servers", i, ref.server)
		}

		retention := server.retention

		if ref.retention > 0 {
			if server.kind != ServerKindLocal {
				return fmt.Errorf("targets[%d]: retention is not valid for server %q (type %s; local only)", i, ref.server, server.kind)
			}

			retention = ref.retention
		}

		cfg.Targets[i] = Target{
			ServerName: server.name,
			Kind:       server.kind,
			Bucket:     ref.bucket,
			Endpoint:   server.endpoint,
			LocalPath:  server.path,
			Retention:  retention,
		}

		if ref.onErrorCommandID != "" {
			command, ok := commands[ref.onErrorCommandID]
			if !ok {
				return fmt.Errorf("targets[%d]: no command named %q defined under commands", i, ref.onErrorCommandID)
			}

			if ref.onErrorAfter <= 0 {
				return fmt.Errorf("targets[%d]: on-error.after must be a positive integer, got %d", i, ref.onErrorAfter)
			}

			cfg.Targets[i].OnErrorCommand = &command
			cfg.Targets[i].OnErrorAfter = ref.onErrorAfter
			cfg.Targets[i].OnErrorOnce = ref.onErrorOnce
		}

		if ref.onRecoverCommandID != "" {
			command, ok := commands[ref.onRecoverCommandID]
			if !ok {
				return fmt.Errorf("targets[%d]: no command named %q defined under commands", i, ref.onRecoverCommandID)
			}

			cfg.Targets[i].OnRecoverCommand = &command
		}
	}

	return nil
}

// newConfigDefaults returns a *config with the built-in defaults applied to
// every job before its config file fields are layered on top.
func newConfigDefaults() *Config {
	return &Config{
		Key:    appconfig.DefaultKeyPattern,
		GPGBin: appconfig.DefaultGPGBin,
	}
}

// prepareJobs narrows jobs to the one named by jobFilter (if any) and
// validates them.
func prepareJobs(jobs []*Config, jobFilter string) ([]*Config, error) {
	jobs, err := applyJobFilter(jobs, jobFilter)
	if err != nil {
		return nil, err
	}

	if err := validateJobs(jobs); err != nil {
		return nil, err
	}

	return jobs, nil
}

// applyJobFilter applies -job, if given, restricting jobs to the single
// named job — none, if the config file doesn't define it: it may be stored
// in the state db instead (see RunConfig.JobFilter).
func applyJobFilter(jobs []*Config, jobFilter string) ([]*Config, error) {
	if jobFilter == "" {
		return jobs, nil
	}

	for _, j := range jobs {
		if j.Name == jobFilter {
			return []*Config{j}, nil
		}
	}

	return nil, nil
}

// validateJobs validates every job, returning the first error found.
func validateJobs(jobs []*Config) error {
	for _, j := range jobs {
		if err := validateJob(j); err != nil {
			return err
		}
	}

	return nil
}

// validateJob checks that a single job's parameters are complete and
// self-consistent.
func validateJob(cfg *Config) error {
	if err := checkJob(cfg); err != nil {
		return JobError(cfg, err)
	}

	return nil
}

// checkJob is validateJob without the job name prefix, for ResolveJob,
// whose caller already knows which job it asked about.
func checkJob(cfg *Config) error {
	switch {
	case strings.TrimSpace(cfg.Cmd) == "":
		return errors.New("cmd is required")
	case len(cfg.Targets) == 0:
		return errors.New("at least one target is required (see targets: and servers:)")
	case len(cfg.Recipients) == 0:
		return errors.New("specify at least one recipient")
	case cfg.Interval < 0:
		return errors.New("interval must not be negative")
	case !cfg.StartTime.IsZero() && cfg.Interval <= 0:
		return errors.New("start-time requires interval")
	}

	return nil
}

// ResolveJob builds and validates one job from fj — a stored job, managed in
// the web UI (see jobs.Manager) — on top of the built-in defaults and gpg
// (the config file's top-level gpg-bin/gpg-homedir, which fj's own override),
// resolving its targets against servers and commands (see ResolveServer/
// ResolveCommand). serverName is copied onto the job (see
// Config.ServerName). fj.FailureNotifications ids aren't checked here.
func ResolveJob(fj FileJob, gpg notify.GPGSettings, servers map[string]ResolvedServer, commands map[string]Command, serverName string) (*Config, error) {
	name := strings.TrimSpace(fj.Name)
	if name == "" {
		return nil, errors.New("name is required")
	}

	cfg := newConfigDefaults()
	cfg.Name = name
	cfg.ServerName = serverName

	applyString(&cfg.GPGBin, gpg.Bin)
	applyString(&cfg.GPGHomedir, gpg.Homedir)

	if err := applyFileJob(cfg, &fj); err != nil {
		return nil, err
	}

	if err := resolveJobTargets(cfg, servers, commands); err != nil {
		return nil, err
	}

	if err := checkJob(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// flattenFileJobs flattens every one of fileCfg's jobs: entries (see
// flattenFileJob).
func flattenFileJobs(fileCfg *fileConfig) []FileJob {
	out := make([]FileJob, len(fileCfg.Jobs))
	for i, fj := range fileCfg.Jobs {
		out[i] = flattenFileJob(fileCfg.FileJob, fj)
	}

	return out
}

// flattenFileJob layers fj (one jobs: entry) over defaults (the config
// file's top-level job fields), the same way buildJobsFromFile does, into
// the single self-contained definition jobs.Manager imports into the state
// db. gpg-bin/gpg-homedir are left as fj's own: the top-level ones stay a
// config file setting every stored job falls back to (see ResolveJob).
func flattenFileJob(defaults, fj FileJob) FileJob {
	out := fj
	out.Name = strings.TrimSpace(fj.Name)

	for _, f := range []struct {
		dst *string
		def string
	}{
		{&out.Cmd, defaults.Cmd},
		{&out.Key, defaults.Key},
		{&out.Interval, defaults.Interval},
		{&out.StartTime, defaults.StartTime},
		{&out.StagingDir, defaults.StagingDir},
	} {
		if *f.dst == "" {
			*f.dst = f.def
		}
	}

	if len(out.Targets) == 0 {
		out.Targets = slices.Clone(defaults.Targets)
	}

	if len(out.Recipients) == 0 {
		out.Recipients = slices.Clone(defaults.Recipients)
	}

	if len(out.FailureNotifications) == 0 {
		out.FailureNotifications = slices.Clone(defaults.FailureNotifications)
	}

	out.Armor = fj.Armor || defaults.Armor

	return out
}

// JobError prefixes err with cfg's job name, so validation errors are
// attributable to the job that caused them.
func JobError(cfg *Config, err error) error {
	return fmt.Errorf("job %q: %w", cfg.Name, err)
}

// loadFileConfig reads and parses the YAML config file at path, rejecting
// any key that doesn't map to a known field (a typo, or an option that has
// since been removed, would otherwise be silently ignored). If explicit is
// false (the caller didn't pass -config), a missing file at the default
// path is not an error and loadFileConfig returns (nil, nil).
func loadFileConfig(path string, explicit bool) (*fileConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied CLI config (-config flag or its default), not untrusted input
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	var fc fileConfig

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	// io.EOF means the file is empty (or only comments): an empty config,
	// same as yaml.Unmarshal would have produced.
	if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	return &fc, nil
}

// applyString sets *dst to val if val is non-empty.
func applyString(dst *string, val string) {
	if val != "" {
		*dst = val
	}
}

// applyBool sets *dst to val if val is true (false is indistinguishable
// from unset, but every bool field here already defaults to false).
func applyBool(dst *bool, val bool) {
	if val {
		*dst = val
	}
}

// newJobTargetRef parses one targets: entry into its raw jobTargetRef,
// leaving server/command id resolution to resolveJobTargets.
func newJobTargetRef(t FileJobTarget) (jobTargetRef, error) {
	retention, err := parseRetention(t.Retention)
	if err != nil {
		return jobTargetRef{}, err
	}

	ref := jobTargetRef{server: t.Server, bucket: t.Bucket, retention: retention}

	if t.OnError != nil {
		commandID := strings.TrimSpace(t.OnError.Command)
		if commandID == "" {
			return jobTargetRef{}, errors.New("on-error.command is required")
		}

		ref.onErrorCommandID = commandID
		ref.onErrorAfter = t.OnError.After
		ref.onErrorOnce = t.OnError.Repeat != nil && !*t.OnError.Repeat
	}

	if t.OnRecover != nil {
		commandID := strings.TrimSpace(t.OnRecover.Command)
		if commandID == "" {
			return jobTargetRef{}, errors.New("on-recover.command is required")
		}

		ref.onRecoverCommandID = commandID
	}

	return ref, nil
}

// applyFileJob fills any field of cfg that fj sets, leaving the rest (its
// current value, typically a built-in default or a shared top-level
// default already applied) untouched. fj.FailureNotifications ids are
// copied as-is: they're checked against the live notification registry at
// startup and looked up there when the job fails (see
// Config.FailureNotifications).
func applyFileJob(cfg *Config, fj *FileJob) error {
	applyString(&cfg.Cmd, fj.Cmd)
	applyString(&cfg.Key, fj.Key)
	applyString(&cfg.GPGBin, fj.GPGBin)
	applyString(&cfg.GPGHomedir, fj.GPGHomedir)
	applyString(&cfg.StagingDir, fj.StagingDir)

	applyBool(&cfg.Armor, fj.Armor)

	if len(fj.Targets) > 0 {
		cfg.targetRefs = make([]jobTargetRef, len(fj.Targets))

		for i, t := range fj.Targets {
			ref, err := newJobTargetRef(t)
			if err != nil {
				return fmt.Errorf("targets[%d]: %w", i, err)
			}

			cfg.targetRefs[i] = ref
		}
	}

	if len(fj.Recipients) > 0 {
		cfg.Recipients = append(stringSlice(nil), fj.Recipients...)
	}

	if len(fj.FailureNotifications) > 0 {
		cfg.FailureNotifications = slices.Clone(fj.FailureNotifications)
	}

	if fj.Interval != "" {
		d, err := time.ParseDuration(fj.Interval)
		if err != nil {
			return fmt.Errorf("parsing interval %q: %w", fj.Interval, err)
		}

		cfg.Interval = d
	}

	if fj.StartTime != "" {
		t, err := time.Parse(time.RFC3339, fj.StartTime)
		if err != nil {
			return fmt.Errorf("parsing start-time %q: %w", fj.StartTime, err)
		}

		// Normalized to UTC: the grid is the same instant either way, but
		// every schedule (and every next-run time it reports) is UTC-based.
		cfg.StartTime = t.UTC()
	}

	return nil
}
