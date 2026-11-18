#!/usr/bin/env sh
# Run a command with the Go toolchain: locally if `go` is installed, otherwise in golang:1.26.
set -eu
cd "$(dirname "$0")/.."
if command -v go >/dev/null 2>&1; then
  exec sh -c "$*"
fi
exec docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOPATH=/go -e GOCACHE=/go/cache \
  -v cyphra-gomod:/go -v "$PWD:/src" -w /src "${GO_IMAGE:-golang:1.26}" sh -c "$*"
