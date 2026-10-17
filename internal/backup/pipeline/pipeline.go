// Package pipeline runs a configured backup job end to end: executing its
// command, encrypting the output with gpg, staging it locally, and
// uploading it to every target — plus the background work that keeps a run
// healthy afterward: scheduling repeats, sweeping expired local objects, and
// sending the daily receiver report.
package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/dockerexec"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteAuth"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/tarball"
)

// remoteDefaultClient is shared by every remote target's upload/delete
// requests that use Go's default TLS verification. It sets no client-side
// timeout: the request's context (derived from the run's overall -timeout,
// if any) governs cancellation instead, consistent with how local
// filesystem writes are bounded only by ctx.
var remoteDefaultClient = &http.Client{}

// remoteTLSClients caches one client per distinct config.TargetTLS (see
// remoteHTTPClient), so targets with the same settings share connections.
// Entries are never evicted: there is one per TLS setting ever used, which
// only changes when an admin edits a server.
var (
	remoteTLSClientsMu sync.Mutex
	remoteTLSClients   = map[config.TargetTLS]*http.Client{}
)

// remoteHTTPClient returns the client for t's TLS settings:
// remoteDefaultClient for the default verification, otherwise a cached
// client whose transport is http.DefaultTransport's settings plus t's
// tls.Config, and, like remoteDefaultClient, no client-side timeout.
func remoteHTTPClient(t *config.Target) (*http.Client, error) {
	if t.TLS.IsZero() {
		return remoteDefaultClient, nil
	}

	remoteTLSClientsMu.Lock()
	defer remoteTLSClientsMu.Unlock()

	if c, ok := remoteTLSClients[t.TLS]; ok {
		return c, nil
	}

	tlsCfg, err := t.TLS.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("server %q: %w", t.ServerName, err)
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}

	transport := base.Clone()
	transport.TLSClientConfig = tlsCfg

	c := &http.Client{Transport: transport}
	remoteTLSClients[t.TLS] = c

	return c, nil
}

// TargetUploadRetryInterval is how long a failed target upload waits before
// Runner.RunOutstandingUploadRetries attempts it again.
const TargetUploadRetryInterval = 60 * time.Second

// removeStagingFileIfUnreferenced removes path (a staged backup file — see
// stageBackup) unless a still-outstanding target upload (see
// uploadStagedToTargets, Runner.RunOutstandingUploadRetries) still needs it
// for a future retry, in which case whichever caller eventually resolves
// that last reference removes it instead. A nil db (no persistence
// available, so no retry can happen either) always removes the file, same
// as before this function existed.
func removeStagingFileIfUnreferenced(ctx context.Context, db *store.Store, path string, log *slog.Logger) {
	if db != nil {
		n, err := db.CountOutstandingTargetUploadsForFile(ctx, path)
		if err != nil {
			log.Warn("checking outstanding target uploads before removing staging file", "path", path, "err", err)
			return
		}

		if n > 0 {
			return
		}
	}

	_ = os.Remove(path)
}

