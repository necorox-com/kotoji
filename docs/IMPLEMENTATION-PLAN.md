# kotoji — Implementation Plan

> **Scope.** The concrete, ordered build plan: exact monorepo folder structure, the Go
> module + npm dependency lists, and a phased task breakdown with parallelization notes.
> It assumes the **canonical resolutions** in
> [`contracts/consistency-report.md`](./contracts/consistency-report.md) are adopted — read
> that first; the type names, DDL, and decisions below follow it, not the (drifted) raw docs.
>
> **Stack (confirmed):** Go 1.24+ backend (chi, pgx v5, sqlc, goose, git CLI, official MCP
> Go SDK, go-oidc) + Next.js 16 / React 19 / TS-strict frontend (TanStack Query, Tailwind
> v4, shadcn, Monaco) + PostgreSQL 17/18, all via Docker Compose behind NPM.

---

## 0. Decisions to confirm with the user BEFORE coding

These are the user-facing forks from the consistency report. A quick yes/no on each unblocks
the corresponding phase; defaults in **bold** are what I'll proceed with absent an answer.

1. **OpenAPI toolchain (P0-3):** spec hand-written, Go types via `oapi-codegen -generate types`, TS client via `openapi-typescript` + `openapi-fetch`, CI staleness gate. **Yes** (recommended).
2. **Role capabilities (P0-4):** owner=all; **editor can publish directly**; **viewers can see drafts/previews**. Optional per-site `require_publish_approval`. Confirm the two bolded behaviors.
3. **Soft-delete (P0-6):** `deleted_at` + **30-day** grace before disk reclaim. Confirm grace period.
4. **Single backend replica for v1 (P1-12):** HA/multi-replica is **not** a v1 goal (keeps the in-proc mutex + flock model). Confirm.
5. **i18n (P1-13):** `next-intl`, **ja-only at launch**, all copy as keys. Confirm ja-only.
6. **Publish mode (P1-3):** per-site `publish_mode ∈ {direct, request}`, **default `direct`**. Confirm per-site (not global).
7. **Second domain for hosted content (P1-1):** v1 ships on one domain (host-only cookies); **recommend** buying `*.kotoji-usercontent.com` for prod hardening. Confirm whether to provision it now.
8. **AI-autonomous `create_site` over MCP (P1-14):** **off by default**, dashboard opt-in only. Confirm it should be exposable at all.

Everything else in the report I will finalize from the recommended resolutions.

---

## 1. Monorepo folder structure

