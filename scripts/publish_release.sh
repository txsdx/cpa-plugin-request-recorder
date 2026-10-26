#!/usr/bin/env bash
# All tests and asset verification must pass before this script is called.
set -euo pipefail
: "${RELEASE_TAG:?validated release tag is required}"
: "${GITHUB_SHA:?source commit SHA is required}"
# Refuse to attach tested binaries to a pre-existing tag for different source.
refs=$(git ls-remote origin "refs/tags/$RELEASE_TAG" "refs/tags/$RELEASE_TAG^{}")
if [[ -n "$refs" ]]; then
  tag_sha=$(printf '%s\n' "$refs" | tail -n 1 | cut -f 1)
  [[ "$tag_sha" == "$GITHUB_SHA" ]] || { echo "Release tag points to a different commit" >&2; exit 1; }
fi
if gh release view "$RELEASE_TAG" --json isDraft > release-state.json 2>/dev/null; then
  python3 -c 'import json; assert json.load(open("release-state.json"))["isDraft"], "Refusing to overwrite an existing published release"'
else
  # Manual runs create the tag on the exact tested commit, even if main moves.
  gh release create "$RELEASE_TAG" --draft --target "$GITHUB_SHA" \
    --title "$RELEASE_TAG" --generate-notes
fi
gh release upload "$RELEASE_TAG" dist/*.zip dist/checksums.txt dist/registry.json --clobber
if [[ "$RELEASE_TAG" == *-* ]]; then
  gh release edit "$RELEASE_TAG" --draft=false --prerelease
else
  gh release edit "$RELEASE_TAG" --draft=false --latest
fi
