# EdgePilot — one entry point for every routine task.
#
# Targets that need Go or Node are marked; the `verify-*` targets need only
# Docker, so the design artifacts can be checked on a clean machine.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# make runs a non-interactive shell, which does not source ~/.bashrc, so a Go
# toolchain installed under $HOME (as scripts/setup.sh does when there is no
# root access) would otherwise be invisible here. Appending rather than
# prepending means a system-wide Go still wins.
export PATH := $(PATH):$(HOME)/.local/go/bin:$(HOME)/go/bin

COMPOSE_DEV  := deployments/docker/docker-compose.dev.yml
COMPOSE_PROD := deployments/docker/docker-compose.yml
OPENAPI      := packages/shared/openapi/openapi.yaml
PROTO_DIR    := packages/proto
# Every Go module in the workspace. Modules with no packages yet are skipped
# rather than failing the target, so `make test` stays green as the tree fills in.
GO_MODULES   := apps/server apps/agent packages/proto
VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT       := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

SQLC    := docker run --rm -u "$(shell id -u):$(shell id -g)" -v "$(CURDIR):/src" -w /src sqlc/sqlc:latest
REDOCLY := docker run --rm -v "$(CURDIR)/packages/shared/openapi:/spec" -w /spec redocly/cli:latest
BUF_HOME := $(HOME)/.cache/buf
BUF     := docker run --rm -u "$(shell id -u):$(shell id -g)" -e HOME=/tmp/bufhome \
             -v "$(BUF_HOME):/tmp/bufhome/.cache" \
             -v "$(CURDIR)/$(PROTO_DIR):/work" -w /work bufbuild/buf:latest

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# --------------------------------------------------------------- verification --
# These check the design artifacts by executing them. Docker only.

.PHONY: verify
verify: verify-schema verify-proto verify-api ## Run every design verification

.PHONY: verify-schema
verify-schema: ## Apply, exercise, reverse, and re-apply migrations on Postgres 16
	@./scripts/verify-schema.sh

.PHONY: verify-proto
verify-proto: ## Lint and compile the device-plane protobuf
	@echo "--> buf lint"
	@$(BUF) lint
	@echo "--> buf build"
	@$(BUF) build -o /dev/null
	@echo "protobuf OK"

.PHONY: verify-api
verify-api: ## Lint and bundle the OpenAPI specification
	@echo "--> redocly lint"
	@$(REDOCLY) lint --config redocly.yaml openapi.yaml
	@echo "--> redocly bundle"
	@$(REDOCLY) bundle --config redocly.yaml openapi.yaml -o /dev/null

.PHONY: verify-proto-breaking
verify-proto-breaking: ## Fail on a breaking protobuf change against main
	@$(BUF) breaking --against 'https://github.com/visionify/edgepilot.git#branch=main,subdir=packages/proto'

# ------------------------------------------------------------------ dev stack --

.PHONY: dev
dev: ## Start Postgres, Redis, Azurite, MailHog
	docker compose -f $(COMPOSE_DEV) up -d
	@echo "postgres :5432  redis :6379  azurite :10000  mailhog :8025"

.PHONY: dev-down
dev-down: ## Stop the development stack
	docker compose -f $(COMPOSE_DEV) down

.PHONY: dev-reset
dev-reset: ## Destroy and recreate the development stack, including volumes
	docker compose -f $(COMPOSE_DEV) down -v
	$(MAKE) dev migrate bootstrap

.PHONY: logs
logs: ## Tail development stack logs
	docker compose -f $(COMPOSE_DEV) logs -f

# ---------------------------------------------------------------------- build --

.PHONY: setup
setup: ## Verify toolchain versions and install dependencies
	@./scripts/setup.sh

.PHONY: proto
proto: ## Regenerate Go code from the protobuf contract
	@mkdir -p $(BUF_HOME)
	@$(BUF) generate
	@cd $(PROTO_DIR) && go build ./... && echo "generated protobuf compiles"

