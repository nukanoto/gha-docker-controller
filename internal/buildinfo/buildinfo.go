// Package buildinfo provides build information injected by ldflags -X.
// version and commit are used by GitHub integration (user-agent,
// SetSystemInfo) and the version subcommand output. Development builds
// without ldflags still get explicit values ("dev"/"unknown"), never empty
// strings.
package buildinfo

import "runtime"

// Version is the release version. It is overridden by ldflags -X
// github.com/nukanoto/gha-docker-controller/internal/buildinfo.Version.
// Returns "dev" when unset.
var Version = "dev"

// Commit is the commit SHA the build came from. It is overridden by ldflags
// -X. Returns "unknown" when unset.
var Commit = "unknown"

// Date is the build date (UTC RFC3339). It is overridden by ldflags -X.
// Returns "unknown" when unset.
var Date = "unknown"

// GoVersion returns the version of the Go toolchain that built the binary.
// It is not injected via ldflags; the actual runtime.Version() value is used.
func GoVersion() string {
	return runtime.Version()
}
