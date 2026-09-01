// Package pipeline runs a configured backup job end to end: executing its
// command, encrypting the output with gpg, staging it locally, and
// uploading it to every target — plus the background work that keeps a run
// healthy afterward: scheduling repeats, sweeping expired local objects, and
// sending the daily receiver report.
package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// remoteHTTPClient is shared by every remote target's upload/delete
// requests. It sets no client-side timeout: the request's context (derived
// from the run's overall -timeout, if any) governs cancellation instead,
// consistent with how local filesystem writes are bounded only by ctx.
var remoteHTTPClient = &http.Client{}

// environWithout returns the current process environment with the given
// variable names removed, for handing to a child process that must not
// inherit them.
func environWithout(names ...string) []string {
	environ := os.Environ()

	filtered := make([]string, 0, len(environ))
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(names, key) {
			filtered = append(filtered, kv)
		}
	}

	return filtered
}

// runPipeline runs the backup in two phases:
//
//	shell command --(stdout)--> gpg encrypt --(stdout)--> local staging file
//	local staging file --(read once per target, independently)--> every target
//
// Each target is either a directory on the local filesystem or another
// go-backup-tool instance's receiver API. The backup command's output is
// encrypted and drained into a private local
// temp file (see stageBackup) before any target ever sees it, rather than
// streamed directly to every target. That has two consequences: a
// source/gpg failure never reaches a target at all (there's nothing to roll
// back, unlike a live streaming design where a target could already have a
// complete-but-truncated object by the time the failure is noticed), and
// each target then uploads from that same stable file independently, so one
// target's failure never affects any other target's own attempt. Each
// target gets exactly one attempt (see uploadStagedToTargets); a failed
// target is not retried.
//
// cfg.Cmd is run through the platform shell ("sh -c" on every OS but
// Windows, "cmd /C" there — see newSourceCommand) deliberately: it lets an
// operator pass a full shell pipeline (e.g. "mysqldump db | gzip") as the
// backup source. cfg.Cmd is operator-supplied CLI configuration, not
// untrusted external input, so this is the intended behavior rather than a
// command-injection risk.
//
// onTargetDone, if non-nil, is called exactly once per target — index-
// aligned with cfg.Targets — as soon as that target's own outcome is known
// (nil means it succeeded), independently of every other target and of the
// job as a whole: a target that finishes early (or fails) is reported right
// away rather than waiting for the slowest target to finish. Callers such
// as the web UI's status store use this to refresh
// each target's displayed state as it happens instead of only once the
// whole run completes. If the pipeline never reaches the upload stage at
// all (e.g. the source command failed), every target is still reported
// exactly once, with that same pipeline-level error, since no more specific
// detail exists.
//
// bytesWritten is the size of the encrypted object written to every target
// (they all upload from the same staged file), for callers that want to
// report the backup's size; it's 0 if the pipeline failed before staging
// finished.
func runPipeline(ctx context.Context, cfg *config.Config, log *slog.Logger, onTargetDone func(index int, err error)) (bytesWritten int64, err error) {
	// Guarantees the onTargetDone contract above even on an early return:
	// uploadStagedToTargets reports each target itself once it starts, so
	// this only fires for a failure earlier in the pipeline, and it always
	// sees the right err — a named return set by any "return ..., err"
	// above is assigned before deferred calls run.
	uploadStarted := false

	defer func() {
		if !uploadStarted {
			for i := range cfg.Targets {
				onTargetDone(i, err)
			}
		}
	}()

	sourceCmd, gpgCmd, gpgOut, err := startEncryptingPipeline(ctx, cfg, log)
	if err != nil {
		return 0, err
	}

	log.Info("staging encrypted backup", "dir", cfg.StagingDir)

	stagingPath, bytesWritten, stageErr := stageBackup(cfg, gpgOut)

	log.Info("backup finished staging", "staging_path", stagingPath, "bytes_written", bytesWritten)

	// gpgOut must be fully drained (which stageBackup does) before Wait, per
	// exec.Cmd.StdoutPipe's documented contract.
	gpgErr := gpgCmd.Wait()
	sourceErr := sourceCmd.Wait()

	if stageErr == nil {
		defer func() { _ = os.Remove(stagingPath) }()
	}

	// Only upload if every earlier stage genuinely succeeded: staging a
	// backup from a failed source command or gpg would just mean uploading
	// corrupt or truncated garbage to every target. Below this point,
	// uploadStagedToTargets takes over reporting each target's outcome
	// (see onTargetDone above), so mark uploadStarted before calling it —
	// not after, so a panic partway through still leaves the deferred
	// fallback disabled rather than double-reporting.
	var uploadErr error

	if sourceErr == nil && gpgErr == nil && stageErr == nil {
		log.Debug("uploading to targets", "targets", len(cfg.Targets))

		uploadStarted = true
		uploadErr = uploadStagedToTargets(ctx, cfg, stagingPath, onTargetDone, log)
	}

	return bytesWritten, firstPipelineError(cfg.Cmd, sourceErr, gpgErr, stageErr, uploadErr)
}

