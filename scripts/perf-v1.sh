#!/bin/sh
set -eu

profile=
output=
while [ "$#" -gt 0 ]; do
	case "$1" in
	--profile) shift; profile=${1:-} ;;
	--output) shift; output=${1:-} ;;
	*) echo "perf-v1: unknown argument: $1" >&2; exit 2 ;;
	esac
	shift
done
case "$profile" in smoke|release) ;; *) echo "perf-v1: --profile must be smoke or release" >&2; exit 2 ;; esac
[ -n "$output" ] || { echo "perf-v1: --output is required" >&2; exit 2; }

case "$output" in
	/*) ;;
	*) output=$PWD/$output ;;
esac

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image="coordplane-pf01:$$"
docker_config=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-pf01-docker.XXXXXX")
cleanup() { docker image rm -f "$image" >/dev/null 2>&1 || true; rm -rf "$docker_config"; }
trap cleanup EXIT HUP INT TERM
export DOCKER_CONFIG=$docker_config
for command in go git docker perl; do command -v "$command" >/dev/null 2>&1 || { echo "INVALID_ENVIRONMENT($command is unavailable)"; exit 77; }; done
docker version >/dev/null 2>&1 || { echo "INVALID_ENVIRONMENT(Docker daemon is unavailable)"; exit 77; }

cd "$root"
make build
docker build -q -t "$image" tests/e2e/testdata/runtime >/dev/null
go test -race -buildvcs=false -tags=contract ./internal/daemon -run '^TestGT03SQLiteTaskRunAndRealGitCaptureRecoverAcrossProcessSIGKILL$' -count=3
test_timeout=25m
if [ "$profile" = release ]; then
	test_timeout=120m
fi
PF01_PROFILE=$profile PF01_OUTPUT=$output E2E_COORDPLANE_BIN=$root/build/bin/coordplane E2E_COORDLINK_BIN=$root/build/bin/coordlink E2E_RUNTIME_IMAGE=$image \
	go test -buildvcs=false -tags=e2e ./tests/e2e -run '^TestPF01FourAgentPerformance$' -count=1 -timeout "$test_timeout"
result=$(perl -MJSON::PP -0777 -e '$r=decode_json(<>); print $r->{result}' "$output")
case "$result" in PASS) echo "PASS(PF-01 $profile)" ;; INVALID_ENVIRONMENT) echo "INVALID_ENVIRONMENT(PF-01 $profile; see $output)"; exit 77 ;; *) echo "FAIL(PF-01 $profile; see $output)"; exit 1 ;; esac
