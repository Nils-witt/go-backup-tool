package backup

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// serverIdentity is this instance's persistent identity: a UUID (see
// ensureServerUUID) and the RSA private key (see ensureServerKeyPair) it
// signs outgoing type: remote target requests with (see
// signRemoteAuthToken, used by uploadToRemote/deleteRemoteObject in
// pipeline.go). A receiver on the other end verifies those requests against
// this instance's public key, configured on its matching receivers: entry.
type serverIdentity struct {
	uuid       string
	privateKey *rsa.PrivateKey
}

// loadServerIdentityAtStartup calls loadServerIdentity, logging (rather than
// failing the run over) any error and returning nil in that case — a
// type: remote target's uploads then fail individually (see
// remoteAuthHeader in pipeline.go) rather than the whole run refusing to
// start over a problem that may not affect every job.
func loadServerIdentityAtStartup(log *slog.Logger) *serverIdentity {
	identity, err := loadServerIdentity(log)
	if err != nil {
		log.Warn("loading server identity", "err", err)
		return nil
	}

	return identity
}

// loadServerIdentity ensures defaultServerKeyDir holds a server key pair and
// UUID (generating them on first use — see ensureServerKeyPair/
// ensureServerUUID), then loads them into a serverIdentity. See
// loadServerIdentityAtStartup, which runWithContext (app.go) calls once at
// startup.
func loadServerIdentity(log *slog.Logger) (*serverIdentity, error) {
	if err := ensureServerKeyPair(defaultServerKeyDir, log); err != nil {
		return nil, fmt.Errorf("ensuring server key pair: %w", err)
	}

	id, err := ensureServerUUID(defaultServerKeyDir, log)
	if err != nil {
		return nil, fmt.Errorf("ensuring server UUID: %w", err)
	}

	key, err := loadServerPrivateKey(defaultServerKeyDir)
	if err != nil {
		return nil, fmt.Errorf("loading server private key: %w", err)
	}

	return &serverIdentity{uuid: id, privateKey: key}, nil
}

// loadServerPrivateKey reads and parses dir/server.key (written by
// ensureServerKeyPair), the PEM-encoded PKCS#1 RSA private key this
// instance signs outgoing remote-target requests with.
func loadServerPrivateKey(dir string) (*rsa.PrivateKey, error) {
	path := filepath.Join(dir, serverPrivateKeyFile)

	data, err := os.ReadFile(path) //nolint:gosec // path is the operator-configured/default key dir plus a fixed literal, not user input
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(data)
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		return nil, fmt.Errorf("%s: not a PEM RSA PRIVATE KEY block", path)
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return key, nil
}