// newSourceCommand builds the (not yet started) command that runs cmd
// through the platform shell. Windows has no "sh" on PATH by default (Go's
// standard installer doesn't ship one, unlike every other supported OS), so
// it uses "cmd /C" there instead; everywhere else it uses "sh -c", per
// runPipeline's doc comment.
func newSourceCommand(ctx context.Context, cmd string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", cmd) //nolint:gosec // cfg.Cmd is operator-supplied CLI config, not untrusted input; see runPipeline's doc comment
	}

	return exec.CommandContext(ctx, "sh", "-c", cmd) //nolint:gosec // cfg.Cmd is operator-supplied CLI config, not untrusted input; see runPipeline's doc comment
}

// startEncryptingPipeline starts cfg.Cmd piped into gpg (sourceCmd's stdout
// wired to gpgCmd's stdin) and returns both commands, already started, plus
// gpgOut: gpg's stdout, ready for the caller to drain (e.g. via stageBackup)
// into the encrypted backup. The caller is responsible for Wait-ing on both
// commands once gpgOut has been fully read, per exec.Cmd.StdoutPipe's
// documented contract — gpgCmd first, since sourceCmd's exit only matters
// once gpg (its downstream reader) is done with it.
func startEncryptingPipeline(ctx context.Context, cfg *config.Config, log *slog.Logger) (sourceCmd, gpgCmd *exec.Cmd, gpgOut io.ReadCloser, err error) {
	sourceCmd = newSourceCommand(ctx, cfg.Cmd)
	sourceCmd.Stderr = &logWriter{log: log, msg: "command stderr"}
	// The backup command may be arbitrary and its output/behavior is
	// outside our control; make sure it can't read the encryption
	// passphrase out of its environment.
	sourceCmd.Env = environWithout("GPG_PASSPHRASE")

	sourceOut, err := sourceCmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("wiring command output: %w", err)
	}

	gpgCmd, passphraseWriter, passphraseReadEnd, err := buildGPGCommand(ctx, cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("building gpg command: %w", err)
	}

	gpgCmd.Stdin = sourceOut
	gpgCmd.Stderr = &logWriter{log: log, msg: "gpg stderr"}
	// gpg itself gets the passphrase via --passphrase-fd, never via env.
	gpgCmd.Env = environWithout("GPG_PASSPHRASE")

	gpgOut, err = gpgCmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("wiring gpg output: %w", err)
	}

	log.Debug("starting source command", "cmd", cfg.Cmd)

	if err := sourceCmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("starting command %q: %w", cfg.Cmd, err)
	}

	log.Debug("starting gpg", "args", gpgCmd.Args)

	if err := gpgCmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("starting gpg: %w", err)
	}

	if passphraseReadEnd != nil {
		// gpg (the child) has its own duplicated copy of this fd; the
		// parent's copy must be closed explicitly or it leaks for the
		// life of the process (exec.Cmd only auto-closes pipes it created
		// itself via StdoutPipe/StdinPipe/StderrPipe, not ExtraFiles).
		_ = passphraseReadEnd.Close()
	}

	if passphraseWriter != nil {
		if err := writePassphrase(passphraseWriter, cfg.Passphrase); err != nil {
			return nil, nil, nil, err
		}
	}

	return sourceCmd, gpgCmd, gpgOut, nil
}

