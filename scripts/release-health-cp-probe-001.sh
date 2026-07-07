#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="${COORDPLANE_RELEASE_HEALTH_DIR:-${ROOT_DIR}/.coordplane-release-health}"
mkdir -p "${WORK_DIR}"
WORK_DIR="$(cd "${WORK_DIR}" && pwd)"
BIN_DIR="${WORK_DIR}/bin"
DB_PATH="${COORDPLANE_RELEASE_HEALTH_DB:-${WORK_DIR}/coordplane.db}"
ARTIFACT_DIR="${COORDPLANE_CP_PROBE_ARTIFACT_DIR:-${WORK_DIR}}"
PORT="${COORDPLANE_RELEASE_HEALTH_PORT:-18080}"
LISTEN="${COORDPLANE_RELEASE_HEALTH_LISTEN:-0.0.0.0:${PORT}}"
PUBLIC_URL="${COORDPLANE_RELEASE_HEALTH_PUBLIC_URL:-http://127.0.0.1:${PORT}}"
NETWORK="${COORDPLANE_RELEASE_HEALTH_NETWORK:-coordplane-release-health}"
IMAGE="${COORDPLANE_RELEASE_HEALTH_IMAGE:-coordplane/claude-runtime:release-health}"
TEAM_ID="${COORDPLANE_RELEASE_HEALTH_TEAM_ID:-cp-probe-001-manual-service}"
TEAM_VERSION="${COORDPLANE_RELEASE_HEALTH_TEAM_VERSION:-1}"
TEAMCONFIG="${COORDPLANE_RELEASE_HEALTH_TEAMCONFIG:-${ROOT_DIR}/team_config/fixtures/cp_probe_001_manual_service.yaml}"
DOCKER_TEAM_ID="${COORDPLANE_CP_PROBE_DOCKER_TEAM_ID:-cp-probe-001-docker-claude}"
DOCKER_TEAM_VERSION="${COORDPLANE_CP_PROBE_DOCKER_TEAM_VERSION:-1}"
DOCKER_TEAMCONFIG="${COORDPLANE_CP_PROBE_DOCKER_TEAMCONFIG:-${ROOT_DIR}/team_config/fixtures/cp_probe_001_docker_claude.yaml}"
CLAUDE_BIN="${COORDPLANE_RELEASE_HEALTH_CLAUDE_BIN:-/usr/local/bin/claude}"
CLAUDE_ENV_KEYS="${COORDPLANE_CLAUDE_ENV:-ANTHROPIC_BASE_URL,ANTHROPIC_AUTH_TOKEN,ANTHROPIC_MODEL,ANTHROPIC_DEFAULT_OPUS_MODEL,ANTHROPIC_DEFAULT_SONNET_MODEL,ANTHROPIC_DEFAULT_HAIKU_MODEL,CLAUDE_CODE_SUBAGENT_MODEL,CLAUDE_CODE_EFFORT_LEVEL}"

mkdir -p "${BIN_DIR}" "${ARTIFACT_DIR}"
export DOCKER_CONFIG="${DOCKER_CONFIG:-${WORK_DIR}/docker-config}"
mkdir -p "${DOCKER_CONFIG}"

require_command() {
  local name="$1"
  if ! command -v "${name}" >/dev/null 2>&1; then
    echo "release-health environment blocked: required command not found: ${name}" >&2
    return 1
  fi
}

blockers=()
# Formal Docker replay blockers are reported as CP-PROBE environment_blocked artifacts.
require_command go || blockers+=("required command not found: go")
require_command python3 || blockers+=("required command not found: python3")
if ! command -v docker >/dev/null 2>&1; then
  blockers+=("required command not found: docker")
fi

auth_env_present=false
IFS=',' read -r -a auth_keys <<< "${CLAUDE_ENV_KEYS}"
for key in "${auth_keys[@]}"; do
  key="$(echo "${key}" | xargs)"
  if [[ -n "${key}" && -n "${!key:-}" ]]; then
    auth_env_present=true
  fi
done
if [[ "${auth_env_present}" != "true" ]]; then
  blockers+=("at least one allowlisted Claude/Anthropic auth env value must be present for Docker/Claude replay")
fi

if ! command -v go >/dev/null 2>&1; then
  exit 2
fi

GOARCH_HOST="$(go env GOARCH)"
GOARCH_CONTAINER="${COORDPLANE_RELEASE_HEALTH_GOARCH:-${GOARCH_HOST}}"

go build -buildvcs=false -o "${BIN_DIR}/coordplane" ./cmd/coordplane
GOOS=linux GOARCH="${GOARCH_CONTAINER}" CGO_ENABLED=0 go build -buildvcs=false -o "${BIN_DIR}/coordlink" ./cmd/coordlink
chmod 0755 "${BIN_DIR}/coordlink"

DOCKER_BUILD_LOG="${WORK_DIR}/cp-probe-docker-build.log"
if command -v docker >/dev/null 2>&1; then
  if ! docker build \
    -t "${IMAGE}" \
    -f "${ROOT_DIR}/docker/claude-runtime/Dockerfile" \
    "${ROOT_DIR}" > "${DOCKER_BUILD_LOG}" 2>&1; then
    blockers+=("Docker image build failed; check Docker registry/npm access; see ${DOCKER_BUILD_LOG}")
  fi
