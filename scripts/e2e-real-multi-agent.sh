#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=${E2E_RUNTIME_IMAGE:-}
network=${E2E_DOCKER_NETWORK:-}
provider_env=ANTHROPIC_AUTH_TOKEN,ANTHROPIC_BASE_URL,ANTHROPIC_MODEL,ANTHROPIC_DEFAULT_OPUS_MODEL,ANTHROPIC_DEFAULT_SONNET_MODEL,ANTHROPIC_DEFAULT_HAIKU_MODEL,CLAUDE_CODE_SUBAGENT_MODEL,CLAUDE_CODE_EFFORT_LEVEL
expected_claude_version='2.1.126 (Claude Code)'

invalid() { echo "INVALID_ENVIRONMENT($1)"; exit 77; }
fail() { class=product; [ ! -s "$failure_class" ] || class=$(cat "$failure_class"); case "$class" in product|provider_environment|task_spec) ;; *) class=product;; esac; echo "failure_class=$class"; echo "FAIL_REAL_MULTI_AGENT"; exit 1; }

temporary_root=${TMPDIR:-/tmp}; case "$temporary_root" in /*) ;; *) invalid "TMPDIR must be absolute" ;; esac
temporary_root=$(CDPATH= cd -- "$temporary_root" 2>/dev/null && pwd -P) || invalid "TMPDIR is unavailable"
requested_report=${RMA02_OUTPUT:-}; report=
if [ -n "$requested_report" ]; then
	case "$requested_report" in /*) ;; *) invalid "RMA02_OUTPUT must be absolute" ;; esac
	report_name=${requested_report##*/}; report_parent=${requested_report%/*}; [ -n "$report_parent" ] || report_parent=/
	case "$report_name" in ''|.|..) invalid "RMA02_OUTPUT filename is invalid" ;; esac
	report_parent=$(CDPATH= cd -- "$report_parent" 2>/dev/null && pwd -P) || invalid "RMA02_OUTPUT parent is unavailable"
	[ "$report_parent" = "$temporary_root" ] || invalid "RMA02_OUTPUT must be directly inside canonical TMPDIR"
	report=$report_parent/$report_name; [ ! -e "$report" ] && [ ! -L "$report" ] || invalid "RMA02_OUTPUT already exists"
fi

case "$image" in sha256:*) digest=${image#sha256:} ;; *) invalid "explicit immutable sha256 image is required" ;; esac
[ "${#digest}" -eq 64 ] || invalid "explicit immutable sha256 image is required"
case "$digest" in *[!0-9a-f]*) invalid "explicit immutable sha256 image is required" ;; esac

run_root=; published_directory=; published=0
cleanup() { [ -z "$run_root" ] || rm -rf "$run_root"; [ -z "$published_directory" ] || [ "$published" -eq 1 ] || rm -rf "$published_directory"; }
trap cleanup EXIT HUP INT TERM
run_root=$(mktemp -d "$temporary_root/coordplane-rma02-run.XXXXXX")
docker_config=$run_root/docker; results=$run_root/results.json; failure_class=$run_root/failure-class; staged_report=$run_root/evidence.json
mkdir "$docker_config"; : >"$failure_class"; export DOCKER_CONFIG="$docker_config"

for command in go git docker make printenv; do command -v "$command" >/dev/null 2>&1 || invalid "$command is unavailable"; done
docker version >/dev/null 2>&1 || invalid "Docker daemon is unavailable"
actual_image=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true); [ "$actual_image" = "$image" ] || invalid "runtime image identity does not match requested digest"
claude_version=$(docker run --rm --network none --read-only --entrypoint claude "$image" --version 2>/dev/null || true); [ "$claude_version" = "$expected_claude_version" ] || invalid "runtime image must contain Claude Code 2.1.126"
[ -n "${ANTHROPIC_AUTH_TOKEN:-}" ] || invalid "ANTHROPIC_AUTH_TOKEN is unavailable"

if [ -z "$network" ]; then network=bridge; for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY; do value=$(printenv "$name" 2>/dev/null || true); case "$value" in *://127.0.0.1:*|*://localhost:*) network=host ;; esac; done; fi

cd "$root"
[ -z "$(git status --porcelain --untracked-files=normal)" ] || invalid "candidate worktree is not clean"
make build || fail
E2E_REAL_MULTI_AGENT=1 \
	E2E_COORDPLANE_BIN="$root/build/bin/coordplane" \
	E2E_COORDLINK_BIN="$root/build/bin/coordlink" \
	E2E_RUNTIME_IMAGE="$image" \
	E2E_CLAUDE_VERSION="$claude_version" \
	E2E_DOCKER_NETWORK="$network" \
	E2E_PROVIDER_ENV_ALLOWLIST="$provider_env" \
	E2E_SOURCE_CLEAN=1 \
	E2E_RMA02_REPORT="$staged_report" E2E_RMA02_FAILURE_CLASS_FILE="$failure_class" \
	go test -tags=e2e ./tests/e2e -run '^TestRealMultiAgentScenarios$' -count=1 -json -timeout 30m >"$results" 2>&1 || fail
go run ./tests/e2e/e2emultiresult <"$results" || fail
[ -s "$staged_report" ] || fail
if [ -z "$report" ]; then published_directory=$(mktemp -d "$temporary_root/coordplane-rma02-evidence.XXXXXX"); report=$published_directory/evidence.json; fi
ln "$staged_report" "$report" 2>/dev/null || fail
published=1
echo "RMA02_EVIDENCE=$report"
echo "PASS_REAL_MULTI_AGENT_LOCAL"
