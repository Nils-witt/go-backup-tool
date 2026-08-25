// Package backup implements go-backup-tool's pipeline: run a shell command,
// encrypt its stdout with gpg, and stream the ciphertext to one or more
// targets: an S3 (or S3-compatible) bucket, a directory on the local
// filesystem, or another go-backup-tool instance's receiver API.
package backup

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
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// config holds one backup job's parameters.
type config struct {
	name       string // job name, from its jobs: entry; always set
	cmd        string
	key        string         // may still contain the {time} placeholder; resolved fresh per run
	targetRefs []jobTargetRef // raw targets: entries, resolved against servers by resolveJobTargets
	targets    []target       // resolved destinations; empty until resolveJobTargets runs
	recipients stringSlice
	symmetric  bool
	armor      bool
	gpgBin     string
	gpgHomedir string
	interval   time.Duration // repeat every interval; 0 runs the job once
	startTime  time.Time     // anchors the interval grid; zero means "run immediately, then every interval"
	passphrase string        // resolved from GPG_PASSPHRASE when symmetric
}

// jobTargetRef is one targets: entry as written in a job: a server name
// (looked up in the top-level servers: list) plus the bucket to use on it.
type jobTargetRef struct {
	server string
	bucket string
}

// serverKind distinguishes a servers: entry's destination type. The zero
// value is serverKindS3, so existing config files (and target literals in
// tests) that never set type: keep working unchanged.
type serverKind string

const (
	serverKindS3     serverKind = ""       // default; type: s3 also selects this
	serverKindLocal  serverKind = "local"  // type: local
	serverKindRemote serverKind = "remote" // type: remote
)

// parseServerKind validates a fileServer's Type field, defaulting an unset
// or explicit "s3" value to serverKindS3.
func parseServerKind(t string) (serverKind, error) {
	switch strings.TrimSpace(t) {
	case "", "s3":
		return serverKindS3, nil
	case string(serverKindLocal):
		return serverKindLocal, nil
	case string(serverKindRemote):
		return serverKindRemote, nil
	default:
		return "", fmt.Errorf("unknown type %q (want \"s3\", %q, or %q)", t, serverKindLocal, serverKindRemote)
	}
}

// target is one upload destination for a job, fully resolved from a
// jobTargetRef against its named server. A job uploads the same encrypted
// object to every one of its targets. Its kind determines which of the
// fields below apply: s3 (the default) uses bucket/region/endpoint/
// pathStyle/credentials; local uses only bucket (as a subdirectory of
// localPath) and localPath itself; remote uses bucket (as the id sent to
// the destination instance), endpoint, and token.
type target struct {
	serverName string // the servers: entry this came from, for diagnostics
	kind       serverKind
	bucket     string
	region     string
	endpoint   string
	pathStyle  bool

	// accessKeyEnv/secretKeyEnv are the server's configured env var names
	// (set at config-build time); accessKey/secretKey are their resolved
	// values, filled in later by resolveTargetCredentials. Both empty means
	// no static credentials: newS3Client falls back to the AWS SDK's
	// default credential chain. Unused for a local target.
	accessKeyEnv string
	secretKeyEnv string
	accessKey    string
	secretKey    string

	// localPath is the local server's root directory (only set when
	// kind == serverKindLocal). The object is written to
	// localPath/bucket/key, mirroring the S3 bucket/key layout.
	localPath string

	// retention is how long a local target's written objects are kept
	// before they're deleted automatically (only set when
	// kind == serverKindLocal). Zero means no automatic expiry. See
	// retention.go.
	retention time.Duration

	// token authenticates to a remote target's destination instance (only
	// set when kind == serverKindRemote), sent as a bearer token. Unlike
	// accessKey/secretKey, it's read directly from the config file's
	// token: field rather than an environment variable.
	token string
}

// runConfig is the result of parseFlags: one or more jobs to run, plus the
// overall run timeout and the optional web UI listen address.
type runConfig struct {
	jobs       []*config
	timeout    time.Duration
	listen     string // empty disables the web UI; see fileConfig.Listen
	configPath string // where the config file was loaded from; state db lives alongside it
	logLevel   slog.Level
	receivers  map[string]resolvedReceiver // this instance's receiver API entries, keyed by id; see receiver.go
}

// Built-in defaults for fields a job's or server's config file entry
// doesn't set.
const (
	defaultConfigPath = "config.yaml" // used when -config isn't given explicitly
	defaultKeyPattern = "backup-{time}.gpg"
	defaultRegion     = "us-east-1"
	defaultGPGBin     = "gpg"
)

