// Package backup implements go-backup-tool's pipeline: run a shell command,
// encrypt its stdout with gpg, stage the ciphertext to a local temp file,
// then upload that file to one or more targets: an S3 (or S3-compatible)
// bucket, a directory on the local filesystem, or another go-backup-tool
// instance's receiver API. Each target uploads (and retries on failure)
// independently from the staged file, so one target's trouble never
// requires re-running the backup command or gpg, and never affects any
// other target.
package backup

import (
	"database/sql"
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

	// Retries is the total number of attempts allowed for each target's
	// upload (1 means no retry) before that target is permanently abandoned:
	// its first attempt happens in-run (see uploadStagedToTargets), and any
	// further attempts happen roughly once a minute afterward, persisted as
	// an outstanding upload and driven by monitorOutstandingUploads
	// (uploadretry.go) rather than an immediate in-run retry loop. Retries
	// are per target: one target exhausting its Retries doesn't affect any
	// other target's attempts, and none of them re-run the backup command or
	// gpg, since every attempt re-reads the same already-staged local file.
	Retries int

	// StateDB is the shared state/retention sqlite database (see
	// schedule_state.go and retention.go), set on each run's own copy of its
	// job's config by runner.runOnce so RecordLocalWrite/RemoveRetentionRecord
	// reach it without every function in the upload call chain needing its
	// own db parameter. Nil disables retention tracking for this run (e.g.
	// the db couldn't be opened at startup) — see RecordLocalWrite.
	StateDB *sql.DB

	// Identity is this instance's own persistent Identity (see
	// loadServerIdentity), set on each run's own copy of its job's config by
	// runner.runOnce the same way stateDB is. uploadToRemote/
	// deleteRemoteObject (pipeline.go) use it to sign a type: remote
	// target's requests. Nil means loadServerIdentity failed at startup (see
	// its own doc comment); any job with a remote target then fails that
	// target's uploads until a later run's Identity loads successfully.
	Identity *ServerIdentity
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

// ServerKind distinguishes a servers: entry's destination type. The zero
// value is serverKindS3, so existing config files (and target literals in
// tests) that never set type: keep working unchanged.
type ServerKind string

// The ServerKind values a servers: entry's type: can resolve to.
const (
	ServerKindS3     ServerKind = ""       // default; type: s3 also selects this
	ServerKindLocal  ServerKind = "local"  // type: local
	ServerKindRemote ServerKind = "remote" // type: remote
)

// parseServerKind validates a fileServer's Type field, defaulting an unset
// or explicit "s3" value to serverKindS3.
func parseServerKind(t string) (ServerKind, error) {
	switch strings.TrimSpace(t) {
	case "", "s3":
		return ServerKindS3, nil
	case string(ServerKindLocal):
		return ServerKindLocal, nil
	case string(ServerKindRemote):
		return ServerKindRemote, nil
	default:
		return "", fmt.Errorf("unknown type %q (want \"s3\", %q, or %q)", t, ServerKindLocal, ServerKindRemote)
	}
}

// serverKindLabel names kind for an error message, since serverKindS3's
// zero value ("") is never what a config file author actually wrote.
func serverKindLabel(kind ServerKind) string {
	if kind == ServerKindS3 {
		return "s3"
	}

	return string(kind)
}

// Target is one upload destination for a job, fully resolved from a
// jobTargetRef against its named server. A job uploads the same encrypted
// object to every one of its targets. Its kind determines which of the
// fields below apply: s3 (the default) uses bucket/region/endpoint/
// pathStyle/credentials; local uses only bucket (as a subdirectory of
// localPath) and localPath itself; remote uses bucket (as the id sent to
// the destination instance) and endpoint. A remote Target authenticates
// with the run's own cfg.identity (see uploadToRemote/deleteRemoteObject in
// pipeline.go), not a field on Target itself.
type Target struct {
	ServerName string // the servers: entry this came from, for diagnostics
	Kind       ServerKind
	Bucket     string
	Region     string
	Endpoint   string
	PathStyle  bool

	// accessKeyEnv/secretKeyEnv are the server's configured env var names
	// (set at config-build time); accessKey/secretKey are their resolved
	// values, filled in later by resolveTargetCredentials. Both empty means
	// no static credentials: newS3Client falls back to the AWS SDK's
	// default credential chain. Unused for a local target.
	accessKeyEnv string
	secretKeyEnv string
	AccessKey    string
	SecretKey    string

	// LocalPath is the local server's root directory (only set when
	// kind == serverKindLocal). The object is written to
	// LocalPath/bucket/key, mirroring the S3 bucket/key layout.
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
	Receivers  map[string]ResolvedReceiver // this instance's receiver API entries, keyed by id; see receiver.go

	// KeysDir is where this instance's persistent identity (its RSA key pair
	// and UUID — see loadServerIdentity) is stored. Defaults to
	// defaultServerKeyDir when the config file's top-level keys-dir: is
	// unset.
	KeysDir string

	// WebUIUsername/WebUIPassword, when both set, gate the entire web UI
	// (the dashboard and its /api/... endpoints, including per-receiver
	// file downloads; not the receiver API, which keeps its own
	// per-receiver public-key-verified JWT auth) behind a login page and
	// session cookie — see requireWebUISession/handleWebUILogin in
	// webui.go. Empty
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
	Report ReportSettings
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
}

// Built-in defaults for fields a job's or server's config file entry
// doesn't set.
const (
	defaultConfigPath = "config.yaml" // used when -config isn't given explicitly
	defaultKeyPattern = "backup-{time}.gpg"
	defaultRegion     = "us-east-1"
	defaultGPGBin     = "gpg"
	defaultRetries    = 3
)

// fileJob mirrors config's per-job fields for YAML unmarshaling, used both
// for the top-level shared defaults and for each entry under jobs:. Any
// field left unset falls through to the built-in default (top-level) or the
// top-level value (a jobs: entry).
//
// A job names its upload destination(s) via targets:, each entry a
// {server, bucket} pair referencing a servers: entry defined at the top
// level (see fileServer) — server connection details (region, endpoint,
// path-style, credentials) live there, not on the job. A targets: entry may
// also set its own retention: (local servers only), overriding the
// server's for that job's writes to that target — see fileJobTarget.
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
	Retries    int             `yaml:"retries"`
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
// "s3" (the default) for an S3 (or S3-compatible) endpoint, using
// region/endpoint/path-style/access-key-env/secret-key-env; "local" for a
// directory on the local filesystem, using only path; or "remote" for
// another go-backup-tool instance's receiver API, using only endpoint —
// auth is this instance's own identity (see loadServerIdentity), not a
// config field.
// AccessKeyEnv/SecretKeyEnv name environment variables to read static S3
// credentials from (both required together, or neither); like
// GPG_PASSPHRASE, they are never read directly out of the config file
// itself. When neither is set, an s3 server falls back to the AWS SDK's
// default credential chain (env vars, shared config, IAM role, ...).
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
	Name         string `yaml:"name"`
	Type         string `yaml:"type"`
	Region       string `yaml:"region"`
	Endpoint     string `yaml:"endpoint"`
	PathStyle    bool   `yaml:"path-style"`
	AccessKeyEnv string `yaml:"access-key-env"`
	SecretKeyEnv string `yaml:"secret-key-env"`
	Path         string `yaml:"path"`      // local only: root directory backups are written under
	Retention    string `yaml:"retention"` // local only: e.g. "7d" or "168h"; unset/"0" keeps objects forever
}

