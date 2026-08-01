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
