// Package version exposes build metadata for ContainerWatch.
package version

import "fmt"

// These values are overridden at build time via -ldflags.
var (
	// Version is the semantic version of the running binary.
	Version = "1.0.0-dev"
	// Commit is the git SHA the binary was built from.
	Commit = "none"
	// BuildDate is the RFC3339 timestamp of the build.
	BuildDate = "unknown"
)

// UserAgent is sent with every request to container engine APIs so that
// operators can identify ContainerWatch traffic in audit logs.
const UserAgent = "ContainerWatch"

// String returns a compact human readable version string.
func String() string {
	if Commit == "none" || Commit == "" {
		return Version
	}
	if len(Commit) > 7 {
		return fmt.Sprintf("%s (%s)", Version, Commit[:7])
	}
	return fmt.Sprintf("%s (%s)", Version, Commit)
}
