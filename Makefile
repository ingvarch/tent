MODULE  := github.com/ingvarch/tent
VERSION = $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  = $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    = $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = \
	-X $(MODULE)/internal/buildinfo.version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.date=$(DATE)
GOBUILD = go build -trimpath -ldflags "$(LDFLAGS)"

# The version CI lints with; another one finds and formats other things. internal/buildconfig keeps the two equal.
GOLANGCI_LINT_VERSION := 2.14.0

.DEFAULT_GOAL := check

.PHONY: check build test lint fmt licenses notices generate dist clean golangci-lint-version

check: fmt lint licenses test build

# build also builds tent-node for linux/amd64 with the same version: the nodes of a development build run it
# (TENT_NODE_URL and TENT_NODE_SHA256).
build:
	$(GOBUILD) -o bin/tent ./cmd/tent
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GOBUILD) -o bin/tent-node_linux_amd64 ./cmd/tent-node

test:
	go test -race ./...

golangci-lint-version:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed: CI lints with v$(GOLANGCI_LINT_VERSION)"; \
		exit 1; \
	}
	@installed=$$(golangci-lint version --short 2>/dev/null); \
	installed=$${installed:-an unknown version}; \
	if [ "$$installed" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "golangci-lint $$installed is installed: CI lints with v$(GOLANGCI_LINT_VERSION)"; \
		exit 1; \
	fi

lint: golangci-lint-version
	golangci-lint run ./...

fmt: golangci-lint-version
	golangci-lint fmt ./...

# licenses fails when a module tent or tent-node links, on any platform the release builds for, has a licence outside
# internal/licenses.Allowed. notices checks the same and writes THIRD_PARTY_NOTICES, which the release ships.
licenses:
	go run ./internal/licenses/cmd/licenses ./cmd/tent ./cmd/tent-node

notices:
	go run ./internal/licenses/cmd/licenses -notices THIRD_PARTY_NOTICES ./cmd/tent ./cmd/tent-node

generate:
	go generate ./...

# dist builds every platform and package the way a release does, publishes nothing and does not sign (keyless
# signing needs the release job's token). It needs goreleaser (see .tool-versions) and syft.
dist:
	goreleaser release --snapshot --clean --skip=sign

clean:
	rm -rf bin dist THIRD_PARTY_NOTICES
