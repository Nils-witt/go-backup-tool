package identity

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

// ensureServerKeyPair makes sure an RSA 4096 server key pair exists under
// dir (server.key holding the PKCS#1 private key, server.pub the PKIX
// public key, both PEM-encoded), generating one if dir/server.key is
// missing. See loadServerIdentity, which reads server.key back to sign
// outgoing type: remote target requests.
func ensureServerKeyPair(dir string, log *slog.Logger) error {
	privPath := filepath.Join(dir, ServerPrivateKeyFile)
	pubPath := filepath.Join(dir, ServerPublicKeyFile)

	if _, err := os.Stat(privPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking %s: %w", privPath, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	key, err := rsa.GenerateKey(rand.Reader, ServerKeyBits)
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

	log.Info("generated server RSA key pair", "dir", dir, "bits", ServerKeyBits)

	return nil
}
