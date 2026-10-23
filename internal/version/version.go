// Package version holds build metadata injected via -ldflags.
package version

import (
	"fmt"
	"runtime"
)

// These are overridden at build time with -ldflags "-X ...".
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a human-readable version line.
func String() string {
	return fmt.Sprintf("qbt-replicator %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