// logWriter adapts a *slog.Logger into an io.Writer, line-buffering writes
// and logging each complete line as msg with an "output" attribute. Used for
// the source command's and gpg's stderr (see startEncryptingPipeline)
// instead of wiring them to os.Stderr directly: under the Windows service
// (see runAsService in service_windows.go) there is no console for
// os.Stderr to reach, so that output would otherwise be silently lost;
// logging through log instead routes it wherever log itself writes —
// os.Stderr for a console run, the Windows Event Log for a service.
type logWriter struct {
	log *slog.Logger
	msg string
	buf bytes.Buffer
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)

	for {
		data := w.buf.Bytes()

		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}

		if line := strings.TrimRight(string(data[:i]), "\r"); line != "" {
			w.log.Warn(w.msg, "output", line)
		}

		w.buf.Next(i + 1)
	}

	return len(p), nil
}

// stageBackup drains r (the gpg pipeline's stdout) into a private temporary
// file under cfg.StagingDir (the OS default temp directory if unset). The
// returned path holds the complete encrypted backup once err is nil; the
// caller owns removing it once every target has finished uploading from it.
//
// bytesWritten reports how many bytes were read from r even on error (e.g.
// a disk-full mid-write), since runPipeline still wants that for its own
// return value and logging.
func stageBackup(cfg *config.Config, r io.Reader) (path string, bytesWritten int64, err error) {
	f, err := os.CreateTemp(cfg.StagingDir, "go-backup-tool-*.staged")
	if err != nil {
		return "", 0, fmt.Errorf("creating staging file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := f.Chmod(0o600); err != nil {
		_ = os.Remove(f.Name())
		return "", 0, fmt.Errorf("setting permissions on staging file %q: %w", f.Name(), err)
	}

	n, err := io.Copy(f, r)
	if err != nil {
		_ = os.Remove(f.Name())
		return "", n, fmt.Errorf("writing staging file %q: %w", f.Name(), err)
	}

	return f.Name(), n, nil
}

// writePassphrase writes the gpg symmetric-encryption passphrase into w and
// closes it, as gpg (given --passphrase-fd) expects: a single write followed
// by EOF.
func writePassphrase(w io.WriteCloser, passphrase string) error {
	if _, err := io.WriteString(w, passphrase); err != nil {
		_ = w.Close()
		return fmt.Errorf("writing gpg passphrase: %w", err)
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("closing gpg passphrase pipe: %w", err)
	}

	return nil
}

// targetLabel identifies t in log/error messages, naming both the server it
// came from and its bucket, so multi-server setups are easy to debug.
func targetLabel(t *config.Target) string {
	return fmt.Sprintf("server %q, bucket %q", t.ServerName, t.Bucket)
}

// firstPipelineError reports the first failure among the pipeline's stages,
// in source -> gpg -> staging -> upload order.
func firstPipelineError(cmd string, sourceErr, gpgErr, stageErr, uploadErr error) error {
	stages := [...]struct {
		err   error
		label string
	}{
		{sourceErr, fmt.Sprintf("command %q failed", cmd)},
		{gpgErr, "gpg failed"},
		{stageErr, "staging encrypted backup"},
		{uploadErr, "uploading to targets"},
	}

	for _, stage := range stages {
		if stage.err != nil {
			return fmt.Errorf("%s: %w", stage.label, stage.err)
		}
	}

	return nil
}

// buildGPGCommand constructs the gpg invocation for the configured mode.
// For symmetric mode it returns an io.WriteCloser that the caller must write
// the passphrase into (and close) after starting the command, plus the read
// end of that same pipe, which the caller must close in the parent process
// once the command has started (see the comment at its call site in
// runPipeline).
func buildGPGCommand(ctx context.Context, cfg *config.Config) (cmd *exec.Cmd, passphraseWriter io.WriteCloser, passphraseReadEnd *os.File, err error) {
	args := []string{"--batch", "--yes"}

	if cfg.GPGHomedir != "" {
		args = append(args, "--homedir", cfg.GPGHomedir)
	}

	if cfg.Armor {
		args = append(args, "--armor")
	}

	if cfg.Symmetric {
		pr, pw, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, nil, nil, fmt.Errorf("creating passphrase pipe: %w", pipeErr)
		}

		passphraseReadEnd = pr
		passphraseWriter = pw
		// fd 0,1,2 are stdin/stdout/stderr; the first ExtraFiles entry
		// becomes fd 3 in the child process.
		args = append(args, "--pinentry-mode", "loopback", "--passphrase-fd", "3", "--symmetric")
	} else {
		args = append(args, "--trust-model", "always", "--encrypt")
		for _, r := range cfg.Recipients {
			args = append(args, "--recipient", r)
		}
	}

	cmd = exec.CommandContext(ctx, cfg.GPGBin, args...) //nolint:gosec // cfg.GPGBin/args are operator-supplied CLI config, not untrusted input
	if passphraseReadEnd != nil {
		cmd.ExtraFiles = []*os.File{passphraseReadEnd}
	}

	return cmd, passphraseWriter, passphraseReadEnd, nil
}

