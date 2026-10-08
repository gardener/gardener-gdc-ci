# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

export GOFLAGS ?= -mod=mod

EXECUTABLE_RELEASE_CLI            := bin/gardener-release-cli
EXECUTABLE_SNAPSHOTTER            := bin/gardener-release-snapshotter
EXECUTABLE_TOKEN_HELPER           := bin/token-helper
REPO_ROOT                         := $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))
VERSION                           ?= v0.1.0-dev

#########################################
# Tools                                 #
#########################################

TOOLS_DIR     := .tools
TOOLS_BIN_DIR := $(TOOLS_DIR)/bin
export PATH   := $(abspath $(TOOLS_BIN_DIR)):$(PATH)

GOIMPORTS      := $(TOOLS_BIN_DIR)/goimports
GOLANGCI_LINT  := $(TOOLS_BIN_DIR)/golangci-lint

GOLANGCI_LINT_VERSION ?= v2.1.6

$(GOIMPORTS):
	@mkdir -p $(TOOLS_BIN_DIR)
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install golang.org/x/tools/cmd/goimports@latest

$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS_BIN_DIR)
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) CGO_ENABLED=1 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: all
all: build-local test

.PHONY: tidy
tidy:
	@go mod tidy

.PHONY: clean
clean:
	@rm -rf $(EXECUTABLE_RELEASE_CLI) $(EXECUTABLE_SNAPSHOTTER) $(EXECUTABLE_TOKEN_HELPER) bin/ $(TOOLS_DIR)

.PHONY: check
check: format $(GOLANGCI_LINT)
	@echo "Running golangci-lint..."
	@$(GOLANGCI_LINT) run --config=./.golangci.yaml ./integration/cmd/... ./integration/pkg/auth/...
	@echo "Running go vet..."
	@go vet ./integration/...

.PHONY: format
format: $(GOIMPORTS)
	@$(GOIMPORTS) -l -w -local github.com/gardener/gardener-gdc-ci ./integration

.PHONY: build-local
build-local:
	@CGO_ENABLED=1 go build -o $(EXECUTABLE_RELEASE_CLI) \
	    -race \
	    ./integration/cmd/gardener-release-cli
	@CGO_ENABLED=1 go build -o $(EXECUTABLE_SNAPSHOTTER) \
	    -race \
	    ./integration/cmd/gardener-release-snapshotter
	@CGO_ENABLED=1 go build -o $(EXECUTABLE_TOKEN_HELPER) \
	    -race \
	    ./integration/cmd/token-helper

.PHONY: release
release: $(EXECUTABLE_RELEASE_CLI) $(EXECUTABLE_SNAPSHOTTER) $(EXECUTABLE_TOKEN_HELPER)

LDFLAGS_RELEASE ?= -s -w
GOFLAGS_RELEASE ?= -trimpath -buildvcs=false

$(EXECUTABLE_RELEASE_CLI): FORCE
	@CGO_ENABLED=0 go build $(GOFLAGS_RELEASE) -o $@ -ldflags '$(LDFLAGS_RELEASE)' ./integration/cmd/gardener-release-cli

$(EXECUTABLE_SNAPSHOTTER): FORCE
	@CGO_ENABLED=0 go build $(GOFLAGS_RELEASE) -o $@ -ldflags '$(LDFLAGS_RELEASE)' ./integration/cmd/gardener-release-snapshotter

$(EXECUTABLE_TOKEN_HELPER): FORCE
	@CGO_ENABLED=0 go build $(GOFLAGS_RELEASE) -o $@ -ldflags '$(LDFLAGS_RELEASE)' ./integration/cmd/token-helper

.PHONY: FORCE
FORCE:

.PHONY: test unittests
test: unittests
unittests:
	@go test -race -timeout=3m ./integration/pkg/...

.PHONY: help
help: ## Display available targets
	@echo "Gardener GDC CI & Release Pipeline Build System"
	@echo "==============================================="
	@echo "Available make targets:"
	@echo "  make format      - Formats all Go source files with goimports"
	@echo "  make check       - Runs code linters (golangci-lint, go vet)"
	@echo "  make test        - Runs unit test suite across all packages"
	@echo "  make build-local - Builds release pipeline CLI binaries locally"
	@echo "  make release     - Builds statically linked release CLI binaries"
	@echo "  make tidy        - Runs go mod tidy"
	@echo "  make clean       - Cleans built binaries and tools cache"