// fileJob mirrors config's per-job fields for YAML unmarshaling, used both
// for the top-level shared defaults and for each entry under jobs:. Any
// field left unset falls through to the built-in default (top-level) or the
// top-level value (a jobs: entry).
//
// A job names its upload destination(s) via targets:, each entry a
// {server, bucket} pair referencing a servers: entry defined at the top
// level (see fileServer) — server connection details (region, endpoint,
// path-style, credentials) live there, not on the job.
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
}

// fileJobTarget mirrors jobTargetRef for YAML unmarshaling.
type fileJobTarget struct {
	Server string `yaml:"server"`
	Bucket string `yaml:"bucket"`
}

// fileServer is one top-level servers: entry, defined once and referenced by
// name from any job's targets: list. type: selects the destination kind:
// "s3" (the default) for an S3 (or S3-compatible) endpoint, using
// region/endpoint/path-style/access-key-env/secret-key-env; "local" for a
// directory on the local filesystem, using only path; or "remote" for
// another go-backup-tool instance's receiver API, using endpoint and token.
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
// it's older than that, tracked via a small sqlite database at
// path/.go-backup-tool-retention.db (see retention.go). Unset or "0"
// disables automatic cleanup. Token (remote only) is the bearer token sent
// to the destination instance's receiver API, matching one of its
// receivers: entries' own token — unlike access-key-env/secret-key-env,
// this is written directly in the config file rather than read from the
// environment.
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
	Token        string `yaml:"token"`     // remote only: bearer token for the destination instance's receiver API
}

// fileConfig is the top-level shape of the YAML config file. Its embedded
// fileJob holds shared defaults applied to every entry in Jobs before that
// entry's own fields override them.
type fileConfig struct {
	fileJob `yaml:",inline"`

	Timeout   string         `yaml:"timeout"`
	Listen    string         `yaml:"listen"` // e.g. ":8080"; empty (the default) disables the web UI
	Servers   []fileServer   `yaml:"servers"`
	Jobs      []fileJob      `yaml:"jobs"`
	Receivers []fileReceiver `yaml:"receivers"`
}

// parseFlags parses args (typically os.Args[1:]) into a runConfig, writing
// usage output to out on error or -h/-help. It takes an explicit argument
// list and a fresh FlagSet (rather than the package-level flag.CommandLine)
// so it can be called repeatedly and in isolation from tests.
//
// All job parameters come from the YAML config file (-config, defaulting to
// config.yaml); there are no CLI flags to set them individually. Every job
// is defined under the config file's jobs: list; -job selects a single one
// to run, or every job runs (in order) when -job isn't given. -log-level
// (debug, info, warn, or error; default info) controls diagnostic log
// verbosity.
//
// Each job's key keeps any {time} placeholder unresolved: it's substituted
// fresh by the caller immediately before every run (see substituteKeyTime),
// not here, so a job with a nonzero interval doesn't overwrite the same
// object on every repeat.
func parseFlags(args []string, out io.Writer) (*runConfig, error) {
	fs := flag.NewFlagSet("go-backup-tool", flag.ContinueOnError)
	fs.SetOutput(out)

	var (
		configPath string
		jobFilter  string
		logLevel   string
	)

	fs.StringVar(&configPath, "config", defaultConfigPath, "path to the YAML config file")
	fs.StringVar(&jobFilter, "job", "", "run only the named job from the config file's jobs: list")
	fs.StringVar(&logLevel, "log-level", "info", "log verbosity: debug, info, warn, or error")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	level, err := parseLogLevel(logLevel)
	if err != nil {
		return nil, err
	}

	configExplicit := false

	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configExplicit = true
		}
	})

	fileCfg, err := loadFileConfig(configPath, configExplicit)
	if err != nil {
		return nil, err
	}

	if fileCfg == nil {
		return nil, fmt.Errorf("no config file found at %q; create one (see config.example.yaml) or pass -config <path>", configPath)
	}

	jobs, err := resolveJobs(fileCfg)
	if err != nil {
		return nil, err
	}

	jobs, err = applyJobFilter(jobs, jobFilter)
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

	receivers, err := buildReceivers(fileCfg.Receivers)
	if err != nil {
		return nil, err
	}

	timeout, err := parseConfigTimeout(fileCfg.Timeout)
	if err != nil {
		return nil, err
	}

	return &runConfig{
		jobs:       jobs,
		timeout:    timeout,
		listen:     strings.TrimSpace(fileCfg.Listen),
		configPath: configPath,
		logLevel:   level,
		receivers:  receivers,
	}, nil
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

