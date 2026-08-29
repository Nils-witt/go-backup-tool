// Package version holds build metadata set at compile time via
// `-ldflags "-X ..."` (see Dockerfile and .goreleaser.yml).
package version

var (
	// Version is the git tag the binary was built from. It is left empty
	// for untagged builds.
	Version = ""
	// Commit is the short git commit hash the binary was built from.
	Commit = "unknown"
)