// fileConfig is the top-level shape of the YAML config file. Its embedded
// fileJob holds shared defaults applied to every entry in Jobs before that
// entry's own fields override them.
type fileConfig struct {
	fileJob `yaml:",inline"`

	Timeout   string         `yaml:"timeout"`
	LogLevel  string         `yaml:"log-level"` // debug, info, warn, or error; overridden by -log-level when that flag is explicitly given
	KeysDir   string         `yaml:"keys-dir"`  // where this instance's persistent identity (RSA key pair + UUID) is stored; defaults to defaultServerKeyDir
	Servers   []fileServer   `yaml:"servers"`
	Jobs      []fileJob      `yaml:"jobs"`
	Receivers []FileReceiver `yaml:"receivers"`
	WebUI     fileWebUI      `yaml:"webui"`
	Report    fileReport     `yaml:"report"`
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
	// /login, with a session remembered via cookie) before the web UI
	// (dashboard and its /api/... endpoints, including per-receiver file
	// downloads) serves anything — see requireWebUISession/handleWebUILogin
	// in webui.go. Unset (the default) leaves the web UI open, as before.
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
	// issued when registering it there. Unlike servers.access-key-env,
	// ClientSecret is written directly in this file rather than read from
	// the environment, so protect this file's permissions accordingly.
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

	fs.StringVar(&configPath, "config", defaultConfigPath, "path to the YAML config file")
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
		keysDir = defaultServerKeyDir
	}

	report, err := resolveReportSettings(fileCfg.Report)
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

	return OIDCSettings{
		Enabled:      true,
		Issuer:       issuer,
		ClientID:     clientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  redirectURL,
		Scopes:       scopes,
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
// validating that every entry has a non-empty, unique name and that
// access-key-env/secret-key-env are set together or not at all. It does
// not read the named environment variables yet: that's deferred to
// resolveTargetCredentials, called only for jobs that survive -job
// filtering, so an unrelated/unused server's credentials don't need to be
// present in the environment for a run that never touches it.
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

		if kind == ServerKindLocal {
			server, err := buildLocalServer(name, &fs)
			if err != nil {
				return nil, err
			}

			servers[name] = server

			continue
		}

		if kind == ServerKindRemote {
			server, err := buildRemoteServer(name, &fs)
			if err != nil {
				return nil, err
			}

			servers[name] = server

			continue
		}

		if fs.Retention != "" {
			return nil, fmt.Errorf("server %q: retention is not valid for type: s3 (local only)", name)
		}

		if (fs.AccessKeyEnv == "") != (fs.SecretKeyEnv == "") {
			return nil, fmt.Errorf("server %q: access-key-env and secret-key-env must be set together", name)
		}

		region := fs.Region
		if region == "" {
			region = defaultRegion
		}

		servers[name] = resolvedServer{
			name:         name,
			region:       region,
			endpoint:     fs.Endpoint,
			pathStyle:    fs.PathStyle,
			accessKeyEnv: fs.AccessKeyEnv,
			secretKeyEnv: fs.SecretKeyEnv,
		}
	}

	return servers, nil
}

