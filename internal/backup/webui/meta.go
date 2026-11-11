package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
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

// healthPingTimeout bounds handleHealth's state db ping, so a hung database
// fails the health check instead of hanging it.
const healthPingTimeout = 2 * time.Second

// Values of healthJSON's Status and Database fields.
const (
	healthOK          = "ok"
	healthDegraded    = "degraded"
	healthUnavailable = "unavailable"
)

// errStateDBUnavailable is pingStateDB's error when the state db never
// opened.
var errStateDBUnavailable = errors.New("state db is not open")

// healthJSON is GET /api/health's response body.
type healthJSON struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Database string `json:"database"`
}

// handleHealth serves GET /api/health: public/unauthenticated, for the
// health subcommand (see the ctl package), container health checks, and
// load balancers. It answers 200 when the state db answers a ping and 503
// when it doesn't (or db is nil, i.e. the state db never opened). The ping
// error is logged, not returned: it can name the database host.
func handleHealth(db store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := pingStateDB(r.Context(), db); err != nil {
			log.Warn("health check: state db unavailable", "err", err)

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(healthJSON{Status: healthDegraded, Version: version.Version, Database: healthUnavailable})

			return
		}

		writeJSON(w, healthJSON{Status: healthOK, Version: version.Version, Database: healthOK})
	}
}

// pingStateDB pings db within healthPingTimeout; a nil db is an error.
func pingStateDB(ctx context.Context, db store.Store) error {
	if db == nil {
		return errStateDBUnavailable
	}

	ctx, cancel := context.WithTimeout(ctx, healthPingTimeout)
	defer cancel()

	return db.Ping(ctx)
}