// runPipeline runs the backup in two phases:
//
//	for each of cfg.Sources, in order:
//	  shell command --(stdout)--> gpg encrypt --(stdout)--> local staging file
//	every staged file --(read once per target, independently)--> every target
//
// Each source is one file of the backup, stored as its Name under cfg.Key.
// Each target is either a directory on the local filesystem or another
// go-backup-tool instance's receiver API. Each command's output is encrypted
// and drained into a private local temp file (see stageSource) before any
// target ever sees it, rather than streamed directly to every target. That
// has two consequences: a source/gpg failure never reaches a target at all
// (there's nothing to roll back, unlike a live streaming design where a
// target could already have a complete-but-truncated object by the time the
// failure is noticed), and each target then uploads from those same stable
// files independently, so one target's failure never affects any other
// target's own attempt. A file whose command fails is left out, while every
// file that was staged is still uploaded: the run then ends with an error
// (and the job as incomplete, see backup.StatusStore.Finished). Each target
// gets exactly one attempt per file within a run (see
// uploadStagedToTargets); a failed upload's staged file is instead handed
// off to Runner.RunOutstandingUploadRetries, which retries it in the
// background every TargetUploadRetryInterval until it succeeds.
//
// Commands run one at a time, so a job's sources never compete with each
// other for the systems they read from. Each one is run through the
// platform shell ("sh -c" on every OS but Windows, "cmd /C" there — see
// newSourceCommand) deliberately: it lets an operator pass a full shell
// pipeline (e.g. "mysqldump db | gzip") as a backup source. It is
// operator-supplied configuration, not untrusted external input, so this is
// the intended behavior rather than a command-injection risk.
//
// onTargetDone, if non-nil, is called exactly once per target — index-
// aligned with cfg.Targets — as soon as that target's own outcome is known
// (nil means it received every staged file), independently of every other
// target and of the job as a whole: a target that finishes early (or fails)
// is reported right away rather than waiting for the slowest target to
// finish. Callers such as the web UI's status store use this to refresh
// each target's displayed state as it happens instead of only once the
// whole run completes. If the pipeline never reaches the upload stage at
// all (no file could be staged), every target is still reported exactly
// once, with that same pipeline-level error, since no more specific detail
// exists.
//
// bytesWritten is the total size of the encrypted files uploaded to every
// target (they all upload from the same staged files), for callers that
// want to report the backup's size; it's 0 if no file was staged. files is
// each source's own outcome, in cfg.Sources order.
func runPipeline(ctx context.Context, cfg *config.Config, log *slog.Logger, onTargetDone func(index int, err error)) (bytesWritten int64, files []backup.FileSnapshot, err error) {
	// Guarantees the onTargetDone contract above even on an early return:
	// uploadStagedToTargets reports each target itself once it starts, so
	// this only fires when nothing could be staged, and it always sees the
	// right err — a named return set by any "return ..., err" below is
	// assigned before deferred calls run.
	uploadStarted := false

	defer func() {
		if !uploadStarted {
			for i := range cfg.Targets {
				onTargetDone(i, err)
			}
		}
	}()

	staged := make([]stagedFile, 0, len(cfg.Sources))
	files = make([]backup.FileSnapshot, 0, len(cfg.Sources))

	var sourceErrs []error

	for i := range cfg.Sources {
		src := &cfg.Sources[i]

		f, err := stageSource(ctx, cfg, src, log.With("file", src.Name))
		if err == nil {
			defer removeStagingFileIfUnreferenced(ctx, cfg.StateDB, f.path, log)

			staged = append(staged, f)
			bytesWritten += f.size
		} else {
			sourceErrs = append(sourceErrs, fmt.Errorf("file %q: %w", src.Name, err))
		}

		files = append(files, backup.NewFileSnapshot(src.Name, src.ID, f.size, err))
	}

	sourceErr := errors.Join(sourceErrs...)

	// Only upload what genuinely succeeded: staging a file from a failed
	// source command or gpg would just mean uploading corrupt or truncated
	// garbage to every target. Below this point, uploadStagedToTargets takes
	// over reporting each target's outcome (see onTargetDone above), so mark
	// uploadStarted before calling it — not after, so a panic partway through
	// still leaves the deferred fallback disabled rather than
	// double-reporting.
	if len(staged) == 0 {
		return 0, files, sourceErr
	}

	log.Debug("uploading to targets", "targets", len(cfg.Targets), "files", len(staged))

	uploadStarted = true

	if uploadErr := uploadStagedToTargets(ctx, cfg, staged, onTargetDone, log); uploadErr != nil {
		return bytesWritten, files, errors.Join(sourceErr, fmt.Errorf("uploading to targets: %w", uploadErr))
	}

	return bytesWritten, files, sourceErr
}

// stagedFile is one of a run's files, encrypted and staged (see
// stageSource): name under the backup's key, the staging file's path, and
// its size.
type stagedFile struct {
	name string
	path string
	size int64
}

