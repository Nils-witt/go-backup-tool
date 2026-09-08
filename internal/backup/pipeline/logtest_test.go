package pipeline

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
)

// discardLogger is a *slog.Logger that writes nowhere, for tests that need
// to pass one but don't assert on its output.
var discardLogger = slog.New(slog.DiscardHandler)

// TestMain removes testGPGKeyring's shared homedir (if it ended up creating
// one) once every test in the package has run, since it lives under the OS
// temp directory rather than a per-test t.TempDir() and so isn't cleaned up
// on its own.
func TestMain(m *testing.M) {
	code := m.Run()

	if testGPGKeyringDir != "" {
		_ = os.RemoveAll(testGPGKeyringDir)
	}

	os.Exit(code)
}

// testGPGRecipient names the identity every testGPGKeyring-generated test
// key is created for.
const testGPGRecipient = "go-backup-tool-test@example.com"

// testGPGKeyringOnce/testGPGKeyringDir/testGPGKeyringFailure back testGPGKeyring:
// the keypair is generated at most once per test binary run (real gpg key
// generation is slow enough that doing it per-test would noticeably slow the
// suite down) and shared, read-only, by every test that needs a working
// recipient to gpg-encrypt to.
var (
	testGPGKeyringOnce    sync.Once
	testGPGKeyringDir     string
	testGPGKeyringFailure error //nolint:errname // not a sentinel; sync.Once-captured setup outcome, checked once and never compared with errors.Is
)

// testGPGKeyring returns the homedir of a shared, no-passphrase GPG keyring
// containing one keypair for testGPGRecipient, generating it on first use.
// It skips the calling test if gpg isn't on PATH or key generation fails.
func testGPGKeyring(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	testGPGKeyringOnce.Do(func() {
		testGPGKeyringDir, testGPGKeyringFailure = generateTestGPGKeyring()
	})

	if testGPGKeyringFailure != nil {
		t.Skipf("generating test gpg keyring: %v", testGPGKeyringFailure)
	}

	return testGPGKeyringDir
}

// generateTestGPGKeyring creates a fresh homedir under the OS temp directory
// (outlives any single test's t.TempDir(), since the keyring is shared
// across the whole test binary run) holding one unattended, unprotected
// RSA keypair for testGPGRecipient. The prefix is kept short deliberately:
// gpg-agent's socket path (homedir + "/S.gpg-agent") is subject to the
// kernel's sun_path limit (~104 bytes on macOS), and a longer prefix here
// has been observed to push it over that, silently breaking agent auto-start
// ("IPC connect call failed") without any indication the path was the cause.
func generateTestGPGKeyring() (string, error) {
	dir, err := os.MkdirTemp("", "gbt-gpg-")
	if err != nil {
		return "", fmt.Errorf("creating gpg homedir: %w", err)
	}

	batch := fmt.Sprintf(`%%no-protection
Key-Type: RSA
Key-Length: 2048
Name-Real: go-backup-tool test
Name-Email: %s
Expire-Date: 0
%%commit
`, testGPGRecipient)

	batchFile := filepath.Join(dir, "gen-key-batch")
	if err := os.WriteFile(batchFile, []byte(batch), 0o600); err != nil {
		return "", fmt.Errorf("writing gpg batch file: %w", err)
	}

	cmd := exec.CommandContext(context.Background(), "gpg", "--homedir", dir, "--batch", "--gen-key", batchFile) //nolint:gosec // dir/batchFile are this test's own MkdirTemp/WriteFile output, not untrusted input
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("gpg --gen-key: %w (output: %s)", err, out)
	}

	return dir, nil
}

// testServerIdentity builds a *backup.ServerIdentity backed by a freshly
// generated RSA key, for tests that need one to exercise signing without
// touching disk.
func testServerIdentity(t *testing.T) *identity.ServerIdentity {
	t.Helper()

	id, _ := testServerIdentityAndKey(t)

	return id
}

// testServerIdentityAndKey is testServerIdentity, additionally returning the
// generated private key directly, for tests that need to verify a signature
// against its public half (ServerIdentity itself keeps the key private).
func testServerIdentityAndKey(t *testing.T) (*identity.ServerIdentity, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	return identity.NewTestServerIdentity("test-sender-uuid", key), key
}
