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

// runPipeline wires three stages into a single streaming pipeline:
//
//	shell command  --(stdout)-->  gpg encrypt  --(stdout)-->  upload/write to targets
//
// Each target is either an S3 (or S3-compatible) bucket, a directory on the
// local filesystem, or another go-backup-tool instance's receiver API. Data
// never touches disk on the source/gpg legs and is never fully buffered in
// memory.
//
// cfg.cmd is run via "sh -c" deliberately: it lets an operator pass a full
// shell pipeline (e.g. "mysqldump db | gzip") as the backup source. cfg.cmd
// is operator-supplied CLI configuration, not untrusted external input, so
// this is the intended behavior rather than a command-injection risk.
//
// The returned targetErrs is index-aligned with cfg.targets (nil entries
// mean that target succeeded), for callers such as the web UI's status
// store that need per-target outcomes rather than just the combined error;
// it's nil if the pipeline failed before reaching the upload stage.
//
// bytesWritten is the size of the encrypted object written to every target
// (they all receive the same byte stream via the fan-out in
// uploadToTargets), for callers that want to report the backup's size; it's
// 0 if the pipeline failed before reaching the upload stage.
func runPipeline(ctx context.Context, cfg *config, log *slog.Logger) (targetErrs []error, bytesWritten int64, err error) {
	sourceCmd := exec.CommandContext(ctx, "sh", "-c", cfg.cmd) //nolint:gosec // cfg.cmd is operator-supplied CLI config, not untrusted input; see comment above
	sourceCmd.Stderr = os.Stderr
	// The backup command may be arbitrary and its output/behavior is
	// outside our control; make sure it can't read the encryption
	// passphrase out of its environment.
	sourceCmd.Env = environWithout("GPG_PASSPHRASE")

	sourceOut, err := sourceCmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("wiring command output: %w", err)
	}

	gpgCmd, passphraseWriter, passphraseReadEnd, err := buildGPGCommand(ctx, cfg)
	if err != nil {
		return nil, 0, fmt.Errorf("building gpg command: %w", err)
	}

	gpgCmd.Stdin = sourceOut
	gpgCmd.Stderr = os.Stderr
	// gpg itself gets the passphrase via --passphrase-fd, never via env.
	gpgCmd.Env = environWithout("GPG_PASSPHRASE")

	gpgOut, err := gpgCmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("wiring gpg output: %w", err)
	}

	log.Debug("starting source command", "cmd", cfg.cmd)

	if err := sourceCmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("starting command %q: %w", cfg.cmd, err)
	}

	log.Debug("starting gpg", "args", gpgCmd.Args)

	if err := gpgCmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("starting gpg: %w", err)
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
			return nil, 0, err
		}
	}

	log.Debug("streaming to targets", "targets", len(cfg.targets))

	targetErrs, bytesWritten, uploadErr := uploadToTargets(ctx, cfg, gpgOut, log)

	// gpgOut must be fully drained (which uploadToTargets does) before Wait,
	// per exec.Cmd.StdoutPipe's documented contract.
	gpgErr := gpgCmd.Wait()
	sourceErr := sourceCmd.Wait()

	cleanupPartialUpload(ctx, cfg, sourceErr, gpgErr, targetErrs, log)

	return targetErrs, bytesWritten, firstPipelineError(cfg.cmd, sourceErr, gpgErr, uploadErr)
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

