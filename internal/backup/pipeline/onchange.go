package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// onChangeWorkers is how many files an on-change run hashes, encrypts and
// uploads at once.
const onChangeWorkers = 4

// maxOnChangeFileSnapshots caps how many files an on-change run lists in
// its status (see status.JobSnapshot.Files); failures are listed first.
const maxOnChangeFileSnapshots = 100

// onChangeCommandLabel is what an on-change run's file snapshots show as
// their command, since no command produced them.
const onChangeCommandLabel = "on-change"

// errNoStateDB fails an on-change run without the state db, which is where
// it remembers what each target already has.
var errNoStateDB = errors.New("on-change jobs need the state db, which is unavailable this run")

// onChangeResult is what one on-change run did.
type onChangeResult struct {
	// bytes is the encrypted size of every file uploaded.
	bytes int64

	// uploaded counts the files uploaded to a target, stored counts those
	// it kept as a new version (the rest it already had).
	uploaded int
	stored   int

	// files are the run's uploaded and failed files, for its status.
	files []status.FileSnapshot

	// targetErrs is each of cfg.Targets' outcome, index-aligned.
	targetErrs []error
}

// err is r's overall error: every target's, joined.
func (r onChangeResult) err() error {
	return errors.Join(r.targetErrs...)
}

// runOnChange runs on-change job cfg once: every regular file under
// cfg.Source.Path (or, when changed is non-nil, just those paths —
// slash-separated, relative to it, a directory standing for everything
// below it) whose content a target doesn't have yet is encrypted on its
// own and uploaded to it (see remoteclient.UploadFile). What each target
// has is remembered in the state db (see store.OnChangeFile): a file whose
// size and modification time are unchanged isn't even read, and one whose
// content hashes the same isn't uploaded. A failed upload isn't recorded,
// so the next run retries it.
func runOnChange(ctx context.Context, env runEnv, cfg *config.Config, changed []string, log *slog.Logger) onChangeResult {
	res := onChangeResult{targetErrs: make([]error, len(cfg.Targets))}

	fail := func(err error) onChangeResult {
		for i := range res.targetErrs {
			res.targetErrs[i] = err
		}

		return res
	}

	if env.db == nil {
		return fail(errNoStateDB)
	}

	files, err := collectSourceFiles(cfg.Source, changed, log)
	if err != nil {
		return fail(err)
	}

	known, err := loadOnChangeState(ctx, env.db, cfg)
	if err != nil {
		return fail(err)
	}

	run := &onChangeRun{env: env, cfg: cfg, known: known, log: log, failures: make([]targetFailures, len(cfg.Targets))}
	run.uploadAll(ctx, files)

	res.bytes, res.uploaded, res.stored, res.files = run.bytes, run.uploaded, run.stored, run.snapshots()

	for i, f := range run.failures {
		res.targetErrs[i] = f.err()
	}

	log.Info("on-change scan finished", "files", len(files), "uploaded", run.uploaded, "stored", run.stored, "bytes", run.bytes)

	return res
}

// sourceFile is one regular file an on-change run considers.
type sourceFile struct {
	rel     string // slash-separated, relative to the source path
	abs     string
	size    int64
	modTime time.Time
}

// collectSourceFiles lists the regular files under src.Path, or just the
// ones changed names (see runOnChange), skipping excluded and reserved
// paths (see excludedPath). A changed path that no longer exists is
// skipped: deleting a file keeps its versions on the receiver.
func collectSourceFiles(src config.OnChangeSource, changed []string, log *slog.Logger) ([]sourceFile, error) {
	if changed == nil {
		info, err := os.Stat(src.Path)
		if err != nil {
			return nil, fmt.Errorf("reading source: %w", err)
		}

		if !info.IsDir() {
			return nil, fmt.Errorf("source %q is not a directory", src.Path)
		}

		return walkSourceFiles(src, src.Path, log)
	}

	seen := make(map[string]bool, len(changed))

	var out []sourceFile

	for _, rel := range changed {
		if excludedPath(src, rel) {
			continue
		}

		abs := filepath.Join(src.Path, filepath.FromSlash(rel))

		info, err := os.Lstat(abs)
		if err != nil {
			continue
		}

		var found []sourceFile

		switch {
		case info.IsDir():
			found, err = walkSourceFiles(src, abs, log)
			if err != nil {
				return nil, err
			}
		case info.Mode().IsRegular():
			found = []sourceFile{{rel: rel, abs: abs, size: info.Size(), modTime: info.ModTime()}}
		}

		for _, f := range found {
			if !seen[f.rel] {
				seen[f.rel] = true

				out = append(out, f)
			}
		}
	}

	return out, nil
}