// uploadStagedToTargets uploads the already-staged backup at stagingPath
// (see stageBackup) to every one of cfg.Targets, each independently and
// concurrently: every target opens its own handle on stagingPath, so one
// target being slow or blocked doesn't affect any other's progress the way
// funneling them all through a single shared reader would.
//
// Each target gets exactly one attempt; a target whose attempt fails is not
// retried, and its failure is permanent for this run. onTargetDone (see its
// doc on runPipeline) is called for target i the moment this attempt's
// outcome is known, independently of every other target still in progress;
// onTargetDone must be safe for concurrent use, since every target's
// goroutine calls it on its own.
//
// It returns the combined error via errors.Join, for callers that just want
// to know whether anything failed.
func uploadStagedToTargets(ctx context.Context, cfg *config.Config, stagingPath string, onTargetDone func(index int, err error), log *slog.Logger) error {
	targetErrs := make([]error, len(cfg.Targets))

	var wg sync.WaitGroup

	for i := range cfg.Targets {
		t := &cfg.Targets[i]

		log.Debug("target upload starting", "target", targetLabel(t))

		wg.Go(func() {
			start := time.Now()

			err := uploadTargetAttempt(ctx, cfg, t, stagingPath, log)
			if err == nil {
				log.Debug("target upload finished", "target", targetLabel(t), "duration", time.Since(start))
				onTargetDone(i, nil)

				return
			}

			log.Warn("target upload failed", "target", targetLabel(t), "duration", time.Since(start), "err", err)

			targetErrs[i] = fmt.Errorf("target (%s): %w", targetLabel(t), err)
			onTargetDone(i, targetErrs[i])
		})
	}

	wg.Wait()

	return errors.Join(targetErrs...)
}

// uploadTargetAttempt runs a single upload attempt for target t, reading
// stagingPath fresh from the start.
func uploadTargetAttempt(ctx context.Context, cfg *config.Config, t *config.Target, stagingPath string, log *slog.Logger) error {
	f, err := os.Open(stagingPath) //nolint:gosec // stagingPath is our own staging file (see stageBackup), not user input
	if err != nil {
		return fmt.Errorf("opening staged backup: %w", err)
	}
	defer func() { _ = f.Close() }()

	switch t.Kind {
	case config.ServerKindLocal:
		if err := backup.WriteLocalObject(cfg, t, f); err != nil {
			return err
		}

		if err := backup.RecordLocalWrite(ctx, cfg, t, log); err != nil {
			// Retention tracking is best-effort auxiliary bookkeeping: the
			// backup itself already succeeded, so a db hiccup here shouldn't
			// fail the whole target (or trigger a retry of the upload).
			log.Warn("retention tracking failed", "target", targetLabel(t), "err", err)
		}

		return nil

	case config.ServerKindRemote:
		return UploadToRemote(ctx, cfg, t, f)
	}

	return nil
}

