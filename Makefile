BUILD_DIR ?= build
BUILD_VERSION ?= devel
BUILD_COMMIT ?= $(shell git rev-parse --verify HEAD 2>/dev/null || printf unknown)
BUILD_DIRTY ?= $(shell if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then if test -n "$$(git status --porcelain --untracked-files=normal 2>/dev/null)"; then printf true; else printf false; fi; else printf unknown; fi)
BUILDINFO_PACKAGE := coordplane/internal/buildinfo
BUILD_LDFLAGS := -X '$(BUILDINFO_PACKAGE).version=$(BUILD_VERSION)' -X '$(BUILDINFO_PACKAGE).commit=$(BUILD_COMMIT)' -X '$(BUILDINFO_PACKAGE).dirty=$(BUILD_DIRTY)'

.PHONY: build test race vet

build:
	mkdir -p $(BUILD_DIR)/bin
	go build -buildvcs=false -ldflags "$(BUILD_LDFLAGS) -X '$(BUILDINFO_PACKAGE).component=coordplane'" -o $(BUILD_DIR)/bin/coordplane ./cmd/coordplane
	go build -buildvcs=false -ldflags "$(BUILD_LDFLAGS) -X '$(BUILDINFO_PACKAGE).component=coordlink'" -o $(BUILD_DIR)/bin/coordlink ./cmd/coordlink
	chmod 0755 $(BUILD_DIR)/bin/coordplane $(BUILD_DIR)/bin/coordlink
	cd $(BUILD_DIR)/bin && sha256sum coordplane coordlink > build-manifest.sha256

test:
	go test -buildvcs=false ./... -count=1

race:
	go test -race -buildvcs=false ./... -count=1

vet:
	go vet -buildvcs=false ./...