// walkSourceFiles lists the regular files under dir, itself src.Path or a
// directory below it. Symlinks aren't followed. A file or directory that
// can't be read is logged and skipped, so one unreadable file doesn't stop
// every other one from being backed up.
func walkSourceFiles(src config.OnChangeSource, dir string, log *slog.Logger) ([]sourceFile, error) {
	var out []sourceFile

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p == dir {
				return walkErr
			}

			log.Warn("on-change: skipping unreadable path", "path", p, "error", walkErr)

			return nil
		}

		if p == src.Path {
			return nil
		}

		rel, err := filepath.Rel(src.Path, p)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)

		if excludedPath(src, rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}

		if err := objectstore.ValidateFilePath(rel); err != nil {
			log.Warn("on-change: skipping file whose path a receiver can't store", "path", rel, "error", err)
			return nil
		}

		info, err := d.Info()
		if err != nil {
			log.Warn("on-change: skipping unreadable file", "path", rel, "error", err)
			return nil
		}

		out = append(out, sourceFile{rel: rel, abs: p, size: info.Size(), modTime: info.ModTime()})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading source: %w", err)
	}

	return out, nil
}

// excludedPath reports whether rel, or any directory above it, matches one
// of src's exclude patterns or has the reserved config.VersionDirSuffix.
func excludedPath(src config.OnChangeSource, rel string) bool {
	for p := rel; p != "." && p != "/" && p != ""; p = path.Dir(p) {
		if src.Excluded(p) || strings.HasSuffix(path.Base(p), config.VersionDirSuffix) {
			return true
		}
	}

	return false
}

// onChangeTargetID names target t in the state db's onchange_files.
func onChangeTargetID(t *config.Target) string {
	return t.ServerName + "/" + t.Bucket
}

// loadOnChangeState reads, for each of cfg.Targets, what it's recorded as
// having, keyed by path.
func loadOnChangeState(ctx context.Context, db store.Store, cfg *config.Config) ([]map[string]store.OnChangeFile, error) {
	known := make([]map[string]store.OnChangeFile, len(cfg.Targets))

	for i := range cfg.Targets {
		rows, err := db.ListOnChangeFiles(ctx, cfg.Name, onChangeTargetID(&cfg.Targets[i]))
		if err != nil {
			return nil, err
		}

		known[i] = make(map[string]store.OnChangeFile, len(rows))
		for _, row := range rows {
			known[i][row.Path] = row
		}
	}

	return known, nil
}

// onChangeRun is one runOnChange in progress, shared by its workers.
type onChangeRun struct {
	env   runEnv
	cfg   *config.Config
	known []map[string]store.OnChangeFile // read-only once the run starts
	log   *slog.Logger

	mu       sync.Mutex
	bytes    int64
	uploaded int
	stored   int
	ok       []status.FileSnapshot
	failed   []status.FileSnapshot
	failures []targetFailures
}

// targetFailures counts one target's failed files in a run.
type targetFailures struct {
	count int
	first error
}

func (f targetFailures) err() error {
	switch f.count {
	case 0:
		return nil
	case 1:
		return f.first
	default:
		return fmt.Errorf("%d files failed, first: %w", f.count, f.first)
	}
}

