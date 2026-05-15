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
COMMANDS   := api worker migrator telephony-bridge recording-uploader synthetic status-page

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

.PHONY: test-smoke
test-smoke: ## Run end-to-end smoke tests (requires Docker; see tests/smoke/README.md)
	$(GO) test -tags=smoke -race -count=1 -timeout=15m ./tests/smoke/... ./cmd/api/...

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

.PHONY: grep-time-after
grep-time-after: ## Fail if time.After appears within a for-loop scope (golang-concurrency § BP8)
	@if grep -RnE --include='*.go' --exclude-dir=vendor --exclude='*_test.go' \
	  -B1 'time\.After\(' . | grep -B1 -E '^\s*for\b' >/dev/null 2>&1; then \
	  echo "ERROR: time.After found inside a for-loop — leaks a timer per iteration."; \
	  echo "Use time.NewTimer + Reset (samber/cc-skills-golang@golang-concurrency § BP8)."; \
	  echo ""; \
	  grep -RnE --include='*.go' --exclude-dir=vendor --exclude='*_test.go' \
	    -B1 'time\.After\(' . | grep -B1 -E '^\s*for\b' || true; \
	  exit 1; \
	fi
	@echo "grep-time-after: OK"

.PHONY: ci
ci: lint vet grep-time-after test ## What CI runs

# ----------------------------------------------------------------------
# Local development stack (Docker Compose)
# ----------------------------------------------------------------------
# `make dev-up` boots Postgres + Redis + NATS in containers; cmd/api etc.
# run natively on the host via `go run ./cmd/api`. Production runs on Yandex
# MKS with managed services — see Plan 01.

PROFILE ?=

.PHONY: dev-up
dev-up: ## Start local dev stack (PROFILE=analytics|storage|full)
	@if [ -z "$(PROFILE)" ]; then \
		docker compose -f docker-compose.dev.yml up -d postgres redis nats; \
	else \
		docker compose -f docker-compose.dev.yml --profile $(PROFILE) up -d; \
	fi
	@echo ""
	@echo "Stack is up. Endpoints (all bound to 127.0.0.1):"
	@echo "  Postgres   : postgres://app:devpass@localhost:5432/sociopulse"
	@echo "  Redis      : redis://localhost:6379"
	@echo "  NATS       : nats://localhost:4222 (monitoring at http://localhost:8222)"
	@if [ "$(PROFILE)" = "analytics" ] || [ "$(PROFILE)" = "full" ]; then \
		echo "  ClickHouse : http://localhost:8123 (user=app)"; \
	fi
	@if [ "$(PROFILE)" = "storage" ] || [ "$(PROFILE)" = "full" ]; then \
		echo "  MinIO      : http://localhost:9090 (console http://localhost:9091, minioadmin/minioadmin)"; \
	fi

.PHONY: dev-down
dev-down: ## Stop local dev stack (preserves volumes)
	docker compose -f docker-compose.dev.yml --profile full down

.PHONY: dev-logs
dev-logs: ## Tail logs from dev stack
	docker compose -f docker-compose.dev.yml logs -f --tail=100

.PHONY: dev-psql
dev-psql: ## Open psql shell against dev Postgres
	docker exec -it sp-postgres psql -U app -d sociopulse

.PHONY: dev-redis-cli
dev-redis-cli: ## Open redis-cli against dev Redis
	docker exec -it sp-redis redis-cli

.PHONY: dev-nats
dev-nats: ## Show NATS monitoring info
	@echo "NATS monitoring: http://localhost:8222"
	@echo "JetStream info:"
	@curl -s http://localhost:8222/jsz | (jq . 2>/dev/null || cat)

.PHONY: dev-reset
dev-reset: ## Stop and DELETE all dev data volumes (destructive)
	@echo "WARNING: deleting all dev data (Postgres, Redis, NATS, CH, MinIO volumes)..."
	docker compose -f docker-compose.dev.yml --profile full down -v
	@echo "Done. Run 'make dev-up' to recreate."

# ----------------------------------------------------------------------
# Database migrations (cmd/migrator wraps golang-migrate v4)
# ----------------------------------------------------------------------
# These targets require a Postgres DSN in DATABASE_URL or fall back to the
# local docker-compose dev stack from `make dev-up`. MIGRATIONS_PATH defaults
# to file://$(PWD)/migrations so `make migrate-up` works from a fresh clone.

DATABASE_URL ?= postgres://app:devpass@localhost:5432/sociopulse?sslmode=disable
MIGRATIONS_PATH ?= file://$(PWD)/migrations

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations against $$DATABASE_URL
	DATABASE_URL='$(DATABASE_URL)' MIGRATIONS_PATH='$(MIGRATIONS_PATH)' \
	  $(GO) run ./cmd/migrator up

.PHONY: migrate-down
migrate-down: ## Revert all migrations against $$DATABASE_URL (DEV ONLY)
	DATABASE_URL='$(DATABASE_URL)' MIGRATIONS_PATH='$(MIGRATIONS_PATH)' \
	  $(GO) run ./cmd/migrator down

.PHONY: migrate-status
migrate-status: ## Print the current migration version + dirty flag
	DATABASE_URL='$(DATABASE_URL)' MIGRATIONS_PATH='$(MIGRATIONS_PATH)' \
	  $(GO) run ./cmd/migrator status

