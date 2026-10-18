package config

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

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

		if err := resolveJobSources(cfg, commands); err != nil {
			return nil, fmt.Errorf("job %q: %w", name, err)
		}

		jobs = append(jobs, cfg)
	}

	return jobs, nil
}

// resolveJobSources resolves cfg's files: entries against commands into
// cfg.Sources, naming each file (see JobSource.Name) and checking the names
// are valid and unique. A job with no files: is left without Sources;
// checkJob reports that. Call it once cfg.Armor is final, since it picks
// the default file extension.
func resolveJobSources(cfg *Config, commands map[string]Command) error {
	ext := ".gpg"
	if cfg.Armor {
		ext = ".asc"
	}

	sources := make([]JobSource, 0, len(cfg.fileRefs))
	names := make(map[string]bool, len(cfg.fileRefs))

	for i, ref := range cfg.fileRefs {
		if ref.Command == "" {
			return fmt.Errorf("files[%d]: command is required", i)
		}

		c, ok := commands[ref.Command]
		if !ok {
			return fmt.Errorf("files[%d]: no command %q defined under commands", i, ref.Command)
		}

		name := cmp.Or(ref.Name, ref.Command+defaultFileExt(c)+ext)
		if err := ValidateFileName(name); err != nil {
			return fmt.Errorf("files[%d]: %w", i, err)
		}

		if names[name] {
			return fmt.Errorf("files[%d]: duplicate file name %q (set name: to tell them apart)", i, name)
		}

		names[name] = true

		sources = append(sources, JobSource{Command: c, Name: name})
	}

	cfg.Sources = sources

	return nil
}

// defaultFileExt is the extension a command's file gets, before the
// encryption's own, when its files: entry sets no name: what a tar command's
// output is; nothing for a shell command, whose output can be anything.
func defaultFileExt(c Command) string {
	switch {
	case c.Kind != CommandKindTar:
		return ""
	case c.Tar.Gzip:
		return ".tar.gz"
	default:
		return ".tar"
	}
}

// NormalizeFiles trims fj's files: entries' command ids and names.
func (fj *FileJob) NormalizeFiles() {
	files := make([]FileJobFile, len(fj.Files))
	for i, f := range fj.Files {
		files[i] = FileJobFile{Command: strings.TrimSpace(f.Command), Name: strings.TrimSpace(f.Name)}
	}

	if len(files) > 0 {
		fj.Files = files
	}
}

// ValidateFileName reports whether name can be one file of a backup: a
// single path segment, safe to join under a backup's key on every
// platform. It rejects a leading "." too (which also rules out "." and
// ".."), since dot-files are hidden from every file listing and hold a
// backup's own bookkeeping (see objectstore.MultiFileMarker).
func ValidateFileName(name string) error {
	const maxLen = 255

	switch {
	case name == "":
		return errors.New("file name is required")
	case len(name) > maxLen:
		return fmt.Errorf("file name %q is longer than %d bytes", name, maxLen)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("file name %q must not start with \".\"", name)
	case strings.ContainsAny(name, `/\:`+"\x00"):
		return fmt.Errorf("file name %q must not contain /, \\, : or NUL", name)
	case !filepath.IsLocal(name):
		return fmt.Errorf("invalid file name %q", name)
	}

	return nil
}

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
			ServerUUID: server.serverUUID,
			TLS:        server.tls,
			LocalPath:  server.path,
			Retention:  retention,
		}

		if ref.onErrorCommandID != "" {
			command, err := hookCommand(commands, ref.onErrorCommandID)
			if err != nil {
				return fmt.Errorf("targets[%d]: on-error: %w", i, err)
			}

			if ref.onErrorAfter <= 0 {
				return fmt.Errorf("targets[%d]: on-error.after must be a positive integer, got %d", i, ref.onErrorAfter)
			}

			cfg.Targets[i].OnErrorCommand = &command
			cfg.Targets[i].OnErrorAfter = ref.onErrorAfter
			cfg.Targets[i].OnErrorOnce = ref.onErrorOnce
		}

		if ref.onRecoverCommandID != "" {
			command, err := hookCommand(commands, ref.onRecoverCommandID)
			if err != nil {
				return fmt.Errorf("targets[%d]: on-recover: %w", i, err)
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
		Key:    DefaultKeyPattern,
		GPGBin: DefaultGPGBin,
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
	if cfg.Kind == JobKindOnChange {
		return checkOnChangeJob(cfg)
	}

	switch {
	case len(cfg.Sources) == 0:
		return errors.New("at least one file (a command under files:) is required")
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

	fj.NormalizeFiles()

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

	if err := resolveJobSources(cfg, commands); err != nil {
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
		{&out.Type, defaults.Type},
		{&out.Key, defaults.Key},
		{&out.Interval, defaults.Interval},
		{&out.StartTime, defaults.StartTime},
		{&out.StagingDir, defaults.StagingDir},
	} {
		if *f.dst == "" {
			*f.dst = f.def
		}
	}

	if out.Source == nil && defaults.Source != nil {
		src := *defaults.Source
		src.Exclude = slices.Clone(src.Exclude)
		out.Source = &src
	}

	if len(out.Files) == 0 {
		out.Files = slices.Clone(defaults.Files)
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
	if err := applyJobKind(cfg, fj); err != nil {
		return err
	}

	if len(fj.Files) > 0 {
		cfg.fileRefs = make([]FileJobFile, len(fj.Files))
		for i, f := range fj.Files {
			cfg.fileRefs[i] = FileJobFile{Command: strings.TrimSpace(f.Command), Name: strings.TrimSpace(f.Name)}
		}
	}

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

// applyJobKind applies fj's type: and source: to cfg. An unset type: keeps
// cfg's current kind (the top-level default's, if any); source: is only
// valid on an on-change job, and resolved once the kind is known.
func applyJobKind(cfg *Config, fj *FileJob) error {
	if strings.TrimSpace(fj.Type) != "" || cfg.Kind == "" {
		kind, err := parseJobKind(fj.Type)
		if err != nil {
			return err
		}

		cfg.Kind = kind
	}

	if fj.Source == nil {
		return nil
	}

	if cfg.Kind != JobKindOnChange {
		return errors.New("source is only valid for an on-change job (type: on-change)")
	}

	src, err := resolveOnChangeSource(fj.Source)
	if err != nil {
		return err
	}

	cfg.Source = src

	return nil
}
