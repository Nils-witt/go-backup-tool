// Package config reads go-backup-tool's configuration: CLI flags (-config,
// -job, -log-level) and the YAML config file they name, resolved into a
// RunConfig — the jobs to run, the overall run timeout, and the optional web
// UI/receiver API settings — ready for the rest of the program to consume.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	appconfig "nilswitt.dev/go-backup-tool/internal/backup/app/config"
	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
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
	Name       string // job name, from its jobs: entry; always set
	Cmd        string
	Key        string         // may still contain the {time} placeholder; resolved fresh per run
	targetRefs []jobTargetRef // raw targets: entries, resolved against servers by resolveJobTargets
	Targets    []Target       // resolved destinations; empty until resolveJobTargets runs
	Recipients stringSlice
	Symmetric  bool
	Armor      bool
	GPGBin     string
	GPGHomedir string
	Interval   time.Duration // repeat every interval; 0 runs the job once
	StartTime  time.Time     // anchors the interval grid; zero means "run immediately, then every interval"
	Passphrase string        // resolved from GPG_PASSPHRASE when symmetric

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
}

// ServerKind distinguishes a servers: entry's destination type. There is no
// default: every servers: entry must set an explicit type:.
type ServerKind string

// The ServerKind values a servers: entry's type: can resolve to.
const (
	ServerKindLocal  ServerKind = "local"  // type: local
	ServerKindRemote ServerKind = "remote" // type: remote
)

// parseServerKind validates a fileServer's Type field.
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
}

// RunConfig is the result of ParseFlags: one or more jobs to run, plus the
// overall run timeout and the optional web UI listen address.
type RunConfig struct {
	Jobs       []*Config
	Timeout    time.Duration
	Listen     string // empty disables the web UI; see resolveWebUIListen
	ConfigPath string // where the config file was loaded from; state db lives alongside it
	LogLevel   slog.Level
	Receivers  map[string]ResolvedReceiver // this instance's receiver API entries, keyed by id; see receivers.go

	// KeysDir is where this instance's persistent identity (its RSA key pair
	// and UUID — see loadServerIdentity) is stored. Defaults to
	// defaultServerKeyDir when the config file's top-level keys-dir: is
	// unset.
	KeysDir string

	// WebUIUsername/WebUIPassword, when both set, gate the web UI's
	// /api/... endpoints (including minting a per-receiver file download
	// ticket; not the receiver API, which keeps its own per-receiver
	// public-key-verified JWT auth) behind a login page and a bearer token
	// — see requireWebUISession/handleWebUILogin in webui.go. Empty
	// WebUIUsername disables the check, leaving the web UI open as before.
	WebUIUsername string
	WebUIPassword string

	// LogViewer enables the web UI's live log viewer (served over
	// /api/logs, see handleLogs/newRunLogger). Off by default: unless
	// WebUIUsername/WebUIPassword above are set, the dashboard has no login
	// of its own, so anyone who can reach it would otherwise see this
	// process's raw log output, which may include operator detail (paths,
	// error text) an operator might not want exposed that widely.
	LogViewer bool

	// TrustProxyHeaders, when set, makes the web UI derive the client
	// address it records (login/download logs, access log) from
	// proxy-supplied headers rather than the raw TCP connection — see
	// fileWebUI.TrustProxyHeaders and clientAddr in webui.go.
	TrustProxyHeaders bool

	// OIDC, when its Enabled field is set, lets a browser log into the web
	// UI via an OpenID Connect provider instead of (or alongside, if
	// WebUIUsername/WebUIPassword are also set) the dashboard's own
	// username/password form — see newOIDCAuth/handleOIDCLogin/
	// handleOIDCCallback in oidc.go. Any account the provider itself
	// authenticates is let in: this doesn't further restrict who's allowed
	// by email or domain, so scoping who can authenticate is left to the
	// provider (e.g. a dedicated app registration or realm).
	OIDC OIDCSettings

	// Report, when its enabled field is set, sends a daily email summarizing
	// this instance's receiver activity (files received per receiver, any
	// errors, and any receiver currently stale) — see report.go. Independent
	// of the web UI: a daily report is useful for anyone monitoring
	// receivers by inbox, not just those watching the dashboard.
	Report report.Settings
}

