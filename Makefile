COORDPLANE_RELEASE_HEALTH_DIR ?= .coordplane-release-health
COORDPLANE_RELEASE_HEALTH_IMAGE ?= coordplane/claude-runtime:release-health
BUILD_VERSION ?= devel
BUILD_COMMIT ?= $(shell git rev-parse --verify HEAD 2>/dev/null || printf unknown)
BUILD_DIRTY ?= $(shell if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then if test -n "$$(git status --porcelain --untracked-files=normal 2>/dev/null)"; then printf true; else printf false; fi; else printf unknown; fi)
BUILDINFO_PACKAGE := coordplane/internal/buildinfo
BUILD_LDFLAGS := -X '$(BUILDINFO_PACKAGE).version=$(BUILD_VERSION)' -X '$(BUILDINFO_PACKAGE).commit=$(BUILD_COMMIT)' -X '$(BUILDINFO_PACKAGE).dirty=$(BUILD_DIRTY)'

.PHONY: build docker-image release-health-cp-accept-001 release-health-cp-probe-001 test

build:
	mkdir -p $(COORDPLANE_RELEASE_HEALTH_DIR)/bin
	go build -buildvcs=false -ldflags "$(BUILD_LDFLAGS) -X '$(BUILDINFO_PACKAGE).component=coordplane'" -o $(COORDPLANE_RELEASE_HEALTH_DIR)/bin/coordplane ./cmd/coordplane
	GOOS=linux GOARCH=$$(go env GOARCH) CGO_ENABLED=0 go build -buildvcs=false -ldflags "$(BUILD_LDFLAGS) -X '$(BUILDINFO_PACKAGE).component=coordlink'" -o $(COORDPLANE_RELEASE_HEALTH_DIR)/bin/coordlink ./cmd/coordlink
	chmod 0755 $(COORDPLANE_RELEASE_HEALTH_DIR)/bin/coordlink
	cd $(COORDPLANE_RELEASE_HEALTH_DIR)/bin && sha256sum coordplane coordlink > build-manifest.sha256

docker-image:
	docker build -t $(COORDPLANE_RELEASE_HEALTH_IMAGE) -f docker/claude-runtime/Dockerfile .

release-health-cp-accept-001:
	bash scripts/release-health-cp-accept-001.sh

release-health-cp-probe-001:
	bash scripts/release-health-cp-probe-001.sh

test:
	go test ./... -count=1