```
kotoji/
├── README.md  LICENSE  .gitignore
├── Makefile                         # dev tasks: gen, migrate, seed, test, lint, up
├── docs/                            # (already authored — see docs/README.md)
│   ├── README.md  architecture.md  design.md  IMPLEMENTATION-PLAN.md
│   └── contracts/
│       ├── consistency-report.md
│       ├── site-service.md  data-model.md  mcp.md  routing-and-serving.md
│       ├── openapi.yaml             # OWED (P0-3) — REST source of truth
│       ├── identifiers.md           # OWED — handle/branch rules + role-capability matrix
│       └── mcp-tools.md             # OWED or fold into mcp.md
│
├── backend/                         # Go 1.24+, single module
│   │                                #   module: github.com/necorox-com/kotoji/backend
│   ├── go.mod  go.sum
│   ├── sqlc.yaml                    # engine postgresql, sql_package pgx/v5
│   ├── oapi-codegen.yaml            # types-only generation config
│   ├── cmd/
│   │   ├── kotojid/main.go          # the server; RUN_MODE=all|control|serve
│   │   ├── kotoji-migrate/main.go   # goose wrapper (or use goose CLI)
│   │   └── seed/main.go             # dev seed (guarded: env==development only)
│   ├── internal/
│   │   ├── config/                  # typed Config + env parse + fail-fast validate
│   │   ├── app/                     # composition root: DI wiring, builds http.Servers
│   │   ├── api/                     # REST handlers (chi). control plane.
│   │   │   ├── router.go  middleware.go  errors.go  me.go config.go
│   │   │   ├── sites.go  files.go  branches.go  publish.go  history.go
│   │   │   ├── members.go  tokens.go  admin.go  upload.go
│   │   ├── openapi/                 # GENERATED Go types from openapi.yaml (oapi-codegen)
│   │   ├── auth/                    # AuthProvider abstraction + session store
│   │   │   ├── provider.go  oidc.go  devauth.go  session.go  csrf.go
│   │   ├── site/                    # THE DI BOUNDARY (canonical interface lives here)
│   │   │   ├── service.go           #   site.Service interface + domain structs (§site-service.md)
│   │   │   ├── errors.go            #   sentinel + typed errors (ConflictError, etc.)
│   │   │   ├── handle.go            #   ValidateHandle + reserved words
│   │   │   ├── path.go              #   validatePath (ZipSlip/traversal/allowlist)
│   │   │   ├── mime.go              #   MIMEByExt (single source: upload allowlist + serve types)
│   │   │   ├── git_service.go       #   gitService: prod impl (Store + gitRunner)
│   │   │   ├── gitrunner.go         #   gitRunner iface + execRunner (os/exec, arg arrays)
│   │   │   ├── lock.go              #   per-site keyed RWMutex registry + flock
│   │   │   ├── zip.go               #   ImportZip guards
│   │   │   ├── mirror.go            #   SetRemote / MirrorPush / FetchAndUpdate
│   │   │   ├── fake.go              #   FakeService (in-memory) for handler/MCP tests
│   │   │   └── *_test.go            #   table-driven contract + git_service + //go:build integration
│   │   ├── resolve/                 # data-plane Host/path resolver (resolve.Resolver)
│   │   │   └── resolver.go  resolver_test.go
│   │   ├── serve/                   # DATA PLANE static handler
│   │   │   ├── static.go            #   StaticHandler (index/MIME/404/headers/cache)
│   │   │   ├── headers.go           #   SecurityHeaderConfig + CSP builder
│   │   │   ├── treeprovider.go      #   TreeProvider (materialized served/ dir, atomic swap)
│   │   │   ├── authz.go             #   PreviewAuthz (preview-grant host-only cookie)
│   │   │   ├── basehref.go          #   path-mode <base> injection
│   │   │   └── *_test.go
│   │   ├── mcpserver/               # MCP server (official Go SDK), Streamable HTTP
│   │   │   ├── server.go  verifier.go  registry.go  tools.go  limits.go  *_test.go
│   │   ├── webhook/                 # GitHub webhook (HMAC verify → FetchAndUpdate)
│   │   │   └── github.go  github_test.go
│   │   ├── db/                      # data access
│   │   │   ├── migrations/          #   goose NNNNN_name.sql (see data-model.md §5)
│   │   │   ├── queries/             #   sqlc INPUT: sites.sql members.sql auth.sql tokens.sql audit.sql
│   │   │   ├── gen/                 #   sqlc OUTPUT (do not hand-edit)
│   │   │   └── store.go  tx.go      #   pool + tx helper + domain-typed wrappers
│   │   └── observability/           # slog setup, request-id, /healthz /readyz, metrics
│   └── testdata/                    # crafted zips (zipslip/bomb), golden fixtures
│
├── frontend/                        # Next.js 16 App Router, React 19, TS strict
│   ├── package.json  tsconfig.json  next.config.ts  postcss.config.mjs
│   ├── components.json              # shadcn config
│   ├── src/
│   │   ├── app/                     # routes & layouts (see design.md §3.5)
│   │   │   ├── globals.css          #   @theme tokens (design.md §2.2)
│   │   │   ├── layout.tsx           #   Providers: Theme, Query, Tooltip, Toaster
│   │   │   ├── (auth)/login/page.tsx
│   │   │   └── (app)/dashboard | sites/new | sites/[handle]/{,branches,publish,history,members,settings} | admin
│   │   ├── components/
│   │   │   ├── ui/                  # shadcn-generated primitives
│   │   │   ├── atoms/  molecules/  organisms/  templates/   # atomic design (design.md §3)
│   │   ├── lib/
│   │   │   ├── api/                 # client.ts, schema.d.ts (GENERATED), keys.ts, hooks/, error.ts
│   │   │   ├── auth/  monaco/  utils.ts
│   │   ├── hooks/                   # useMediaQuery, useCopyToClipboard, useDebounce
│   │   └── messages/               # next-intl ja.json (en.json later)
│   └── public/
│
├── deploy/
│   ├── docker-compose.yml           # backend, frontend, postgres
│   ├── docker-compose.dev.yml       # + adminer, dev-auth, live reload
│   ├── backend.Dockerfile           # multi-stage; FINAL image INCLUDES git binary
│   ├── frontend.Dockerfile          # Next.js standalone
│   ├── .env.example
│   └── npm/                         # sample NPM / Caddy / nginx configs (prod + dev notes)
│
└── .github/workflows/
    ├── ci.yml                       # go test + lint, npm test + tsc, openapi drift gate
    └── docker.yml                   # build images
```

