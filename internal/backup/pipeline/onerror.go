package pipeline

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// runOnErrorCommand runs cmd (a target's resolved on-error.command) through
// the platform shell, the same way a job's own cmd: is (see
// newSourceCommand). It never returns an error to its caller: a failure
// running the on-error command itself (nonzero exit, command not found,
// timeout) is only logged, since it must never affect the target upload run
// that triggered it — that run has already finished by the time this is
// called (see Runner.handleTargetOutcome).
//
// ctx is expected to already be detached from the triggering run's own
// cancellation (context.WithoutCancel) and bounded by cmd.Timeout — see
// Runner.handleTargetOutcome.
//
// Failure context is passed via environment variables, not interpolated
// into the command string, so an operator's command never has to worry
// about quoting an error message that might itself contain shell
// metacharacters: GBT_JOB, GBT_TARGET, GBT_SERVER, GBT_ERROR,
// GBT_CONSECUTIVE_FAILURES, and GBT_TIME (RFC 3339).
func runOnErrorCommand(ctx context.Context, job string, t *config.Target, cmd config.Command, streak int, terr error, log *slog.Logger) {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmd.Cmd) //nolint:gosec // cmd.Cmd is operator-supplied CLI config, not untrusted input; see newSourceCommand
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmd.Cmd) //nolint:gosec // cmd.Cmd is operator-supplied CLI config, not untrusted input; see newSourceCommand
	}

	c.Env = append(os.Environ(),
		"GBT_JOB="+job,
		"GBT_TARGET="+t.Bucket,
		"GBT_SERVER="+t.ServerName,
		"GBT_ERROR="+terr.Error(),
		"GBT_CONSECUTIVE_FAILURES="+strconv.Itoa(streak),
		"GBT_TIME="+time.Now().UTC().Format(time.RFC3339),
	)

	c.Stdout = &logWriter{log: log, msg: "on-error command output"}
	c.Stderr = &logWriter{log: log, msg: "on-error command output"}

	log.Info("on-error command firing", "job", job, "target", targetLabel(t), "command_id", cmd.ID, "streak", streak)

	if err := c.Run(); err != nil {
		log.Warn("on-error command failed", "job", job, "target", targetLabel(t), "command_id", cmd.ID, "err", err)
		return
	}

	log.Info("on-error command fired", "job", job, "target", targetLabel(t), "command_id", cmd.ID)
}