// buildLocalServer validates and builds a resolvedServer for a type: local
// servers: entry, which uses only path and none of the S3-specific fields.
func buildLocalServer(name string, fs *fileServer) (resolvedServer, error) {
	if strings.TrimSpace(fs.Path) == "" {
		return resolvedServer{}, fmt.Errorf("server %q: path is required for type: local", name)
	}

	if fs.Endpoint != "" || fs.PathStyle || fs.AccessKeyEnv != "" || fs.SecretKeyEnv != "" {
		return resolvedServer{}, fmt.Errorf("server %q: endpoint/path-style/access-key-env/secret-key-env are not valid for type: local", name)
	}

	retention, err := parseRetention(fs.Retention)
	if err != nil {
		return resolvedServer{}, fmt.Errorf("server %q: %w", name, err)
	}

	return resolvedServer{name: name, kind: ServerKindLocal, path: fs.Path, retention: retention}, nil
}

// buildRemoteServer validates and builds a resolvedServer for a type: remote
// servers: entry, which uses only endpoint and none of the S3/local-specific
// fields — auth is this instance's own identity (see loadServerIdentity),
// not a config field.
func buildRemoteServer(name string, fs *fileServer) (resolvedServer, error) {
	if strings.TrimSpace(fs.Endpoint) == "" {
		return resolvedServer{}, fmt.Errorf("server %q: endpoint is required for type: remote", name)
	}

	if fs.Region != "" || fs.PathStyle || fs.AccessKeyEnv != "" || fs.SecretKeyEnv != "" || fs.Path != "" || fs.Retention != "" {
		return resolvedServer{}, fmt.Errorf("server %q: region/path-style/access-key-env/secret-key-env/path/retention are not valid for type: remote", name)
	}

	return resolvedServer{name: name, kind: ServerKindRemote, endpoint: fs.Endpoint}, nil
}