> Two `go.mod`? **No — one** Go module rooted at `backend/`. One `package.json` at
> `frontend/`. The repo is a monorepo but each language has a single module/package root.

---

## 2. Dependencies

### 2.1 Go (`backend/go.mod`)

| Module | Purpose |
|---|---|
| `github.com/go-chi/chi/v5` | HTTP router + middleware |
| `github.com/go-chi/cors` | CORS middleware (control plane) |
| `github.com/jackc/pgx/v5` (+ `/pgxpool`, `/pgtype`) | Postgres driver + pool |
| `github.com/google/uuid` | `uuid.UUID` (canonical site/user ID type) |
| `github.com/pressly/goose/v3` | migrations (library + CLI) |
| `github.com/modelcontextprotocol/go-sdk` | MCP server (`mcp`, `auth` packages) |
| `github.com/coreos/go-oidc/v3/oidc` | OIDC id-token verification |
| `golang.org/x/oauth2` (+ `google`) | OAuth2 flow |
| `golang.org/x/crypto/bcrypt` | admin-password mode hash |
| `github.com/go-git/go-git/v5` | go-git for cheap read ops (optional optimization) |
| `github.com/getkin/kin-openapi` | OpenAPI load + request/response conformance tests |
| stdlib | `net/http`, `archive/zip`, `os/exec`, `log/slog`, `crypto/*`, `io/fs`, `path` |
| **tools (not imported):** `oapi-codegen`, `sqlc`, `goose` CLIs (pinned via `go tool` / Makefile) | codegen + migrate |
| **test:** `github.com/stretchr/testify` (assert/require), `net/http/httptest`, `testing/fstest` | tests; consider `testcontainers-go` or `pg-tmp` for integration Postgres |

`git` binary is a **runtime** dependency of the backend image (not a Go dep) — the final
Docker stage must `apk add git` / `apt-get install git`.

### 2.2 npm (`frontend/package.json`)

| Package | Purpose |
|---|---|
| `next@16`, `react@19`, `react-dom@19`, `typescript` | framework + TS strict |
| `@tanstack/react-query` | fetching/caching; `@tanstack/react-query-devtools` (dev) |
| `tailwindcss@4`, `@tailwindcss/postcss`, `tw-animate-css` | styling + Tailwind-v4 animations |
| `shadcn` (CLI, devDep), `@radix-ui/*` (as shadcn pulls), `lucide-react` | primitives + icons |
| `class-variance-authority`, `tailwind-merge`, `clsx` | variant + class composition (`cn()`) |
| `next-themes` | light/dark/system |
| `sonner` | toasts |
| `@monaco-editor/react` | editor + diff view |
| `zod`, `react-hook-form`, `@hookform/resolvers` | forms + validation |
| `next-intl` | i18n (ja launch) |
| `cmdk` | command palette (⌘K) |
| `react-resizable-panels` | split-pane (ProjectDetail) |
| **devDeps:** `openapi-typescript`, `openapi-fetch`, `eslint`, `eslint-plugin-jsx-a11y`, `@axe-core/react`, `vitest`/`jest` + `@testing-library/react` + `jest-axe`, `prettier` | API gen, lint, a11y, tests |

