// Package version reports build metadata injected at link time.
package version

import (
	"fmt"
	"runtime"
)

// Set by the linker: -X github.com/quyendv/opsward/internal/version.Version=v0.1.0
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a single-line description of the running build.
func String() string {
	return fmt.Sprintf("opsward %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
