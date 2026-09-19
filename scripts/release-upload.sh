#!/usr/bin/env bash
# The ONLY sanctioned way to put an .abook on a public release: the publish gate
# runs first and refuses on any red. Usage: scripts/release-upload.sh <tag> <file.abook>...
set -euo pipefail
TAG="${1:?release tag}"; shift
[ "$#" -ge 1 ] || { echo "usage: $0 <tag> <file.abook>..."; exit 2; }
cd "$(dirname "$0")/.."
python3 testing/provenance/publish_check.py abook "$@"
gh release upload "$TAG" "$@" --repo abookify/abookify-server --clobber
