#!/usr/bin/env bash
set -euo pipefail
cd /workspace/cpa-plugin-request-recorder
export GOPATH=/workspace/.cache/go GOMODCACHE=/workspace/.cache/go-mod GOCACHE=/workspace/.cache/go-build
export PATH=/workspace/.tools/go/bin:$PATH
if ! /workspace/.tools/go/bin/go version 2>/dev/null | grep -q 'go1.26.10 '; then
  mkdir -p /workspace/.tools
  archive=$(mktemp /tmp/recorder-go.XXXXXX.tar.gz)
  trap 'rm -f "$archive"' EXIT
  curl --fail --location --retry 2 https://go.dev/dl/go1.26.10.linux-amd64.tar.gz -o "$archive"
  echo "ccfe7ae822d2c2f41f4fe7c956199967a4dac22d0ee0d2b2bfdb440aab3b20c5  $archive" | sha256sum --check --status
  tar -xzf "$archive" -C /workspace/.tools
fi
go mod download
make test build
npm ci --ignore-scripts --no-audit --no-fund
