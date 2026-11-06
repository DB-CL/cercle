#!/bin/sh
# Runs the Go toolchain in Docker (no Go installed on the host).
# Usage: deploy/go.sh test ./...   |   deploy/go.sh run ./cmd/cercle
# CGO_ENABLED, CERCLE_BENCH_MB, CERCLE_SCAN_FILES, CERCLE_QUEUE_FILES and CERCLE_REAL_GOSSIP are passed to the container when set.
set -e
cd "$(dirname "$0")/.."
mkdir -p "$HOME/.cache/cercle-go"
exec docker run --rm --network host \
  -u "$(stat -c %u .):$(stat -c %g .)" \
  -e HOME=/tmp -e CGO_ENABLED -e CERCLE_BENCH_MB -e CERCLE_SCAN_FILES -e CERCLE_QUEUE_FILES -e CERCLE_REAL_GOSSIP -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e GOFLAGS=-buildvcs=false \
  -v "$PWD":/src -v "$HOME/.cache/cercle-go":/cache -w /src \
  golang:1.27.3-bookworm go "$@"