// uploadAll processes files on onChangeWorkers workers, stopping early
// once ctx is done.
func (run *onChangeRun) uploadAll(ctx context.Context, files []sourceFile) {
	work := make(chan sourceFile)

	var wg sync.WaitGroup

	for range min(onChangeWorkers, len(files)) {
		wg.Go(func() {
			for f := range work {
				run.process(ctx, f)
			}
		})
	}

feed:
	for _, f := range files {
		select {
		case <-ctx.Done():
			break feed
		case work <- f:
		}
	}

	close(work)
	wg.Wait()

	if err := ctx.Err(); err != nil {
		run.mu.Lock()
		for i := range run.failures {
			run.failures[i].add(fmt.Errorf("run stopped: %w", err))
		}
		run.mu.Unlock()
	}
}

func (f *targetFailures) add(err error) {
	if f.count == 0 {
		f.first = err
	}

	f.count++
}

// process brings every target up to date with one file.
func (run *onChangeRun) process(ctx context.Context, f sourceFile) {
	needed := run.targetsNeeding(f, "")
	if len(needed) == 0 {
		return
	}

	plain, err := hashFile(f.abs)
	if err != nil {
		run.fileFailed(f, needed, err)
		return
	}

	needed = run.targetsNeeding(f, plain)

	// Touched but unchanged for these: remember the new metadata, so the
	// next scan doesn't read the file again.
	run.recordUnchanged(ctx, f, plain)

	if len(needed) == 0 {
		return
	}

	staged, size, plain, err := encryptFile(ctx, run.cfg, f.abs, run.log)
	if err != nil {
		run.fileFailed(f, needed, err)
		return
	}
	defer func() { _ = os.Remove(staged) }()

	for _, i := range needed {
		t := &run.cfg.Targets[i]

		res, err := uploadStagedFileVersion(ctx, run.env, t, f.rel, plain, staged)
		if err != nil {
			run.fileFailed(f, []int{i}, fmt.Errorf("%s: %w", targetLabel(t), err))
			continue
		}

		run.fileUploaded(ctx, f, i, plain, size, res)
	}
}

// targetsNeeding returns the indices of the targets that don't have f's
// content: with plain "", judged by its size and modification time alone;
// otherwise by its plaintext hash plain.
func (run *onChangeRun) targetsNeeding(f sourceFile, plain string) []int {
	var out []int

	for i := range run.cfg.Targets {
		k, ok := run.known[i][f.rel]

		switch {
		case !ok:
			out = append(out, i)
		case plain == "" && (k.Size != f.size || !k.ModTime.Equal(f.modTime)):
			out = append(out, i)
		case plain != "" && k.SHA256 != plain:
			out = append(out, i)
		}
	}

	return out
}

// recordUnchanged saves f's new size and modification time for every
// target that already has its content plain but recorded other metadata.
func (run *onChangeRun) recordUnchanged(ctx context.Context, f sourceFile, plain string) {
	for i := range run.cfg.Targets {
		k, ok := run.known[i][f.rel]
		if ok && k.SHA256 == plain && (k.Size != f.size || !k.ModTime.Equal(f.modTime)) {
			run.saveState(ctx, f, i, plain, k.UploadedAt)
		}
	}
}

// saveState records that target i has f's content plain.
func (run *onChangeRun) saveState(ctx context.Context, f sourceFile, i int, plain string, uploadedAt time.Time) {
	err := run.env.db.SaveOnChangeFile(ctx, store.OnChangeFile{
		JobName: run.cfg.Name, Target: onChangeTargetID(&run.cfg.Targets[i]), Path: f.rel,
		Size: f.size, ModTime: f.modTime, SHA256: plain, UploadedAt: uploadedAt,
	})
	if err != nil {
		// Only costs a re-upload next time, which the receiver dedups.
		run.log.Warn("on-change: recording uploaded file", "path", f.rel, "error", err)
	}
}