// stageSource runs src piped into gpg and drains the encrypted output into
// a staging file (see stageBackup), returning it once the command, gpg and
// staging all succeeded. On failure the staging file is already removed and
// the error names the first stage that failed; size still reports how many
// bytes were staged before that, for logging.
func stageSource(ctx context.Context, cfg *config.Config, src *config.JobSource, log *slog.Logger) (stagedFile, error) {
	sourceCmd, gpgCmd, gpgOut, err := startEncryptingPipeline(ctx, cfg, &src.Command, log)
	if err != nil {
		return stagedFile{name: src.Name}, err
	}

	log.Info("staging encrypted backup file", "dir", cfg.StagingDir)

	stagingPath, size, stageErr := stageBackup(cfg, gpgOut)

	log.Info("backup file finished staging", "staging_path", stagingPath, "bytes_written", size)

	// gpgOut must be fully drained (which a successful stageBackup does)
	// before Wait, per exec.Cmd.StdoutPipe's documented contract. A failed
	// stageBackup stops reading early, which would leave gpg blocked on a
	// full pipe and Wait hanging until ctx ends (forever without a
	// -timeout); closing gpgOut makes gpg's next write fail with EPIPE, so
	// it exits and the source behind it does too.
	if stageErr != nil {
		_ = gpgOut.Close()
	}

	gpgErr := gpgCmd.Wait()
	sourceErr := sourceCmd.Wait()

	f := stagedFile{name: src.Name, path: stagingPath, size: size}

	if err := firstPipelineError(sourceLabel(&src.Command), sourceErr, gpgErr, stageErr); err != nil {
		if stageErr == nil {
			_ = os.Remove(stagingPath)
		}

		f.path = ""

		return f, err
	}

	return f, nil
}

// newSourceCommand builds the (not yet started) command that runs cmd
// through the platform shell. Windows has no "sh" on PATH by default (Go's
// standard installer doesn't ship one, unlike every other supported OS), so
// it uses "cmd /C" there instead; everywhere else it uses "sh -c", per
// runPipeline's doc comment.
func newSourceCommand(ctx context.Context, cmd string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", cmd) //nolint:gosec // cfg.Source.Cmd is operator-supplied CLI config, not untrusted input; see runPipeline's doc comment
	}

	return exec.CommandContext(ctx, "sh", "-c", cmd) //nolint:gosec // cfg.Source.Cmd is operator-supplied CLI config, not untrusted input; see runPipeline's doc comment
}

// sourceProcess is the running backup source command: an *exec.Cmd, or an
// asyncSource (a command running inside a container, or a tar archive being
// written; see startContainerPipeline and startTarPipeline).
type sourceProcess interface {
	Wait() error
}

// sourceLabel names src in errors, with its container when it runs in one
// or the path it archives.
func sourceLabel(src *config.Command) string {
	switch {
	case src.Kind == config.CommandKindTar:
		return fmt.Sprintf("tar command %q of %q", src.ID, src.Tar.Root)
	case src.Container != "":
		return fmt.Sprintf("command %q in container %q", src.ID, src.Container)
	default:
		return fmt.Sprintf("command %q", src.ID)
	}
}

// startEncryptingPipeline starts src piped into gpg (the source's
// stdout wired to gpgCmd's stdin) and returns both, already started, plus
// gpgOut: gpg's stdout, ready for the caller to drain (e.g. via stageBackup)
// into the encrypted backup. The caller is responsible for Wait-ing on both
// once gpgOut has been fully read, per exec.Cmd.StdoutPipe's documented
// contract — gpgCmd first, since the source's exit only matters once gpg
// (its downstream reader) is done with it.
//
// When the source command sets its own timeout (src.TimeoutSet), it bounds
// the source alone: gpg, staging and uploads only answer to ctx.
func startEncryptingPipeline(ctx context.Context, cfg *config.Config, src *config.Command, log *slog.Logger) (source sourceProcess, gpgCmd *exec.Cmd, gpgOut io.ReadCloser, err error) {
	srcCtx, cancel := ctx, context.CancelFunc(func() {})
	if src.TimeoutSet {
		srcCtx, cancel = context.WithTimeout(ctx, src.Timeout)
	}

	switch {
	case src.Kind == config.CommandKindTar:
		source, gpgCmd, gpgOut, err = startTarPipeline(ctx, srcCtx, cfg, src, log)
	case src.Container != "":
		source, gpgCmd, gpgOut, err = startContainerPipeline(ctx, srcCtx, cfg, src, log)
	default:
		source, gpgCmd, gpgOut, err = startLocalPipeline(ctx, srcCtx, cfg, src, log)
	}

	if err != nil {
		cancel()
		return nil, nil, nil, err
	}

	timedOut := func() bool { return errors.Is(srcCtx.Err(), context.DeadlineExceeded) }

	return &timedSource{sourceProcess: source, timedOut: timedOut, cancel: cancel, timeout: src.Timeout}, gpgCmd, gpgOut, nil
}

// timedSource releases a source's timeout context once it has exited, and
// says so when that timeout is what ended it.
type timedSource struct {
	sourceProcess
	timedOut func() bool
	cancel   context.CancelFunc
	timeout  time.Duration
}

