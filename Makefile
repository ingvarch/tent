MODULE  := github.com/ingvarch/tent
VERSION = $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  = $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    = $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = \
	-X $(MODULE)/internal/buildinfo.version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.date=$(DATE)

# The version CI lints with; another one finds and formats other things. internal/buildconfig keeps the two equal.
GOLANGCI_LINT_VERSION := 2.13.2

.DEFAULT_GOAL := check

.PHONY: check build test lint fmt generate dist clean golangci-lint-version

check: fmt lint test build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tent ./cmd/tent

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

generate:
	go generate ./...

# dist builds every platform and package the way a release does, publishes nothing and does not sign (keyless
# signing needs the release job's token). It needs goreleaser (see .tool-versions) and syft.
dist:
	goreleaser release --snapshot --clean --skip=sign

clean:
	rm -rf bin dist
