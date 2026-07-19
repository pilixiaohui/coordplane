#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image="coordplane-e2e-deterministic:$$"
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-e2e.XXXXXX")
build_dir="$temporary_root/build"
docker_config="$temporary_root/docker"

cleanup() {
	docker image rm -f "$image" >/dev/null 2>&1 || true
	rm -rf "$temporary_root"
}
trap cleanup EXIT HUP INT TERM
mkdir -p "$docker_config"
export DOCKER_CONFIG="$docker_config"

for command in go git docker; do
	if ! command -v "$command" >/dev/null 2>&1; then
		echo "SKIP($command is unavailable)"
		exit 77
	fi
done

if ! docker version >/dev/null 2>&1; then
	echo "SKIP(Docker daemon is unavailable)"
	exit 77
fi

cd "$root"
make BUILD_DIR="$build_dir" build
if ! docker build -q -t "$image" tests/e2e/testdata/runtime >/dev/null; then
	echo "FAIL(build deterministic runtime image)"
	exit 1
fi

if ! go run ./scripts/locguard --live-integration tests/e2e/real_cli_test.go >/dev/null ||
	! E2E_COORDPLANE_BIN="$build_dir/bin/coordplane" \
	E2E_COORDLINK_BIN="$build_dir/bin/coordlink" \
	E2E_RUNTIME_IMAGE="$image" \
	go test -tags=e2e ./tests/e2e -run '^(TestDeterministicTwoAgentConvergence|TestRealCLIGateRejectsMutableAndScriptedImagesBeforeLiveTests|TestRealCLIGatePreservesFailureDiagnosticsBeforeCleanupWithoutProvider)$' -count=1 -v; then
	echo "FAIL(deterministic two-Agent E2E, real image admission negatives, or live failure diagnostics)"
	exit 1
fi

echo "PASS(deterministic two-Agent E2E, real image admission negatives, and live failure diagnostics)"