// RemoteObjectURL returns the URL a remote target (t.Kind ==
// config.ServerKindRemote) uploads/deletes key at: t.Endpoint's
// /api/v1/objects/{bucket}/{key}, where t.Bucket is the id identifying the
// destination instance's storage path (see receiver.go) and key is
// path-escaped since it may contain characters not otherwise safe in a URL
// path segment.
func RemoteObjectURL(t *config.Target, key string) string {
	return fmt.Sprintf("%s/api/v1/objects/%s/%s",
		strings.TrimSuffix(t.Endpoint, "/"), url.PathEscape(t.Bucket), url.PathEscape(key))
}

// remoteAuthHeader signs a fresh JWT with cfg.Identity (this instance's own
// RSA key and UUID — see loadServerIdentity) scoped to t.Bucket (the
// destination instance's receiver id) and formats it as an Authorization:
// Bearer header value, for UploadToRemote/DeleteRemoteObject. Errors if
// cfg.Identity is nil, meaning loadServerIdentity failed at startup.
func remoteAuthHeader(cfg *config.Config, t *config.Target) (string, error) {
	if cfg.Identity == nil {
		return "", errors.New("no server identity available (see startup log for why loadServerIdentity failed)")
	}

	token, err := cfg.Identity.SignRequest(t.Bucket)
	if err != nil {
		return "", fmt.Errorf("signing request: %w", err)
	}

	return "Bearer " + token, nil
}

// UploadToRemote streams r as the body of a PUT request to another
// go-backup-tool instance's receiver API (target t, kind ==
// config.ServerKindRemote), authenticated with a JWT signed by this instance's own
// identity (see remoteAuthHeader). r is streamed directly as the request
// body, never buffered.
func UploadToRemote(ctx context.Context, cfg *config.Config, t *config.Target, r io.Reader) error {
	return doAuthenticatedRemoteRequest(ctx, cfg, t, http.MethodPut, r, "application/octet-stream")
}

// DeleteRemoteObject removes the object at target t's destination instance
// (bucket as id, cfg.Key as key): the client-side counterpart of the
// receiver API's DELETE endpoint (see handleDeleteObject in webui.go). The
// pipeline itself has no caller for this — staging the backup locally
// before any target upload starts means a failed run never leaves a target
// with a partial object to clean up — but it remains available (and
// tested against the real receiver handler, see remote_test.go) as part of
// the receiver API's client surface.
func DeleteRemoteObject(ctx context.Context, cfg *config.Config, t *config.Target) error {
	return doAuthenticatedRemoteRequest(ctx, cfg, t, http.MethodDelete, nil, "")
}

// doAuthenticatedRemoteRequest signs, sends, and status-checks a method
// request (with body, whose Content-Type is set only when contentType is
// non-empty) to target t's receiver API — shared by UploadToRemote and
// DeleteRemoteObject, which otherwise duplicate this build/send/check
// sequence identically apart from method, body, and Content-Type.
func doAuthenticatedRemoteRequest(ctx context.Context, cfg *config.Config, t *config.Target, method string, body io.Reader, contentType string) error {
	auth, err := remoteAuthHeader(cfg, t)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, method, RemoteObjectURL(t, cfg.Key), body)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Authorization", auth)

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := remoteHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return remoteResponseError(resp)
	}

	return nil
}

// remoteResponseError builds an error from a non-2xx response, including a
// bounded read of the response body so a misbehaving or malicious server
// can't exhaust memory via an unbounded response.
func remoteResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	return fmt.Errorf("remote instance returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
}