func (t *timedSource) Wait() error {
	err := t.sourceProcess.Wait()
	if err != nil && t.timedOut() {
		err = fmt.Errorf("timed out after %s: %w", t.timeout, err)
	}

	t.cancel()

	return err
}

// startLocalPipeline is startEncryptingPipeline for a source that runs on
// this machine, through the platform shell (see newSourceCommand), under
// srcCtx.
func startLocalPipeline(ctx, srcCtx context.Context, cfg *config.Config, src *config.Command, log *slog.Logger) (source sourceProcess, gpgCmd *exec.Cmd, gpgOut io.ReadCloser, err error) {
	sourceCmd := newSourceCommand(srcCtx, src.Cmd)
	sourceCmd.Stderr = &logWriter{log: log, msg: "command stderr"}

	sourceOut, err := sourceCmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("wiring command output: %w", err)
	}

	gpgCmd = buildGPGCommand(ctx, cfg)
	gpgCmd.Stdin = sourceOut
	gpgCmd.Stderr = &logWriter{log: log, msg: "gpg stderr"}

	gpgOut, err = gpgCmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("wiring gpg output: %w", err)
	}

	log.Debug("starting source command", "command_id", src.ID, "cmd", src.Cmd)

	if err := sourceCmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("starting %s: %w", sourceLabel(src), err)
	}

	log.Debug("starting gpg", "args", gpgCmd.Args)

	if err := gpgCmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("starting gpg: %w", err)
	}

	// gpg now holds its own copy of the pipe's read end. Closing ours (as
	// startContainerPipeline does) means gpg exiting early fails the
	// source's next write with EPIPE instead of leaving it blocked on a
	// pipe this process would otherwise keep open until sourceCmd.Wait.
	_ = sourceOut.Close()

	return sourceCmd, gpgCmd, gpgOut, nil
}

// asyncSource is a source producing its output from a goroutine of this
// process into the pipe gpg reads (see startGPGOnPipe): a command running
// inside a container, or a tar archive being written. Wait returns its
// outcome once it has finished and all of its output has been handed to gpg.
type asyncSource struct {
	done chan error
}

func (a *asyncSource) Wait() error { return <-a.done }

// startAsyncSource runs produce in a goroutine, writing into pw, and closes
// pw once it returns — gpg's EOF.
func startAsyncSource(pw *os.File, produce func(w io.Writer) error) *asyncSource {
	src := &asyncSource{done: make(chan error, 1)}

	go func() {
		err := produce(pw)
		_ = pw.Close()

		src.done <- err
	}()

	return src
}

// startGPGOnPipe starts gpg reading from a new OS pipe and returns it with
// gpgOut, its stdout, and pw, the pipe's write end for the source to write
// into. This process closes its own copy of the read end once gpg holds
// one, so gpg exiting early fails the source's next write (EPIPE) instead
// of leaving it blocked.
func startGPGOnPipe(ctx context.Context, cfg *config.Config, log *slog.Logger) (gpgCmd *exec.Cmd, gpgOut io.ReadCloser, pw *os.File, err error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating command output pipe: %w", err)
	}

	gpgCmd = buildGPGCommand(ctx, cfg)
	gpgCmd.Stdin = pr
	gpgCmd.Stderr = &logWriter{log: log, msg: "gpg stderr"}

	gpgOut, err = gpgCmd.StdoutPipe()
	if err == nil {
		log.Debug("starting gpg", "args", gpgCmd.Args)
		err = gpgCmd.Start()
	}

	_ = pr.Close()

	if err != nil {
		_ = pw.Close()
		return nil, nil, nil, fmt.Errorf("starting gpg: %w", err)
	}

	return gpgCmd, gpgOut, pw, nil
}

