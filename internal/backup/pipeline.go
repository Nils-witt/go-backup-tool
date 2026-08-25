package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// remoteHTTPClient is shared by every remote target's upload/delete
// requests. It sets no client-side timeout: the request's context (derived
// from the run's overall -timeout, if any) governs cancellation instead,
// consistent with how the S3 SDK client and local filesystem writes are
// bounded only by ctx.
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
//	local staging file --(read once per target, retried independently)--> every target
//
// Each target is either an S3 (or S3-compatible) bucket, a directory on the
// local filesystem, or another go-backup-tool instance's receiver API. The
// backup command's output is encrypted and drained into a private local
// temp file (see stageBackup) before any target ever sees it, rather than
// streamed directly to every target. That has two consequences: a
// source/gpg failure never reaches a target at all (there's nothing to roll
// back, unlike a live streaming design where a target could already have a
// complete-but-truncated object by the time the failure is noticed), and
// each target then uploads from that same stable file independently — so a
// target that fails is retried (see uploadTargetWithRetry) by re-reading
// the file, without re-running the backup command or gpg and without
// affecting any other target's own attempts.
//
// cfg.cmd is run through the platform shell ("sh -c" on every OS but
// Windows, "cmd /C" there — see newSourceCommand) deliberately: it lets an
// operator pass a full shell pipeline (e.g. "mysqldump db | gzip") as the
// backup source. cfg.cmd is operator-supplied CLI configuration, not
// untrusted external input, so this is the intended behavior rather than a
// command-injection risk.
//
// onTargetDone, if non-nil, is called exactly once per target — index-
// aligned with cfg.targets — as soon as that target's own outcome is known
// (nil means it succeeded), independently of every other target and of the
// job as a whole: a target that finishes early (or fails) is reported right
// away rather than waiting for the slowest target, or any retries, to
// finish. Callers such as the web UI's status store use this to refresh
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
func runPipeline(ctx context.Context, cfg *config, log *slog.Logger, onTargetDone func(index int, err error)) (bytesWritten int64, err error) {
	// Guarantees the onTargetDone contract above even on an early return:
	// uploadStagedToTargets reports each target itself once it starts, so
	// this only fires for a failure earlier in the pipeline, and it always
	// sees the right err — a named return set by any "return ..., err"
	// above is assigned before deferred calls run.
	uploadStarted := false

	defer func() {
		if !uploadStarted {
			for i := range cfg.targets {
				onTargetDone(i, err)
			}
		}
	}()

	sourceCmd, gpgCmd, gpgOut, err := startEncryptingPipeline(ctx, cfg, log)
	if err != nil {
		return 0, err
	}

	log.Debug("staging encrypted backup", "dir", cfg.stagingDir)

	stagingPath, bytesWritten, stageErr := stageBackup(cfg, gpgOut)

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
		log.Debug("uploading to targets", "targets", len(cfg.targets))

		uploadStarted = true
		uploadErr = uploadStagedToTargets(ctx, cfg, stagingPath, onTargetDone, log)
	}

	return bytesWritten, firstPipelineError(cfg.cmd, sourceErr, gpgErr, stageErr, uploadErr)
}

// newSourceCommand builds the (not yet started) command that runs cmd
// through the platform shell. Windows has no "sh" on PATH by default (Go's
// standard installer doesn't ship one, unlike every other supported OS), so
// it uses "cmd /C" there instead; everywhere else it uses "sh -c", per
// runPipeline's doc comment.
func newSourceCommand(ctx context.Context, cmd string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", cmd) //nolint:gosec // cfg.cmd is operator-supplied CLI config, not untrusted input; see runPipeline's doc comment
	}

	return exec.CommandContext(ctx, "sh", "-c", cmd) //nolint:gosec // cfg.cmd is operator-supplied CLI config, not untrusted input; see runPipeline's doc comment
}