---

## 3. Phased build plan

Legend: **[seq]** must precede later phases · **[∥]** parallelizable with siblings · each
phase ends with green tests (TDD: write the table-driven test alongside).

### Phase 0 — Foundation / scaffold **[seq]**
- Monorepo skeleton, `Makefile` (`gen`/`migrate`/`seed`/`test`/`lint`/`up`/`db-reset`).
- `backend/go.mod`, `internal/config` (typed `Config`, env parse, **fail-fast** validation, `KOTOJI_BASE_DOMAIN`, host-only cookie defaults). `internal/observability` (slog JSON, request-id, `/healthz` `/readyz`).
- `frontend` scaffold: Next 16 + TS strict + Tailwind v4 `@theme` tokens from design.md §2.2 + `next-themes` + Providers shell. shadcn init.
- `deploy/`: compose (backend/frontend/postgres), both Dockerfiles (backend final stage **includes git**), `.env.example`, sample NPM config. Prove `docker compose up` boots an empty stack + `/healthz`.
- **Exit:** stack boots; `/healthz` 200; frontend renders a blank themed shell.

### Phase 1 — DB migrations + sqlc **[seq, depends P0-2/P0-4/P0-6 DDL decisions]**
- Write goose migrations per data-model.md **as corrected by the consistency report**: `site_tokens` (`scopes TEXT[]`, `created_by`, `can_create_sites`, prefix=12), `sites` (3-val `visibility`, `published_commit_sha`+`published_at`, `deleted_at`, `publish_mode`, reserve `web_root`, `description`), `site_members` (`owner/editor/viewer`), `audit_log` (`source` enum), `users`, `user_identities`, `sessions`, `handle_redirects`, `reserved_handles` + seed.
- `sqlc.yaml` + `queries/*.sql` (hot paths from data-model.md §4) → `db/gen`. `store.go` wrappers + `tx.go`.
- Goose runs on boot behind `pg_advisory_lock`. `cmd/seed` (guarded).
- **Tests:** migration up/down round-trip; a `go test` asserting the Go `ReservedHandles` constant == seeded rows; sqlc queries compile.
- **Exit:** schema migrates; seed creates admin+sample rows (sites come in Phase 2).

### Phase 2 — SiteService (git core) **[seq — the keystone; depends P0-1]**
The DI boundary; everything downstream mocks it. Build strictly to the canonical interface.
- `site/service.go` (canonical `Service` + structs), `errors.go`, `handle.go`, `path.go`, `mime.go` (`MIMEByExt`).
- `gitrunner.go` (`execRunner`: arg arrays, no shell, `ctx` deadline, scrubbed env, `GIT_TERMINAL_PROMPT=0`), `lock.go` (per-site keyed RWMutex + flock).
- `git_service.go`: CreateSite (init→seed→commit), ReadFile/ListFiles, **WriteFile/DeleteFile/Commit (optimistic lock under lock, atomic check-then-commit)**, Publish (ff/merge per §6), GetDiff/GetLog, Rollback (forward commit, ancestor-only), CreateBranch/DeleteBranch, RenameHandle (+redirect row), ImportZip (guards-then-write), `ServedTree`.
- `zip.go` (ZipSlip + bomb + allowlist guards — security-critical), `mirror.go` (SetRemote/MirrorPush/FetchAndUpdate, best-effort).
- `fake.go` (`FakeService`, same contract) for downstream tests.
- **Tests (the contract suite):** the shared `testContract` run against **both** `FakeService` and real `gitService` (t.TempDir); fake `gitRunner` for arg-golden + error-mapping; `//go:build integration` real-git round-trip + crafted zipslip/bomb zips. (site-service.md §13.)
- **Exit:** full contract suite green; seed can now create real on-disk sample sites.

