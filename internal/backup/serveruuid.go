package backup

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// serverUUIDFile is the name ensureServerUUID stores this instance's
// persistent identity under, inside defaultServerKeyDir (the same directory
// ensureServerKeyPair uses for the RSA key pair).
const serverUUIDFile = "server.uuid"

// ensureServerUUID returns dir/server.uuid's contents, generating and
// persisting a fresh random UUID there first if the file doesn't exist yet.
// Once written, that value is read back as-is on every later call and never
// regenerated or changed, giving this instance a stable identity across
// restarts for as long as dir is preserved. See loadServerIdentity, which
// uses this as a signed request's issuer.
func ensureServerUUID(dir string, log *slog.Logger) (string, error) {
	path := filepath.Join(dir, serverUUIDFile)

	existing, err := os.ReadFile(path) //nolint:gosec // path is dir (the operator-configured/default key dir) plus a fixed literal, not user input
	if err == nil {
		return strings.TrimSpace(string(existing)), nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("checking %s: %w", path, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}

	id := uuid.NewString()

	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}

	log.Info("generated server UUID", "dir", dir, "uuid", id)

	return id, nil
}
