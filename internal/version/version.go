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