// startContainerPipeline is startEncryptingPipeline for a source with a
// container: src.Cmd runs inside it through the Docker daemon (via
// "sh -c", as it would locally) under srcCtx, and its demultiplexed stdout
// is written into the pipe gpg reads (see startGPGOnPipe).
func startContainerPipeline(ctx, srcCtx context.Context, cfg *config.Config, src *config.Command, log *slog.Logger) (source sourceProcess, gpgCmd *exec.Cmd, gpgOut io.ReadCloser, err error) {
	client, err := dockerexec.New(cfg.DockerHost)
	if err != nil {
		return nil, nil, nil, err
	}

	gpgCmd, gpgOut, pw, err := startGPGOnPipe(ctx, cfg, log)
	if err != nil {
		return nil, nil, nil, err
	}

	log.Debug("starting source command in container", "command_id", src.ID, "cmd", src.Cmd,
		"container", src.Container, "docker_host", cfg.DockerHost)

	source = startAsyncSource(pw, func(w io.Writer) error {
		return client.Run(srcCtx, dockerexec.Exec{
			Container: src.Container,
			User:      src.ContainerUser,
			Cmd:       []string{"sh", "-c", src.Cmd},
			Stdout:    w,
			Stderr:    &logWriter{log: log, msg: "command stderr"},
		})
	})

	return source, gpgCmd, gpgOut, nil
}

// startTarPipeline is startEncryptingPipeline for a type: tar source: a tar
// archive of src.Tar.Root is written in-process (see tarball.Write) under
// srcCtx into the pipe gpg reads (see startGPGOnPipe). Entries tar can't
// hold are logged as they're skipped.
func startTarPipeline(ctx, srcCtx context.Context, cfg *config.Config, src *config.Command, log *slog.Logger) (source sourceProcess, gpgCmd *exec.Cmd, gpgOut io.ReadCloser, err error) {
	gpgCmd, gpgOut, pw, err := startGPGOnPipe(ctx, cfg, log)
	if err != nil {
		return nil, nil, nil, err
	}

	opts := src.Tar
	opts.OnSkip = func(name, reason string) {
		log.Warn("tar command skipped an entry", "command_id", src.ID, "entry", name, "reason", reason)
	}

	log.Debug("starting tar command", "command_id", src.ID, "path", opts.Root,
		"exclude", opts.Exclude, "gzip", opts.Gzip)

	source = startAsyncSource(pw, func(w io.Writer) error {
		return tarball.Write(srcCtx, w, opts)
	})

	return source, gpgCmd, gpgOut, nil
}

