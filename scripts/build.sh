#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"

# Do not let Go silently download a different toolchain during production
# builds. The required Go version must be installed by the operator or CI.
export GOTOOLCHAIN=local

if ! command -v go >/dev/null 2>&1; then
  echo "error: Go is not installed or is not in PATH" >&2
  exit 1
fi

goos="${GOOS:-$(go env GOOS)}"
goarch="${GOARCH:-$(go env GOARCH)}"
version="${VERSION:-v0.2.0-dev}"
commit="${COMMIT:-$(git rev-parse --short=7 HEAD 2>/dev/null || printf unknown)}"

# BUILD_TIME and SOURCE_DATE_EPOCH allow release automation to provide a fixed
# timestamp. Otherwise record the actual UTC build time.
if [[ -n "${BUILD_TIME:-}" ]]; then
  build_time="$BUILD_TIME"
elif [[ -n "${SOURCE_DATE_EPOCH:-}" ]]; then
  if date -u -r "$SOURCE_DATE_EPOCH" '+%Y-%m-%dT%H:%M:%SZ' >/dev/null 2>&1; then
    build_time="$(date -u -r "$SOURCE_DATE_EPOCH" '+%Y-%m-%dT%H:%M:%SZ')"
  else
    build_time="$(date -u -d "@$SOURCE_DATE_EPOCH" '+%Y-%m-%dT%H:%M:%SZ')"
  fi
else
  build_time="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
fi

output="${OUTPUT:-bin/komari-bridge}"
mkdir -p "$(dirname "$output")"

package="github.com/LJAYi/komari-bridge/internal/buildinfo"
ldflags="-s -w -buildid= -X ${package}.Version=${version} -X ${package}.Commit=${commit} -X ${package}.BuildTime=${build_time}"

echo "building ${output} (${goos}/${goarch}, ${version}, ${commit})"
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
  go build -trimpath -ldflags "$ldflags" -o "$output" ./cmd/komari-bridge
