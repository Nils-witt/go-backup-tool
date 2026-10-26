package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// fileSentry is the config file's sentry: entry, reporting errors to
// Sentry. DSN turns it on: error log records and panics of this process
// are sent there (see the telemetry package). FrontendDSN is the DSN the
// dashboard reports its own JavaScript errors to, usually a separate
// Sentry project; unset, the dashboard uses DSN too. Environment tags
// every event (e.g. "production"); unset leaves Sentry's default.
type fileSentry struct {
	DSN         string `yaml:"dsn"`
	FrontendDSN string `yaml:"frontend-dsn"`
	Environment string `yaml:"environment"`
}

// SentrySettings is the resolved sentry: entry. The zero value (DSN
// empty) disables Sentry reporting entirely, backend and dashboard alike.
type SentrySettings struct {
	// DSN is where this process reports to; empty disables Sentry.
	DSN string

	// FrontendDSN is the DSN served to the dashboard (see GET
	// /api/client-config): the config file's frontend-dsn:, or DSN when
	// that's unset. Empty exactly when DSN is.
	FrontendDSN string

	Environment string
}

// Enabled reports whether Sentry reporting is configured.
func (s SentrySettings) Enabled() bool { return s.DSN != "" }

// resolveSentry validates cfg (the config file's sentry: entry) and
// resolves it into SentrySettings. Errors name the offending key but never
// echo the DSN itself.
func resolveSentry(cfg fileSentry) (SentrySettings, error) {
	dsn := strings.TrimSpace(cfg.DSN)
	frontendDSN := strings.TrimSpace(cfg.FrontendDSN)
	environment := strings.TrimSpace(cfg.Environment)

	if dsn == "" {
		if frontendDSN != "" || environment != "" {
			return SentrySettings{}, errors.New("sentry: frontend-dsn and environment need sentry.dsn")
		}

		return SentrySettings{}, nil
	}

	if err := validateSentryDSN(dsn); err != nil {
		return SentrySettings{}, fmt.Errorf("sentry.dsn: %w", err)
	}

	if frontendDSN == "" {
		frontendDSN = dsn
	} else if err := validateSentryDSN(frontendDSN); err != nil {
		return SentrySettings{}, fmt.Errorf("sentry.frontend-dsn: %w", err)
	}

	return SentrySettings{DSN: dsn, FrontendDSN: frontendDSN, Environment: environment}, nil
}

// validateSentryDSN checks that dsn has a Sentry DSN's shape:
// http(s)://<public key>@<host>/<project id>.
func validateSentryDSN(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return errors.New("not a valid URL")
	}

	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("must be an http:// or https:// URL")
	case u.Host == "":
		return errors.New("missing host")
	case u.User == nil || u.User.Username() == "":
		return errors.New("missing public key (https://<key>@<host>/<project>)")
	case strings.Trim(u.Path, "/") == "":
		return errors.New("missing project id (https://<key>@<host>/<project>)")
	}

	return nil
}
