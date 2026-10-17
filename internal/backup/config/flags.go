package config

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"nilswitt.dev/go-backup-tool/internal/backup/report"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
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

	fs.StringVar(&configPath, "config", DefaultConfigPath, "path to the YAML config file")
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
		keysDir = DefaultKeysDir
	}

	if _, err := report.ResolveSettings(fileCfg.Report, nil); err != nil {
		return nil, err
	}

	return &RunConfig{
		Warnings:          fileCfg.warnings,
		Jobs:              jobs,
		FileJobs:          flattenFileJobs(fileCfg),
		FileServers:       fileCfg.Servers,
		FileCommands:      fileCfg.Commands,
		JobFilter:         jobFilter,
		JobEditing:        fileCfg.WebUI.JobEditing,
		Timeout:           timeout,
		Listen:            listen,
		DockerSocket:      strings.TrimSpace(fileCfg.DockerSocket),
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
