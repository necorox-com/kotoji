# kotoji backend Makefile. Targets are phony; run from the backend module root.
.PHONY: build vet test tidy smoke run-dev migrate seed gen ops all

GOOSE_DIR := internal/db/migrations

# build compiles every package + the kotojid AND kotoji-ops binaries into ./bin.
build:
	go build -o bin/kotojid ./cmd/kotojid
	go build -o bin/kotoji-ops ./cmd/kotoji-ops
	go build ./...

# gen regenerates the sqlc query accessors from internal/db/queries.
gen:
	sqlc generate

# ops runs the background operability jobs once (reap|gc|check|all; default all).
# Expects the same KOTOJI_* env as kotojid. Use for cron/manual invocation.
ops:
	go run ./cmd/kotoji-ops $(JOB)

# vet runs the static analyzer over all packages.
vet:
	go vet ./...

# test runs the full unit/integration test suite.
test:
	go test ./...

# tidy is the single dependency-tidy point for the module.
tidy:
	go mod tidy

# all is the CI gate: build, vet, then test.
all: build vet test

# smoke is the end-to-end boot smoke test: throwaway Postgres -> migrate -> seed
# -> boot kotojid (RUN_MODE=all, dev auth) -> exercise control + data planes ->
# tear down. Requires docker + goose on PATH.
smoke:
	./scripts/boot_smoke.sh

# migrate applies goose migrations to KOTOJI_DATABASE_URL (must be exported).
migrate:
	goose -dir $(GOOSE_DIR) postgres "$(KOTOJI_DATABASE_URL)" up

# seed populates a development DB with the admin user + dev identity.
seed:
	go run ./cmd/seed

# run-dev boots the server with the current environment (expects KOTOJI_* set).
run-dev:
	go run ./cmd/kotojid