// parseRetention parses a local server's retention: string into a
// time.Duration. An empty string means no automatic expiry (the zero
// value); a negative duration is rejected since "delete files from the
// future" isn't meaningful.
func parseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	d, err := parseDayDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parsing retention %q: %w", s, err)
	}

	if d < 0 {
		return 0, fmt.Errorf("retention must not be negative, got %q", s)
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

// resolvedServer is one servers: entry after defaulting its region, ready
// to be combined with a job's targetRef bucket into a target. Its
// credentials, if any, are still just environment variable names at this
// point; resolveTargetCredentials reads their values later.
type resolvedServer struct {
	name         string
	kind         ServerKind
	region       string
	endpoint     string
	pathStyle    bool
	accessKeyEnv string
	secretKeyEnv string
	path         string        // local only: root directory backups are written under
	retention    time.Duration // local only: 0 means no automatic expiry
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
				return fmt.Errorf("targets[%d]: retention is not valid for server %q (type %s; local only)", i, ref.server, serverKindLabel(server.kind))
			}

			retention = ref.retention
		}

		cfg.Targets[i] = Target{
			ServerName:   server.name,
			Kind:         server.kind,
			Bucket:       ref.bucket,
			Region:       server.region,
			Endpoint:     server.endpoint,
			PathStyle:    server.pathStyle,
			accessKeyEnv: server.accessKeyEnv,
			secretKeyEnv: server.secretKeyEnv,
			LocalPath:    server.path,
			Retention:    retention,
		}
	}

	return nil
}

// resolveTargetCredentials reads each job's targets' static credentials
// from the environment (the access-key-env/secret-key-env named by the
// target's server), for every job in jobs — call this only after -job
// filtering has narrowed jobs down to what will actually run. A target
// whose server configured no credentials is left with none, and
// newS3Client falls back to the AWS SDK's default credential chain for it.
func resolveTargetCredentials(jobs []*Config) error {
	for _, j := range jobs {
		for i := range j.Targets {
			t := &j.Targets[i]
			if t.accessKeyEnv == "" {
				continue
			}

			accessKey := os.Getenv(t.accessKeyEnv)
			if accessKey == "" {
				return fmt.Errorf("server %q: environment variable %q (access-key-env) is not set", t.ServerName, t.accessKeyEnv)
			}

			secretKey := os.Getenv(t.secretKeyEnv)
			if secretKey == "" {
				return fmt.Errorf("server %q: environment variable %q (secret-key-env) is not set", t.ServerName, t.secretKeyEnv)
			}

			t.AccessKey, t.SecretKey = accessKey, secretKey
		}
	}

	return nil
}

// newConfigDefaults returns a *config with the built-in defaults applied to
// every job before its config file fields are layered on top.
func newConfigDefaults() *Config {
	return &Config{
		Key:     defaultKeyPattern,
		GPGBin:  defaultGPGBin,
		Retries: defaultRetries,
	}
}

// prepareJobs narrows jobs to the one named by jobFilter (if any), validates
// them, and resolves their passphrases and target credentials.
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

	if err := resolveTargetCredentials(jobs); err != nil {
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

	// fj.Retries == 0 is indistinguishable from "not set in this fj" (YAML's
	// zero value for an omitted int), same ambiguity applyBool documents for
	// bool fields; retries: 0 wouldn't be a meaningful setting anyway (there
	// would be no attempts at all), so treating it as unset costs nothing.
	if fj.Retries > 0 {
		cfg.Retries = fj.Retries
	}

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
