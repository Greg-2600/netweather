// Package version carries build identity. Values are injected at link time:
//
//	go build -ldflags "-X .../internal/version.Version=1.2.3 ..."
package version

import "fmt"

var (
	// Version is the release version, or "dev" for a local build.
	Version = "dev"
	// Commit is the git SHA the binary was built from.
	Commit = "none"
	// Date is the build timestamp in RFC 3339 form.
	Date = "unknown"
)

// String returns a single-line identity suitable for logs and `netweather version`.
func String() string {
	return fmt.Sprintf("netweather %s (commit %s, built %s)", Version, Commit, Date)
}
