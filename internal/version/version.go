// Package version holds build metadata set at compile time via
// `-ldflags "-X ..."` (see Dockerfile and .goreleaser.yaml).
package version

var (
	// Version is the git tag the binary was built from, or "dev" for a
	// local/untagged build (CI's untagged images stamp "dev-<sha>").
	Version = "dev"
	// Commit is the short git commit hash the binary was built from.
	Commit = "unknown"
)

// DeprecatedConfigRemoval is the release that removes the config file's
// deprecated settings: the receivers:, notifications: and report: sections,
// a job's cmd: and command:, a receiver's public-key:, and importing jobs:,
// servers: and commands: under webui.job-editing. Every deprecation warning
// names it, and config.example.yaml must too (see
// TestExampleConfigNamesRemovalRelease).
const DeprecatedConfigRemoval = "v8.0.0"
