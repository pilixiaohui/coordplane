#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="${COORDPLANE_RELEASE_HEALTH_DIR:-${ROOT_DIR}/.coordplane-release-health}"
BIN_DIR="${WORK_DIR}/bin"
DB_PATH="${COORDPLANE_RELEASE_HEALTH_DB:-${WORK_DIR}/coordplane.db}"
PORT="${COORDPLANE_RELEASE_HEALTH_PORT:-18080}"
LISTEN="${COORDPLANE_RELEASE_HEALTH_LISTEN:-0.0.0.0:${PORT}}"
PUBLIC_URL="${COORDPLANE_RELEASE_HEALTH_PUBLIC_URL:-http://127.0.0.1:${PORT}}"
NETWORK="${COORDPLANE_RELEASE_HEALTH_NETWORK:-coordplane-release-health}"
IMAGE="${COORDPLANE_RELEASE_HEALTH_IMAGE:-coordplane/claude-runtime:release-health}"
TEAM_ID="${COORDPLANE_RELEASE_HEALTH_TEAM_ID:-cp-accept-001-three-agent-docker-claude}"
TEAM_VERSION="${COORDPLANE_RELEASE_HEALTH_TEAM_VERSION:-1}"
ROOT_CONTRACT_ID="${COORDPLANE_ROOT_CONTRACT_ID:-}"
TEAMCONFIG="${COORDPLANE_RELEASE_HEALTH_TEAMCONFIG:-${ROOT_DIR}/team_config/fixtures/cp_accept_001_three_agent_docker_claude.yaml}"
CLAUDE_BIN="${COORDPLANE_RELEASE_HEALTH_CLAUDE_BIN:-/usr/local/bin/claude}"
CLAUDE_ENV_KEYS="${COORDPLANE_CLAUDE_ENV:-ANTHROPIC_BASE_URL,ANTHROPIC_AUTH_TOKEN,ANTHROPIC_MODEL,ANTHROPIC_DEFAULT_OPUS_MODEL,ANTHROPIC_DEFAULT_SONNET_MODEL,ANTHROPIC_DEFAULT_HAIKU_MODEL,CLAUDE_CODE_SUBAGENT_MODEL,CLAUDE_CODE_EFFORT_LEVEL}"

mkdir -p "${BIN_DIR}"

require_command() {
  local name="$1"
  if ! command -v "${name}" >/dev/null 2>&1; then
    echo "release-health environment blocked: required command not found: ${name}" >&2
    exit 2
  fi
}

environment_blocked() {
  echo "release-health environment blocked: $*" >&2
  exit 2
}

require_command go
require_command docker
require_command python3

auth_env_present=false
IFS=',' read -r -a auth_keys <<< "${CLAUDE_ENV_KEYS}"
for key in "${auth_keys[@]}"; do
  key="$(echo "${key}" | xargs)"
  if [[ -n "${key}" && -n "${!key:-}" ]]; then
    auth_env_present=true
  fi
done
if [[ "${auth_env_present}" != "true" ]]; then
  environment_blocked "at least one allowlisted Claude/Anthropic auth env value must be present for the real Docker/Claude gate"
fi

GOARCH_HOST="$(go env GOARCH)"
GOARCH_CONTAINER="${COORDPLANE_RELEASE_HEALTH_GOARCH:-${GOARCH_HOST}}"

go build -buildvcs=false -o "${BIN_DIR}/coordplane" ./cmd/coordplane
GOOS=linux GOARCH="${GOARCH_CONTAINER}" CGO_ENABLED=0 go build -buildvcs=false -o "${BIN_DIR}/coordlink" ./cmd/coordlink
chmod 0755 "${BIN_DIR}/coordlink"

DOCKER_BUILD_LOG="${WORK_DIR}/docker-build.log"
if ! docker build \
  -t "${IMAGE}" \
  -f "${ROOT_DIR}/docker/claude-runtime/Dockerfile" \
  "${ROOT_DIR}" > "${DOCKER_BUILD_LOG}" 2>&1; then
  echo "release-health environment blocked: Docker image build failed; check Docker registry/npm access for node base image and Claude Code npm package; see ${DOCKER_BUILD_LOG}" >&2
  if [[ -s "${DOCKER_BUILD_LOG}" ]]; then
    cat "${DOCKER_BUILD_LOG}" >&2
  fi
  exit 2
