package config

// Default values used when the corresponding configuration or flag isn't
// given explicitly.
const (
	DefaultConfigPath = "config.yaml"
	DefaultKeyPattern = "backup-{time}"
	DefaultGPGBin     = "gpg"

	// DefaultKeysDir is where this instance's identity (see the identity
	// package) is stored when keys-dir: is unset.
	DefaultKeysDir = "data/keys"

	// DefaultEventLogLimit is how many of the most recent entries each of
	// the web UI's event logs (job runs, target runs, logins, downloads,
	// receiver events) shows when webui.event-log-limit: is unset.
	DefaultEventLogLimit = 200
)