// cleanupPartialUpload removes the object at each target's destination
// (bucket/cfg.key for S3, localPath/bucket/cfg.key for local) whose upload
// succeeded (targetErrs[i] == nil) if the source command or gpg failed. A
// target whose own upload failed is skipped: it left nothing (or nothing
// complete enough to matter) behind.
//
// Because this is a live streaming pipeline, a mid-stream failure in the
// source command or gpg can still leave a well-formed (but truncated)
// object successfully written, since gpg happily finalizes encryption of
// whatever partial input it received before the pipe closed. Treat that as
// corrupt and remove it rather than leaving a silent partial backup behind.
//
// It overwrites targetErrs[i] for every target it processes here, even when
// the removal itself succeeds: a target that gets to this loop had a
// successful upload rolled back, so nil (meaning "succeeded") would no
// longer be true — callers such as the web UI's status store rely on
// targetErrs reflecting the pipeline's final outcome, not just the upload
// stage's.
func cleanupPartialUpload(ctx context.Context, cfg *config, sourceErr, gpgErr error, targetErrs []error, log *slog.Logger) {
	if sourceErr == nil && gpgErr == nil {
		return
	}

	log.Debug("pipeline failed mid-stream, rolling back completed target uploads", "source_err", sourceErr, "gpg_err", gpgErr)

	for i := range cfg.targets {
		if targetErrs[i] != nil {
			continue
		}

		t := &cfg.targets[i]

		switch t.kind {
		case serverKindLocal:
			if delErr := deleteLocalObject(cfg, t); delErr != nil {
				log.Warn("failed to remove partial write", "path", localObjectPath(cfg, t), "target", targetLabel(t), "err", delErr)
				targetErrs[i] = fmt.Errorf("upload rolled back after pipeline failure, and removing the partial write also failed: %w", delErr)

				continue
			}

			if recErr := removeRetentionRecord(ctx, cfg, t); recErr != nil {
				log.Warn("failed to remove retention db record", "path", localObjectPath(cfg, t), "target", targetLabel(t), "err", recErr)
			}

			log.Info("rolled back partial write", "path", localObjectPath(cfg, t), "target", targetLabel(t))

			targetErrs[i] = errors.New("upload rolled back after pipeline failure")

		case serverKindRemote:
			if delErr := deleteRemoteObject(ctx, cfg, t); delErr != nil {
				log.Warn("failed to remove partial upload", "url", remoteObjectURL(t, cfg.key), "target", targetLabel(t), "err", delErr)
				targetErrs[i] = fmt.Errorf("upload rolled back after pipeline failure, and removing it also failed: %w", delErr)

				continue
			}

			log.Info("rolled back partial upload", "url", remoteObjectURL(t, cfg.key), "target", targetLabel(t))

			targetErrs[i] = errors.New("upload rolled back after pipeline failure")

		case serverKindS3:
			client, err := newS3Client(ctx, t)
			if err != nil {
				log.Warn("failed to set up s3 client to remove partial upload", "bucket", t.bucket, "key", cfg.key, "target", targetLabel(t), "err", err)
				targetErrs[i] = fmt.Errorf("upload rolled back after pipeline failure, and setting up the s3 client to remove it also failed: %w", err)

				continue
			}

			if delErr := deleteS3Object(ctx, client, cfg, t); delErr != nil {
				log.Warn("failed to remove partial upload", "bucket", t.bucket, "key", cfg.key, "target", targetLabel(t), "err", delErr)
				targetErrs[i] = fmt.Errorf("upload rolled back after pipeline failure, and removing it also failed: %w", delErr)

				continue
			}

			log.Info("rolled back partial upload", "bucket", t.bucket, "key", cfg.key, "target", targetLabel(t))

			targetErrs[i] = errors.New("upload rolled back after pipeline failure")
		}
	}
}

// targetLabel identifies t in log/error messages, naming both the server it
// came from and its bucket, so multi-server setups are easy to debug.
func targetLabel(t *target) string {
	return fmt.Sprintf("server %q, bucket %q", t.serverName, t.bucket)
}

