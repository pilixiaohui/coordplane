#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image="coordplane-e2e-deterministic:$$"
docker_config=$(mktemp -d "${TMPDIR:-/tmp}/coordplane-e2e-docker.XXXXXX")
export DOCKER_CONFIG="$docker_config"

cleanup() {
	docker image rm -f "$image" >/dev/null 2>&1 || true
	rm -rf "$docker_config"
}
trap cleanup EXIT HUP INT TERM

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
make build
if ! docker build -q -t "$image" tests/e2e/testdata/runtime >/dev/null; then
	echo "FAIL(build deterministic runtime image)"
	exit 1
fi

if ! E2E_COORDPLANE_BIN="$root/build/bin/coordplane" \
	E2E_COORDLINK_BIN="$root/build/bin/coordlink" \
	E2E_RUNTIME_IMAGE="$image" \
	go test -tags=e2e ./tests/e2e -run '^(TestDeterministicTwoAgentConvergence|TestRealCLIGateRejectsMutableAndScriptedImagesBeforeLiveTests)$' -count=1 -v; then
	echo "FAIL(deterministic two-Agent E2E or real image admission negatives)"
	exit 1
fi

echo "PASS(deterministic two-Agent E2E and real image admission negatives)"
