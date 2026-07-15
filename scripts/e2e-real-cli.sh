#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=${E2E_RUNTIME_IMAGE:-orchestrator-v4-agent:latest}
network=${E2E_DOCKER_NETWORK:-}
runtime_gid=${E2E_RUNTIME_GID:-$(id -g)}
docker_config=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-live-docker.XXXXXX")
preflight_log=$(mktemp "${TMPDIR:-/tmp}/coordplane-live-provider.XXXXXX")
auth_home=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-live-auth.XXXXXX")
provider_env=
auth_file=
image_ready=0
export DOCKER_CONFIG="$docker_config"

cleanup() {
	if [ "$image_ready" -eq 1 ] && [ -d "$auth_home" ] &&
		[ -n "$(find "$auth_home" -mindepth 1 -print -quit 2>/dev/null)" ]; then
		docker run --rm --network none --user 0:0 -v "$auth_home:/cleanup" \
			--entrypoint sh "$image" -c 'find /cleanup -mindepth 1 -delete' >/dev/null 2>&1 || true
	fi
	rm -rf "$docker_config" "$auth_home"
	rm -f "$preflight_log"
}
trap cleanup EXIT HUP INT TERM

skip() {
	echo "SKIP($1)"
	exit 77
}

for command in go git docker install printenv timeout; do
	command -v "$command" >/dev/null 2>&1 || skip "$command is unavailable"
done
docker version >/dev/null 2>&1 || skip "Docker daemon is unavailable"
docker image inspect "$image" >/dev/null 2>&1 || skip "runtime image $image is unavailable"
image_ready=1

if [ -n "${E2E_CODEX_AUTH_FILE:-}" ]; then
	[ -f "$E2E_CODEX_AUTH_FILE" ] || skip "E2E_CODEX_AUTH_FILE is unavailable"
	auth_file=$E2E_CODEX_AUTH_FILE
	install -m 0640 "$auth_file" "$auth_home/auth.json"
	chmod 0770 "$auth_home"
elif [ -n "${OPENAI_API_KEY:-}" ]; then
	provider_env=OPENAI_API_KEY
else
	skip "real Codex credential is unavailable"
fi

if [ -z "$network" ]; then
	network=bridge
	for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY; do
		value=$(printenv "$name" 2>/dev/null || true)
		case "$value" in
		*://127.0.0.1:*|*://localhost:*) network=host ;;
		esac
	done
fi

set -- timeout 90s docker run --rm --network "$network" --user "65532:$runtime_gid" --read-only \
	--tmpfs "/tmp:rw,uid=65532,gid=$runtime_gid,mode=1770" \
	-e HOME=/home/agent -e CODEX_HOME=/home/agent --workdir /tmp --entrypoint codex
if [ -n "$auth_file" ]; then
	set -- "$@" -v "$auth_home:/home/agent"
else
	set -- "$@" --tmpfs "/home/agent:rw,uid=65532,gid=$runtime_gid,mode=0770" -e OPENAI_API_KEY
fi
for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY; do
	if value=$(printenv "$name" 2>/dev/null) && [ -n "$value" ]; then
		set -- "$@" -e "$name"
		if [ -n "$provider_env" ]; then
			provider_env="$provider_env,$name"
		else
			provider_env=$name
		fi
	fi
done
set -- "$@" "$image" exec --json --color never --dangerously-bypass-approvals-and-sandbox \
	--skip-git-repo-check -- "Reply with exactly LIVE_PROVIDER_OK and do nothing else."

if "$@" >"$preflight_log" 2>&1; then
	if ! grep -F '"type":"turn.completed"' "$preflight_log" >/dev/null 2>&1 ||
		! grep -F 'LIVE_PROVIDER_OK' "$preflight_log" >/dev/null 2>&1; then
		echo "FAIL(real Codex provider preflight returned no completed turn)"
		exit 1
	fi
else
	if grep -E 'invalid_api_key|Incorrect API key|401 Unauthorized' "$preflight_log" >/dev/null 2>&1; then
		skip "real Codex credential was rejected by the provider"
	fi
	if grep -E 'Network unreachable|connection refused|failed to connect|timed out' "$preflight_log" >/dev/null 2>&1; then
		skip "real Codex provider network is unavailable"
	fi
	echo "FAIL(real Codex provider preflight failed)"
	exit 1
fi

cd "$root"
make build
if ! E2E_REAL_CLI=1 \
	E2E_COORDPLANE_BIN="$root/build/bin/coordplane" \
	E2E_COORDLINK_BIN="$root/build/bin/coordlink" \
	E2E_RUNTIME_IMAGE="$image" \
	E2E_DOCKER_NETWORK="$network" \
	E2E_PROVIDER_ENV_ALLOWLIST="$provider_env" \
	E2E_CODEX_AUTH_FILE="$auth_file" \
	go test -tags=e2e ./tests/e2e \
		-run '^(TestRealCodexAdapterSmoke|TestRealCodexTwoAgentConvergence)$' -count=1 -v -timeout 20m; then
	echo "FAIL(real Codex live E2E)"
	exit 1
fi

echo "PASS(real Codex adapter smoke and two-Agent E2E)"
