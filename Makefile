COORDPLANE_RELEASE_HEALTH_DIR ?= .coordplane-release-health
COORDPLANE_RELEASE_HEALTH_IMAGE ?= coordplane/claude-runtime:release-health

.PHONY: build docker-image release-health-cp-accept-001 release-health-cp-probe-001 test

build:
	mkdir -p $(COORDPLANE_RELEASE_HEALTH_DIR)/bin
	go build -buildvcs=false -o $(COORDPLANE_RELEASE_HEALTH_DIR)/bin/coordplane ./cmd/coordplane
	GOOS=linux GOARCH=$$(go env GOARCH) CGO_ENABLED=0 go build -buildvcs=false -o $(COORDPLANE_RELEASE_HEALTH_DIR)/bin/coordlink ./cmd/coordlink
	chmod 0755 $(COORDPLANE_RELEASE_HEALTH_DIR)/bin/coordlink

docker-image:
	docker build -t $(COORDPLANE_RELEASE_HEALTH_IMAGE) -f docker/claude-runtime/Dockerfile .

release-health-cp-accept-001:
	bash scripts/release-health-cp-accept-001.sh

release-health-cp-probe-001:
	bash scripts/release-health-cp-probe-001.sh

test:
	go test ./... -count=1