// runInContainer runs e through the Docker daemon at dockerHost.
func runInContainer(ctx context.Context, dockerHost string, e dockerexec.Exec) error {
	client, err := dockerexec.New(dockerHost)
	if err != nil {
		return err
	}

	return client.Run(ctx, e)
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

// targetLabel identifies t in log/error messages, naming both the server it
// came from and its bucket, so multi-server setups are easy to debug.
func targetLabel(t *config.Target) string {
	return fmt.Sprintf("server %q, bucket %q", t.ServerName, t.Bucket)
}

// firstPipelineError reports the first failure among one file's stages, in
// source -> gpg -> staging order.
func firstPipelineError(source string, sourceErr, gpgErr, stageErr error) error {
	stages := [...]struct {
		err   error
		label string
	}{
		{sourceErr, source + " failed"},
		{gpgErr, "gpg failed"},
		{stageErr, "staging encrypted backup"},
	}

	for _, stage := range stages {
		if stage.err != nil {
			return fmt.Errorf("%s: %w", stage.label, stage.err)
		}
	}

	return nil
}

// buildGPGCommand constructs the gpg invocation that encrypts a backup to
// cfg.Recipients. --force-mdc makes gpg always add a modification detection
// code, so a tampered ciphertext fails to decrypt instead of yielding
// silently altered plaintext, even for recipient keys that don't advertise
// MDC support.
func buildGPGCommand(ctx context.Context, cfg *config.Config) *exec.Cmd {
	args := []string{"--batch", "--yes"}

	if cfg.GPGHomedir != "" {
		args = append(args, "--homedir", cfg.GPGHomedir)
	}

	if cfg.Armor {
		args = append(args, "--armor")
	}

	args = append(args, "--trust-model", "always", "--force-mdc", "--encrypt")
	for _, r := range cfg.Recipients {
		args = append(args, "--recipient", r)
	}

	return exec.CommandContext(ctx, cfg.GPGBin, args...) //nolint:gosec // cfg.GPGBin/args are operator-supplied CLI config, not untrusted input
}

// uploadStagedToTargets uploads every already-staged file (see stageSource)
// to every one of cfg.Targets, each target independently and concurrently,
// its files one after another: every upload opens its own handle on the
// staging file, so one target being slow or blocked doesn't affect any
// other's progress the way funneling them all through a single shared
// reader would.
//
// Each file gets exactly one attempt per target here; a failed upload is
// reported in its target's outcome for this run (see onTargetDone), but —
// when cfg.StateDB is set — is also recorded as an outstanding upload for
// Runner.RunOutstandingUploadRetries to keep retrying in the background
// every TargetUploadRetryInterval until it eventually succeeds. A target
// still gets its other files when one fails. onTargetDone (see its doc on
// runPipeline) is called for target i the moment all of its uploads'
// outcomes are known, independently of every other target still in
// progress; onTargetDone must be safe for concurrent use, since every
// target's goroutine calls it on its own.
//
// It returns the combined error via errors.Join, for callers that just want
// to know whether anything failed.
func uploadStagedToTargets(ctx context.Context, cfg *config.Config, files []stagedFile, onTargetDone func(index int, err error), log *slog.Logger) error {
	targetErrs := make([]error, len(cfg.Targets))

	var wg sync.WaitGroup

	for i := range cfg.Targets {
		t := &cfg.Targets[i]

		log.Debug("target upload starting", "target", targetLabel(t))

		wg.Go(func() {
			start := time.Now()

			var fileErrs []error

			for _, f := range files {
				if err := uploadStagedFile(ctx, cfg, t, f, log); err != nil {
					fileErrs = append(fileErrs, err)
				}
			}

			if err := errors.Join(fileErrs...); err != nil {
				targetErrs[i] = fmt.Errorf("target (%s): %w", targetLabel(t), err)
			}

			log.Debug("target upload finished", "target", targetLabel(t), "duration", time.Since(start), "failed_files", len(fileErrs))
			onTargetDone(i, targetErrs[i])
		})
	}

	wg.Wait()

	return errors.Join(targetErrs...)
}

// uploadStagedFile makes the one in-run attempt to upload f to t, recording
// a failed attempt as an outstanding upload (see uploadStagedToTargets).
func uploadStagedFile(ctx context.Context, cfg *config.Config, t *config.Target, f stagedFile, log *slog.Logger) error {
	fileCfg := *cfg
	fileCfg.File = f.name

	err := uploadTargetAttempt(ctx, &fileCfg, t, f.path, log)
	if err == nil {
		return nil
	}

	log.Warn("target upload failed", "target", targetLabel(t), "file", f.name, "err", err)

	if cfg.StateDB != nil {
		row := store.OutstandingTargetUpload{
			JobName: cfg.Name, Target: t.ServerName, FileName: f.path, Key: cfg.Key, BackupFile: f.name,
			CreatedAt: cfg.CreatedAt, RetryAt: time.Now().Add(TargetUploadRetryInterval),
		}
		if err1 := cfg.StateDB.AddOutstandingTargetUpload(ctx, row); err1 != nil {
			log.Warn("recording outstanding target upload failed", "target", targetLabel(t), "file", f.name, "err", err1)
		}
	}

	return fmt.Errorf("file %q: %w", f.name, err)
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
// config.ServerKindRemote) uploads key at: t.Endpoint's
// /api/v1/objects/{bucket}/{key} for a flat object, or — when file is set,
// one file of a multi-file backup — /api/v1/backups/{bucket}/{key} (the
// file itself goes in the query, see remoteObjectURL). t.Bucket is the id
// identifying the destination instance's receiver (see receiver.go); key is
// path-escaped since it may contain characters not otherwise safe in a URL
// path segment.
func RemoteObjectURL(t *config.Target, key, file string) string {
	route := "objects"
	if file != "" {
		route = "backups"
	}

	return fmt.Sprintf("%s/api/v1/%s/%s/%s",
		strings.TrimSuffix(t.Endpoint, "/"), route, url.PathEscape(t.Bucket), url.PathEscape(key))
}

// remoteObjectURL is RemoteObjectURL with its query string appended: file
// (when set) and query (e.g. UploadToRemote's creationTime), for
// doAuthenticatedRemoteRequest.
func remoteObjectURL(t *config.Target, key, file string, query url.Values) string {
	u := RemoteObjectURL(t, key, file)

	if file != "" {
		query = maps.Clone(query)
		if query == nil {
			query = url.Values{}
		}

		query.Set("file", file)
	}

	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	return u
}

// remoteAuthHeader signs a fresh JWT with cfg.Identity (this instance's own
// RSA key and UUID — see loadServerIdentity) scoped to t.Bucket (the
// destination instance's receiver id) and bound to req (see
// remoteAuth.Request), and formats it as an Authorization: Bearer header
// value, for UploadToRemote. Errors if cfg.Identity is
// nil, meaning loadServerIdentity failed at startup.
func remoteAuthHeader(cfg *config.Config, t *config.Target, req remoteAuth.Request) (string, error) {
	if cfg.Identity == nil {
		return "", errors.New("no server identity available (see startup log for why loadServerIdentity failed)")
	}

	token, err := cfg.Identity.SignRequest(t.Bucket, req)
	if err != nil {
		return "", fmt.Errorf("signing request: %w", err)
	}

	return "Bearer " + token, nil
}

// UploadToRemote streams r as the body of a PUT request to another
// go-backup-tool instance's receiver API (target t, kind ==
// config.ServerKindRemote), authenticated with a JWT signed by this instance's own
// identity (see remoteAuthHeader). r is streamed directly as the request
// body, never buffered. When cfg.CreatedAt is set, it's sent as the
// creationTime query parameter (RFC3339), so the receiver bases retention on
// when this run's backup content was actually produced rather than on
// upload time — see backup.ParseCreationTime. A zero cfg.CreatedAt sends no
// creationTime at all, matching the receiver's own upload-time fallback.
// r must also implement io.Seeker (the staged *os.File does): it's read
// once up front to sign its SHA-256 into the request token (see
// remoteAuth.Request), then rewound and streamed.
func UploadToRemote(ctx context.Context, cfg *config.Config, t *config.Target, r io.Reader) error {
	var query url.Values
	if !cfg.CreatedAt.IsZero() {
		query = url.Values{"creationTime": {cfg.CreatedAt.UTC().Format(time.RFC3339)}}
	}

	return doAuthenticatedRemoteRequest(ctx, cfg, t, http.MethodPut, r, "application/octet-stream", query)
}

// doAuthenticatedRemoteRequest signs, sends, and status-checks a method
// request (with body, whose Content-Type is set only when contentType is
// non-empty, and an optional query string) to target t's receiver API, for
// UploadToRemote.
func doAuthenticatedRemoteRequest(ctx context.Context, cfg *config.Config, t *config.Target, method string, body io.Reader, contentType string, query url.Values) error {
	bound := remoteAuth.Request{Method: method, Key: cfg.Key, File: cfg.File, CreationTime: query.Get("creationTime")}

	if body != nil {
		digest, err := bodySHA256(body)
		if err != nil {
			return err
		}

		bound.BodySHA256 = digest
	}

	auth, err := remoteAuthHeader(cfg, t, bound)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, method, remoteObjectURL(t, cfg.Key, cfg.File, query), body)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Authorization", auth)
	setFileContentLength(req, body)

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	client, err := remoteHTTPClient(t)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return remoteResponseError(resp)
	}

	return nil
}

