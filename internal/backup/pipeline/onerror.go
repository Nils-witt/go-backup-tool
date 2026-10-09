package pipeline

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/dockerexec"
)

// runTargetCommand runs cmd (a target's resolved on-error.command or
// on-recover.command) through the platform shell, the same way a job's own
// cmd: is (see newSourceCommand). kind ("on-error" or "on-recover") only
// labels the log lines and sets GBT_EVENT. It never returns an error to its
// caller: a failure running the command itself (nonzero exit, command not
// found, timeout) is only logged, since it must never affect the target
// upload run that triggered it — that run has already finished by the time
// this is called (see Runner.handleTargetOutcome).
//
// ctx is expected to already be detached from the triggering run's own
// cancellation (context.WithoutCancel) and bounded by cmd.Timeout — see
// fireTargetCommand.
//
// Context is passed via environment variables, not interpolated into the
// command string, so an operator's command never has to worry about quoting
// an error message that might itself contain shell metacharacters: GBT_EVENT
// ("error" or "recover"), GBT_JOB, GBT_TARGET, GBT_SERVER,
// GBT_CONSECUTIVE_FAILURES (for on-recover, the length of the streak that
// just ended), GBT_TIME (RFC 3339), and GBT_ERROR (only when terr is
// non-nil, i.e. never for on-recover).
//
// A command with a container: runs inside it instead, through the Docker
// daemon at dockerHost, with the same GBT_* variables added to the
// container's own environment (this process's environment isn't passed in).
func runTargetCommand(ctx context.Context, dockerHost, kind, job string, t *config.Target, cmd config.Command, streak int, terr error, log *slog.Logger) {
	env := []string{
		"GBT_EVENT=" + strings.TrimPrefix(kind, "on-"),
		"GBT_JOB=" + job,
		"GBT_TARGET=" + t.Bucket,
		"GBT_SERVER=" + t.ServerName,
		"GBT_CONSECUTIVE_FAILURES=" + strconv.Itoa(streak),
		"GBT_TIME=" + time.Now().UTC().Format(time.RFC3339),
	}

	if terr != nil {
		env = append(env, "GBT_ERROR="+terr.Error())
	}

	output := &logWriter{log: log, msg: kind + " command output"}

	log.Info(kind+" command firing", "job", job, "target", targetLabel(t), "command_id", cmd.ID, "container", cmd.Container, "streak", streak)

	var err error

	if cmd.Container != "" {
		err = runInContainer(ctx, dockerHost, dockerexec.Exec{
			Container: cmd.Container,
			User:      cmd.ContainerUser,
			Cmd:       []string{"sh", "-c", cmd.Cmd},
			Env:       env,
			Stdout:    output,
			Stderr:    output,
		})
	} else {
		var c *exec.Cmd
		if runtime.GOOS == "windows" {
			c = exec.CommandContext(ctx, "cmd", "/C", cmd.Cmd) //nolint:gosec // cmd.Cmd is operator-supplied CLI config, not untrusted input; see newSourceCommand
		} else {
			c = exec.CommandContext(ctx, "sh", "-c", cmd.Cmd) //nolint:gosec // cmd.Cmd is operator-supplied CLI config, not untrusted input; see newSourceCommand
		}

		c.Env = append(os.Environ(), env...)
		c.Stdout = output
		c.Stderr = output
		err = c.Run()
	}

	if err != nil {
		log.Warn(kind+" command failed", "job", job, "target", targetLabel(t), "command_id", cmd.ID, "err", err)
		return
	}

	log.Info(kind+" command fired", "job", job, "target", targetLabel(t), "command_id", cmd.ID)
}
