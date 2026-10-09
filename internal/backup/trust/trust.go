// Package trust manages the trusted servers registry: every remote
// go-backup-tool instance this one accepts receiver API requests from,
// identified by that instance's persistent server UUID (the issuer of every
// request token it signs) and its RSA public key. Receivers name the
// trusted servers allowed to write to them by id.
package trust

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Server is one resolved trusted server.
type Server struct {
	ID          string
	Name        string
	PublicKey   *rsa.PublicKey
	Fingerprint string
}

// Registry holds every trusted server currently active, keyed by id. A
// receiver looks its allowed servers up here on every request, so a trusted
// server created, edited, or deleted in the web UI takes effect without a
// restart. Safe for concurrent use; a nil *Registry holds no servers.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Server
}

// NewRegistry builds a registry holding servers (copied).
func NewRegistry(servers map[string]Server) *Registry {
	return &Registry{byID: maps.Clone(servers)}
}

// Get returns trusted server id, reporting false if there is none.
func (r *Registry) Get(id string) (Server, bool) {
	if r == nil {
		return Server{}, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	s, ok := r.byID[id]

	return s, ok
}

// List returns every trusted server, in id order.
func (r *Registry) List() []Server {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Server, 0, len(r.byID))
	for _, id := range slices.Sorted(maps.Keys(r.byID)) {
		out = append(out, r.byID[id])
	}

	return out
}

// Put adds s, replacing any existing trusted server with the same id.
func (r *Registry) Put(s Server) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.byID == nil {
		r.byID = make(map[string]Server)
	}

	r.byID[s.ID] = s
}

// Delete removes trusted server id, a no-op if there is none.
func (r *Registry) Delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.byID, id)
}

// ParsePublicKey parses raw as a PEM-encoded PKIX public key — the format
// a sending instance writes to its server.pub — requiring it to be an RSA
// key, since that's the only algorithm remote request tokens are signed
// with.
func ParsePublicKey(raw string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("public-key is required and must be a PEM-encoded PUBLIC KEY block")
	}

	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing public-key: %w", err)
	}

	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public-key must be an RSA key, got %T", key)
	}

	return rsaKey, nil
}

// Fingerprint returns key's SHA-256 fingerprint in OpenSSH's notation
// ("SHA256:" plus the unpadded base64 digest of its PKIX DER encoding), so
// an operator can compare the key a receiving instance trusts against the
// one shown on the sending instance's Identity page at a glance.
func Fingerprint(key *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return ""
	}

	sum := sha256.Sum256(der)

	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
