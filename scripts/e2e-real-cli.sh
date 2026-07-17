#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=${E2E_RUNTIME_IMAGE:-orchestrator-v4-agent:latest}
network=${E2E_DOCKER_NETWORK:-}
docker_config=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-live-docker.XXXXXX")
provider_env=ANTHROPIC_API_KEY
export DOCKER_CONFIG="$docker_config"

cleanup() {
	rm -rf "$docker_config"
}
trap cleanup EXIT HUP INT TERM

skip() {
	echo "SKIP($1)"
	exit 77
}

for command in go git docker printenv; do
	command -v "$command" >/dev/null 2>&1 || skip "$command is unavailable"
done
docker version >/dev/null 2>&1 || skip "Docker daemon is unavailable"
docker image inspect "$image" >/dev/null 2>&1 || skip "runtime image $image is unavailable"
[ -n "${ANTHROPIC_API_KEY:-}" ] || skip "ANTHROPIC_API_KEY is unavailable"

if [ -z "$network" ]; then
	network=bridge
	for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY; do
		value=$(printenv "$name" 2>/dev/null || true)
		case "$value" in
		*://127.0.0.1:*|*://localhost:*) network=host ;;
		esac
	done
fi

docker run --rm --network none --read-only --entrypoint claude "$image" --version
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
	E2E_DOCKER_NETWORK="$network" \
	E2E_PROVIDER_ENV_ALLOWLIST="$provider_env" \
	go test -tags=e2e ./tests/e2e \
		-run '^(TestRealClaudeAdapterSmoke|TestRealClaudeTwoAgentConvergence)$' -count=1 -v -timeout 20m; then
	echo "FAIL(real Claude live E2E)"
	exit 1
fi

echo "PASS(real Claude adapter smoke and two-Agent E2E)"
