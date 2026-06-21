# kotoji developer task runner.
# Conventions:
#   - Backend is a single Go module rooted at backend/ (module
#     github.com/necorox-com/kotoji/backend).
#   - Frontend is a single npm package rooted at frontend/.
#   - Compose files live in deploy/; build contexts are the sibling dirs.
# Installed CLIs (sqlc, goose) are on PATH (asdf shims) or under $(GOPATH)/bin.

# --- paths ---
BACKEND      := backend
FRONTEND     := frontend
DEPLOY       := deploy
DOCS         := docs
MODULE       := github.com/necorox-com/kotoji/backend
OPENAPI_SPEC := $(DOCS)/contracts/openapi.yaml

# --- tools (overridable) ---
GOBIN        := $(shell go env GOPATH)/bin
SQLC         ?= sqlc
GOOSE        ?= goose
# oapi-codegen is a pinned codegen tool invoked via `go run` (no global install).
OAPI_CODEGEN ?= go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1

# --- compose ---
COMPOSE      := docker compose -f $(DEPLOY)/docker-compose.yml
COMPOSE_DEV  := $(COMPOSE) -f $(DEPLOY)/docker-compose.dev.yml

# --- database / migrations ---
# goose reads DATABASE_URL-style DSNs; KOTOJI_DATABASE_URL is the project var.
DB_URL       ?= $(KOTOJI_DATABASE_URL)
MIGRATIONS   := $(BACKEND)/internal/db/migrations
SEED_DSN      = $(DB_URL)

.DEFAULT_GOAL := help
.PHONY: help gen gen-backend gen-frontend migrate migrate-down seed test test-backend \
        test-frontend lint lint-backend lint-frontend build build-backend build-frontend \
        up down db-reset dev

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS=":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Code generation
# ---------------------------------------------------------------------------
gen: gen-backend gen-frontend ## Run all codegen (sqlc, oapi-codegen, openapi-typescript)

gen-backend: ## Generate Go: sqlc queries + oapi-codegen types
	cd $(BACKEND) && $(SQLC) generate
	# oapi-codegen reads ../oapi-codegen.yaml and the OpenAPI spec (authored in Phase 4).
	cd $(BACKEND) && $(OAPI_CODEGEN) -config oapi-codegen.yaml ../$(OPENAPI_SPEC)

gen-frontend: ## Generate the TS API types from the OpenAPI spec
	cd $(FRONTEND) && npx openapi-typescript ../$(OPENAPI_SPEC) -o src/lib/api/schema.d.ts

# ---------------------------------------------------------------------------
# Migrations (goose)
# ---------------------------------------------------------------------------
migrate: ## Apply all up migrations (needs KOTOJI_DATABASE_URL)
	$(GOOSE) -dir $(MIGRATIONS) postgres "$(DB_URL)" up

migrate-down: ## Roll back the latest migration
	$(GOOSE) -dir $(MIGRATIONS) postgres "$(DB_URL)" down

# ---------------------------------------------------------------------------
# Seed (dev only; the seed binary guards on env==development)
# ---------------------------------------------------------------------------
seed: ## Run the dev seed (Phase 1+: cmd/seed)
	cd $(BACKEND) && go run ./cmd/seed

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------
test: test-backend test-frontend ## Run all tests (Go + frontend)

test-backend: ## Go unit tests
	cd $(BACKEND) && go test ./...

test-frontend: ## Frontend tests (vitest)
	cd $(FRONTEND) && npx vitest run --passWithNoTests

# ---------------------------------------------------------------------------
# Lint
# ---------------------------------------------------------------------------
lint: lint-backend lint-frontend ## Lint everything (go vet + eslint)

lint-backend: ## go vet
	cd $(BACKEND) && go vet ./...

lint-frontend: ## eslint
	cd $(FRONTEND) && npm run lint

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
build: build-backend build-frontend ## Build backend binary + frontend bundle

build-backend: ## Compile kotojid
	cd $(BACKEND) && go build -o bin/kotojid ./cmd/kotojid

build-frontend: ## Build the Next.js standalone bundle
	cd $(FRONTEND) && npm run build

# ---------------------------------------------------------------------------
# Docker Compose
# ---------------------------------------------------------------------------
up: ## Start the production-shaped stack (postgres + backend + frontend)
	$(COMPOSE) --env-file $(DEPLOY)/.env up -d --build

down: ## Stop the stack
	$(COMPOSE) down

dev: ## Start the stack with the dev overlay (adminer + dev-auth) in the foreground
	$(COMPOSE_DEV) --env-file $(DEPLOY)/.env up --build

db-reset: ## Drop the postgres volume and re-create a fresh DB (DESTRUCTIVE)
	$(COMPOSE) down -v
	$(COMPOSE) --env-file $(DEPLOY)/.env up -d postgres
