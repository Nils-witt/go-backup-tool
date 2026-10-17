package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/tarball"
)

// Command is one top-level commands: entry after validation, ready to be
// run by the pipeline package: as one of a job's backup sources (see
// Config.Sources), or (see pipeline.runTargetCommand) once a target's
// consecutive-failure streak reaches its on-error.after threshold, or once
// a target recovers from a failure streak (on-recover:).
type Command struct {
	ID string

	// Kind is what the command does: run Cmd (CommandKindShell, also what
	// the zero value means) or archive Tar.Root (CommandKindTar).
	Kind CommandKind

	Cmd     string
	Timeout time.Duration

	// TimeoutSet reports whether timeout: was given, rather than Timeout
	// being defaultOnErrorCommandTimeout: only an explicit timeout bounds a
	// job's source.
	TimeoutSet bool

	// Container, when set, runs Cmd inside that already-running container
	// through the Docker daemon (see pipeline.runTargetCommand), as
	// ContainerUser when that's set.
	Container     string
	ContainerUser string

	// Tar is what a CommandKindTar command archives; unused otherwise.
	Tar tarball.Options
}

// CommandKind distinguishes a commands: entry's type:.
type CommandKind string

// The CommandKind values a commands: entry's type: can resolve to.
const (
	CommandKindShell CommandKind = "shell" // type: shell, or unset
	CommandKindTar   CommandKind = "tar"   // type: tar
)

// compressGzip is the only compress: value a tar command accepts.
const compressGzip = "gzip"

// parseCommandKind validates a FileCommand's Type field; unset means shell.
func parseCommandKind(t string) (CommandKind, error) {
	switch strings.TrimSpace(t) {
	case "", string(CommandKindShell):
		return CommandKindShell, nil
	case string(CommandKindTar):
		return CommandKindTar, nil
	default:
		return "", fmt.Errorf("unknown type %q (want %q or %q)", t, CommandKindShell, CommandKindTar)
	}
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

	// Jobs' inline cmd: become commands: entries first, so they're built
	// and validated with the rest.
	fileCfg.warnings, err = convertInlineJobCommands(fileCfg)
	if err != nil {
		return resolvedNotifications{}, nil, err
	}

	commandWarnings, err := convertSingleJobCommands(fileCfg)
	if err != nil {
		return resolvedNotifications{}, nil, err
	}

	fileCfg.warnings = append(fileCfg.warnings, commandWarnings...)

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
// stored in the state db (see jobs.Manager) — into a Command: a non-empty id,
// an optional positive timeout (default defaultOnErrorCommandTimeout), and
// what its type: needs (see resolveShellCommand and resolveTarCommand).
func ResolveCommand(fc FileCommand) (Command, error) {
	id := strings.TrimSpace(fc.ID)
	if id == "" {
		return Command{}, errors.New("id is required")
	}

	kind, err := parseCommandKind(fc.Type)
	if err != nil {
		return Command{}, fmt.Errorf("command %q: %w", id, err)
	}

	timeout, err := parseOnErrorCommandTimeout(fc.Timeout)
	if err != nil {
		return Command{}, fmt.Errorf("command %q: %w", id, err)
	}

	c := Command{ID: id, Kind: kind, Timeout: timeout, TimeoutSet: strings.TrimSpace(fc.Timeout) != ""}

	switch kind {
	case CommandKindShell:
		err = resolveShellCommand(&c, fc)
	case CommandKindTar:
		err = resolveTarCommand(&c, fc)
	}

	if err != nil {
		return Command{}, fmt.Errorf("command %q: %w", id, err)
	}

	return c, nil
}

// resolveShellCommand fills c from a type: shell entry: a non-empty cmd,
// an optional container, and none of the tar-only fields.
func resolveShellCommand(c *Command, fc FileCommand) error {
	c.Cmd = strings.TrimSpace(fc.Cmd)
	if c.Cmd == "" {
		return errors.New("cmd is required")
	}

	hasTarFields := strings.TrimSpace(fc.Path) != "" || len(fc.Exclude) > 0 || strings.TrimSpace(fc.Compress) != ""
	if hasTarFields {
		return errors.New("path, exclude and compress only apply to type: tar")
	}

	c.Container = strings.TrimSpace(fc.Container)
	c.ContainerUser = strings.TrimSpace(fc.ContainerUser)

	return nil
}

// resolveTarCommand fills c from a type: tar entry: an absolute path,
// well-formed exclude globs, an optional compress: gzip, and none of the
// shell-only fields.
//
// security: path may name anything this process can read. That's no new
// capability: whoever may edit commands can already run any shell cmd.
func resolveTarCommand(c *Command, fc FileCommand) error {
	hasShellFields := strings.TrimSpace(fc.Cmd) != "" || strings.TrimSpace(fc.Container) != "" ||
		strings.TrimSpace(fc.ContainerUser) != ""
	if hasShellFields {
		return errors.New("cmd, container and container-user don't apply to type: tar")
	}

	root := strings.TrimSpace(fc.Path)
	if root == "" {
		return errors.New("path is required for type: tar")
	}

	if !filepath.IsAbs(root) {
		return fmt.Errorf("path %q must be absolute", root)
	}

	exclude := make([]string, 0, len(fc.Exclude))

	for _, p := range fc.Exclude {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}

		if err := tarball.ValidatePattern(p); err != nil {
			return err
		}

		exclude = append(exclude, p)
	}

	compress := strings.TrimSpace(fc.Compress)
	if compress != "" && compress != compressGzip {
		return fmt.Errorf("unknown compress %q (want %q or unset)", compress, compressGzip)
	}

	c.Tar = tarball.Options{Root: filepath.Clean(root), Exclude: exclude, Gzip: compress == compressGzip}

	return nil
}