.PHONY: sqlc
sqlc: ## Regenerate query code from queries/*.sql against the migration schema
	@$(SQLC) generate
	@cd apps/server && go build ./... && echo "generated query code compiles"

.PHONY: sqlc-vet
sqlc-vet: ## Check queries against the schema without generating
	@$(SQLC) vet

.PHONY: api-client
api-client: ## Regenerate the dashboard's TypeScript API client
	cd apps/dashboard && pnpm run generate:api

.PHONY: server
server: ## Run the server
	cd apps/server && go run ./cmd/server

.PHONY: dashboard
dashboard: ## Run the dashboard
	cd apps/dashboard && pnpm dev

.PHONY: build
build: build-server build-agent build-dashboard ## Build everything

.PHONY: build-server
build-server: ## Build the server and epctl
	@mkdir -p bin
	@for c in edgepilot-server:server epctl:epctl; do \
	  out=$${c%%:*}; pkg=$${c##*:}; \
	  if ! ls apps/server/cmd/$$pkg/*.go >/dev/null 2>&1; then \
	    echo "  skip  $$out (cmd/$$pkg has no sources yet)"; continue; \
	  fi; \
	  echo "  build bin/$$out"; \
	  (cd apps/server && CGO_ENABLED=0 go build -trimpath \
	     -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" \
	     -o ../../bin/$$out ./cmd/$$pkg) || exit 1; \
	done

.PHONY: build-agent
build-agent: ## Build the agent for macOS and Linux, both architectures
	@if ! ls apps/agent/cmd/edgepilot-agent/*.go >/dev/null 2>&1; then \
	  echo "  skip  agent (no sources yet)"; exit 0; \
	fi; \
	mkdir -p bin; \
	for arch in arm64 amd64; do \
	  echo "  build bin/edgepilot-agent-$$arch"; \
	  (cd apps/agent && GOOS=darwin GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
	     -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" \
	     -o ../../bin/edgepilot-agent-$$arch ./cmd/edgepilot-agent) || exit 1; \
	done; \
	for arch in amd64 arm64; do \
	  echo "  build bin/edgepilot-agent-linux-$$arch"; \
	  (cd apps/agent && GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
	     -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" \
	     -o ../../bin/edgepilot-agent-linux-$$arch ./cmd/edgepilot-agent) || exit 1; \
	done; \
	lipo -create -output bin/edgepilot-agent bin/edgepilot-agent-arm64 bin/edgepilot-agent-amd64 \
	  2>/dev/null || echo "  note: lipo is macOS-only; per-arch binaries are in bin/"

.PHONY: package-agent
package-agent: build-agent ## Package the agent bundles Caddy serves at /install.sh and /dl
	@rm -rf dist/edgepilot-agent dist/edgepilot-agent-linux dist/publish
	@mkdir -p dist/edgepilot-agent dist/edgepilot-agent-linux dist/publish
	@cp bin/edgepilot-agent-arm64 bin/edgepilot-agent-amd64 dist/edgepilot-agent/
	@cp deployments/macos/install.sh \
	    deployments/macos/ai.visionify.edgepilot.agent.plist \
	    deployments/macos/edgepilot.newsyslog.conf dist/edgepilot-agent/
	@cp bin/edgepilot-agent-linux-amd64 bin/edgepilot-agent-linux-arm64 dist/edgepilot-agent-linux/
	@cp deployments/linux/install.sh \
	    deployments/linux/edgepilot-agent.service \
	    deployments/linux/edgepilot.logrotate dist/edgepilot-agent-linux/
	@chmod 0755 dist/edgepilot-agent/install.sh dist/edgepilot-agent/edgepilot-agent-* \
	    dist/edgepilot-agent-linux/install.sh dist/edgepilot-agent-linux/edgepilot-agent-*
# COPYFILE_DISABLE: bsdtar otherwise writes an AppleDouble ._ entry beside every
# file, and those land in the extracted bundle on the customer's Mac.
	@COPYFILE_DISABLE=1 tar --no-xattrs -czf dist/publish/edgepilot-agent.tgz -C dist edgepilot-agent
	@COPYFILE_DISABLE=1 tar --no-xattrs -czf dist/publish/edgepilot-agent-linux.tgz -C dist edgepilot-agent-linux
	@cp deployments/bootstrap.sh dist/publish/install.sh
	@chmod 0755 dist/publish/install.sh
	@cd dist/publish && shasum -a 256 edgepilot-agent.tgz edgepilot-agent-linux.tgz > SHA256SUMS
	@echo "  dist/publish/ ready — copy its contents to deployments/docker/agent-dist/ on the server"
	@ls -l dist/publish

.PHONY: build-dashboard
build-dashboard:
	@if [ ! -f apps/dashboard/package.json ]; then echo "  skip  dashboard (no sources yet)"; exit 0; fi; \
	cd apps/dashboard && pnpm build

# --------------------------------------------------------------------- schema --

.PHONY: migrate
migrate: ## Apply pending migrations
	cd apps/server && go run ./cmd/epctl migrate up

.PHONY: migrate-down
migrate-down: ## Reverse the most recent migration
	cd apps/server && go run ./cmd/epctl migrate down 1

.PHONY: migrate-status
migrate-status: ## Show the current schema version
	cd apps/server && go run ./cmd/epctl migrate status

.PHONY: bootstrap
bootstrap: ## Initialise the device CA and create the first org and admin
	cd apps/server && go run ./cmd/epctl ca init
	cd apps/server && go run ./cmd/epctl bootstrap

# ---------------------------------------------------------------------- tests --

.PHONY: test
test: test-go test-ts ## Run every test suite

.PHONY: test-go
test-go:
	@for m in $(GO_MODULES); do \
	  if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
	    echo "  skip  $$m (no packages yet)"; continue; \
	  fi; \
	  echo "  test  $$m"; \
	  (cd $$m && go test -race -count=1 ./...) || exit 1; \
	done

.PHONY: test-ts
test-ts:
	cd apps/dashboard && pnpm test

.PHONY: test-integration
test-integration: ## Integration tests against real Postgres and Redis
	cd apps/server && go test -race -count=1 -tags=integration ./...

.PHONY: cover
cover: ## Coverage report
	cd apps/server && go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

# ----------------------------------------------------------------------- lint --

.PHONY: lint
lint: lint-go lint-ts lint-arch ## Run every linter

.PHONY: lint-go
lint-go:
	@for m in $(GO_MODULES); do \
	  if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then continue; fi; \
	  echo "  lint  $$m"; \
	  out=$$(cd $$m && gofumpt -l .); \
	  if [ -n "$$out" ]; then echo "not gofumpt-formatted:"; echo "$$out"; exit 1; fi; \
	  (cd $$m && go vet ./...) || exit 1; \
	  if command -v golangci-lint >/dev/null 2>&1; then (cd $$m && golangci-lint run) || exit 1; fi; \
	done

.PHONY: lint-ts
lint-ts:
	cd apps/dashboard && pnpm lint

.PHONY: lint-arch
lint-arch: ## Enforce the clean-architecture import boundaries
	@./scripts/lint-arch.sh

.PHONY: fmt
fmt: ## Format everything
	@for m in $(GO_MODULES); do \
	  if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then continue; fi; \
	  (cd $$m && gofumpt -w .); \
	done
	@if [ -f apps/dashboard/package.json ]; then cd apps/dashboard && pnpm format; fi

# ------------------------------------------------------------------- security --

.PHONY: security
security: ## Vulnerability and static security scans
	@for m in $(GO_MODULES); do \
	  if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then continue; fi; \
	  echo "  scan  $$m"; \
	  (cd $$m && govulncheck ./...) || exit 1; \
	  if command -v gosec >/dev/null 2>&1; then (cd $$m && gosec -quiet ./...) || exit 1; fi; \
	done
	@if [ -f apps/dashboard/package.json ]; then cd apps/dashboard && pnpm audit --audit-level=moderate; fi

.PHONY: clean
clean: ## Remove build output
	rm -rf bin/ apps/dashboard/.next apps/server/coverage.out
