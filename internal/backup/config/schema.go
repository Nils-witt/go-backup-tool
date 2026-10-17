package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
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
	Name string `yaml:"name" json:"name"`

	// Files are the backup's files, each one a commands: entry (by id) whose
	// encrypted output is stored under the backup's key, run in order. The
	// key is files:, not commands:, since the top-level job defaults share
	// the config file's top level with the commands: list itself.
	Files []FileJobFile `yaml:"files" json:"files"`

	// Command is DEPRECATED: a job's single command, the same as Files with
	// just that one entry. Setting both is an error.
	Command string `yaml:"command" json:"command,omitempty"`

	// Cmd is DEPRECATED: a job's own inline shell command. ParseFlags turns
	// it into a commands: entry of its own (see convertInlineJobCommands),
	// so it never reaches anything past config parsing.
	Cmd string `yaml:"cmd" json:"-"`

	Key string `yaml:"key" json:"key"`

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

// FileJobFile is one entry of a job's files:. Command names a top-level
// commands: entry; Name (optional) is the file its encrypted output is
// stored as under the backup's key, defaulting to the command's id plus
// ".gpg" (".asc" with armor). Names must be unique within a job.
type FileJobFile struct {
	Command string `yaml:"command" json:"command"`
	Name    string `yaml:"name" json:"name,omitempty"`
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
// not a config field — plus, optionally, server-uuid: the destination
// instance's own server UUID (shown on its Identity page), recording which
// instance the endpoint reaches. type: is required; there is no default.
// Retention (local only) is a duration string (e.g. "7d" or "168h" for 7
// days, "30m" for 30 minutes) — like time.ParseDuration but with "d" also
// accepted for days (parsed by parseDayDuration, since the standard library
// has no day unit; "m" is already minutes in time.ParseDuration, not
// months); when set, any object this tool writes under path is deleted once
// it's older than that, tracked in the shared state sqlite database kept
// alongside the config file (see objectstore/retention.go and the store
// package). Unset or "0" disables automatic cleanup. A remote server needs no auth field of its
// own: it authenticates to the destination instance's receiver API with
// this instance's own persistent identity (see loadServerIdentity), which
// the destination instance verifies against the public key configured on
// its matching receivers: entry's public-key: (see fileReceiver).
type FileServer struct {
	Name       string `yaml:"name" json:"name"`
	Type       string `yaml:"type" json:"type"`
	Endpoint   string `yaml:"endpoint" json:"endpoint"`
	ServerUUID string `yaml:"server-uuid" json:"server_uuid"` // remote only, optional: the destination instance's server UUID
	Path       string `yaml:"path" json:"path"`               // local only: root directory backups are written under
	Retention  string `yaml:"retention" json:"retention"`     // local only: e.g. "7d" or "168h"; unset/"0" keeps objects forever

	// TLS verification of an https endpoint (remote only; see TargetTLS).
	// TLSInsecureSkipVerify accepts any certificate and excludes the other
	// two. TLSSkipHostnameVerify still verifies the chain but not that it
	// names the endpoint's host. TLSCA is PEM certificates that replace the
	// system roots.
	TLSInsecureSkipVerify bool   `yaml:"tls-insecure-skip-verify" json:"tls_insecure_skip_verify"`
	TLSSkipHostnameVerify bool   `yaml:"tls-skip-hostname-verify" json:"tls_skip_hostname_verify"`
	TLSCA                 string `yaml:"tls-ca" json:"tls_ca"`
}

// FileCommand is one top-level commands: entry, defined once and referenced
// by id from a target's on-error.command or on-recover.command — the same "define once, reference
// by id" shape as notify.FileNotification. Cmd is run through the platform
// shell, the same way a job's own cmd: is (see pipeline.newSourceCommand).
// Timeout (optional) bounds how long one firing may run; defaults to
// defaultOnErrorCommandTimeout when unset; for a job's source, unset means no
// limit of its own. Container (an already-running container's name or id)
// runs Cmd inside it through the Docker daemon (see fileConfig.DockerSocket)
// instead, via "sh -c" — so the container needs a sh — as ContainerUser
// (user[:group]) when set.
//
// Type selects what the command does: "shell" (the default when unset) runs
// Cmd as above; "tar" instead archives Path — a file or directory on this
// machine — in-process (see the tarball package), leaving out entries that
// match an Exclude glob, gzip-compressed when Compress is "gzip". A tar
// command sets none of Cmd/Container/ContainerUser, and a shell command none
// of Path/Exclude/Compress.
type FileCommand struct {
	ID            string   `yaml:"id" json:"id"`
	Type          string   `yaml:"type" json:"type"`
	Cmd           string   `yaml:"cmd" json:"cmd"`
	Timeout       string   `yaml:"timeout" json:"timeout"`
	Container     string   `yaml:"container" json:"container"`
	ContainerUser string   `yaml:"container-user" json:"container_user"`
	Path          string   `yaml:"path" json:"path"`
	Exclude       []string `yaml:"exclude" json:"exclude"`
	Compress      string   `yaml:"compress" json:"compress"`
}

// fileConfig is the top-level shape of the YAML config file. Its embedded
// FileJob holds shared defaults applied to every entry in Jobs before that
// entry's own fields override them.
type fileConfig struct {
	FileJob `yaml:",inline"`

	Timeout  string       `yaml:"timeout"`
	LogLevel string       `yaml:"log-level"` // debug, info, warn, or error; overridden by -log-level when that flag is explicitly given
	KeysDir  string       `yaml:"keys-dir"`  // where this instance's persistent identity (RSA key pair + UUID) is stored; defaults to DefaultKeysDir
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

	// DockerSocket is the Docker daemon a job's or command's container:
	// runs through: a Unix socket path (optionally as unix://<path>) or a
	// plain-HTTP tcp://<host>:<port>. Unset falls back to DOCKER_HOST, then
	// /var/run/docker.sock. Mounting the socket into this tool's own
	// container grants it root-equivalent control of the host.
	DockerSocket string `yaml:"docker-socket"`

	Receivers []FileReceiver    `yaml:"receivers"`
	WebUI     fileWebUI         `yaml:"webui"`
	Report    report.FileReport `yaml:"report"`

	// warnings collects deprecation notices found while resolving the
	// file (see convertInlineJobCommands), surfaced as RunConfig.Warnings.
	warnings []string
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
	// to DefaultEventLogLimit. Must be between 1 and
	// maxEventLogLimit when set.
	EventLogLimit int `yaml:"event-log-limit"`

	// TrustProxyHeaders makes the web UI take the client address recorded
	// in the login/download logs and access log (see clientAddr in
	// webui/middleware.go) from the Forwarded/X-Forwarded-For/X-Real-Ip request
	// headers instead of the raw TCP connection's address, when present.
	// Only enable this when the web UI sits behind a reverse proxy that
	// itself sets these headers and strips any client-supplied copies
	// first — otherwise any client can spoof its own logged address by
	// sending these headers itself. Unset/false (the default) always uses
	// the TCP connection's own address. It also makes the live status
	// WebSocket check the browser's Origin against the host a proxy reports
	// in Forwarded host=/X-Forwarded-Host (see forwardedHost in webui/middleware.go),
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
