package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

func TestEnvironWithout(t *testing.T) {
	t.Setenv("GPG_PASSPHRASE", "should-be-dropped")
	t.Setenv("GO_BACKUP_TOOL_TEST_KEEP", "should-be-kept")

	filtered := environWithout("GPG_PASSPHRASE")

	for _, kv := range filtered {
		if strings.HasPrefix(kv, "GPG_PASSPHRASE=") {
			t.Fatalf("environWithout() kept GPG_PASSPHRASE: %q", kv)
		}
	}

	if !slices.ContainsFunc(filtered, func(kv string) bool {
		return strings.HasPrefix(kv, "GO_BACKUP_TOOL_TEST_KEEP=")
	}) {
		t.Error("environWithout() dropped an unrelated variable it should have kept")
	}
}

func TestWriteLocalObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config.Config{Key: "backup.gpg"}
	tgt := &config.Target{Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir}

	const content = "ciphertext bytes"

	if err := backup.WriteLocalObject(cfg, tgt, strings.NewReader(content)); err != nil {
		t.Fatalf("backup.WriteLocalObject() unexpected error: %v", err)
	}

	wantPath := filepath.Join(dir, "sub", "backup.gpg")

	got, err := os.ReadFile(wantPath) //nolint:gosec // wantPath is built from t.TempDir() plus fixed test literals
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}

	if string(got) != content {
		t.Errorf("written content = %q, want %q", got, content)
	}

	// No leftover temp file should remain in the destination directory.
	entries, err := os.ReadDir(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatalf("reading destination directory: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("destination directory entries = %v, want exactly the written file", entries)
	}
}

func TestWriteLocalObjectCreatesMissingDirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config.Config{Key: "backup.gpg"}
	tgt := &config.Target{Kind: config.ServerKindLocal, Bucket: "does/not/exist", LocalPath: dir}

	if err := backup.WriteLocalObject(cfg, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("backup.WriteLocalObject() unexpected error: %v", err)
	}

	if _, err := os.Stat(backup.LocalObjectPath(cfg, tgt)); err != nil {
		t.Errorf("written file not found: %v", err)
	}
}

func TestDeleteLocalObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config.Config{Key: "backup.gpg"}
	tgt := &config.Target{Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir}

	if err := backup.WriteLocalObject(cfg, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("backup.WriteLocalObject() unexpected error: %v", err)
	}

	if err := backup.DeleteLocalObject(cfg, tgt); err != nil {
		t.Fatalf("backup.DeleteLocalObject() unexpected error: %v", err)
	}

	if _, err := os.Stat(backup.LocalObjectPath(cfg, tgt)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists after backup.DeleteLocalObject(): err = %v", err)
	}
}

func TestDeleteLocalObjectMissingFileIsNotError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config.Config{Key: "backup.gpg"}
	tgt := &config.Target{Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir}

	if err := backup.DeleteLocalObject(cfg, tgt); err != nil {
		t.Errorf("backup.DeleteLocalObject() on missing file = %v, want nil", err)
	}
}

// stageTestContent stages content into a private temp file via the real
// stageBackup, for tests exercising uploadStagedToTargets/uploadTargetAttempt
// against a real file on disk the way runPipeline does, rather than an
// in-memory reader. The file is removed automatically when the test ends.
func stageTestContent(t *testing.T, content string) string {
	t.Helper()

	path, n, err := stageBackup(&config.Config{}, strings.NewReader(content))
	if err != nil {
		t.Fatalf("stageBackup() unexpected error: %v", err)
	}

	if n != int64(len(content)) {
		t.Fatalf("stageBackup() bytesWritten = %d, want %d", n, len(content))
	}

	t.Cleanup(func() { _ = os.Remove(path) })

	return path
}

// collectTargetDone returns an onTargetDone callback safe for the
// concurrent use uploadStagedToTargets requires (every target's own
// goroutine calls it), plus a getter for what it collected so far, for
// tests that need to inspect each target's reported outcome.
func collectTargetDone(n int) (onDone func(index int, err error), results func() []error) {
	var mu sync.Mutex

	errs := make([]error, n)

	onDone = func(index int, err error) {
		mu.Lock()
		defer mu.Unlock()

		errs[index] = err
	}

	results = func() []error {
		mu.Lock()
		defer mu.Unlock()

		return append([]error(nil), errs...)
	}

	return onDone, results
}

