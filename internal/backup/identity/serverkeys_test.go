package identity

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureServerKeyPairGeneratesRSA4096(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "keys")

	if err := ensureServerKeyPair(dir, discardLogger); err != nil {
		t.Fatalf("ensureServerKeyPair() unexpected error: %v", err)
	}

	privPEM, err := os.ReadFile(filepath.Join(dir, ServerPrivateKeyFile)) //nolint:gosec // dir is t.TempDir() plus fixed test literals
	if err != nil {
		t.Fatalf("reading private key: %v", err)
	}

	block, _ := pem.Decode(privPEM)
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		t.Fatalf("private key PEM block = %+v, want type RSA PRIVATE KEY", block)
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parsing private key: %v", err)
	}

	if bits := key.N.BitLen(); bits != ServerKeyBits {
		t.Errorf("private key size = %d bits, want %d", bits, ServerKeyBits)
	}

	pubPEM, err := os.ReadFile(filepath.Join(dir, ServerPublicKeyFile)) //nolint:gosec // dir is t.TempDir() plus fixed test literals
	if err != nil {
		t.Fatalf("reading public key: %v", err)
	}

	pubBlock, _ := pem.Decode(pubPEM)
	if pubBlock == nil || pubBlock.Type != "PUBLIC KEY" {
		t.Fatalf("public key PEM block = %+v, want type PUBLIC KEY", pubBlock)
	}

	if _, err := x509.ParsePKIXPublicKey(pubBlock.Bytes); err != nil {
		t.Fatalf("parsing public key: %v", err)
	}
}

func TestEnsureServerKeyPairIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "keys")

	if err := ensureServerKeyPair(dir, discardLogger); err != nil {
		t.Fatalf("first ensureServerKeyPair() unexpected error: %v", err)
	}

	privPath := filepath.Join(dir, ServerPrivateKeyFile)

	before, err := os.ReadFile(privPath) //nolint:gosec // privPath is t.TempDir() plus fixed test literals
	if err != nil {
		t.Fatalf("reading private key: %v", err)
	}

	if err := ensureServerKeyPair(dir, discardLogger); err != nil {
		t.Fatalf("second ensureServerKeyPair() unexpected error: %v", err)
	}

	after, err := os.ReadFile(privPath) //nolint:gosec // privPath is t.TempDir() plus fixed test literals
	if err != nil {
		t.Fatalf("reading private key: %v", err)
	}

	if string(before) != string(after) {
		t.Error("ensureServerKeyPair() regenerated an already-existing key pair")
	}
}