fi

NETWORK_CREATED=false
BACKEND_URL_FOR_DOCKER="${PUBLIC_URL}"
if [[ "${#blockers[@]}" -eq 0 ]]; then
  if ! docker network inspect "${NETWORK}" >/dev/null 2>&1; then
    if ! docker network create "${NETWORK}" >/dev/null; then
      blockers+=("Docker network could not be created; check Docker daemon permissions")
    else
      NETWORK_CREATED=true
    fi
  fi
fi
if [[ "${#blockers[@]}" -eq 0 ]]; then
  if gateway="$(docker network inspect -f '{{(index .IPAM.Config 0).Gateway}}' "${NETWORK}")"; then
    BACKEND_URL_FOR_DOCKER="${COORDPLANE_BACKEND_URL_FOR_DOCKER:-http://${gateway}:${PORT}}"
  else
    blockers+=("Docker network gateway could not be inspected")
  fi
fi
BLOCKER_TEXT="$(IFS='; '; echo "${blockers[*]}")"

cleanup() {
  if command -v docker >/dev/null 2>&1; then
    mapfile -t containers < <(docker ps -aq \
      --filter "label=coordplane.managed=true" \
      --filter "label=coordplane.team_id=${DOCKER_TEAM_ID}" || true)
    if [[ "${#containers[@]}" -gt 0 ]]; then
      docker rm -f "${containers[@]}" >/dev/null || true
    fi
    if [[ "${NETWORK_CREATED}" == "true" ]]; then
      docker network rm "${NETWORK}" >/dev/null 2>&1 || true
    fi
  fi
}
trap cleanup EXIT

release_args=(
  release-health cp-probe-001
  --db "${DB_PATH}"
  --team-id "${TEAM_ID}"
  --team-version "${TEAM_VERSION}"
  --teamconfig "${TEAMCONFIG}"
  --docker-team-id "${DOCKER_TEAM_ID}"
  --docker-team-version "${DOCKER_TEAM_VERSION}"
  --docker-teamconfig "${DOCKER_TEAMCONFIG}"
  --listen "${LISTEN}"
  --backend-url "${BACKEND_URL_FOR_DOCKER}"
  --coordlink "${BIN_DIR}/coordlink"
  --docker-network "${NETWORK}"
  --claude-bin "${CLAUDE_BIN}"
  --claude-env "${CLAUDE_ENV_KEYS}"
  --workdir "${WORK_DIR}"
  --artifact-dir "${ARTIFACT_DIR}"
  --environment-blocker "${BLOCKER_TEXT}"
)

set +e
"${BIN_DIR}/coordplane" "${release_args[@]}" \
  > "${WORK_DIR}/cp_probe_001_result.json" \
  2> "${WORK_DIR}/cp_probe_001_backend.log"
release_status="$?"
set -e

artifact_files=(
  "${ARTIFACT_DIR}/cp_probe_001_manual_trace.md"
  "${ARTIFACT_DIR}/cp_probe_001_inspect_redacted.json"
  "${ARTIFACT_DIR}/cp_probe_001_git_operation_summary.json"
  "${ARTIFACT_DIR}/cp_probe_001_failure_matrix.md"
  "${ARTIFACT_DIR}/cp_probe_001_conclusion.md"
)
for artifact in "${artifact_files[@]}"; do
  if [[ ! -s "${artifact}" ]]; then
    echo "CP-PROBE artifact missing or empty: ${artifact}" >&2
    exit 1
  fi
done

for forbidden in \
  "coordplane.db" \
  "${WORK_DIR}/runtime" \
  "Bearer " \
  "sk-" \
  "ANTHROPIC_AUTH_TOKEN" \
  "ANTHROPIC_API_KEY" \
  "CLAUDE_AUTH_TOKEN" \
  "CLAUDE_CODE_OAUTH_TOKEN"; do
  if grep -F "${forbidden}" "${artifact_files[@]}" >/dev/null; then
    echo "CP-PROBE artifact leaked forbidden marker: ${forbidden}" >&2
    exit 1
  fi
done

status="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "${WORK_DIR}/cp_probe_001_result.json")"
echo "cp_probe_001.status=${status}"
echo "db=${DB_PATH}"
echo "artifact_dir=${ARTIFACT_DIR}"
printf 'artifacts=%s\n' "${artifact_files[*]}"

if [[ "${release_status}" -ne 0 ]]; then
  echo "CP-PROBE release-health did not pass; see ${WORK_DIR}/cp_probe_001_result.json and ${ARTIFACT_DIR}/cp_probe_001_conclusion.md" >&2
  if [[ -s "${WORK_DIR}/cp_probe_001_backend.log" ]]; then
    cat "${WORK_DIR}/cp_probe_001_backend.log" >&2
  fi
  exit "${release_status}"
fi

if [[ "${status}" != "passed" ]]; then
  echo "cp_probe_001.status=${status}; expected passed" >&2
  exit 1
fi