func TestUploadStagedToTargetsLocal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config.Config{
		Key: "backup.gpg",
		Targets: []config.Target{
			{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	const content = "hello from the pipeline"

	onDone, results := collectTargetDone(len(cfg.Targets))

	err := uploadStagedToTargets(t.Context(), cfg, stageTestContent(t, content), onDone, discardLogger)
	if err != nil {
		t.Fatalf("uploadStagedToTargets() unexpected error: %v", err)
	}

	if targetErrs := results(); len(targetErrs) != 1 || targetErrs[0] != nil {
		t.Fatalf("uploadStagedToTargets() reported target results = %v, want [nil]", targetErrs)
	}

	got, err := os.ReadFile(backup.LocalObjectPath(cfg, &cfg.Targets[0]))
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}

	if string(got) != content {
		t.Errorf("written content = %q, want %q", got, content)
	}
}

// TestUploadStagedToTargetsContinuesAfterOneTargetFails verifies that a
// failure writing to one target (here, a local target whose destination
// directory path is occupied by a file, so os.MkdirAll for it errors)
// doesn't stop uploadStagedToTargets from finishing the other targets: they
// should still receive the full stream and succeed independently.
func TestUploadStagedToTargetsContinuesAfterOneTargetFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Make the "bad" target's parent directory path a regular file, so
	// writeLocalObject's os.MkdirAll for it fails.
	badParent := filepath.Join(dir, "blocked")
	if err := os.WriteFile(badParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("setting up blocked path: %v", err)
	}

	cfg := &config.Config{
		Key: "backup.gpg",
		Targets: []config.Target{
			{ServerName: "bad", Kind: config.ServerKindLocal, Bucket: "blocked/sub", LocalPath: dir},
			{ServerName: "good", Kind: config.ServerKindLocal, Bucket: "sub", LocalPath: dir},
		},
	}

	const content = "hello from the pipeline"

	onDone, results := collectTargetDone(len(cfg.Targets))

	err := uploadStagedToTargets(t.Context(), cfg, stageTestContent(t, content), onDone, discardLogger)
	if err == nil {
		t.Fatal("uploadStagedToTargets() error = nil, want non-nil because the bad target failed")
	}

	targetErrs := results()
	if len(targetErrs) != 2 || targetErrs[0] == nil {
		t.Fatalf("uploadStagedToTargets() reported target results = %v, want [<err>, nil]", targetErrs)
	}

	if targetErrs[1] != nil {
		t.Errorf("uploadStagedToTargets() good target err = %v, want nil (other target's failure shouldn't affect it)", targetErrs[1])
	}

	got, err := os.ReadFile(backup.LocalObjectPath(cfg, &cfg.Targets[1]))
	if err != nil {
		t.Fatalf("reading good target's written file: %v", err)
	}

	if string(got) != content {
		t.Errorf("good target written content = %q, want %q", got, content)
	}
}

// TestUploadStagedToTargetsIsolatesSlowTarget guards against a bug fixed
// earlier: every target now opens its own independent handle on the staged
// file and uploads in its own goroutine, so a target that's slow or stuck
// (here, a remote target whose dial never completes) cannot block or
// corrupt any other target's upload, unlike an earlier design that streamed
// the backup directly to every target through one shared reader.
func TestUploadStagedToTargetsIsolatesSlowTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	cfg := &config.Config{
		Key:      "backup.gpg",
		Identity: testServerIdentity(t),
		Targets: []config.Target{
			{ServerName: "sibling-instance", Kind: config.ServerKindRemote, Endpoint: "http://10.255.255.1:8050", Bucket: "from-primary"},
			{ServerName: "nas", Kind: config.ServerKindLocal, Bucket: "my-backup-bucket-local", LocalPath: dir},
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	const content = "hello from the pipeline"

	onDone, results := collectTargetDone(len(cfg.Targets))

	err := uploadStagedToTargets(ctx, cfg, stageTestContent(t, content), onDone, discardLogger)
	if err == nil {
		t.Fatal("uploadStagedToTargets() error = nil, want non-nil because the remote target failed")
	}

	targetErrs := results()
	if targetErrs[0] == nil {
		t.Error("uploadStagedToTargets() remote target err = nil, want non-nil")
	}

	if targetErrs[1] != nil {
		t.Errorf("uploadStagedToTargets() local target err = %v, want nil (the remote target's failure shouldn't affect it)", targetErrs[1])
	}

	got, readErr := os.ReadFile(backup.LocalObjectPath(cfg, &cfg.Targets[1]))
	if readErr != nil {
		t.Fatalf("reading local target's written file: %v", readErr)
	}

	if string(got) != content {
		t.Error("local target's written content does not match source")
	}
}

// TestUploadStagedToTargetsSingleAttemptOnFailure verifies that a failing
// target is attempted exactly once, with no retry, and its failure is
// reported immediately.
func TestUploadStagedToTargetsSingleAttemptOnFailure(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "always fails", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Name:     "job-a",
		Key:      "backup.gpg",
		Identity: testServerIdentity(t),
		Targets:  []config.Target{{ServerName: "always-down", Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}},
	}

	onDone, results := collectTargetDone(len(cfg.Targets))

	if err := uploadStagedToTargets(t.Context(), cfg, stageTestContent(t, "hello"), onDone, discardLogger); err == nil {
		t.Fatal("uploadStagedToTargets() error = nil, want non-nil")
	}

	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (no retry)", got)
	}

	if targetErrs := results(); len(targetErrs) != 1 || targetErrs[0] == nil {
		t.Fatalf("uploadStagedToTargets() reported target results = %v, want [<err>]", targetErrs)
	}
}

func TestBuildGPGCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		cfg            *config.Config
		wantArgs       []string // exact args, in order
		wantPassphrase bool
	}{
		{
			name: "recipient mode, single recipient",
			cfg: &config.Config{
				GPGBin:     "gpg",
				Recipients: []string{"me@example.com"},
			},
			wantArgs: []string{
				"--batch", "--yes", "--trust-model", "always", "--encrypt",
				"--recipient", "me@example.com",
			},
		},
		{
			name: "recipient mode, multiple recipients plus armor and homedir",
			cfg: &config.Config{
				GPGBin:     "gpg",
				Recipients: []string{"a@example.com", "b@example.com"},
				Armor:      true,
				GPGHomedir: "/tmp/gnupg-test",
			},
			wantArgs: []string{
				"--batch", "--yes", "--homedir", "/tmp/gnupg-test", "--armor",
				"--trust-model", "always", "--encrypt",
				"--recipient", "a@example.com", "--recipient", "b@example.com",
			},
		},
		{
			name: "symmetric mode",
			cfg: &config.Config{
				GPGBin:    "gpg",
				Symmetric: true,
			},
			wantArgs: []string{
				"--batch", "--yes",
				"--pinentry-mode", "loopback", "--passphrase-fd", "3", "--symmetric",
			},
			wantPassphrase: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd, passphraseWriter, passphraseReadEnd, err := buildGPGCommand(context.Background(), tt.cfg)
			if err != nil {
				t.Fatalf("buildGPGCommand() unexpected error: %v", err)
			}

			defer func() {
				if passphraseWriter != nil {
					_ = passphraseWriter.Close()
				}

				if passphraseReadEnd != nil {
					_ = passphraseReadEnd.Close()
				}
			}()

			gotArgs := cmd.Args[1:] // cmd.Args[0] is the binary name
			if !slices.Equal(gotArgs, tt.wantArgs) {
				t.Errorf("buildGPGCommand() args = %v, want %v", gotArgs, tt.wantArgs)
			}

			checkPassphraseWiring(t, tt.wantPassphrase, cmd, passphraseWriter, passphraseReadEnd)
		})
	}
}

// checkPassphraseWiring asserts buildGPGCommand's passphrase-related return
// values match what's expected for symmetric (wantPassphrase) vs recipient
// mode.
func checkPassphraseWiring(t *testing.T, wantPassphrase bool, cmd *exec.Cmd, passphraseWriter io.WriteCloser, passphraseReadEnd *os.File) {
	t.Helper()

	wantExtraFiles := 0
	if wantPassphrase {
		wantExtraFiles = 1
	}

	if (passphraseWriter != nil) != wantPassphrase {
		t.Errorf("buildGPGCommand() passphraseWriter non-nil = %v, want %v", passphraseWriter != nil, wantPassphrase)
	}

	if (passphraseReadEnd != nil) != wantPassphrase {
		t.Errorf("buildGPGCommand() passphraseReadEnd non-nil = %v, want %v", passphraseReadEnd != nil, wantPassphrase)
	}

	if len(cmd.ExtraFiles) != wantExtraFiles {
		t.Errorf("buildGPGCommand() ExtraFiles = %d files, want %d", len(cmd.ExtraFiles), wantExtraFiles)
	}
}