// convertInlineJobCommands turns every job's deprecated inline cmd: (its
// own, or the top-level default it inherits) into a commands: entry of its
// own — id "job-<name>", suffixed "-2", "-3", ... if taken — which the job
// then names as its command:, so everything past config parsing only ever
// sees command: references. A job's own command: takes precedence over an
// inherited cmd:, and its own cmd: over an inherited command:; setting both
// at the same level is an error. It returns one deprecation warning per
// converted job, for the caller to log.
func convertInlineJobCommands(fileCfg *fileConfig) ([]string, error) {
	defaults := &fileCfg.FileJob
	if strings.TrimSpace(defaults.Cmd) != "" && defaults.hasCommand() {
		return nil, errors.New("set either files or the deprecated cmd, not both")
	}

	taken := make(map[string]bool, len(fileCfg.Commands))
	for _, fc := range fileCfg.Commands {
		taken[strings.TrimSpace(fc.ID)] = true
	}

	var warnings []string

	for i := range fileCfg.Jobs {
		fj := &fileCfg.Jobs[i]
		name := strings.TrimSpace(fj.Name)

		cmd := strings.TrimSpace(fj.Cmd)

		switch {
		case cmd != "" && fj.hasCommand():
			return nil, fmt.Errorf("jobs[%d]: set either files or the deprecated cmd, not both", i)
		case cmd == "" && (fj.hasCommand() || defaults.hasCommand()):
			continue
		case cmd == "":
			cmd = strings.TrimSpace(defaults.Cmd)
		}

		if cmd == "" {
			continue
		}

		id := uniqueCommandID("job-"+name, taken)
		taken[id] = true

		fileCfg.Commands = append(fileCfg.Commands, FileCommand{ID: id, Cmd: cmd})
		fj.Files, fj.Cmd = []FileJobFile{{Command: id}}, ""

		warnings = append(warnings, fmt.Sprintf("job %q: cmd: is deprecated; it now runs as commands: entry %q, which the job lists under files:", name, id))
	}

	defaults.Cmd = ""

	return warnings, nil
}

// convertSingleJobCommands turns every job's deprecated command: (and the
// top-level default's) into a one-entry files: list (see
// FileJob.NormalizeFiles), returning one deprecation warning per converted
// entry, for the caller to log.
func convertSingleJobCommands(fileCfg *fileConfig) ([]string, error) {
	var warnings []string

	convert := func(fj *FileJob, label string) error {
		hadCommand := strings.TrimSpace(fj.Command) != ""

		if err := fj.NormalizeFiles(); err != nil {
			return err
		}

		if hadCommand {
			warnings = append(warnings, label+": command: is deprecated; list the command under files: instead")
		}

		return nil
	}

	if err := convert(&fileCfg.FileJob, "top-level job defaults"); err != nil {
		return nil, err
	}

	for i := range fileCfg.Jobs {
		fj := &fileCfg.Jobs[i]
		if err := convert(fj, fmt.Sprintf("job %q", strings.TrimSpace(fj.Name))); err != nil {
			return nil, fmt.Errorf("jobs[%d]: %w", i, err)
		}
	}

	return warnings, nil
}

// uniqueCommandID returns base, or base with the lowest "-N" suffix (from
// 2) that isn't in taken.
func uniqueCommandID(base string, taken map[string]bool) string {
	id := base
	for n := 2; taken[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}

	return id
}

// hookCommand looks up a target's on-error/on-recover command by id. Only a
// shell command can be one: a hook's output is discarded, so a tar
// command's archive would be produced for nothing.
func hookCommand(commands map[string]Command, id string) (Command, error) {
	command, ok := commands[id]
	if !ok {
		return Command{}, fmt.Errorf("no command named %q defined under commands", id)
	}

	if command.Kind == CommandKindTar {
		return Command{}, fmt.Errorf("command %q is type: tar, which can't be run as a hook", id)
	}

	return command, nil
}
