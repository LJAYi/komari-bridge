// Package buildinfo exposes version metadata embedded in the bridge binary.
package buildinfo

import (
	"fmt"
	"io"
)

// These values are replaced by scripts/build.sh using -ldflags. Keep useful
// development defaults so `go run` and ad-hoc local builds remain identifiable.
var (
	Version   = "v0.2.0-dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// Write prints build metadata in a stable, human-readable format.
func Write(w io.Writer) {
	fmt.Fprintf(w, "version: %s\ncommit: %s\nbuild_time: %s\n", Version, Commit, BuildTime)
}
