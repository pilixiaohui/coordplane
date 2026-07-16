#!/bin/sh
set -eu

profile=
output=
image=${PF01_RUNTIME_IMAGE_DIGEST:-}
while [ "$#" -gt 0 ]; do
	case "$1" in
	--profile) shift; profile=${1:-} ;;
	--output) shift; output=${1:-} ;;
	--image-digest) shift; image=${1:-} ;;
	*) echo "perf-v1: unknown argument: $1" >&2; exit 2 ;;
	esac
	shift
done
case "$profile" in smoke|release) ;; *) echo "perf-v1: --profile must be smoke or release" >&2; exit 2 ;; esac
[ -n "$output" ] || { echo "perf-v1: --output is required" >&2; exit 2; }
case "$image" in sha256:????????????????????????????????????????????????????????????????) ;; *) echo "perf-v1: --image-digest must be a prebuilt immutable sha256 identity" >&2; exit 2 ;; esac

case "$output" in
	/*) ;;
	*) output=$PWD/$output ;;
esac

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
docker_config=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-pf01-docker.XXXXXX")
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-pf01-build.XXXXXX")
cleanup() { rm -rf "$docker_config" "$build_dir"; }
trap cleanup EXIT HUP INT TERM
export DOCKER_CONFIG=$docker_config
for command in go git docker perl; do command -v "$command" >/dev/null 2>&1 || { echo "INVALID_ENVIRONMENT($command is unavailable)"; exit 77; }; done
docker version >/dev/null 2>&1 || { echo "INVALID_ENVIRONMENT(Docker daemon is unavailable)"; exit 77; }
actual_image=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true)
[ "$actual_image" = "$image" ] || { echo "INVALID_ENVIRONMENT(prebuilt PF-01 image digest is unavailable or mutable)"; exit 77; }

cd "$root"
source_clean=false
if [ -z "$(git status --porcelain --untracked-files=normal -- . ':(exclude)build')" ]; then
	source_clean=true
fi
if [ "$profile" = release ] && [ "$source_clean" != true ]; then
	revision=$(git rev-parse --verify HEAD 2>/dev/null || printf unknown)
	printf '{\n  "schema_version": 2,\n  "scenario": "PF-01",\n  "profile": "release",\n  "result": "INVALID_ENVIRONMENT",\n  "reason": "release source tree must be clean before build",\n  "revision": "%s",\n  "binary_sha256": null\n}\n' "$revision" > "$output"
	echo "INVALID_ENVIRONMENT(PF-01 release source tree is dirty)"
	exit 77
fi
build_dirty=true
[ "$source_clean" = true ] && build_dirty=false
GOFLAGS=-tags=perf,contract make BUILD_DIR="$build_dir" BUILD_DIRTY=$build_dirty build
test_timeout=25m
if [ "$profile" = release ]; then
	test_timeout=120m
fi
PF01_PROFILE=$profile PF01_OUTPUT=$output PF01_SOURCE_CLEAN=$source_clean PF01_IMAGE_DIGEST=$image E2E_COORDPLANE_BIN=$build_dir/bin/coordplane E2E_COORDLINK_BIN=$build_dir/bin/coordlink E2E_GIT_HELPER_BIN=$build_dir/bin/coordplane-git-helper E2E_RUNTIME_IMAGE=$image \
	go test -buildvcs=false -tags=e2e ./tests/e2e -run '^TestPF01FourAgentPerformance$' -count=1 -timeout "$test_timeout"
result=$(perl -MJSON::PP -0777 -e '$r=decode_json(<>); print $r->{result}' "$output")
case "$result" in PASS) echo "PASS(PF-01 $profile)" ;; INVALID_ENVIRONMENT) echo "INVALID_ENVIRONMENT(PF-01 $profile; see $output)"; exit 77 ;; BASELINE_BOOTSTRAP) echo "BASELINE_BOOTSTRAP(PF-01 release; owner approval required)"; exit 78 ;; *) echo "FAIL(PF-01 $profile; see $output)"; exit 1 ;; esac
