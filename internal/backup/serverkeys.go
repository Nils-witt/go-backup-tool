package backup

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	// defaultServerKeyDir is where ensureServerKeyPair stores the server's
	// RSA key pair by default.
	defaultServerKeyDir = "data/keys"

	serverPrivateKeyFile = "server.key"
	serverPublicKeyFile  = "server.pub"
	serverKeyBits        = 4096
)

// ensureServerKeyPairAtStartup calls ensureServerKeyPair for
// defaultServerKeyDir, the default location every run maintains a server
// key pair under, logging (rather than failing the run over) any error —
// this is best-effort housekeeping, not something any run depends on yet.
func ensureServerKeyPairAtStartup(log *slog.Logger) {
	if err := ensureServerKeyPair(defaultServerKeyDir, log); err != nil {
		log.Warn("ensuring server key pair", "dir", defaultServerKeyDir, "err", err)
	}
}

// ensureServerKeyPair makes sure an RSA 4096 server key pair exists under
// dir (server.key holding the PKCS#1 private key, server.pub the PKIX
// public key, both PEM-encoded), generating one if dir/server.key is
// missing. Nothing currently reads these files back; this only guarantees
// they exist for future use.
func ensureServerKeyPair(dir string, log *slog.Logger) error {
	privPath := filepath.Join(dir, serverPrivateKeyFile)
	pubPath := filepath.Join(dir, serverPublicKeyFile)

	if _, err := os.Stat(privPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking %s: %w", privPath, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	key, err := rsa.GenerateKey(rand.Reader, serverKeyBits)
	if err != nil {
		return fmt.Errorf("generating RSA key: %w", err)
	}

	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", privPath, err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("marshaling public key: %w", err)
	}

	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	})

	if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", pubPath, err)
	}

	log.Info("generated server RSA key pair", "dir", dir, "bits", serverKeyBits)

	return nil
}
