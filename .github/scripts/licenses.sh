#!/bin/sh
set -eu

GOBIN="$(pwd)/.stemma/bin" go install github.com/google/go-licenses/v2@v2.0.1
# Collect each platform separately so native-only dependencies are included.
for target in darwin linux windows; do
  GOOS="$target" CGO_ENABLED=0 .stemma/bin/go-licenses save ./cmd/stemma \
    --save_path=".stemma/release-licenses/$target" --force
done
