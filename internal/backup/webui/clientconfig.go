package webui

import (
	"net/http"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/telemetry"
)

// clientConfigJSON is GET /api/client-config's response: settings the
// dashboard needs before anyone signs in. Sentry is nil (omitted) while
// the config file's sentry: entry is unset.
type clientConfigJSON struct {
	Sentry *clientSentryJSON `json:"sentry,omitempty"`
}

// clientSentryJSON is where the dashboard reports its own errors: the
// config file's sentry.frontend-dsn (or dsn), tagged like this process's
// own events (see telemetry.Init).
type clientSentryJSON struct {
	DSN         string `json:"dsn"`
	Environment string `json:"environment,omitempty"`
	Release     string `json:"release"`
}

// handleClientConfig serves GET /api/client-config: always public, since
// the dashboard reports errors on the login page too. A Sentry DSN is
// public by design: it only allows sending events to its project.
func handleClientConfig(s config.SentrySettings) http.HandlerFunc {
	var cfg clientConfigJSON
	if s.Enabled() {
		cfg.Sentry = &clientSentryJSON{DSN: s.FrontendDSN, Environment: s.Environment, Release: telemetry.Release()}
	}

	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, cfg)
	}
}