// startEncryptingPipeline starts cfg.cmd piped into gpg (sourceCmd's stdout
// wired to gpgCmd's stdin) and returns both commands, already started, plus
// gpgOut: gpg's stdout, ready for the caller to drain (e.g. via stageBackup)
// into the encrypted backup. The caller is responsible for Wait-ing on both
// commands once gpgOut has been fully read, per exec.Cmd.StdoutPipe's
// documented contract — gpgCmd first, since sourceCmd's exit only matters
// once gpg (its downstream reader) is done with it.
func startEncryptingPipeline(ctx context.Context, cfg *config, log *slog.Logger) (sourceCmd, gpgCmd *exec.Cmd, gpgOut io.ReadCloser, err error) {
	sourceCmd = newSourceCommand(ctx, cfg.cmd)
	sourceCmd.Stderr = os.Stderr
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
	gpgCmd.Stderr = os.Stderr
	// gpg itself gets the passphrase via --passphrase-fd, never via env.
	gpgCmd.Env = environWithout("GPG_PASSPHRASE")

	gpgOut, err = gpgCmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("wiring gpg output: %w", err)
	}

	log.Debug("starting source command", "cmd", cfg.cmd)

	if err := sourceCmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("starting command %q: %w", cfg.cmd, err)
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
		if err := writePassphrase(passphraseWriter, cfg.passphrase); err != nil {
			return nil, nil, nil, err
		}
	}

	return sourceCmd, gpgCmd, gpgOut, nil
}

// stageBackup drains r (the gpg pipeline's stdout) into a private temporary
// file under cfg.stagingDir (the OS default temp directory if unset). The
// returned path holds the complete encrypted backup once err is nil; the
// caller owns removing it once every target has finished uploading from it.
//
// bytesWritten reports how many bytes were read from r even on error (e.g.
// a disk-full mid-write), since runPipeline still wants that for its own
// return value and logging.
func stageBackup(cfg *config, r io.Reader) (path string, bytesWritten int64, err error) {
	f, err := os.CreateTemp(cfg.stagingDir, "go-backup-tool-*.staged")
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
func targetLabel(t *target) string {
	return fmt.Sprintf("server %q, bucket %q", t.serverName, t.bucket)
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
func buildGPGCommand(ctx context.Context, cfg *config) (cmd *exec.Cmd, passphraseWriter io.WriteCloser, passphraseReadEnd *os.File, err error) {
	args := []string{"--batch", "--yes"}

	if cfg.gpgHomedir != "" {
		args = append(args, "--homedir", cfg.gpgHomedir)
	}

	if cfg.armor {
		args = append(args, "--armor")
	}

	if cfg.symmetric {
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
		for _, r := range cfg.recipients {
			args = append(args, "--recipient", r)
		}
	}

	cmd = exec.CommandContext(ctx, cfg.gpgBin, args...) //nolint:gosec // cfg.gpgBin/args are operator-supplied CLI config, not untrusted input
	if passphraseReadEnd != nil {
		cmd.ExtraFiles = []*os.File{passphraseReadEnd}
	}

	return cmd, passphraseWriter, passphraseReadEnd, nil
}

// newS3Client builds an S3 client for target t. When t's server configured
// static credentials (access-key-env/secret-key-env), those are used
// directly; otherwise it falls back to the AWS SDK's standard
// credential/region resolution (env vars, shared config, IAM roles, ...).
// The optional custom endpoint and path-style addressing support
// self-hosted S3 servers.
func newS3Client(ctx context.Context, t *target) (*s3.Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(t.region)}

	if t.accessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(t.accessKey, t.secretKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if t.endpoint != "" {
			o.BaseEndpoint = aws.String(t.endpoint)
		}

		o.UsePathStyle = t.pathStyle
	}), nil
}

