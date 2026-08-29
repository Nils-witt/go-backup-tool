package identity

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"nilswitt.dev/go-backup-tool/internal/backup/remoteAuth"
)

// ServerIdentity is this instance's persistent identity: a UUID (see
// ensureServerUUID) and the RSA private key (see ensureServerKeyPair) it
// signs outgoing type: remote target requests with (see
// signRemoteAuthToken, used by uploadToRemote/deleteRemoteObject in
// pipeline.go). A receiver on the other end verifies those requests against
// this instance's public key, configured on its matching receivers: entry.
// publicKeyPEM is that same public key, PEM-encoded, so the web UI dashboard
// (see handleIdentity) can display it for an operator to paste into a
// receiving instance's config.
type ServerIdentity struct {
	uuid         string
	privateKey   *rsa.PrivateKey
	publicKeyPEM string
}

// LoadServerIdentityAtStartup calls loadServerIdentity, logging (rather than
// failing the run over) any error and returning nil in that case — a
// type: remote target's uploads then fail individually (see
// remoteAuthHeader in pipeline.go) rather than the whole run refusing to
// start over a problem that may not affect every job. dir is the config
// file's resolved keys-dir: (see runConfig.keysDir), defaulting to
// defaultServerKeyDir when unset.
func LoadServerIdentityAtStartup(log *slog.Logger, dir string) (*ServerIdentity, error) {
	identity, err := loadServerIdentity(dir, log)
	if err != nil {
		log.Warn("loading server identity", "dir", dir, "err", err)
		return nil, err
	}

	return identity, nil
}

// loadServerIdentity ensures dir holds a server key pair and UUID
// (generating them on first use — see ensureServerKeyPair/
// ensureServerUUID), then loads them into a serverIdentity. See
// loadServerIdentityAtStartup, which runWithContext (app.go) calls once at
// startup.
func loadServerIdentity(dir string, log *slog.Logger) (*ServerIdentity, error) {
	if err := ensureServerKeyPair(dir, log); err != nil {
		return nil, fmt.Errorf("ensuring server key pair: %w", err)
	}

	id, err := ensureServerUUID(dir, log)
	if err != nil {
		return nil, fmt.Errorf("ensuring server UUID: %w", err)
	}

	key, err := loadServerPrivateKey(dir)
	if err != nil {
		return nil, fmt.Errorf("loading server private key: %w", err)
	}

	pubPEM, err := os.ReadFile(filepath.Join(dir, ServerPublicKeyFile)) //nolint:gosec // path is the operator-configured/default key dir plus a fixed literal, not user input
	if err != nil {
		return nil, fmt.Errorf("loading server public key: %w", err)
	}

	return &ServerIdentity{uuid: id, privateKey: key, publicKeyPEM: string(pubPEM)}, nil
}

// NewTestServerIdentity builds a ServerIdentity directly from uuid and
// privateKey, for tests in other packages that need one without touching
// disk (see LoadServerIdentity for the real, on-disk-backed constructor).
func NewTestServerIdentity(uuid string, privateKey *rsa.PrivateKey) *ServerIdentity {
	return &ServerIdentity{uuid: uuid, privateKey: privateKey}
}

// UUID returns this instance's persistent UUID (see ensureServerUUID).
func (si *ServerIdentity) UUID() string {
	return si.uuid
}

// PublicKeyPEM returns this instance's PEM-encoded RSA public key, for
// display to an operator (see handleIdentity) to paste into a receiving
// instance's config.
func (si *ServerIdentity) PublicKeyPEM() string {
	return si.publicKeyPEM
}

// SignRequest signs a fresh short-lived JWT (see SignRemoteAuthToken)
// identifying this instance as issuer and scoped to audience (the
// destination receiver's id), for uploadToRemote/deleteRemoteObject's
// Authorization: Bearer header.
func (si *ServerIdentity) SignRequest(audience string) (string, error) {
	return remoteAuth.SignRemoteAuthToken(si.privateKey, si.uuid, audience)
}

// loadServerPrivateKey reads and parses dir/server.key (written by
// ensureServerKeyPair), the PEM-encoded PKCS#1 RSA private key this
// instance signs outgoing remote-target requests with.
func loadServerPrivateKey(dir string) (*rsa.PrivateKey, error) {
	path := filepath.Join(dir, ServerPrivateKeyFile)

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