### Phase 3 — Auth (OIDC + session + dev modes) **[∥ with Phase 4 after Phase 1]**
- `auth/provider.go` (`AuthProvider` iface), `oidc.go` (go-oidc + x/oauth2, Google default; state+nonce+PKCE; allowlist by `hd`/email), `devauth.go` (no-auth + admin-password/bcrypt), `session.go` (server-side Postgres sessions; rotate id on login; **host-only `__Host-` cookie**), `csrf.go` (double-submit).
- Routes: `/auth/login` `/auth/callback` `/auth/logout`; `/api/me`; `/api/config`.
- **Tests:** OIDC callback (state mismatch, allowlist reject, session rotation) with a mock provider; session middleware (expired/inactive); CSRF; dev/password modes.
- **Exit:** login round-trip works in dev (devauth) and against a mock OIDC.

### Phase 4 — REST API + upload **[seq after 2 & 3; handlers mock site.Service in tests]**
- `api/router.go` + middleware chain (reqid→slog→recover→CORS→session-auth→csrf). `errors.go` (typed→RFC7807-ish envelope; `statusFor` switch). `me.go`, `config.go`.
- Resource handlers calling `site.Service`: `sites.go` (list/get/create/rename/delete), `files.go` (read/write/delete, baseSHA), `branches.go`, `publish.go`, `history.go` (log/diff/rollback), `members.go`, `tokens.go` (issue/list/revoke — copy-once), `admin.go`, `upload.go` (streamed multipart → tmp → `ImportZip`, hard size cap).
- **OpenAPI:** author `openapi.yaml` in lockstep; generate Go types (`internal/openapi`) + run kin-openapi conformance test; this is also the input for the frontend client (Phase 7).
- **Tests:** each handler table-driven with `FakeService` + mock Store; authz matrix (role × action); upload guard rejections; conflict→409 envelope.
- **Exit:** full REST surface green; `openapi.yaml` conformance test passes.

### Phase 5 — Data-plane serving **[∥ with Phase 4 after Phase 2]**
- `resolve/resolver.go` (Host + path fallback, `--` split, control-vs-project, effective-host) per routing-and-serving.md §2–4.
- `serve/treeprovider.go` (materialized `served/published/` + on-demand preview checkout, atomic `rename` swap, LRU/TTL eviction), `headers.go` (CSP + nosniff + frame-ancestors + SVG `script-src 'none'`), `static.go` (index/trailing-slash/MIME/404/methods/ETag/cache), `authz.go` (preview-grant → host-only `kotoji_preview`), `basehref.go` (path-mode injection).
- Wire `RUN_MODE=serve` / `all` in `cmd/kotojid` (`:8081`).
- **Tests:** the big resolver table (both base domains); static (MIME/index/traversal/methods/headers/cache/base-href); preview authz (grant flow, wrong-site reject, 404-not-401); integration `httptest` incl. atomic-swap-no-half-tree.
- **Exit:** routing-and-serving.md §11 URL table passes end-to-end.