// uploadStagedToTargets uploads the already-staged backup at stagingPath
// (see stageBackup) to every one of cfg.targets, each independently and
// concurrently: every target opens its own handle on stagingPath, so one
// target being slow, blocked, or retrying doesn't affect any other's
// progress the way funneling them all through a single shared reader would.
//
// A target whose upload fails is retried in place (see
// uploadTargetWithRetry) without involving any other target; only after its
// attempts are exhausted does it count as failed. onTargetDone (see its doc
// on runPipeline) is called for target i the moment its own outcome — after
// any retries — is known, independently of every other target still in
// progress; onTargetDone must be safe for concurrent use, since every
// target's goroutine calls it on its own.
//
// It returns the combined error via errors.Join, for callers that just want
// to know whether anything failed.
func uploadStagedToTargets(ctx context.Context, cfg *config, stagingPath string, onTargetDone func(index int, err error), log *slog.Logger) error {
	targetErrs := make([]error, len(cfg.targets))

	var wg sync.WaitGroup

	for i := range cfg.targets {
		t := &cfg.targets[i]

		log.Debug("target upload starting", "target", targetLabel(t))

		wg.Go(func() {
			start := time.Now()

			err := uploadTargetWithRetry(ctx, cfg, t, stagingPath, log)
			if err != nil {
				targetErrs[i] = fmt.Errorf("target (%s): %w", targetLabel(t), err)
				log.Warn("target upload failed", "target", targetLabel(t), "duration", time.Since(start), "err", err)
			} else {
				log.Debug("target upload finished", "target", targetLabel(t), "duration", time.Since(start))
			}

			onTargetDone(i, targetErrs[i])
		})
	}

	wg.Wait()

	return errors.Join(targetErrs...)
}

// uploadTargetWithRetry attempts to upload stagingPath to target t, retrying
// up to cfg.retries times (waiting cfg.retryDelay between attempts) before
// giving up. Every attempt re-opens stagingPath from the start, so a target
// that fails part-way through never leaves later attempts reading from a
// stale offset, and retrying it never involves re-running the backup
// command or gpg, or touches any other target.
//
// cfg.retries < 1 (the zero value, e.g. for a *config built directly in a
// test rather than via newConfigDefaults) is treated as 1: always try
// exactly once, no retries, rather than silently uploading nothing.
func uploadTargetWithRetry(ctx context.Context, cfg *config, t *target, stagingPath string, log *slog.Logger) error {
	attempts := max(cfg.retries, 1)

	var err error

	for attempt := 1; attempt <= attempts; attempt++ {
		err = uploadTargetAttempt(ctx, cfg, t, stagingPath, log)
		if err == nil {
			return nil
		}

		if attempt == attempts {
			break
		}

		log.Warn("target upload attempt failed, retrying", "target", targetLabel(t), "attempt", attempt, "max_attempts", attempts, "retry_delay", cfg.retryDelay, "err", err)

		if !sleepOrDone(ctx, cfg.retryDelay) {
			return fmt.Errorf("attempt %d/%d: %w (retry canceled: %w)", attempt, attempts, err, ctx.Err())
		}
	}

	return fmt.Errorf("attempt %d/%d: %w", attempts, attempts, err)
}

// sleepOrDone blocks for d, or until ctx is done, whichever comes first,
// reporting whether the wait completed normally (false means ctx ended it
// early, so the caller should stop rather than proceed).
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// uploadTargetAttempt runs a single upload attempt for target t, reading
// stagingPath fresh from the start.
func uploadTargetAttempt(ctx context.Context, cfg *config, t *target, stagingPath string, log *slog.Logger) error {
	f, err := os.Open(stagingPath) //nolint:gosec // stagingPath is our own staging file (see stageBackup), not user input
	if err != nil {
		return fmt.Errorf("opening staged backup: %w", err)
	}
	defer func() { _ = f.Close() }()

	switch t.kind {
	case serverKindLocal:
		if err := writeLocalObject(cfg, t, f); err != nil {
			return err
		}

		if err := recordLocalWrite(ctx, cfg, t, log); err != nil {
			// Retention tracking is best-effort auxiliary bookkeeping: the
			// backup itself already succeeded, so a db hiccup here shouldn't
			// fail the whole target (or trigger a retry of the upload).
			log.Warn("retention tracking failed", "target", targetLabel(t), "err", err)
		}

		return nil

	case serverKindRemote:
		return uploadToRemote(ctx, cfg, t, f)

	case serverKindS3:
		client, err := newS3Client(ctx, t)
		if err != nil {
			return fmt.Errorf("setting up s3 client: %w", err)
		}

		return uploadToS3(ctx, client, cfg, t, f)
	}

	return nil
}

