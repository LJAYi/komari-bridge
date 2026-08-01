// Package buildinfo exposes version metadata embedded in the bridge binary.
package buildinfo

import (
	"fmt"
	"io"
	"strings"
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

// ClientVersion returns the value reported to Komari's client-version field.
// Keep the bridge build identity separate from the observed operating-system
// version: this field describes the software that submitted the report.
func ClientVersion(collector string) string {
	version := strings.TrimSpace(Version)
	if version == "" {
		version = "unknown"
	}
	result := "komari-bridge " + version
	if commit := shortCommit(Commit); commit != "" {
		result += " (" + commit + ")"
	}
	if collector = strings.TrimSpace(strings.ReplaceAll(collector, "_", "-")); collector != "" {
		result += " / " + collector
	}
	return result
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if commit == "" || strings.EqualFold(commit, "unknown") {
		return ""
	}
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