// OIDCSettings is runConfig's resolved form of the config file's
// webui.oidc: entry (see fileWebUIOIDC), used to build an *oidcAuth (see
// newOIDCAuth in oidc.go) once the web UI starts.
type OIDCSettings struct {
	Enabled      bool
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string

	// DefaultPermissions is granted to every session an SSO login starts
	// (see handleOIDCCallback in oidc.go) — OIDC has no per-account
	// permissions of its own the way a web UI "Users" admin-managed account
	// does (see WebUIUser in webusers.go), so every account the provider
	// lets in gets the same fixed set. Defaults to permission.PermissionView|
	// permission.PermissionDownload (see resolveOIDCSettings) when
	// webui.oidc.default-permissions: is unset, preserving the full access
	// every SSO login had before per-user permissions existed.
	DefaultPermissions permission.Permission
}

// fileJob mirrors config's per-job fields for YAML unmarshaling, used both
// for the top-level shared defaults and for each entry under jobs:. Any
// field left unset falls through to the built-in default (top-level) or the
// top-level value (a jobs: entry).
//
// A job names its upload destination(s) via targets:, each entry a
// {server, bucket} pair referencing a servers: entry defined at the top
// level (see fileServer) — server connection details (endpoint) live there,
// not on the job. A targets: entry may also set its own retention: (local
// servers only), overriding the server's for that job's writes to that
// target — see fileJobTarget.
type fileJob struct {
	Name       string          `yaml:"name"`
	Cmd        string          `yaml:"cmd"`
	Key        string          `yaml:"key"`
	Targets    []fileJobTarget `yaml:"targets"`
	Recipients []string        `yaml:"recipients"`
	Symmetric  bool            `yaml:"symmetric"`
	Armor      bool            `yaml:"armor"`
	GPGBin     string          `yaml:"gpg-bin"`
	GPGHomedir string          `yaml:"gpg-homedir"`
	Interval   string          `yaml:"interval"`
	StartTime  string          `yaml:"start-time"`
	StagingDir string          `yaml:"staging-dir"`
}

// fileJobTarget mirrors jobTargetRef for YAML unmarshaling. Retention (local
// servers only) overrides, for this job's writes to this target, the
// retention its server otherwise applies — same duration syntax as a
// server's own retention: (see fileServer). Unset keeps the server's
// retention: unchanged; it's an error to set it against a target whose
// server isn't type: local.
type fileJobTarget struct {
	Server    string `yaml:"server"`
	Bucket    string `yaml:"bucket"`
	Retention string `yaml:"retention"`
}

// fileServer is one top-level servers: entry, defined once and referenced by
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
type fileServer struct {
	Name      string `yaml:"name"`
	Type      string `yaml:"type"`
	Endpoint  string `yaml:"endpoint"`
	Path      string `yaml:"path"`      // local only: root directory backups are written under
	Retention string `yaml:"retention"` // local only: e.g. "7d" or "168h"; unset/"0" keeps objects forever
}

