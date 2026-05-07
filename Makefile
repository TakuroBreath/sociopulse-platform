# СоциоПульс — top-level Makefile
# All commands run from the repo root.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# Where to put built binaries
BIN_DIR := bin

# Go-specific
GO         ?= go
GOFLAGS    ?=
LDFLAGS    ?= -s -w
PKG        := github.com/sociopulse/platform
COMMANDS   := api worker migrator telephony-bridge recording-uploader

# Tooling pin (override locally if needed)
# Note: v1.59.1 install fails due to broken transitive dep (asciicheck@v0.2.0).
# v1.64.8+ avoids the issue and supports all linters from .golangci.yml.
GOLANGCI_LINT_VERSION ?= v1.64.8

# ----- targets -----

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: tools
tools: ## Install development tools (golangci-lint)
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)…"
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: lint
lint: ## Run golangci-lint on all Go code
	golangci-lint run ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: test
test: ## Run all Go tests
	$(GO) test -race -count=1 ./...

.PHONY: test-cover
test-cover: ## Run tests with coverage report
	$(GO) test -race -count=1 -coverprofile=coverage.txt -covermode=atomic ./...
	$(GO) tool cover -html=coverage.txt -o coverage.html
	@echo "Coverage report: coverage.html"

.PHONY: build
build: $(addprefix build-, $(COMMANDS)) ## Build all binaries

build-%:
	@echo "Building $*…"
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$* ./cmd/$*

.PHONY: run
run: ## Run cmd/api locally with development config
	HTTP_ADDR=:8080 $(GO) run ./cmd/api

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.txt coverage.html

.PHONY: docker-build
docker-build: ## Build the cmd/api Docker image
	docker build -t sociopulse-api:dev -f Dockerfile .

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

.PHONY: ci
ci: lint vet test ## What CI runs
