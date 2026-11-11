// Package ctl implements the client subcommands that query a running
// instance's HTTP API instead of running one: "health" (GET /api/health)
// and "status" (GET /api/status and /api/receivers). app.Run dispatches to
// Run when its first argument names one of them (see IsCommand).
//
// The instance is found by -url, else the GO_BACKUP_TOOL_URL environment
// variable, else the config file's webui.listen (see resolveBaseURL).
// Endpoints other than /api/health need a read-only API token (Settings ›
// API tokens in the dashboard), passed as GO_BACKUP_TOOL_TOKEN or -token.
package ctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

const (
	// urlEnv and tokenEnv are the environment variables -url and -token
	// fall back to. The token belongs here rather than in -token, which
	// other users on the host can read off the process list.
	urlEnv   = "GO_BACKUP_TOOL_URL"
	tokenEnv = "GO_BACKUP_TOOL_TOKEN" //nolint:gosec // an environment variable name, not a credential

	// defaultTimeout is -timeout's default: how long one subcommand may
	// take, across all of its requests.
	defaultTimeout = 10 * time.Second
)

// Exit codes Run returns.
const (
	exitOK = 0
	// exitError is a failed request, or an unhealthy instance for health.
	exitError = 1
	// exitUsage is a bad command line, matching the instance's own flag
	// errors.
	exitUsage = 2
	// exitAttention is status's code when a job, target, or receiver has
	// failed or gone stale, so monitoring can alert on it.
	exitAttention = 3
)

// command is one subcommand: it gets the parsed options and an API client,
// writes its output to stdout, and returns its exit code.
type command func(ctx context.Context, c *client, opts options, stdout io.Writer) (int, error)

// commands maps every subcommand name to its implementation.
var commands = map[string]command{
	"health": runHealth,
	"status": runStatus,
}

// IsCommand reports whether name is one of this package's subcommands.
func IsCommand(name string) bool {
	_, ok := commands[name]
	return ok
}

// CommandNames returns every subcommand name, sorted, for usage text.
func CommandNames() []string {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// options are a subcommand's parsed flags.
type options struct {
	url            string
	configPath     string
	configExplicit bool
	token          string
	timeout        time.Duration
	json           bool
}

// Run runs subcommand name with args (the arguments after it), writing its
// output to stdout and errors and usage to stderr, and returns the process
// exit code. name must satisfy IsCommand.
func Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	cmd, ok := commands[name]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", name)
		return exitUsage
	}

	opts, err := parseFlags(name, args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}

		return exitUsage
	}

	base, err := resolveBaseURL(opts)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	code, err := cmd(ctx, newClient(base, opts.token, opts.timeout), opts, stdout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
	}

	return code
}

// parseFlags parses name's args. The flag package reports parse errors
// and -help usage to stderr itself.
func parseFlags(name string, args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("go-backup-tool "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)

	opts := options{}

	fs.StringVar(&opts.url, "url", "", "base URL of the running instance, e.g. http://localhost:8080 (default $"+urlEnv+", else from the config file's webui.listen)")
	fs.StringVar(&opts.configPath, "config", config.DefaultConfigPath, "config file to read webui.listen from when no URL is given")
	fs.DurationVar(&opts.timeout, "timeout", defaultTimeout, "give up after this long")
	fs.BoolVar(&opts.json, "json", false, "print the instance's JSON response instead of text")

	if name != "health" {
		fs.StringVar(&opts.token, "token", "", "API token (default $"+tokenEnv+"; prefer the variable, since flags are visible in the process list)")
	}

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()

		return options{}, errors.New("unexpected arguments")
	}

	if opts.timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "-timeout must be positive")
		return options{}, errors.New("invalid timeout")
	}

	fs.Visit(func(f *flag.Flag) { opts.configExplicit = opts.configExplicit || f.Name == "config" })

	return opts, nil
}