// fileConfig is the top-level shape of the YAML config file. Its embedded
// fileJob holds shared defaults applied to every entry in Jobs before that
// entry's own fields override them.
type fileConfig struct {
	fileJob `yaml:",inline"`

	Timeout   string            `yaml:"timeout"`
	LogLevel  string            `yaml:"log-level"` // debug, info, warn, or error; overridden by -log-level when that flag is explicitly given
	KeysDir   string            `yaml:"keys-dir"`  // where this instance's persistent identity (RSA key pair + UUID) is stored; defaults to defaultServerKeyDir
	Servers   []fileServer      `yaml:"servers"`
	Jobs      []fileJob         `yaml:"jobs"`
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

	// Username/Password, when both set, require a browser to log in (at
	// /login, with a session remembered via a bearer token) before the web
	// UI's /api/... endpoints (including minting a per-receiver file
	// download ticket) serve anything — see
	// requireWebUISession/handleWebUILogin in webui.go. Unset (the default)
	// leaves the web UI open, as before.
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	// LogViewer turns on the web UI's live log viewer (a "Logs" section on
	// the dashboard, polling /api/logs). Unset/false (the default) keeps it
	// off, since the dashboard has no login guarding it on its own unless
	// Username/Password above are also set.
	LogViewer bool `yaml:"log-viewer"`

	// TrustProxyHeaders makes the web UI take the client address recorded
	// in the login/download logs and access log (see clientAddr in
	// webui.go) from the Forwarded/X-Forwarded-For/X-Real-Ip request
	// headers instead of the raw TCP connection's address, when present.
	// Only enable this when the web UI sits behind a reverse proxy that
	// itself sets these headers and strips any client-supplied copies
	// first — otherwise any client can spoof its own logged address by
	// sending these headers itself. Unset/false (the default) always uses
	// the TCP connection's own address.
	TrustProxyHeaders bool `yaml:"trust-proxy-headers"`

	// OIDC configures Single Sign-On via an OpenID Connect provider,
	// alongside (or instead of) Username/Password above — see
	// fileWebUIOIDC.
	OIDC fileWebUIOIDC `yaml:"oidc"`
}

// fileWebUIOIDC is the webui.oidc: entry, configuring Single Sign-On for the
// web UI dashboard via an OpenID Connect provider (Google, Okta, Keycloak, a
// generic OIDC-compliant IdP, ...). When Enabled, the dashboard's login page
// (see renderLoginPage) shows a "Log in with SSO" link alongside its
// username/password form (if Username/Password are also set), taking the
// browser through the provider's own login before starting the same kind of
// dashboard session a password login would (see handleOIDCLogin/
// handleOIDCCallback in oidc.go). Any account the provider itself
// authenticates is let in.
type fileWebUIOIDC struct {
	Enabled bool `yaml:"enabled"`

	// Issuer is the provider's issuer URL (e.g.
	// "https://accounts.google.com"), used to discover its authorization,
	// token, and JWKS endpoints via OpenID Connect Discovery
	// (/.well-known/openid-configuration).
	Issuer string `yaml:"issuer"`

	// ClientID/ClientSecret identify this application to the provider, as
	// issued when registering it there. ClientSecret is written directly in
	// this file rather than read from the environment, so protect this
	// file's permissions accordingly.
	ClientID     string `yaml:"client-id"`
	ClientSecret string `yaml:"client-secret"`

	// RedirectURL is the callback URL registered with the provider that it
	// redirects back to after a login — this instance's own address plus
	// /login/oidc/callback (e.g.
	// "https://backups.example.com/login/oidc/callback").
	RedirectURL string `yaml:"redirect-url"`

	// Scopes are the OpenID Connect scopes requested at login, in addition
	// to "openid" (always requested, and required by the protocol). Unset
	// defaults to {"profile", "email"}, enough for most providers to return
	// a usable display name.
	Scopes []string `yaml:"scopes"`

	// DefaultPermissions lists the dashboard permissions ("view", "download",
	// "admin", "login-log", and/or "download-log") granted to every session
	// an SSO login starts — see OIDCSettings.DefaultPermissions. Unset
	// defaults to "view" and "download", matching the full access every SSO
	// login had before per-user permissions existed; "admin", "login-log",
	// and "download-log" are never defaulted in.
	DefaultPermissions []string `yaml:"default-permissions"`
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

	listen, oidc, err := resolveWebUISettings(fileCfg.WebUI)
	if err != nil {
		return nil, err
	}

	jobs, err := resolveJobs(fileCfg, listen)
	if err != nil {
		return nil, err
	}

	jobs, err = prepareJobs(jobs, jobFilter)
	if err != nil {
		return nil, err
	}

	receivers, err := buildReceivers(fileCfg.Receivers)
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

	report, err := report.ResolveSettings(fileCfg.Report)
	if err != nil {
		return nil, err
	}

	return &RunConfig{
		Jobs:              jobs,
		Timeout:           timeout,
		Listen:            listen,
		ConfigPath:        configPath,
		LogLevel:          level,
		Receivers:         receivers,
		KeysDir:           keysDir,
		WebUIUsername:     strings.TrimSpace(fileCfg.WebUI.Username),
		WebUIPassword:     fileCfg.WebUI.Password,
		LogViewer:         fileCfg.WebUI.LogViewer,
		TrustProxyHeaders: fileCfg.WebUI.TrustProxyHeaders,
		OIDC:              oidc,
		Report:            report,
	}, nil
}