.PHONY: migrate-create
migrate-create: ## Create a new migration pair: NAME=add_users_table
	@if [ -z "$(NAME)" ]; then \
	  echo "ERROR: NAME is required, e.g. make migrate-create NAME=add_users_table"; \
	  exit 1; \
	fi
	@mkdir -p migrations
	@last=$$(ls migrations/ 2>/dev/null | grep -E '^[0-9]{6}_' | sed -E 's/^([0-9]{6})_.*/\1/' | sort -n | tail -1); \
	if [ -z "$$last" ]; then next=000001; else next=$$(printf '%06d' $$((10#$$last + 1))); fi; \
	touch migrations/$${next}_$(NAME).up.sql migrations/$${next}_$(NAME).down.sql; \
	echo "Created migrations/$${next}_$(NAME).up.sql"; \
	echo "Created migrations/$${next}_$(NAME).down.sql"

# ----- ClickHouse migrations (analytics) -----
# CH migrations live in migrations/clickhouse/. Apply against
# CLICKHOUSE_DSN; default DSN points at the dev compose stack
# (see make dev-up PROFILE=analytics). x-multi-statement=true is
# required by golang-migrate's CH driver to split multi-statement
# migrations on `;`.

CLICKHOUSE_DSN ?= clickhouse://app:devpass@localhost:9000/sociopulse?x-multi-statement=true
CH_MIGRATIONS_PATH ?= file://$(PWD)/migrations/clickhouse

.PHONY: migrate-ch-up
migrate-ch-up: ## Apply all pending CH migrations against $$CLICKHOUSE_DSN
	CLICKHOUSE_DSN='$(CLICKHOUSE_DSN)' CLICKHOUSE_MIGRATIONS_PATH='$(CH_MIGRATIONS_PATH)' \
	  $(GO) run ./cmd/migrator --target=clickhouse up

.PHONY: migrate-ch-down
migrate-ch-down: ## Revert all CH migrations (DEV ONLY)
	CLICKHOUSE_DSN='$(CLICKHOUSE_DSN)' CLICKHOUSE_MIGRATIONS_PATH='$(CH_MIGRATIONS_PATH)' \
	  $(GO) run ./cmd/migrator --target=clickhouse down

.PHONY: migrate-ch-status
migrate-ch-status: ## Print the current CH migration version + dirty flag
	CLICKHOUSE_DSN='$(CLICKHOUSE_DSN)' CLICKHOUSE_MIGRATIONS_PATH='$(CH_MIGRATIONS_PATH)' \
	  $(GO) run ./cmd/migrator --target=clickhouse status

.PHONY: migrate-ch-create
migrate-ch-create: ## Create a new CH migration pair: NAME=add_some_table
	@if [ -z "$(NAME)" ]; then \
	  echo "ERROR: NAME is required, e.g. make migrate-ch-create NAME=add_some_table"; \
	  exit 1; \
	fi
	@mkdir -p migrations/clickhouse
	@last=$$(ls migrations/clickhouse/ 2>/dev/null | grep -E '^[0-9]{6}_' | sed -E 's/^([0-9]{6})_.*/\1/' | sort -n | tail -1); \
	if [ -z "$$last" ]; then next=000001; else next=$$(printf '%06d' $$((10#$$last + 1))); fi; \
	touch migrations/clickhouse/$${next}_$(NAME).up.sql migrations/clickhouse/$${next}_$(NAME).down.sql; \
	echo "Created migrations/clickhouse/$${next}_$(NAME).up.sql"; \
	echo "Created migrations/clickhouse/$${next}_$(NAME).down.sql"

# ──────────────────────────── proto codegen ───────────────────────────────────
# Plugins are pinned via go.mod `tool` directive (Go 1.24+). `go tool` resolves
# the binary from the module cache without installing anything globally — the
# trampoline shims below adapt protoc's plugin protocol to that resolver.
PROTOC_TOOL_BIN := .protoc-tools

$(PROTOC_TOOL_BIN)/protoc-gen-go:
	@mkdir -p $(PROTOC_TOOL_BIN)
	@printf '#!/usr/bin/env bash\nexec $(GO) tool protoc-gen-go "$$@"\n' > $@
	@chmod +x $@

$(PROTOC_TOOL_BIN)/protoc-gen-go-grpc:
	@mkdir -p $(PROTOC_TOOL_BIN)
	@printf '#!/usr/bin/env bash\nexec $(GO) tool protoc-gen-go-grpc "$$@"\n' > $@
	@chmod +x $@

.PHONY: proto-recording
proto-recording: $(PROTOC_TOOL_BIN)/protoc-gen-go $(PROTOC_TOOL_BIN)/protoc-gen-go-grpc ## Generate Go bindings for the RecordingService proto.
	PATH="$(CURDIR)/$(PROTOC_TOOL_BIN):$$PATH" protoc \
	  -I=docs/api \
	  --go_out=. \
	  --go_opt=module=github.com/sociopulse/platform \
	  --go-grpc_out=. \
	  --go-grpc_opt=module=github.com/sociopulse/platform \
	  docs/api/recording/v1/recording.proto
