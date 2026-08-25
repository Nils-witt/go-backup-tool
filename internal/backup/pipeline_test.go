package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindLocal, bucket: "sub", localPath: dir}

	const content = "ciphertext bytes"

	if err := writeLocalObject(cfg, tgt, strings.NewReader(content)); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
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
	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindLocal, bucket: "does/not/exist", localPath: dir}

	if err := writeLocalObject(cfg, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if _, err := os.Stat(localObjectPath(cfg, tgt)); err != nil {
		t.Errorf("written file not found: %v", err)
	}
}

func TestDeleteLocalObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindLocal, bucket: "sub", localPath: dir}

	if err := writeLocalObject(cfg, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("writeLocalObject() unexpected error: %v", err)
	}

	if err := deleteLocalObject(cfg, tgt); err != nil {
		t.Fatalf("deleteLocalObject() unexpected error: %v", err)
	}

	if _, err := os.Stat(localObjectPath(cfg, tgt)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists after deleteLocalObject(): err = %v", err)
	}
}

func TestDeleteLocalObjectMissingFileIsNotError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindLocal, bucket: "sub", localPath: dir}

	if err := deleteLocalObject(cfg, tgt); err != nil {
		t.Errorf("deleteLocalObject() on missing file = %v, want nil", err)
	}
}

func TestUploadToTargetsLocal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config{
		key: "backup.gpg",
		targets: []target{
			{serverName: "nas", kind: serverKindLocal, bucket: "sub", localPath: dir},
		},
	}

	const content = "hello from the pipeline"

	targetErrs, err := uploadToTargets(t.Context(), cfg, strings.NewReader(content))
	if err != nil {
		t.Fatalf("uploadToTargets() unexpected error: %v", err)
	}

	if len(targetErrs) != 1 || targetErrs[0] != nil {
		t.Fatalf("uploadToTargets() targetErrs = %v, want [nil]", targetErrs)
	}

	got, err := os.ReadFile(localObjectPath(cfg, &cfg.targets[0]))
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}

	if string(got) != content {
		t.Errorf("written content = %q, want %q", got, content)
	}
}

func TestBuildGPGCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		cfg            *config
		wantArgs       []string // exact args, in order
		wantPassphrase bool
	}{
		{
			name: "recipient mode, single recipient",
			cfg: &config{
				gpgBin:     "gpg",
				recipients: stringSlice{"me@example.com"},
			},
			wantArgs: []string{
				"--batch", "--yes", "--trust-model", "always", "--encrypt",
				"--recipient", "me@example.com",
			},
		},
		{
			name: "recipient mode, multiple recipients plus armor and homedir",
			cfg: &config{
				gpgBin:     "gpg",
				recipients: stringSlice{"a@example.com", "b@example.com"},
				armor:      true,
				gpgHomedir: "/tmp/gnupg-test",
			},
			wantArgs: []string{
				"--batch", "--yes", "--homedir", "/tmp/gnupg-test", "--armor",
				"--trust-model", "always", "--encrypt",
				"--recipient", "a@example.com", "--recipient", "b@example.com",
			},
		},
		{
			name: "symmetric mode",
			cfg: &config{
				gpgBin:    "gpg",
				symmetric: true,
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
// pipe, drain stdout, wait) without touching the network/S3 leg of the
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

	cfg := &config{gpgBin: "gpg", symmetric: true}

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
