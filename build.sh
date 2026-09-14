#!/usr/bin/env bash
# Builds cpa-balancer.so for linux/amd64 against Debian bookworm's glibc, the
# same base as the eceasy/cli-proxy-api image. Runs the tests first.
#
#   ./build.sh          test + build to dist/linux/amd64/cpa-balancer.so
#   ./build.sh test     tests only
#   ./build.sh tidy     refresh go.sum
set -euo pipefail
cd "$(dirname "$0")"

run() {
  docker run --rm -v "$PWD":/src -w /src -v cpa-balancer-gomod:/go/pkg/mod -v cpa-balancer-gocache:/root/.cache/go-build \
    -e CGO_ENABLED=1 -e GOFLAGS=-mod=mod golang:1.26-bookworm sh -c "$1"
}

case "${1:-build}" in
  tidy) run "go mod tidy" ;;
  test) run "go vet ./... && go test ./..." ;;
  build)
    run "go vet ./... && go test ./... && mkdir -p dist/linux/amd64 && go build -trimpath -buildmode=c-shared -ldflags '-s -w' -o dist/linux/amd64/cpa-balancer.so . && rm -f dist/linux/amd64/cpa-balancer.h"
    ls -la dist/linux/amd64/cpa-balancer.so
    ;;
  *) echo "usage: $0 [build|test|tidy]" >&2; exit 2 ;;
esac