// resolveWebUISettings resolves cfg (the config file's webui: entry) into
// its listen address (see resolveWebUIListen) and its SSO settings (see
// resolveOIDCSettings), the two pieces of runConfig ParseFlags derives from
// webui: that need validation beyond a plain field copy.
func resolveWebUISettings(cfg fileWebUI) (listen string, oidc OIDCSettings, err error) {
	listen, err = resolveWebUIListen(cfg)
	if err != nil {
		return "", OIDCSettings{}, err
	}

	oidc, err = resolveOIDCSettings(cfg.OIDC, listen)
	if err != nil {
		return "", OIDCSettings{}, err
	}

	return listen, oidc, nil
}

// resolveOIDCSettings validates cfg (the config file's webui.oidc: entry)
// against listen (the web UI's resolved listen address, empty if the web UI
// itself is disabled — see resolveWebUIListen) and returns runConfig's
// resolved oidcSettings. An unset/false cfg.Enabled returns the zero value,
// leaving SSO disabled. It's an error to enable OIDC without the web UI
// itself enabled (there'd be no dashboard to log into), or without every one
// of issuer/client-id/client-secret/redirect-url set, since newOIDCAuth
// needs all four to talk to the provider.
func resolveOIDCSettings(cfg fileWebUIOIDC, listen string) (OIDCSettings, error) {
	if !cfg.Enabled {
		return OIDCSettings{}, nil
	}

	if listen == "" {
		return OIDCSettings{}, errors.New("webui.oidc.enabled is true but webui.enabled is not")
	}

	issuer := strings.TrimSpace(cfg.Issuer)
	clientID := strings.TrimSpace(cfg.ClientID)
	redirectURL := strings.TrimSpace(cfg.RedirectURL)

	required := []struct{ name, val string }{
		{"issuer", issuer},
		{"client-id", clientID},
		{"client-secret", cfg.ClientSecret},
		{"redirect-url", redirectURL},
	}

	for _, r := range required {
		if r.val == "" {
			return OIDCSettings{}, fmt.Errorf("webui.oidc.enabled is true but webui.oidc.%s is not set", r.name)
		}
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"profile", "email"}
	}

	defaultPerm := permission.PermissionView | permission.PermissionDownload

	if len(cfg.DefaultPermissions) > 0 {
		parsed, err := permission.ParsePermissions(cfg.DefaultPermissions)
		if err != nil {
			return OIDCSettings{}, fmt.Errorf("webui.oidc.default-permissions: %w", err)
		}

		defaultPerm = parsed
	}

	return OIDCSettings{
		Enabled:            true,
		Issuer:             issuer,
		ClientID:           clientID,
		ClientSecret:       cfg.ClientSecret,
		RedirectURL:        redirectURL,
		Scopes:             scopes,
		DefaultPermissions: defaultPerm,
	}, nil
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
// listen is the web UI's resolved effective listen address (see
// resolveWebUIListen).
//
// An empty jobs: list is only allowed when the web UI is enabled, since that
// still leaves the web UI (and receiver API) as a reason to run; otherwise
// the process would start and immediately have nothing to do.
func resolveJobs(fileCfg *fileConfig, listen string) ([]*Config, error) {
	if len(fileCfg.Jobs) == 0 && listen == "" {
		return nil, errors.New("config file must define at least one job under a jobs list, or set webui.enabled: true to run without any")
	}

	return buildJobsFromFile(fileCfg)
}