// fileUploaded records target i's successful upload of f.
func (run *onChangeRun) fileUploaded(ctx context.Context, f sourceFile, i int, plain string, size int64, res remoteclient.FileUploadResult) {
	run.saveState(ctx, f, i, plain, time.Now())

	run.log.Debug("on-change: file uploaded", "path", f.rel, "target", targetLabel(&run.cfg.Targets[i]), "stored", res.Stored, "version", res.Version)

	run.mu.Lock()
	defer run.mu.Unlock()

	run.uploaded++
	run.bytes += size

	if res.Stored {
		run.stored++
	}

	run.ok = append(run.ok, status.NewFileSnapshot(f.rel, onChangeCommandLabel, size, nil))
}

// fileFailed records that f couldn't be brought up to date on targets.
func (run *onChangeRun) fileFailed(f sourceFile, targets []int, err error) {
	run.log.Warn("on-change: file failed", "path", f.rel, "error", err)

	run.mu.Lock()
	defer run.mu.Unlock()

	for _, i := range targets {
		run.failures[i].add(fmt.Errorf("%s: %w", f.rel, err))
	}

	run.failed = append(run.failed, status.NewFileSnapshot(f.rel, onChangeCommandLabel, 0, err))
}

// snapshots returns the run's file snapshots: failures first, capped at
// maxOnChangeFileSnapshots.
func (run *onChangeRun) snapshots() []status.FileSnapshot {
	out := append(run.failed, run.ok...) //nolint:gocritic // run is done; its slices aren't used again

	return out[:min(len(out), maxOnChangeFileSnapshots)]
}

// hashFile returns the hex SHA-256 of the file at p.
func hashFile(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // p is a file under the operator-configured source path
	if err != nil {
		return "", fmt.Errorf("opening file: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// encryptFile encrypts the file at p with gpg (see buildGPGCommand) into a
// staging file (see stageBackup), returning its path, its encrypted size,
// and the plaintext's hex SHA-256 — taken from the very bytes gpg read, so
// it matches the ciphertext even if the file changed since it was last
// hashed. The caller removes the staging file.
func encryptFile(ctx context.Context, cfg *config.Config, p string, log *slog.Logger) (staged string, size int64, plain string, err error) {
	f, err := os.Open(p) //nolint:gosec // p is a file under the operator-configured source path
	if err != nil {
		return "", 0, "", fmt.Errorf("opening file: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()

	gpgCmd := buildGPGCommand(ctx, cfg)
	gpgCmd.Stdin = io.TeeReader(f, h)
	gpgCmd.Stderr = &logWriter{log: log, msg: "gpg stderr"}

	gpgOut, err := gpgCmd.StdoutPipe()
	if err != nil {
		return "", 0, "", fmt.Errorf("wiring gpg output: %w", err)
	}

	if err := gpgCmd.Start(); err != nil {
		return "", 0, "", fmt.Errorf("starting gpg: %w", err)
	}

	staged, size, stageErr := stageBackup(cfg, gpgOut)
	if stageErr != nil {
		_ = gpgCmd.Process.Kill()
	}

	// Wait also waits for the goroutine copying the file into gpg, so h
	// holds every byte gpg read once it returns.
	gpgErr := gpgCmd.Wait()

	if err := firstPipelineError("reading file", nil, gpgErr, stageErr); err != nil {
		if staged != "" {
			_ = os.Remove(staged)
		}

		return "", 0, "", err
	}

	return staged, size, hex.EncodeToString(h.Sum(nil)), nil
}

// uploadStagedFileVersion uploads the staged file at staged to remote
// target t as rel.
func uploadStagedFileVersion(ctx context.Context, env runEnv, t *config.Target, rel, plain, staged string) (remoteclient.FileUploadResult, error) {
	f, err := os.Open(staged) //nolint:gosec // staged is our own staging file
	if err != nil {
		return remoteclient.FileUploadResult{}, fmt.Errorf("opening staged file: %w", err)
	}
	defer func() { _ = f.Close() }()

	return remoteclient.UploadFile(ctx, env.identity, t, rel, plain, f)
}
