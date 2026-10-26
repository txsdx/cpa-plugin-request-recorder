#!/usr/bin/env bash
set -euo pipefail
: "${1:?release version is required}"
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
# Exact CPA source used to verify public store contracts; never track main here.
cpa_commit=307803510bce87f986fcf7b1a84190f105c7b4aa
sdk_dir=${CPA_STORE_SDK_DIR:-}
if [[ -z "$sdk_dir" ]]; then
  download_dir=$(mktemp -d)
  trap 'rm -rf "$download_dir"' EXIT
  curl --fail --location --retry 3 \
    "https://codeload.github.com/router-for-me/CLIProxyAPI/tar.gz/$cpa_commit" \
    -o "$download_dir/cpa.tar.gz"
  tar -xzf "$download_dir/cpa.tar.gz" -C "$download_dir"
  sdk_dir="$download_dir/CLIProxyAPI-$cpa_commit"
fi
assets_dir=${CPA_STORE_ASSETS_DIR:-"$repo_dir/dist"}
cd "$sdk_dir"
if [[ "${CPA_STORE_CHECK_MODE:-contract}" == "live" ]]; then
  go run "$repo_dir/scripts/check_live_store.go" "$1"
else
  go run "$repo_dir/scripts/check_store_contract.go" "$assets_dir/registry.json" "$assets_dir" "$1"
fi