// firstPipelineError reports the first failure among the pipeline's three
// stages, in source -> gpg -> upload order.
func firstPipelineError(cmd string, sourceErr, gpgErr, uploadErr error) error {
	stages := [...]struct {
		err   error
		label string
	}{
		{sourceErr, fmt.Sprintf("command %q failed", cmd)},
		{gpgErr, "gpg failed"},
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

// uploadToTargets streams r, encrypted, to every one of cfg.targets, fanning
// it out to each target's uploader concurrently via io.Pipe (rather than
// buffering the whole object) so every target gets the same byte stream
// without spooling it to disk or memory first.
//
// It returns one error per target, index-aligned with cfg.targets (nil
// means that target's upload succeeded), the number of bytes read from r
// (and so written to every target, since they all receive the same fanned-
// out stream), plus the combined error via errors.Join for callers that
// just want to know if anything failed.
//
// r is always fully drained before this returns, even if every target fails
// (e.g. because its client couldn't be set up) — callers such as
// runPipeline rely on that per exec.Cmd.StdoutPipe's documented contract.
func uploadToTargets(ctx context.Context, cfg *config, r io.Reader, log *slog.Logger) (targetErrs []error, bytesWritten int64, joined error) {
	targetErrs = make([]error, len(cfg.targets))
	// uploaders[i] streams its target's share of the fan-out to wherever
	// cfg.targets[i] belongs (S3 or local filesystem); nil means that
	// target's setup already failed and targetErrs[i] explains why.
	uploaders := make([]func(io.Reader) error, len(cfg.targets))

	for i := range cfg.targets {
		t := &cfg.targets[i]

		switch t.kind {
		case serverKindLocal:
			uploaders[i] = func(r io.Reader) error {
				if err := writeLocalObject(cfg, t, r); err != nil {
					return err
				}

				if err := recordLocalWrite(ctx, cfg, t, log); err != nil {
					// Retention tracking is best-effort auxiliary
					// bookkeeping: the backup itself already succeeded, so a
					// db hiccup here shouldn't fail the whole target.
					log.Warn("retention tracking failed", "target", targetLabel(t), "err", err)
				}

				return nil
			}

		case serverKindRemote:
			uploaders[i] = func(r io.Reader) error {
				return uploadToRemote(ctx, cfg, t, r)
			}

		case serverKindS3:
			client, err := newS3Client(ctx, t)
			if err != nil {
				targetErrs[i] = fmt.Errorf("target (%s): setting up s3 client: %w", targetLabel(t), err)
				continue
			}

			uploaders[i] = func(r io.Reader) error {
				return uploadToS3(ctx, client, cfg, t, r)
			}
		}
	}

	var (
		wg          sync.WaitGroup
		writers     []io.Writer
		pipeWriters []*io.PipeWriter
	)

	for i := range cfg.targets {
		if uploaders[i] == nil {
			continue // setup already failed for this target
		}

		pr, pw := io.Pipe()
		pipeWriters = append(pipeWriters, pw)
		writers = append(writers, pw)

		t := &cfg.targets[i]

		log.Debug("target upload starting", "target", targetLabel(t))

		wg.Go(func() {
			start := time.Now()

			err := uploaders[i](pr)
			if err != nil {
				targetErrs[i] = fmt.Errorf("target (%s): %w", targetLabel(t), err)
				log.Warn("target upload failed", "target", targetLabel(t), "duration", time.Since(start), "err", err)
			} else {
				log.Debug("target upload finished", "target", targetLabel(t), "duration", time.Since(start))
			}
			// Drain any input the uploader didn't consume (e.g. because it
			// returned early on error) so the fan-out copy below, which
			// writes to every target's pipe in lockstep, can't block
			// forever waiting for this target's reader.
			_, _ = io.Copy(io.Discard, pr)
		})
	}

	// With zero writers (every target failed setup), fanOutCopy still drains
	// r: it just has no writers to fan out to.
	bytesWritten, copyErr := fanOutCopy(writers, r)

	for _, pw := range pipeWriters {
		_ = pw.CloseWithError(copyErr)
	}

	wg.Wait()

	allErrs := append(append([]error(nil), targetErrs...), copyErr)

	return targetErrs, bytesWritten, errors.Join(allErrs...)
}

// fanOutCopy streams r to every writer in ws, writing each chunk to all of
// them concurrently rather than through a single io.MultiWriter.
//
// A plain io.MultiWriter writes to its writers one at a time within a
// single Write call, so a writer that blocks (e.g. a pipe whose reader is
// still dialing a dead remote) head-of-line-blocks every writer after it,
// and a writer that errors aborts the whole Write before the remaining
// writers ever see that chunk. Worse, since every writer here is an
// *io.PipeWriter, an error on one (including io.ErrClosedPipe, which
// net/http raises by closing a request's body — our pipe reader — once it
// gives up on a stuck target) becomes the single copyErr the caller then
// uses to CloseWithError every pipe, corrupting targets that had nothing
// wrong with them.
//
// fanOutCopy avoids both problems: each writer gets its chunk in its own
// goroutine, and a writer that errors is dropped (replaced with io.Discard)
// for the rest of the copy instead of stopping the others. The dropped
// writer's own error isn't returned here — its target's goroutine in
// uploadToTargets already captured it via the uploader's own return value,
// so surfacing it again here would just duplicate it under a less accurate
// label. The returned error reports only a genuine failure reading r, which
// legitimately ends the stream for every target.
func fanOutCopy(ws []io.Writer, r io.Reader) (int64, error) {
	live := make([]io.Writer, len(ws))
	copy(live, ws)

	buf := make([]byte, 32*1024)

	var total int64

	for {
		nr, rerr := r.Read(buf)
		if nr > 0 {
			chunk := buf[:nr]

			var wg sync.WaitGroup

			for i, w := range live {
				if w == io.Discard {
					continue
				}

				wg.Add(1)

				go func(i int, w io.Writer) {
					defer wg.Done()

					if _, err := w.Write(chunk); err != nil {
						live[i] = io.Discard
					}
				}(i, w)
			}

			wg.Wait()

			total += int64(nr)
		}

		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}

			return total, rerr
		}
	}
}

// uploadToS3 streams r as the body of an S3 object at target t using a
// multipart uploader, so the ciphertext never needs to be fully buffered in
// memory.
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
		// r is backed by an *os.File pipe, which has a Seek method that
		// fails at the syscall level since pipes aren't seekable. Hiding
		// it behind a bare io.Reader stops the SDK from type-asserting to
		// io.ReadSeeker and attempting to probe the body's size that way.
		Body: struct{ io.Reader }{r},
	})

	return err
}

// deleteS3Object removes the object at target t's bucket/cfg.key, used to
// clean up a partial upload after a mid-stream failure.
func deleteS3Object(ctx context.Context, client *s3.Client, cfg *config, t *target) error {
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(t.bucket),
		Key:    aws.String(cfg.key),
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

// deleteLocalObject removes the file at localObjectPath(cfg, t), used to
// clean up a partial write after a mid-stream failure. A file that's
// already gone is not an error.
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
// (bucket as id, cfg.key as key), used to clean up a partial upload after a
// mid-stream failure.
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