// parseLogLevel parses the -log-level flag's value into a slog.Level,
// accepting the same case-insensitive names slog itself recognizes
// (debug, info, warn, error).
func parseLogLevel(s string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("parsing -log-level %q: %w", s, err)
	}

	return level, nil
}

// resolveJobs builds the list of jobs to run from fileCfg's jobs: list,
// layering fileCfg's top-level fields as shared defaults under each entry's
// own fields, and resolving each job's targets: against fileCfg's servers:.
//
// An empty jobs: list is only allowed when listen: is set, since that still
// leaves the web UI (and receiver API) as a reason to run; otherwise the
// process would start and immediately have nothing to do.
func resolveJobs(fileCfg *fileConfig) ([]*config, error) {
	if len(fileCfg.Jobs) == 0 && strings.TrimSpace(fileCfg.Listen) == "" {
		return nil, errors.New("config file must define at least one job under a jobs list, or set listen: to run without any")
	}

	return buildJobsFromFile(fileCfg)
}

// buildJobsFromFile builds one *config per entry in fileCfg.Jobs, layering
// fileCfg's top-level fields as defaults under each entry's own fields and
// resolving each job's targets: against fileCfg.Servers.
func buildJobsFromFile(fileCfg *fileConfig) ([]*config, error) {
	servers, err := buildServers(fileCfg.Servers)
	if err != nil {
		return nil, err
	}

	jobs := make([]*config, 0, len(fileCfg.Jobs))
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
		cfg.name = name

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

		if kind == serverKindLocal {
			server, err := buildLocalServer(name, &fs)
			if err != nil {
				return nil, err
			}

			servers[name] = server

			continue
		}

		if kind == serverKindRemote {
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

	return resolvedServer{name: name, kind: serverKindLocal, path: fs.Path, retention: retention}, nil
}

// buildRemoteServer validates and builds a resolvedServer for a type: remote
// servers: entry, which uses only endpoint and token and none of the
// S3/local-specific fields.
func buildRemoteServer(name string, fs *fileServer) (resolvedServer, error) {
	if strings.TrimSpace(fs.Endpoint) == "" {
		return resolvedServer{}, fmt.Errorf("server %q: endpoint is required for type: remote", name)
	}

	if strings.TrimSpace(fs.Token) == "" {
		return resolvedServer{}, fmt.Errorf("server %q: token is required for type: remote", name)
	}

	if fs.Region != "" || fs.PathStyle || fs.AccessKeyEnv != "" || fs.SecretKeyEnv != "" || fs.Path != "" || fs.Retention != "" {
		return resolvedServer{}, fmt.Errorf("server %q: region/path-style/access-key-env/secret-key-env/path/retention are not valid for type: remote", name)
	}

	return resolvedServer{name: name, kind: serverKindRemote, endpoint: fs.Endpoint, token: fs.Token}, nil
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
	kind         serverKind
	region       string
	endpoint     string
	pathStyle    bool
	accessKeyEnv string
	secretKeyEnv string
	path         string        // local only: root directory backups are written under
	retention    time.Duration // local only: 0 means no automatic expiry
	token        string        // remote only: bearer token for the destination instance
}

// resolveJobTargets resolves cfg's raw target references (targetRefs, from
// targets:) against servers, building cfg.targets. A job with no target
// references at all is left with an empty cfg.targets; validateJob reports
// that as an error.
func resolveJobTargets(cfg *config, servers map[string]resolvedServer) error {
	if len(cfg.targetRefs) == 0 {
		return nil
	}

	cfg.targets = make([]target, len(cfg.targetRefs))

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

		cfg.targets[i] = target{
			serverName:   server.name,
			kind:         server.kind,
			bucket:       ref.bucket,
			region:       server.region,
			endpoint:     server.endpoint,
			pathStyle:    server.pathStyle,
			accessKeyEnv: server.accessKeyEnv,
			secretKeyEnv: server.secretKeyEnv,
			localPath:    server.path,
			retention:    server.retention,
			token:        server.token,
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
func resolveTargetCredentials(jobs []*config) error {
	for _, j := range jobs {
		for i := range j.targets {
			t := &j.targets[i]
			if t.accessKeyEnv == "" {
				continue
			}

			accessKey := os.Getenv(t.accessKeyEnv)
			if accessKey == "" {
				return fmt.Errorf("server %q: environment variable %q (access-key-env) is not set", t.serverName, t.accessKeyEnv)
			}

			secretKey := os.Getenv(t.secretKeyEnv)
			if secretKey == "" {
				return fmt.Errorf("server %q: environment variable %q (secret-key-env) is not set", t.serverName, t.secretKeyEnv)
			}

			t.accessKey, t.secretKey = accessKey, secretKey
		}
	}

	return nil
}

// newConfigDefaults returns a *config with the built-in defaults applied to
// every job before its config file fields are layered on top.
func newConfigDefaults() *config {
	return &config{
		key:    defaultKeyPattern,
		gpgBin: defaultGPGBin,
	}
}

// applyJobFilter applies -job, if given, restricting jobs to the single
// named job. It's an error to name a job that doesn't exist.
func applyJobFilter(jobs []*config, jobFilter string) ([]*config, error) {
	if jobFilter == "" {
		return jobs, nil
	}

	for _, j := range jobs {
		if j.name == jobFilter {
			return []*config{j}, nil
		}
	}

	return nil, fmt.Errorf("-job %q: no such job in config file", jobFilter)
}

// validateJobs validates every job, returning the first error found.
func validateJobs(jobs []*config) error {
	for _, j := range jobs {
		if err := validateJob(j); err != nil {
			return err
		}
	}

	return nil
}

// validateJob checks that a single job's parameters are complete and
// self-consistent.
func validateJob(cfg *config) error {
	switch {
	case strings.TrimSpace(cfg.cmd) == "":
		return jobError(cfg, errors.New("cmd is required"))
	case len(cfg.targets) == 0:
		return jobError(cfg, errors.New("at least one target is required (see targets: and servers:)"))
	case cfg.symmetric && len(cfg.recipients) > 0:
		return jobError(cfg, errors.New("symmetric cannot be combined with recipients"))
	case !cfg.symmetric && len(cfg.recipients) == 0:
		return jobError(cfg, errors.New("specify at least one recipient, or set symmetric: true"))
	case cfg.interval < 0:
		return jobError(cfg, errors.New("interval must not be negative"))
	case !cfg.startTime.IsZero() && cfg.interval <= 0:
		return jobError(cfg, errors.New("start-time requires interval"))
	}

	return nil
}

// jobError prefixes err with cfg's job name, so validation and passphrase
// errors are attributable to the job that caused them.
func jobError(cfg *config, err error) error {
	return fmt.Errorf("job %q: %w", cfg.name, err)
}

// resolvePassphrases reads GPG_PASSPHRASE once (if any job needs it) and
// assigns it to every symmetric job, then clears it from the environment.
// All symmetric jobs in a run share the same passphrase; per-job passphrases
// aren't supported.
func resolvePassphrases(jobs []*config) error {
	needsPassphrase := false

	for _, j := range jobs {
		if j.symmetric {
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
		if j.symmetric {
			j.passphrase = passphrase
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
func applyFileJob(cfg *config, fj *fileJob) error {
	applyString(&cfg.cmd, fj.Cmd)
	applyString(&cfg.key, fj.Key)
	applyString(&cfg.gpgBin, fj.GPGBin)
	applyString(&cfg.gpgHomedir, fj.GPGHomedir)

	applyBool(&cfg.symmetric, fj.Symmetric)
	applyBool(&cfg.armor, fj.Armor)

	if len(fj.Targets) > 0 {
		cfg.targetRefs = make([]jobTargetRef, len(fj.Targets))
		for i, t := range fj.Targets {
			cfg.targetRefs[i] = jobTargetRef{server: t.Server, bucket: t.Bucket}
		}
	}

	if len(fj.Recipients) > 0 {
		cfg.recipients = append(stringSlice(nil), fj.Recipients...)
	}

	if fj.Interval != "" {
		d, err := time.ParseDuration(fj.Interval)
		if err != nil {
			return fmt.Errorf("parsing interval %q: %w", fj.Interval, err)
		}

		cfg.interval = d
	}

	if fj.StartTime != "" {
		t, err := time.Parse(time.RFC3339, fj.StartTime)
		if err != nil {
			return fmt.Errorf("parsing start-time %q: %w", fj.StartTime, err)
		}

		cfg.startTime = t
	}

	return nil
}
