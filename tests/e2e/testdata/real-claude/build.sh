#!/bin/sh
# Builds the immutable real-Claude runtime image and prints its sha256 digest
# for scripts/e2e-real-cli.sh. Requires Docker with network access.
set -eu
expected='2.1.126 (Claude Code)'
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
image="coordplane-real-claude:$$"
trap 'docker image rm -f "$image" >/dev/null 2>&1 || true' EXIT
docker build -q -t "$image" "$root/tests/e2e/testdata/real-claude"
version=$(docker run --rm --read-only --entrypoint claude "$image" --version)
[ "$version" = "$expected" ] || { echo "SKIP(claude version $version, want $expected)" >&2; exit 77; }
digest=$(docker image inspect --format '{{.Id}}' "$image")
echo "$digest"