### Phase 6 — MCP server **[∥ after Phase 2; depends token DDL from Phase 1]**
- `mcpserver/verifier.go` (prefix fast-reject → hash lookup → scope/expiry/revoke), `registry.go` (`guard` decorator: site-pin + scope check + path confinement), `tools.go` (the 10 tools + `create_branch`), `limits.go` (size + per-token rate buckets, `Limiter` iface). Mount `/mcp` on control plane only; Streamable HTTP **stateless** (P1-9); no cookies/CORS-credentials.
- **Tests:** verifier (valid/malformed/revoked/expired/DB-error); scope×tool matrix; **pivot test** (no tool struct has a site field; mock asserts always called with `claims.SiteID`); per-tool behavior (baseSHA conflict→`current_sha`+`changed_paths`, push-fail→warning-not-error, binary base64, etc.); limits/rate; integration with the real SDK transport + real git; security regression (token A can't reach site B; revoke mid-session).
- **Exit:** MCP test matrix (mcp.md §12) green; a real Claude client can read→write→publish a seeded site.

### Phase 7 — Frontend: tokens → API client → atomic components → pages **[∥ tracks after API exists]**
- **7a [seq]** Generate TS client from `openapi.yaml` (`openapi-typescript` + `openapi-fetch`), `lib/api/{client,keys,error,hooks}`. CI drift gate (regenerate → fail if dirty).
- **7b [∥]** Atomic build bottom-up: `ui/` (shadcn) → `atoms/` (Button/StatusBadge/CodeText/Spinner/…) → `molecules/` (FormField/ProjectCard/BranchSelect/CopyableUrl/ConfirmDialog/EmptyState/CommitItem/…) → `organisms/` (TopNav/AppSidebar/ProjectGrid/FileTree/MonacoEditorPanel/DiffViewer/BranchBar/PublishPanel/HistoryTimeline/UploadDropzone/MemberTable/CreateSiteForm/ConflictResolver/CommandPalette/MCP-token panel). Each with RTL + jest-axe tests. (design.md §3.)
- **7c [∥]** Templates (Auth/Dashboard/ProjectDetail split-pane/Admin) + responsive bands.
- **7d [seq after 7b/7c]** Pages: Login → Dashboard → CreateSite → ProjectDetail tabs (Files/Editor, Branches, Publish, History, Members, Settings incl. MCP tokens) → Admin. TanStack hooks per resource; loading/error/empty triplets; route guards via `/api/me`; Monaco lazy `ssr:false` + theme sync; next-intl ja messages.
- **Exit:** all screens functional against the live backend; a11y lint clean; mobile/tablet/desktop QA at 375/768/1280.

### Phase 8 — Webhook, hardening, ops **[∥ tail]**
- `webhook/github.go` (HMAC constant-time → `FetchAndUpdate` → publish refresh → activity row).
- Rate limiters (API per-session, serve per-IP), `git gc --auto`, soft-delete reaper + backup `git bundle`, startup stale-lock + orphan-repo consistency check.
- Optional Prometheus metrics (lock-wait, git-op duration, mirror failures).
- **Exit:** webhook-driven publish works; ops jobs scheduled; metrics exposed.

---

## 4. Parallelization summary

```
P0 Foundation ─┐
               ├─► P1 DB+sqlc ─┬─► P2 SiteService ─┬─► P4 REST+upload ─┐
               │               │                    ├─► P5 Data plane  ─┤
               │               │                    └─► P6 MCP         ─┤
               │               └─► P3 Auth ─────────────────────────────┤
               └────────────────────────────────────────────────────────► P7 Frontend ─► P8 hardening
```

- **Hard serial spine:** P0 → P1 → **P2** → (P4/P5/P6 fan out). P2 (SiteService) is the keystone; nothing real builds before its interface is frozen and its fake exists.
- **Parallel once P2 lands:** P4 (REST), P5 (data plane), P6 (MCP) are independent — all consume `site.Service`, all mock it in tests. P3 (Auth) can start right after P1 (it only needs the `users`/`sessions`/`identities` tables) and run alongside P2.
- **Frontend (P7)** starts its atomic components (7b/7c) anytime after P0 (pure UI against mocked hooks); 7a/7d need `openapi.yaml` (produced in P4) to wire real data.
- **Two-person split that works well:** Dev A = P1→P2→P5/P6 (Go core/git/serve/MCP); Dev B = P3 (auth) then P4 (REST) then P7 (frontend). They meet at `openapi.yaml`, which both author against.

## 5. The single biggest project risk + its mitigation

The **two-language type drift** (Go ↔ TS). Mitigation is non-negotiable and built in from
P4: `openapi.yaml` is the single source of truth; Go DTOs and the TS client are both
*generated* from it; a **CI staleness gate** (regenerate → fail if the working tree
changes) makes drift unmergeable. The MCP tool schemas are validated separately by the Go
SDK's struct→JSON-schema registration, with a test asserting they match the documented
contract. Do not let any hand-written DTO exist on either side of the wire.
