package webui

import (
	"net/http"
	"os"
	"strings"

	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
	"nilswitt.dev/go-backup-tool/internal/version"
)

// identityJSON is serverIdentity's wire shape for handleIdentity, matching
// the dashboard's own field naming (snake_case, as every other /api/...
// endpoint here uses).
type identityJSON struct {
	UUID        string `json:"uuid"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

// handleIdentity serves GET /api/identity: this instance's persistent UUID
// and PEM-encoded public key (see serverIdentity), for the dashboard's
// "Server identity" section — an operator reads them off there to fill in a
// receiving instance's matching receiver (its sender public key, in Receiver settings)
// rather than digging through this instance's keys-dir: on disk. identity
// nil (loadServerIdentityAtStartup failed at startup, or the receiver API
// isn't used by any type: remote target) serves a zero-value identityJSON,
// which the dashboard's JS treats as "no identity to show".
func handleIdentity(identity *identity.ServerIdentity) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var out identityJSON

		if identity != nil {
			out = identityJSON{UUID: identity.UUID(), PublicKey: identity.PublicKeyPEM()}
			if key := identity.PrivateKey(); key != nil {
				out.Fingerprint = trust.Fingerprint(&key.PublicKey)
			}
		}

		writeJSON(w, out)
	}
}

// metaJSON is this instance's build metadata, as served by GET /api/meta,
// for the dashboard's footer — the SPA's index.html is a static build artifact with no server-side
// templating, so this replaces what dashboardHTML's now-removed
// {{VERSION}}/{{COMMIT}} placeholder substitutions did.
type metaJSON struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	InstanceName string `json:"instanceName,omitempty"`
}

// instanceNameEnv names the environment variable holding an optional,
// free-form label for this instance (e.g. "prod" or "nas-01"), shown on the
// login page and in the dashboard's app bar to tell instances apart. Only a
// fallback: the config file's top-level server-name: takes precedence (see
// resolveInstanceName).
const instanceNameEnv = "INSTANCE_NAME"

// resolveInstanceName returns configured (the config file's server-name:)
// trimmed, falling back to instanceNameEnv when it's empty.
func resolveInstanceName(configured string) string {
	if name := strings.TrimSpace(configured); name != "" {
		return name
	}

	return strings.TrimSpace(os.Getenv(instanceNameEnv))
}

// handleMeta serves GET /api/meta: always public/unauthenticated, since the
// footer it feeds is shown on the login page too. Leaks nothing the
// binary's own --version doesn't already report.
func handleMeta(instanceName string) http.HandlerFunc {
	meta := metaJSON{
		Version:      version.Version,
		Commit:       version.Commit,
		InstanceName: resolveInstanceName(instanceName),
	}

	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, meta)
	}
}
