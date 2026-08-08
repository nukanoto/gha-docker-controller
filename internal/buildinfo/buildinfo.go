// Package buildinfo holds values injected into release builds with ldflags.
package buildinfo

import "runtime"

// Version is the release version.
var Version = "dev"

// Commit is the source commit.
var Commit = "unknown"

// Date is the UTC build date.
var Date = "unknown"

// GoVersion returns the compiler version embedded by the runtime.
func GoVersion() string {
	return runtime.Version()
}