// buildJobsFromFile builds one *config per entry in fileCfg.Jobs, layering
// fileCfg's top-level fields as defaults under each entry's own fields and
// resolving each job's targets: against fileCfg.Servers.
func buildJobsFromFile(fileCfg *fileConfig) ([]*Config, error) {
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

		if err := applyFileJob(cfg, &fileCfg.fileJob); err != nil {
			return nil, fmt.Errorf("job %q: %w", name, err)
		}

		if err := applyFileJob(cfg, &fj); err != nil {
			return nil, fmt.Errorf("job %q: %w", name, err)
		}

		if err := resolveJobTargets(cfg, servers); err != nil {
			return nil, fmt.Errorf("job %q: %w", name, err)
		}

		jobs = append(jobs, cfg)
	}

	return jobs, nil
}

// buildServers resolves fileServers into a name -> resolvedServer map,
// validating that every entry has a non-empty, unique name and an explicit,
// valid type:.
func buildServers(fileServers []fileServer) (map[string]resolvedServer, error) {
	servers := make(map[string]resolvedServer, len(fileServers))

	for i, fs := range fileServers {
		name := strings.TrimSpace(fs.Name)
		if name == "" {
			return nil, fmt.Errorf("servers[%d]: name is required", i)
		}

		if _, exists := servers[name]; exists {
			return nil, fmt.Errorf("servers[%d]: duplicate server name %q", i, name)
		}

		kind, err := parseServerKind(fs.Type)
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", name, err)
		}

		var server resolvedServer

		switch kind {
		case ServerKindLocal:
			server, err = buildLocalServer(name, &fs)
		case ServerKindRemote:
			server, err = buildRemoteServer(name, &fs)
		}

		if err != nil {
			return nil, err
		}

		servers[name] = server
	}

	return servers, nil
}

// buildLocalServer validates and builds a resolvedServer for a type: local
// servers: entry, which uses only path and retention.
func buildLocalServer(name string, fs *fileServer) (resolvedServer, error) {
	if strings.TrimSpace(fs.Path) == "" {
		return resolvedServer{}, fmt.Errorf("server %q: path is required for type: local", name)
	}

	if fs.Endpoint != "" {
		return resolvedServer{}, fmt.Errorf("server %q: endpoint is not valid for type: local", name)
	}

	retention, err := parseRetention(fs.Retention)
	if err != nil {
		return resolvedServer{}, fmt.Errorf("server %q: %w", name, err)
	}

	return resolvedServer{name: name, kind: ServerKindLocal, path: fs.Path, retention: retention}, nil
}

