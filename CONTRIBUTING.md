# Contributing to kotoji

Thanks for your interest in improving kotoji! This guide covers how to build,
test, lint, and regenerate code so your change lands green in CI. The repo is a
single git repo with two trees: a Go backend (`backend/`, module
`github.com/necorox-com/kotoji/backend`) and a Next.js frontend (`frontend/`).
Compose files and Dockerfiles live in `deploy/`.

By participating you agree to abide by our
[Code of Conduct](./CODE_OF_CONDUCT.md).

## Prerequisites

- **Go** — the toolchain pinned in [`backend/go.mod`](./backend/go.mod) (1.25.x).
- **Node.js** — 20+ (the floor Next 16 requires).
- **Docker** + Docker Compose v2 (for the local stack and integration tests).
- For regenerating committed artifacts (only if you touch the API contract or DB
  queries): `sqlc`, `goose`, Python 3 with `PyYAML`, and `npx`. The
  `oapi-codegen` codegen runs via `go run` and needs no install.

A `Makefile` wraps the common tasks — run `make help` to list them. The commands
below are exactly what CI runs (`.github/workflows/ci.yml`).

## Run it locally

The fastest path is the proxy-less local stack on `*.localhost` (no DNS/TLS
setup needed):

```bash
cp deploy/.env.example deploy/.env   # then edit as needed
make up                              # postgres + backend + frontend, built from source
# or directly:
docker compose -f deploy/docker-compose.yml up -d
```

Open `http://kotoji.localhost:8080`. To run prebuilt images instead of building,
add the GHCR overlay (`-f deploy/docker-compose.ghcr.yml`). The full deployment
guide is in [`deploy/README.md`](./deploy/README.md).

To iterate on a single tree without Docker:

```bash
# backend
cd backend && go run ./cmd/kotojid
# frontend
cd frontend && npm install && npm run dev
```

## Build, test, lint

### Backend (`backend/`)

```bash
go build ./...
go vet ./...
gofmt -l .                       # must print nothing; run `gofmt -w .` to fix
go test ./...                    # unit tests
go test -tags conformance ./...  # contract conformance suite
```

Integration tests need a Postgres. CI spins one up and sets
`KOTOJI_DATABASE_URL`; locally you can do the same:

```bash
export KOTOJI_DATABASE_URL='postgres://kotoji:kotoji@localhost:5432/kotoji_test?sslmode=disable'
go test -tags=integration ./...
```

The migration round-trip test drives goose off the embedded migration FS, so it
is CWD-independent.

### Frontend (`frontend/`)

```bash
npm ci          # clean install from the lockfile (use `npm install` when adding deps)
npx tsc --noEmit # type-check (there is no `typecheck` npm script)
npm run lint     # eslint
npm run build    # production Next.js build
npm test         # vitest (`make test-frontend` passes --passWithNoTests)
```

### Everything at once

```bash
make test   # go test ./...  +  vitest
make lint   # go vet  +  eslint
make build  # kotojid binary + Next standalone bundle
```

## Generated code (don't hand-edit)

Three artifacts are generated from sources and **checked in**; the CI `drift` job
fails if the committed copy is stale. If you change the OpenAPI spec
(`docs/contracts/openapi.yaml`), the sqlc queries, or the migration schema,
regenerate and commit the result:

```bash
make gen           # runs all three generators
# individually:
make gen-backend   # sqlc generate  +  oapi-codegen Go DTOs
make gen-frontend  # openapi-typescript -> frontend/src/lib/api/schema.d.ts
```

Details:

- **sqlc** (`backend/sqlc.yaml`) → `backend/internal/db/gen` — DB accessors from
  `queries/*.sql` + the migration schema. Pinned to `1.31.1` in CI.
- **oapi-codegen** (`backend/oapi-codegen.yaml`) → `backend/internal/openapi/types.gen.go`
  — the Go DTOs. The frozen 3.1 spec is lowered to 3.0 by
  `backend/internal/openapi/spec31to30.py` (needs `PyYAML`), then fed to the
  pinned `oapi-codegen@v2.4.1` via `go run`. Mirrors `internal/openapi/gen.go`'s
  `go:generate`.
- **openapi-typescript** → `frontend/src/lib/api/schema.d.ts` — the TS API types.

The OpenAPI spec is the contract source of truth — edit it there, never the
generated files.

## Database migrations

Migrations are embedded goose SQL under
`backend/internal/db/migrations` and run automatically at boot
(`KOTOJI_AUTO_MIGRATE`). To apply/roll back manually:

```bash
export KOTOJI_DATABASE_URL='postgres://...'
make migrate        # goose up
make migrate-down   # roll back the latest
```

Add a migration as the next numbered `NNNN_description.sql` with `-- +goose Up` /
`-- +goose Down` blocks, then regenerate sqlc (`make gen-backend`) if it changes
the schema your queries touch.

## Submitting a change

1. **Branch** off `main` and keep the change focused.
2. **Run the relevant checks** above — your PR must pass `ci.yml` (backend
   build/vet/fmt/test + conformance, backend-integration, frontend tsc/lint/build,
   and drift).
3. **Commit messages: Conventional Commits.** Match the existing history, e.g.
   `feat:`, `fix:`, `chore:`, `docs:`, `ci:`, `security:`, with an optional scope
   like `feat(auth):` or `fix(frontend):`. Keep the subject imperative and under
   ~72 chars.
4. **No sign-off / CLA required.** This project does not use a DCO sign-off or a
   CLA — just open the PR. (If your employer requires it, add a
   `Signed-off-by:` trailer with `git commit -s`; it's accepted but optional.)
5. **Open a PR** using the template; fill in what changed, why, and how you
   tested it. Link any related issue.
6. **Update docs** (`docs/`, `README*.md`, `deploy/README.md`) and
   [`CHANGELOG.md`](./CHANGELOG.md) under **Unreleased** when your change is
   user-visible.

## Keep it necorox-agnostic

Committed files must not contain secrets, real IPs/hostnames, or org-specific
values. Use placeholder domains (`example.com`), the documented env vars, and the
`deploy/.env.example` template. CI uses only disposable, throwaway values.

## Reporting security issues

Do **not** open a public issue for vulnerabilities — see
[SECURITY.md](./SECURITY.md) for the private reporting channel.
