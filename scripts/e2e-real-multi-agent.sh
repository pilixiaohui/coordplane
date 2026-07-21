#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=${E2E_RUNTIME_IMAGE:-}
network=${E2E_DOCKER_NETWORK:-}
provider_env=ANTHROPIC_AUTH_TOKEN,ANTHROPIC_BASE_URL,ANTHROPIC_MODEL,ANTHROPIC_DEFAULT_OPUS_MODEL,ANTHROPIC_DEFAULT_SONNET_MODEL,ANTHROPIC_DEFAULT_HAIKU_MODEL,CLAUDE_CODE_SUBAGENT_MODEL,CLAUDE_CODE_EFFORT_LEVEL
expected_claude_version='2.1.126 (Claude Code)'

invalid() {
	echo "INVALID_ENVIRONMENT($1)"
	exit 77
}

case "$image" in
sha256:*) digest=${image#sha256:} ;;
*) invalid "explicit immutable sha256 image is required" ;;
esac
[ "${#digest}" -eq 64 ] || invalid "explicit immutable sha256 image is required"
case "$digest" in *[!0-9a-f]*) invalid "explicit immutable sha256 image is required" ;; esac

docker_config=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-rma02-docker.XXXXXX")
results=$(mktemp "${TMPDIR:-/tmp}/coordplane-rma02-results.XXXXXX")
report=${RMA02_OUTPUT:-}
if [ -z "$report" ]; then
	report=$(mktemp "${TMPDIR:-/tmp}/coordplane-rma02-evidence.XXXXXX.json")
fi
case "$report" in /*) ;; *) invalid "RMA02_OUTPUT must be absolute" ;; esac
cleanup() { rm -rf "$docker_config" "$results"; }
trap cleanup EXIT HUP INT TERM
export DOCKER_CONFIG="$docker_config"

for command in go git docker make printenv; do
	command -v "$command" >/dev/null 2>&1 || invalid "$command is unavailable"
done
docker version >/dev/null 2>&1 || invalid "Docker daemon is unavailable"
actual_image=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true)
[ "$actual_image" = "$image" ] || invalid "runtime image identity does not match requested digest"
claude_version=$(docker run --rm --network none --read-only --entrypoint claude "$image" --version 2>/dev/null || true)
[ "$claude_version" = "$expected_claude_version" ] || invalid "runtime image must contain Claude Code 2.1.126"
[ -n "${ANTHROPIC_AUTH_TOKEN:-}" ] || invalid "ANTHROPIC_AUTH_TOKEN is unavailable"

if [ -z "$network" ]; then
	network=bridge
	for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY; do
		value=$(printenv "$name" 2>/dev/null || true)
		case "$value" in *://127.0.0.1:*|*://localhost:*) network=host ;; esac
	done
fi

cd "$root"
[ -z "$(git status --porcelain --untracked-files=normal)" ] || invalid "candidate worktree is not clean"
make build
if ! E2E_REAL_MULTI_AGENT=1 \
	E2E_COORDPLANE_BIN="$root/build/bin/coordplane" \
	E2E_COORDLINK_BIN="$root/build/bin/coordlink" \
	E2E_RUNTIME_IMAGE="$image" \
	E2E_CLAUDE_VERSION="$claude_version" \
	E2E_DOCKER_NETWORK="$network" \
	E2E_PROVIDER_ENV_ALLOWLIST="$provider_env" \
	E2E_SOURCE_CLEAN=1 \
	E2E_RMA02_REPORT="$report" \
	go test -tags=e2e ./tests/e2e -run '^TestRealMultiAgentScenarios$' -count=1 -json -timeout 30m >"$results"; then
	cat "$results"
	echo "FAIL_REAL_MULTI_AGENT"
	exit 1
fi
cat "$results"
if ! go run ./tests/e2e/e2emultiresult <"$results"; then
	echo "FAIL_REAL_MULTI_AGENT"
	exit 1
fi
[ -s "$report" ] || { echo "FAIL_REAL_MULTI_AGENT"; exit 1; }
echo "RMA02_EVIDENCE=$report"
echo "PASS_REAL_MULTI_AGENT_LOCAL"
