#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=${E2E_RUNTIME_IMAGE:-}
network=${E2E_DOCKER_NETWORK:-}
provider_env=ANTHROPIC_AUTH_TOKEN,ANTHROPIC_BASE_URL,ANTHROPIC_MODEL,ANTHROPIC_DEFAULT_OPUS_MODEL,ANTHROPIC_DEFAULT_SONNET_MODEL,ANTHROPIC_DEFAULT_HAIKU_MODEL,CLAUDE_CODE_SUBAGENT_MODEL,CLAUDE_CODE_EFFORT_LEVEL
expected_claude_version='2.1.126 (Claude Code)'

skip() {
	echo "SKIP($1)"
	exit 77
}

case "$image" in
sha256:*) digest=${image#sha256:} ;;
*) skip "explicit immutable sha256 image is required" ;;
esac
[ "${#digest}" -eq 64 ] || skip "explicit immutable sha256 image is required"
case "$digest" in *[!0-9a-f]*) skip "explicit immutable sha256 image is required" ;; esac

docker_config=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-live-docker.XXXXXX")
results=$(mktemp "${TMPDIR:-/tmp}/coordplane-live-results.XXXXXX")
cleanup() { rm -rf "$docker_config" "$results"; }
trap cleanup EXIT HUP INT TERM
export DOCKER_CONFIG="$docker_config"

for command in go git docker printenv; do
	command -v "$command" >/dev/null 2>&1 || skip "$command is unavailable"
done
docker version >/dev/null 2>&1 || skip "Docker daemon is unavailable"
actual_image=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true)
[ "$actual_image" = "$image" ] || skip "runtime image identity does not match requested digest"
claude_version=$(docker run --rm --network none --read-only --entrypoint claude "$image" --version 2>/dev/null || true)
[ "$claude_version" = "$expected_claude_version" ] || skip "runtime image must contain Claude Code 2.1.126"
echo "REAL_CLAUDE_IMAGE=$image"
echo "REAL_CLAUDE_VERSION=$claude_version"
[ -n "${ANTHROPIC_AUTH_TOKEN:-}" ] || skip "ANTHROPIC_AUTH_TOKEN is unavailable"

if [ -z "$network" ]; then
	network=bridge
	for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY; do
		value=$(printenv "$name" 2>/dev/null || true)
		case "$value" in
		*://127.0.0.1:*|*://localhost:*) network=host ;;
		esac
	done
fi

for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY; do
	if value=$(printenv "$name" 2>/dev/null) && [ -n "$value" ]; then
		provider_env="$provider_env,$name"
	fi
done

cd "$root"
make build
if ! E2E_REAL_CLI=1 \
	E2E_COORDPLANE_BIN="$root/build/bin/coordplane" \
	E2E_COORDLINK_BIN="$root/build/bin/coordlink" \
	E2E_RUNTIME_IMAGE="$image" \
	E2E_CLAUDE_VERSION="$claude_version" \
	E2E_DOCKER_NETWORK="$network" \
	E2E_PROVIDER_ENV_ALLOWLIST="$provider_env" \
	go test -tags=e2e ./tests/e2e \
		-run '^(TestRealClaudeAdapterSmoke|TestRealClaudeTwoAgentConvergence)$' -count=1 -json -timeout 20m >"$results"; then
	cat "$results"
	echo "FAIL(real Claude live E2E)"
	exit 1
fi
cat "$results"
if ! go run ./tests/e2e/e2eresult <"$results"; then
	echo "FAIL(real Claude live E2E evidence)"
	exit 1
fi

echo "PASS(real Claude adapter smoke and two-Agent E2E)"