// buildRemoteServer validates and builds a resolvedServer for a type: remote
// servers: entry, which uses only endpoint — auth is this instance's own
// identity (see loadServerIdentity), not a config field.
func buildRemoteServer(name string, fs *fileServer) (resolvedServer, error) {
	if strings.TrimSpace(fs.Endpoint) == "" {
		return resolvedServer{}, fmt.Errorf("server %q: endpoint is required for type: remote", name)
	}

	if fs.Path != "" || fs.Retention != "" {
		return resolvedServer{}, fmt.Errorf("server %q: path/retention are not valid for type: remote", name)
	}

	return resolvedServer{name: name, kind: ServerKindRemote, endpoint: fs.Endpoint}, nil
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

// resolvedServer is one servers: entry, ready to be combined with a job's
// targetRef bucket into a target.
type resolvedServer struct {
	name      string
	kind      ServerKind
	endpoint  string
	path      string        // local only: root directory backups are written under
	retention time.Duration // local only: 0 means no automatic expiry
}

// resolveJobTargets resolves cfg's raw target references (targetRefs, from
// targets:) against servers, building cfg.targets. A job with no target
// references at all is left with an empty cfg.targets; validateJob reports
// that as an error.
func resolveJobTargets(cfg *Config, servers map[string]resolvedServer) error {
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

// prepareJobs narrows jobs to the one named by jobFilter (if any), validates
// them, and resolves their passphrases.
func prepareJobs(jobs []*Config, jobFilter string) ([]*Config, error) {
	jobs, err := applyJobFilter(jobs, jobFilter)
	if err != nil {
		return nil, err
	}

	if err := validateJobs(jobs); err != nil {
		return nil, err
	}

	if err := resolvePassphrases(jobs); err != nil {
		return nil, err
	}

	return jobs, nil
}

// applyJobFilter applies -job, if given, restricting jobs to the single
// named job. It's an error to name a job that doesn't exist.
func applyJobFilter(jobs []*Config, jobFilter string) ([]*Config, error) {
	if jobFilter == "" {
		return jobs, nil
	}

	for _, j := range jobs {
		if j.Name == jobFilter {
			return []*Config{j}, nil
		}
	}

	return nil, fmt.Errorf("-job %q: no such job in config file", jobFilter)
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
	switch {
	case strings.TrimSpace(cfg.Cmd) == "":
		return JobError(cfg, errors.New("cmd is required"))
	case len(cfg.Targets) == 0:
		return JobError(cfg, errors.New("at least one target is required (see targets: and servers:)"))
	case cfg.Symmetric && len(cfg.Recipients) > 0:
		return JobError(cfg, errors.New("symmetric cannot be combined with recipients"))
	case !cfg.Symmetric && len(cfg.Recipients) == 0:
		return JobError(cfg, errors.New("specify at least one recipient, or set symmetric: true"))
	case cfg.Interval < 0:
		return JobError(cfg, errors.New("interval must not be negative"))
	case !cfg.StartTime.IsZero() && cfg.Interval <= 0:
		return JobError(cfg, errors.New("start-time requires interval"))
	}

	return nil
}

// JobError prefixes err with cfg's job name, so validation and passphrase
// errors are attributable to the job that caused them.
func JobError(cfg *Config, err error) error {
	return fmt.Errorf("job %q: %w", cfg.Name, err)
}

// resolvePassphrases reads GPG_PASSPHRASE once (if any job needs it) and
// assigns it to every symmetric job, then clears it from the environment.
// All symmetric jobs in a run share the same passphrase; per-job passphrases
// aren't supported.
func resolvePassphrases(jobs []*Config) error {
	needsPassphrase := false

	for _, j := range jobs {
		if j.Symmetric {
			needsPassphrase = true

			break
		}
	}

	if !needsPassphrase {
		return nil
	}

	passphrase := os.Getenv("GPG_PASSPHRASE")
	if passphrase == "" {
		return errors.New("symmetric: true requires the GPG_PASSPHRASE environment variable to be set")
	}
	// Cleared once captured so it doesn't linger in this process's own
	// environment any longer than necessary; runPipeline also strips it
	// explicitly from every child process's environment. Unsetenv only
	// fails for an invalid (empty) name, which this literal isn't.
	_ = os.Unsetenv("GPG_PASSPHRASE")

	for _, j := range jobs {
		if j.Symmetric {
			j.Passphrase = passphrase
		}
	}

	return nil
}

// loadFileConfig reads and parses the YAML config file at path. If explicit
// is false (the caller didn't pass -config), a missing file at the default
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
	if err := yaml.Unmarshal(data, &fc); err != nil {
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

// applyFileJob fills any field of cfg that fj sets, leaving the rest (its
// current value, typically a built-in default or a shared top-level
// default already applied) untouched.
func applyFileJob(cfg *Config, fj *fileJob) error {
	applyString(&cfg.Cmd, fj.Cmd)
	applyString(&cfg.Key, fj.Key)
	applyString(&cfg.GPGBin, fj.GPGBin)
	applyString(&cfg.GPGHomedir, fj.GPGHomedir)
	applyString(&cfg.StagingDir, fj.StagingDir)

	applyBool(&cfg.Symmetric, fj.Symmetric)
	applyBool(&cfg.Armor, fj.Armor)

	if len(fj.Targets) > 0 {
		cfg.targetRefs = make([]jobTargetRef, len(fj.Targets))

		for i, t := range fj.Targets {
			retention, err := parseRetention(t.Retention)
			if err != nil {
				return fmt.Errorf("targets[%d]: %w", i, err)
			}

			cfg.targetRefs[i] = jobTargetRef{server: t.Server, bucket: t.Bucket, retention: retention}
		}
	}

	if len(fj.Recipients) > 0 {
		cfg.Recipients = append(stringSlice(nil), fj.Recipients...)
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

		cfg.StartTime = t
	}

	return nil
}
