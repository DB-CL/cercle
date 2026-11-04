#!/bin/sh
# Lance la chaîne Go dans Docker (pas de Go installé sur l'hôte).
# Usage : deploy/go.sh test ./...   |   deploy/go.sh run ./cmd/cercle
# CGO_ENABLED, CERCLE_BENCH_MB, CERCLE_SCAN_FILES et CERCLE_QUEUE_FILES sont transmis au conteneur s'ils sont définis.
set -e
cd "$(dirname "$0")/.."
mkdir -p "$HOME/.cache/cercle-go"
exec docker run --rm --network host \
  -u "$(stat -c %u .):$(stat -c %g .)" \
  -e HOME=/tmp -e CGO_ENABLED -e CERCLE_BENCH_MB -e CERCLE_SCAN_FILES -e CERCLE_QUEUE_FILES -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e GOFLAGS=-buildvcs=false \
  -v "$PWD":/src -v "$HOME/.cache/cercle-go":/cache -w /src \
  golang:1.27.3-bookworm go "$@"