// bodySHA256 returns the hex SHA-256 of body's remaining content, then
// rewinds body to where it was, so it can still be streamed as the request
// body. body must implement io.Seeker; buffering an arbitrarily large
// backup in memory just to hash it isn't an option.
func bodySHA256(body io.Reader) (string, error) {
	seeker, ok := body.(io.ReadSeeker)
	if !ok {
		return "", errors.New("remote upload body must be seekable to sign its digest")
	}

	start, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", fmt.Errorf("finding body offset: %w", err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, seeker); err != nil {
		return "", fmt.Errorf("hashing body: %w", err)
	}

	if _, err := seeker.Seek(start, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewinding body: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// setFileContentLength sets req's ContentLength when body is a regular
// *os.File (the staged backup UploadToRemote streams), so the upload is
// sent with a Content-Length instead of chunked encoding, letting the
// receiving instance see the object's size up front. Still streamed from
// the file, never buffered. Any other body (or a Stat/Seek failure) keeps
// net/http's default of an unknown length.
func setFileContentLength(req *http.Request, body io.Reader) {
	f, ok := body.(*os.File)
	if !ok {
		return
	}

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}

	off, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}

	req.ContentLength = info.Size() - off
	if req.ContentLength == 0 {
		req.Body = http.NoBody
	}
}

// remoteResponseError builds an error from a non-2xx response, including a
// bounded read of the response body so a misbehaving or malicious server
// can't exhaust memory via an unbounded response.
func remoteResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	return fmt.Errorf("remote instance returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
}
