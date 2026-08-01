package buildinfo

import (
	"bytes"
	"testing"
)

func TestWrite(t *testing.T) {
	oldVersion, oldCommit, oldBuildTime := Version, Commit, BuildTime
	t.Cleanup(func() {
		Version, Commit, BuildTime = oldVersion, oldCommit, oldBuildTime
	})
	Version = "v1.2.3"
	Commit = "abcdef0"
	BuildTime = "2026-08-01T12:00:00Z"

	var output bytes.Buffer
	Write(&output)
	const want = "version: v1.2.3\ncommit: abcdef0\nbuild_time: 2026-08-01T12:00:00Z\n"
	if output.String() != want {
		t.Fatalf("unexpected version output:\n%s\nwant:\n%s", output.String(), want)
	}
}

func TestClientVersion(t *testing.T) {
	oldVersion, oldCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })

	Version = "v0.2.0-dev"
	Commit = "bf27876abcdef"
	if got, want := ClientVersion("windows_ssh"), "komari-bridge v0.2.0-dev (bf27876) / windows-ssh"; got != want {
		t.Fatalf("ClientVersion() = %q, want %q", got, want)
	}

	Commit = "unknown"
	if got, want := ClientVersion(""), "komari-bridge v0.2.0-dev"; got != want {
		t.Fatalf("ClientVersion() without build metadata = %q, want %q", got, want)
	}

	Commit = "bf27876"
	for collector, want := range map[string]string{
		"linux_ssh":       "komari-bridge v0.2.0-dev (bf27876) / agentless-ssh",
		"windows_wsl":     "komari-bridge v0.2.0-dev (bf27876) / windows-wsl",
		"windows_ssh_wsl": "komari-bridge v0.2.0-dev (bf27876) / windows-wsl",
	} {
		if got := ClientVersion(collector); got != want {
			t.Errorf("ClientVersion(%q) = %q, want %q", collector, got, want)
		}
	}
}
