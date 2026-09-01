// Package config holds default values shared across the backup app's
// configuration and flag parsing.
package config

// Default values used when the corresponding configuration or flag isn't
// given explicitly.
const (
	DefaultConfigPath = "config.yaml"
	DefaultKeyPattern = "backup-{time}.gpg"
	DefaultGPGBin     = "gpg"
)