fi

NETWORK_CREATED=false
if ! docker network inspect "${NETWORK}" >/dev/null 2>&1; then
  if ! docker network create "${NETWORK}" >/dev/null; then
    environment_blocked "Docker network ${NETWORK} could not be created; check Docker daemon permissions"
  fi
  NETWORK_CREATED=true
fi

if ! gateway="$(docker network inspect -f '{{(index .IPAM.Config 0).Gateway}}' "${NETWORK}")"; then
  environment_blocked "Docker network ${NETWORK} gateway could not be inspected"
fi
BACKEND_URL_FOR_DOCKER="${COORDPLANE_BACKEND_URL_FOR_DOCKER:-http://${gateway}:${PORT}}"

cleanup() {
  mapfile -t containers < <(docker ps -aq \
    --filter "label=coordplane.managed=true" \
    --filter "label=coordplane.team_id=${TEAM_ID}" || true)
  if [[ "${#containers[@]}" -gt 0 ]]; then
    docker rm -f "${containers[@]}" >/dev/null || true
  fi
  if [[ "${NETWORK_CREATED}" == "true" ]]; then
    docker network rm "${NETWORK}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

release_args=(
  release-health cp-accept-001
  --db "${DB_PATH}" \
  --team-id "${TEAM_ID}" \
  --team-version "${TEAM_VERSION}" \
  --teamconfig "${TEAMCONFIG}" \
  --listen "${LISTEN}" \
  --backend-url "${BACKEND_URL_FOR_DOCKER}" \
  --coordlink "${BIN_DIR}/coordlink" \
  --docker-network "${NETWORK}" \
  --claude-bin "${CLAUDE_BIN}" \
  --claude-env "${CLAUDE_ENV_KEYS}" \
  --workdir "${WORK_DIR}" \
  --inspect-out "${WORK_DIR}/inspect.json" \
  --run-label "cp-accept-001-release-health" \
)

if [[ -n "${ROOT_CONTRACT_ID}" ]]; then
  release_args+=(--root-contract "${ROOT_CONTRACT_ID}")
fi

set +e
"${BIN_DIR}/coordplane" "${release_args[@]}" \
  > "${WORK_DIR}/release_acceptance.json" \
  2> "${WORK_DIR}/backend.log"
release_status="$?"
set -e

evidence_files=()
if [[ -f "${WORK_DIR}/inspect.json" ]]; then
  evidence_files+=("${WORK_DIR}/inspect.json")
fi
if [[ -f "${WORK_DIR}/release_acceptance.json" ]]; then
  evidence_files+=("${WORK_DIR}/release_acceptance.json")
fi

for forbidden in \
  "ANTHROPIC_AUTH_TOKEN=" \
  "CLAUDE_AUTH_TOKEN=" \
  "CLAUDE_CODE_OAUTH_TOKEN=" \
  "sk-ant-" \
  "Bearer " \
  "/var/run/docker.sock" \
  "/run/docker.sock" \
  "/.claude" \
  "/.config" \
  "/.ssh"; do
  if [[ "${#evidence_files[@]}" -gt 0 ]] && grep -F "${forbidden}" "${evidence_files[@]}" >/dev/null; then
    echo "release-health evidence leaked forbidden marker: ${forbidden}" >&2
    exit 1
  fi
done

if [[ "${release_status}" -ne 0 ]]; then
  echo "release-health command failed; see ${WORK_DIR}/backend.log and ${WORK_DIR}/release_acceptance.json" >&2
  if [[ -s "${WORK_DIR}/backend.log" ]]; then
    cat "${WORK_DIR}/backend.log" >&2
  fi
  exit "${release_status}"
fi

status="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "${WORK_DIR}/release_acceptance.json")"
if [[ "${status}" != "passed" ]]; then
  echo "release_acceptance.status=${status}; expected passed" >&2
  exit 1
fi

echo "release-health passed"
echo "db=${DB_PATH}"
echo "inspect=${WORK_DIR}/inspect.json"
echo "release_acceptance=${WORK_DIR}/release_acceptance.json"