// uploadToS3 uploads r (an already-staged local file, opened fresh for this
// attempt by uploadTargetAttempt) as the body of an S3 object at target t
// using a multipart uploader.
//
// r is a genuine *os.File standing in for io.Reader here, so it satisfies
// io.ReadSeeker; passed through as-is (rather than hidden behind a bare
// io.Reader), the SDK's uploader type-asserts that and uses it to read the
// object's size upfront instead of buffering parts speculatively.
//
// feature/s3/manager is deprecated in favor of feature/s3/transfermanager,
// but as of writing transfermanager is still pre-1.0 (v0.3.x) with no API
// stability guarantee, so manager remains the safer dependency choice.
// Revisit once transfermanager reaches a stable v1 release.
func uploadToS3(ctx context.Context, client *s3.Client, cfg *config, t *target, r io.Reader) error {
	uploader := manager.NewUploader(client) //nolint:staticcheck // see deprecation note above

	_, err := uploader.Upload(ctx, &s3.PutObjectInput{ //nolint:staticcheck // see deprecation note above
		Bucket: aws.String(t.bucket),
		Key:    aws.String(cfg.key),
		Body:   r,
	})

	return err
}

// localObjectPath returns the filesystem path a local target (t.kind ==
// serverKindLocal) writes cfg.key to: t.localPath, with t.bucket as a
// subdirectory, mirroring the bucket/key layout used for S3 targets so both
// kinds share the same targets: schema.
func localObjectPath(cfg *config, t *target) string {
	return filepath.Join(t.localPath, t.bucket, cfg.key)
}

// writeLocalObject streams r to the local filesystem at
// localObjectPath(cfg, t), for a target whose server has type: local.
//
// It writes to a temporary file in the destination directory first and
// renames it into place once fully written, so a reader never observes a
// partially written object and a mid-stream failure leaves nothing at the
// final path.
func writeLocalObject(cfg *config, t *target, r io.Reader) error {
	dst := localObjectPath(cfg, t)
	dir := filepath.Dir(dst)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating directory %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}

	// A successful rename below moves tmp.Name() to dst, so this Remove
	// then finds nothing and is a harmless no-op; on any earlier error path
	// it cleans up the leftover temp file.
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting permissions on %q: %w", tmp.Name(), err)
	}

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %q: %w", tmp.Name(), err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", tmp.Name(), err)
	}

	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("renaming %q to %q: %w", tmp.Name(), dst, err)
	}

	return nil
}

// deleteLocalObject removes the file at localObjectPath(cfg, t), used by the
// receiver API's DELETE endpoint (see handleDeleteObject in webui.go). A
// file that's already gone is not an error.
func deleteLocalObject(cfg *config, t *target) error {
	err := os.Remove(localObjectPath(cfg, t))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// remoteObjectURL returns the URL a remote target (t.kind ==
// serverKindRemote) uploads/deletes key at: t.endpoint's
// /api/v1/objects/{bucket}/{key}, where t.bucket is the id identifying the
// destination instance's storage path (see receiver.go) and key is
// path-escaped since it may contain characters not otherwise safe in a URL
// path segment.
func remoteObjectURL(t *target, key string) string {
	return fmt.Sprintf("%s/api/v1/objects/%s/%s",
		strings.TrimSuffix(t.endpoint, "/"), url.PathEscape(t.bucket), url.PathEscape(key))
}

// uploadToRemote streams r as the body of a PUT request to another
// go-backup-tool instance's receiver API (target t, kind ==
// serverKindRemote), authenticated with t.token as a bearer token. r is
// streamed directly as the request body, never buffered.
func uploadToRemote(ctx context.Context, cfg *config, t *target, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, remoteObjectURL(t, cfg.key), r)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Content-Type", "application/octet-stream")

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

// deleteRemoteObject removes the object at target t's destination instance
// (bucket as id, cfg.key as key): the client-side counterpart of the
// receiver API's DELETE endpoint (see handleDeleteObject in webui.go). The
// pipeline itself has no caller for this — staging the backup locally
// before any target upload starts means a failed run never leaves a target
// with a partial object to clean up — but it remains available (and
// tested against the real receiver handler, see remote_test.go) as part of
// the receiver API's client surface.
func deleteRemoteObject(ctx context.Context, cfg *config, t *target) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, remoteObjectURL(t, cfg.key), nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+t.token)

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