// TestSymmetricEncryptDecryptRoundTrip exercises buildGPGCommand end-to-end
// against the real gpg binary: encrypt via the command this package
// constructs, then decrypt independently and check the plaintext survives.
// It mirrors the wiring in runPipeline (start, write+close the passphrase
// pipe, drain stdout, wait) without touching the network leg of the
// pipeline.
func TestSymmetricEncryptDecryptRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	t.Parallel()

	const (
		plaintext  = "hello from go-backup-tool\n"
		passphrase = "unit-test-passphrase"
	)

	cfg := &config.Config{GPGBin: "gpg", Symmetric: true}

	cmd, passphraseWriter, passphraseReadEnd, err := buildGPGCommand(t.Context(), cfg)
	if err != nil {
		t.Fatalf("buildGPGCommand() error: %v", err)
	}

	cmd.Stdin = strings.NewReader(plaintext)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error: %v", err)
	}

	var stderr strings.Builder

	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("gpg Start() error: %v", err)
	}

	_ = passphraseReadEnd.Close()

	if _, err := io.WriteString(passphraseWriter, passphrase); err != nil {
		t.Fatalf("writing passphrase: %v", err)
	}

	if err := passphraseWriter.Close(); err != nil {
		t.Fatalf("closing passphrase pipe: %v", err)
	}

	ciphertext, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("reading gpg stdout: %v", err)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("gpg Wait() error: %v (stderr: %s)", err, stderr.String())
	}

	if len(ciphertext) == 0 {
		t.Fatal("gpg produced no ciphertext")
	}

	decrypt := exec.CommandContext(t.Context(), "gpg", "--batch", "--yes", "--pinentry-mode", "loopback",
		"--passphrase", passphrase, "--decrypt")
	decrypt.Stdin = strings.NewReader(string(ciphertext))

	got, err := decrypt.Output()
	if err != nil {
		t.Fatalf("decrypting round-trip output: %v", err)
	}

	if string(got) != plaintext {
		t.Errorf("round trip = %q, want %q", got, plaintext)
	}
}

// loggedLines runs writes (each a separate Write call) through a logWriter
// and returns the "output" attribute of every line it logged, in order.
func loggedLines(t *testing.T, writes []string) []string {
	t.Helper()

	var buf strings.Builder

	log := slog.New(slog.NewTextHandler(&buf, nil))
	w := &logWriter{log: log, msg: "command stderr"}

	for _, chunk := range writes {
		n, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}

		if n != len(chunk) {
			t.Fatalf("Write(%q) = %d, want %d", chunk, n, len(chunk))
		}
	}

	var lines []string

	for entry := range strings.SplitSeq(strings.TrimRight(buf.String(), "\n"), "\n") {
		if entry == "" {
			continue
		}

		_, after, found := strings.Cut(entry, "output=")
		if !found {
			t.Fatalf("log entry %q has no output attribute", entry)
		}

		lines = append(lines, strings.Trim(after, `"`))
	}

	return lines
}

func TestLogWriterLineBuffering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		writes []string
		want   []string
	}{
		{
			name:   "single write, single line",
			writes: []string{"hello world\n"},
			want:   []string{"hello world"},
		},
		{
			name:   "line split across writes",
			writes: []string{"hel", "lo wor", "ld\n"},
			want:   []string{"hello world"},
		},
		{
			name:   "multiple lines in one write",
			writes: []string{"line one\nline two\n"},
			want:   []string{"line one", "line two"},
		},
		{
			name:   "no trailing newline still buffers and later completes",
			writes: []string{"partial", " line\n"},
			want:   []string{"partial line"},
		},
		{
			name:   "blank lines are dropped",
			writes: []string{"first\n\nsecond\n"},
			want:   []string{"first", "second"},
		},
		{
			name:   "CRLF line endings are trimmed",
			writes: []string{"windows line\r\n"},
			want:   []string{"windows line"},
		},
		{
			name:   "incomplete line at end is never logged",
			writes: []string{"complete\nincomplete"},
			want:   []string{"complete"},
		},
		{
			name:   "many single-byte writes reassemble a line",
			writes: []string{"a", "b", "c\n"},
			want:   []string{"abc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := loggedLines(t, tt.writes)
			if !slices.Equal(got, tt.want) {
				t.Errorf("logged lines = %q, want %q", got, tt.want)
			}
		})
	}
}
