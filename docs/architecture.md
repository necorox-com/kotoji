# kotoji — Architecture (implementation-level)

> Status: design-locked, pre-alpha. This document turns the [README](../README.md) and the
> locked Japanese spec (`プロダクト/kotoji/仕様書.md`) into an implementation-ready blueprint:
> concrete Go package layout, interface signatures, SQL DDL, request flows, deployment
> topology, the env matrix, the Go↔Next API contract, and an exhaustive gap / 考慮漏れ analysis.
>
> Companion documents (authored alongside this one):
> - `docs/contracts/` — OpenAPI spec + MCP tool schemas (the API source of truth).
> - `docs/siteservice.md` — full `SiteService` interface contract (referenced here).
> - `docs/db-schema.md` — full DDL + sqlc query catalog (the DDL here is the canonical subset).
>
> Everything below is *within* the locked decisions. Where a decision was under-specified, the
> resolution is called out inline and re-listed in [§8 Gap analysis](#8-gap-analysis--考慮漏れ).

---

## 0. Vocabulary & invariants

| Term | Meaning |
|---|---|
| **site** | One hosted web tool. `1 site = 1 git repo = 1 UUID = 1 handle`. |
| **uuid** | Immutable PK. Storage path, git identity, internal references. Never shown in URLs. |
| **handle** | Mutable, unique, DNS-safe english name. Used in subdomain + GitHub repo name. |
| **branch** | Generalized: `published` (prod), `draft` (default working), `feature-*` (per-user/AI). |
| **control plane** | Go backend (API/auth/MCP) + Next.js frontend (UI). The "operate" surface. |
| **data plane** | Go static serving that resolves `{handle}/{branch}` from the `Host` header. The "consume" surface. |
| **SiteService** | The single Go interface that touches git. The DI boundary. The only writer to `/data/sites`. |

**Hard invariants (enforced in code, asserted in tests):**

1. *Nothing* writes to `/data/sites/**` except `SiteService`. The data plane and DB layer are **read-only** w.r.t. git.
2. Every mutation (zip/Monaco/MCP) → a git commit on a branch. No "loose" file writes.
3. Every write carries a **base commit SHA**; the server rejects on mismatch (optimistic lock).
4. `published` is *only* advanced by an explicit `publish` action (or a GitHub-merge webhook), never by a save.
5. The data plane never trusts the proxy for authn/authz of *private* branches — it re-resolves and re-checks.

---

## 1. System diagram

```
                            INTERNET
                               │
              ┌────────────────┴─────────────────┐
              │   NPM / Caddy / nginx (proxy)     │   TLS terminates here.
              │   wildcard cert *.hosting.ex.com  │   Proxy is "dumb": routes by Host only,
              │   + cert hosting.example.com      │   holds NO authority over {handle}/{branch}.
              └───┬───────────────────────────┬───┘
                  │                           │
   Host = hosting.example.com          Host = *.hosting.example.com
   (the bare control-plane host)       (any per-site / preview subdomain)
                  │                           │
        ┌─────────┴──────────┐                │
        │  path-based split   │               │
        │  (in the proxy)     │               │
        │                     │               │
   /  (+ static UI)      /api  /auth  /mcp     │
        │                     │               │
        ▼                     ▼               ▼
 ┌─────────────┐      ┌───────────────────────────────────────────────┐
 │  FRONTEND   │      │                BACKEND (Go, one module)         │
 │ Next.js 16  │      │                                                 │
 │ standalone  │      │  ┌───────────┐  ┌───────────┐  ┌─────────────┐ │
 │  :3000      │      │  │ api (REST)│  │ auth(OIDC)│  │ mcp (SSE)   │ │
 │  UI only;   │      │  │  /api/*   │  │  /auth/*  │  │  /mcp       │ │
 │  calls      │      │  └─────┬─────┘  └─────┬─────┘  └──────┬──────┘ │
 │  backend    │      │        │              │               │        │
 │  via /api   │      │        └──────────────┼───────────────┘        │
 └─────────────┘      │                       ▼                        │
                      │              ┌──────────────────┐              │
                      │              │   SiteService    │  ← DI seam   │
                      │              │  (git interface) │  ONLY writer │
                      │              └────────┬─────────┘              │
                      │                       │                        │
                      │   ┌───────────────────┼──────────────────┐     │
                      │   │  serve (DATA PLANE, same binary,      │     │
                      │   │  'serve' run-mode OR sibling process) │     │
                      │   │  Host resolver → read published/branch│     │
                      │   │  tree (read-only) → security headers  │     │
                      │   └───────────────────┬──────────────────┘     │
                      └───────────┬───────────┼────────────────────────┘
                                  │           │
                    ┌─────────────▼──┐   ┌────▼───────────────────────┐
                    │  PostgreSQL 18 │   │  /data/sites/{uuid}/.git    │
                    │ users, sites,  │   │  (bare repo, 1 per site)    │
                    │ sessions,      │   │  + /data/published/{uuid}/  │
                    │ tokens, audit  │   │    (worktree checkout)      │
                    │ (metadata only)│   │  + /data/tmp (uploads)      │
                    └────────────────┘   └─────────────┬───────────────┘
                                                        │ mirror push / fetch
                                                        ▼
                                              GitHub remote (backup,
                                              PR delegation, webhook)
```

### 1.1 Proxy routing rules (the only thing the proxy knows)

The proxy is configured **once** and never again per-site. Two Proxy Hosts:

**A. `hosting.example.com` (bare control host)** — *path-based* split, all to internal services:

| Path prefix | Upstream | Notes |
|---|---|---|
| `/api/` | backend `:8080` | REST JSON API |
| `/auth/` | backend `:8080` | OIDC login/callback/logout |
| `/mcp` | backend `:8080` | MCP SSE/Streamable HTTP. Long-lived; disable proxy buffering, 7d read timeout. |
| `/healthz`, `/readyz` | backend `:8080` | liveness/readiness |
| everything else (`/`, `/_next/`, …) | frontend `:3000` | Next.js standalone server |

**B. `*.hosting.example.com` (wildcard, every site + preview)** — *single* upstream:

| Match | Upstream | Notes |
|---|---|---|
| `*` | backend data-plane `:8081` | The Go `serve` mode. It re-parses the `Host` header itself. |

> The proxy passes `Host` unchanged (`proxy_set_header Host $host`). The data plane is the
> authority for `{handle}` / `{branch}` resolution — see [§3e](#3e-data-plane-get).

> **Path-based fallback** (`/host/{handle}/...` and `/host/{handle}--{branch}/...`) is served by
> the *same* resolver abstraction (`serve.Resolver`), so a proxy that can't do wildcards still works.

---

## 2. Repo layout (monorepo)

```
kotoji/
├── README.md  LICENSE  .gitignore
├── docs/
│   ├── architecture.md            ← this file
│   ├── siteservice.md             ← SiteService contract
│   ├── db-schema.md               ← full DDL + sqlc catalog
│   └── contracts/
│       ├── openapi.yaml           ← Go↔Next REST contract (source of truth)
│       └── mcp-tools.md           ← MCP tool JSON schemas
│
├── backend/                       ← Go 1.24+, single module (go.mod: github.com/necorox-com/kotoji/backend)
│   ├── go.mod  go.sum
│   ├── sqlc.yaml                  ← sqlc config (engine: postgresql, sql package: pgx/v5)
│   ├── cmd/
│   │   ├── kotojid/main.go        ← the server. flag/env RUN_MODE selects sub-servers.
│   │   └── kotoji-migrate/main.go ← goose wrapper (or use goose CLI directly)
│   ├── internal/
│   │   ├── config/                ← typed Config struct + env parsing + validation
│   │   │   └── config.go
│   │   ├── app/                   ← composition root: wires DI, builds http.Servers
│   │   │   └── app.go             ←   constructs SiteService, Stores, Providers, mounts routers
│   │   ├── api/                   ← REST handlers (control plane). chi router.
│   │   │   ├── router.go          ←   mounts /api/*; middleware chain
│   │   │   ├── middleware.go      ←   reqid, slog, recover, CORS, session-auth, csrf
│   │   │   ├── sites.go files.go branches.go publish.go history.go members.go admin.go me.go
│   │   │   └── errors.go          ←   typed → JSON error envelope (RFC 7807-ish)
│   │   ├── auth/                  ← AuthProvider abstraction + session store
│   │   │   ├── provider.go        ←   AuthProvider interface
│   │   │   ├── oidc.go            ←   go-oidc/v3 + x/oauth2 impl (Google default)
│   │   │   ├── devauth.go         ←   no-auth + admin-password modes
│   │   │   └── session.go         ←   server-side session (Postgres-backed)
│   │   ├── siteservice/           ← THE DI BOUNDARY (interface lives here)
│   │   │   ├── service.go         ←   type SiteService interface { ... }
│   │   │   ├── types.go           ←   DTOs: FileEntry, CommitInfo, DiffEntry, WriteResult...
│   │   │   ├── errors.go          ←   ErrSHAMismatch, ErrReservedHandle, ErrNotFound...
│   │   │   └── mock/mock.go       ←   generated/handwritten mock for table-driven tests
│   │   ├── git/                   ← default SiteService impl (git CLI via os/exec; go-git for reads)
│   │   │   ├── gitservice.go      ←   implements siteservice.SiteService
│   │   │   ├── exec.go            ←   safe os/exec wrapper (no shell, arg arrays, ctx, timeout)
│   │   │   ├── repo.go            ←   per-repo lifecycle (init, worktree, gc)
│   │   │   ├── lock.go            ←   per-repo keyed mutex (in-proc) + flock (cross-proc)
│   │   │   └── mirror.go          ←   GitHub remote add / mirror push / fetch
│   │   ├── serve/                 ← DATA PLANE
│   │   │   ├── server.go          ←   http.Handler; security headers; read-only file serve
│   │   │   ├── resolver.go        ←   Resolver interface (Host-based + path-based impls)
│   │   │   └── publishtree.go     ←   reads /data/published/{uuid} (see §4)
│   │   ├── mcp/                   ← MCP server (official Go SDK), HTTP/SSE
│   │   │   ├── server.go          ←   tool registration + transport
│   │   │   ├── tools.go           ←   list_sites, read_file, write_file, save, publish, ...
│   │   │   └── auth.go            ←   per-project token scope check
│   │   ├── store/                 ← DB access (sqlc-generated + thin wrappers)
│   │   │   ├── db/                ←   sqlc OUTPUT (queries.sql.go, models.go, db.go) — generated
│   │   │   ├── queries/*.sql      ←   sqlc INPUT (hand-written SQL)
│   │   │   ├── sites.go users.go sessions.go tokens.go audit.go  ← wrappers w/ domain types
│   │   │   └── tx.go             ←   pgx pool + tx helper
│   │   ├── handle/                ← handle validation + reserved words + normalization
│   │   │   └── handle.go
│   │   ├── webhook/               ← GitHub webhook receiver (HMAC verify → pull → redeploy)
│   │   │   └── github.go
│   │   └── observability/         ← slog setup, request-id, metrics (optional Prometheus)
│   │       └── log.go
│   └── migrations/                ← goose .sql files (NNNN_description.sql)
│       └── 0001_init.sql ...
│
├── frontend/                      ← Next.js 16 App Router, React 19, TS strict
│   ├── package.json  tsconfig.json  next.config.ts
│   ├── postcss.config.mjs         ← @tailwindcss/postcss
│   ├── app/
│   │   ├── globals.css            ← @theme tokens + base layer
│   │   ├── layout.tsx             ← ThemeProvider, QueryProvider, Toaster
│   │   ├── (auth)/login/page.tsx
│   │   ├── (app)/
│   │   │   ├── dashboard/page.tsx
│   │   │   ├── sites/new/page.tsx
│   │   │   ├── sites/[handle]/page.tsx        ← project detail (tree+Monaco+branches+publish+history)
│   │   │   └── admin/page.tsx
│   ├── components/                ← atomic design: atoms / molecules / organisms / templates
│   │   └── (see docs/responsive-design.md — companion doc)
│   ├── lib/
│   │   ├── api/                   ← GENERATED TS client (from openapi.yaml) — do not hand-edit
│   │   ├── query.ts              ← TanStack Query setup
│   │   └── auth.ts              ← /api/me guard helpers
│   └── public/
│
└── deploy/
    ├── docker-compose.yml         ← backend, frontend, postgres
    ├── docker-compose.dev.yml     ← + adminer, live reload, dev-auth on
    ├── backend.Dockerfile         ← multi-stage; final stage INCLUDES git binary
    ├── frontend.Dockerfile        ← Next.js standalone
    ├── .env.example
    └── npm/                       ← sample NPM/Caddy/nginx configs for prod & dev notes
```

### 2.1 Why this split

- `siteservice` (interface) is *separate* from `git` (impl) so handlers depend on the interface, not the
  implementation. `siteservice/mock` enables table-driven tests of `api`, `mcp`, and `serve` with **no real git**.
- `serve` imports `siteservice` (or a narrower read-only sub-interface, see §4) but is wired so it can run
  in-process or as a sibling binary unchanged.
- `store/db` is the sqlc generated output and must never be edited by hand; `store/*.go` wrap it with domain types.

---

## 2.2 The SiteService interface (the contract handlers code against)

> Full doc in `docs/siteservice.md`; the signature here is canonical. All methods take `ctx` first and
> return typed errors from `siteservice/errors.go`. **No method exposes a raw file path** — paths are
> repo-relative, validated, and never reach `os.*` without cleaning (see [§8 path traversal](#83-correctness-gaps)).

```go
package siteservice

// SiteService is the ONLY component that touches git on disk.
// Every writer (zip upload, Monaco, MCP) funnels through this interface.
type SiteService interface {
    // --- lifecycle ---
    CreateSite(ctx context.Context, in CreateSiteInput) (SiteRef, error)        // git init bare + seed draft
    InitFromZip(ctx context.Context, ref SiteRef, branch string, zr *zip.Reader, author Author) (CommitInfo, error)
    DeleteSite(ctx context.Context, ref SiteRef) error                          // archive then rm (soft, §8.4)

    // --- read (go-git or git cat-file; never mutate) ---
    ListBranches(ctx context.Context, ref SiteRef) ([]BranchInfo, error)
    ListFiles(ctx context.Context, ref SiteRef, branch, dir string) ([]FileEntry, error)
    ReadFile(ctx context.Context, ref SiteRef, branch, path string) (FileBlob, error) // includes blob SHA + content
    HeadSHA(ctx context.Context, ref SiteRef, branch string) (string, error)

    // --- write (git CLI; per-repo lock; base SHA enforced) ---
    WriteFile(ctx context.Context, in WriteFileInput) (WriteResult, error)      // stage one file (in-tree)
    Commit(ctx context.Context, in CommitInput) (CommitInfo, error)            // "save"
    Publish(ctx context.Context, ref SiteRef, opts PublishOptions) (CommitInfo, error) // draft→published fast-fwd/merge
    Rollback(ctx context.Context, ref SiteRef, branch, toSHA string, author Author) (CommitInfo, error)
    CreateBranch(ctx context.Context, ref SiteRef, name, fromBranch string) (BranchInfo, error)
    DeleteBranch(ctx context.Context, ref SiteRef, name string) error

    // --- history ---
    Log(ctx context.Context, ref SiteRef, branch string, limit, offset int) ([]CommitInfo, error)
    Diff(ctx context.Context, ref SiteRef, fromRef, toRef string) ([]DiffEntry, error)

    // --- github mirror ---
    SetRemote(ctx context.Context, ref SiteRef, url string) error
    MirrorPush(ctx context.Context, ref SiteRef, branches ...string) error      // best-effort, async-able
    FetchAndUpdate(ctx context.Context, ref SiteRef, branch string) (CommitInfo, error) // webhook pull

    // --- data-plane support ---
    OpenTree(ctx context.Context, ref SiteRef, branch string) (TreeFS, error)   // read-only fs.FS over a branch
}

// SiteRef carries the UUID (storage identity), not the handle.
type SiteRef struct{ UUID uuid.UUID }

// WriteFileInput — base SHA is REQUIRED for optimistic locking.
type WriteFileInput struct {
    Ref      SiteRef
    Branch   string
    Path     string      // repo-relative, validated by caller AND re-validated here
    Content  []byte
    BaseSHA  string      // branch HEAD the client based its edit on; mismatch → ErrSHAMismatch
    Author   Author
}

type Author struct{ Name, Email string; UserID uuid.UUID } // git author = real identity for audit
```

The default impl `git.GitService` wraps the **git CLI via `os/exec`** (arg arrays, never a shell string) for
write fidelity, and may use **go-git** for pure reads (`ReadFile`, `ListFiles`, `OpenTree`).

---

## 3. Request flows

### 3a. Upload zip → commit → serve

```
Browser ──POST /api/sites/{handle}/upload (multipart: file=.zip, branch=draft, baseSHA) ──▶ backend api.sites
  1. auth middleware: resolve session → user; authz: user can write this site?
  2. CSRF check (custom header; see §8.1).
  3. Stream upload to /data/tmp/{rand}.zip with a hard MAX_UPLOAD_BYTES cap on the io.Copy.
  4. Open as archive/zip; run ZIP GUARDS before extracting (see §8.1):
        - file count ≤ ZIP_MAX_FILES
        - per-file declared+actual uncompressed size, running total ≤ ZIP_MAX_TOTAL_BYTES
        - compression ratio guard (zip-bomb)
        - each name: reject absolute, '..', backslash, NUL, symlink entries
        - extension allowlist (ZIP_ALLOWED_EXT)
  5. SiteService.InitFromZip(ref, "draft", zipReader, author):
        - acquire per-repo lock
        - verify branch HEAD == baseSHA (if site non-empty) → else ErrSHAMismatch
        - write blobs into a fresh tree (git mktree / git read-tree from clean), commit on draft
        - release lock
  6. Best-effort MirrorPush(ref, "draft") (queued; failure does not fail the request — §8.2).
  7. store.audit.Insert(upload event); return {commitSHA, files[]}.
Later: visiting {handle}--draft.hosting.example.com → data plane reads draft tree → 200.
```

### 3b. Monaco save → POST backend → SiteService commit → mirror push

```
Editor (frontend) holds: handle, branch, path, content, baseSHA (HEAD when file was opened).
Browser ──PUT /api/sites/{handle}/branches/{branch}/files?path=... (JSON: content, baseSHA) ──▶ api.files
  1. auth + authz(write) + CSRF.
  2. handle→uuid lookup (store.sites). path validated by handle/path rules.
  3. SiteService.WriteFile{...baseSHA...}  then  SiteService.Commit (or a combined Save):
        - per-repo lock
        - branch HEAD == baseSHA ? no → return 409 ErrSHAMismatch (client must reload+merge)
        - stage content, commit (author = user identity), get newSHA
        - unlock
  4. async MirrorPush(ref, branch).  audit insert.
  5. 200 {commitSHA: newSHA, baseSHA→newSHA}. Frontend updates its baseSHA to newSHA.
```

> "Save also pushes" = interpretation #1: the **server is the source of truth**; save commits locally and
> *mirror-pushes* to GitHub for backup/external diff. **push ≠ publish.**

### 3c. MCP write → commit

```
Personal-PC AI (Claude) ──MCP over HTTP/SSE──▶ backend mcp.server (/mcp)
  1. Transport auth: bearer token in header → store.tokens lookup → scope = {site_uuid, perms}.
  2. Tool call write_file{handle|uuid, branch, path, content, baseSHA}:
        - mcp.auth: token scope MUST include this site_uuid + write → else error -32001 (unauthorized).
        - SAME path: SiteService.WriteFile (base SHA required) → Commit.
  3. Tool save{...} → Commit. publish{...} → Publish. Identical funnel as Monaco/Upload.
  4. audit insert (actor=token's user, via=mcp). async MirrorPush.
```

MCP tools and REST handlers both call the *same* `SiteService` methods — the funnel invariant.

### 3d. Publish (draft → published)

```
Browser ──POST /api/sites/{handle}/publish (JSON: fromBranch=draft, baseSHA) ──▶ api.publish
  1. auth + authz(publish perm — may differ from write; §8.1 private-preview note).
  2. SiteService.Publish(ref, {From: "draft", BaseSHA: ...}):
        - per-repo lock
        - verify draft HEAD == baseSHA
        - fast-forward published → draft  (or merge if published diverged via GitHub; §8.2)
        - checkout/refresh /data/published/{uuid} worktree to published HEAD  (see §4)
        - unlock
  3. store.sites.UpdatePublishState(published_sha, published_at). async MirrorPush(published).
  4. 200. {handle}.hosting.example.com now serves the new tree.
Non-engineer variant: "Request publish" → opens/updates a GitHub PR via mirror, does NOT auto-publish.
  Merge on GitHub → webhook (§3f) advances published.
```

### 3e. Data-plane GET

```
Visitor ──GET https://expense-calc--draft.hosting.example.com/reports/q1.html──▶ proxy ──▶ serve :8081
  1. serve.Resolver.Resolve(Host="expense-calc--draft.hosting.example.com"):
        - strip the configured base domain (HOSTING_BASE_DOMAIN) → label = "expense-calc--draft"
        - split on "--": handle="expense-calc", branch="draft"  (no "--" ⇒ branch="published")
        - validate handle (reuse handle validator); reject reserved/invalid → 404 (not 400, no info leak)
  2. store.sites.LookupByHandle(handle) → uuid, visibility, branch policy.
  3. AUTHZ: if branch != "published" AND site/preview is private → require a valid preview session/token,
        else 401/redirect to control-plane login (§8.1 private-preview). published is public-by-config.
  4. SiteService.OpenTree(uuid, branch) → fs.FS  (served from /data/published/{uuid} for published,
        or an ephemeral checkout / git-archive cache for preview branches — §4).
  5. Path resolution: clean URL path → repo-relative; directory ⇒ append "index.html";
        trailing-slash + index.html rules (§8.3). 404 page if missing.
  6. MIME by extension allowlist; SECURITY HEADERS injected (CSP, nosniff, frame-ancestors, etc. §8.1).
  7. Stream file (read-only). ETag = blob SHA; cache headers per branch (published: cacheable; preview: no-store).
```

### 3f. GitHub mirror push + webhook redeploy

```
Save/Publish ──async──▶ git.MirrorPush ──▶ GitHub remote (per-site repo)   [backup + external diff]

Maintainer merges a PR into `published` on GitHub
  ──▶ GitHub webhook POST /api/webhooks/github (X-Hub-Signature-256)
  1. webhook.github: verify HMAC with GITHUB_WEBHOOK_SECRET (constant-time). reject else.
  2. map repo → site uuid (store.sites by github_repo). ignore non-published ref pushes (configurable).
  3. SiteService.FetchAndUpdate(ref, "published"):
        - per-repo lock; git fetch origin; fast-forward local published → origin/published
        - refresh /data/published/{uuid} worktree.   (reject non-FF unless FORCE policy set — §8.2)
  4. store.sites.UpdatePublishState. audit(via=webhook). 200 quickly (do heavy work async if needed).
```

### 3g. Login OIDC flow

```
Browser ──GET /login (frontend)──▶ "Sign in with Google" → href = /auth/login?next=/dashboard
  1. ──GET /auth/login──▶ backend auth.oidc:
        - generate state + nonce + PKCE verifier; store in a short-lived signed, HttpOnly cookie
          (or server-side ephemeral row keyed by state) — see §8.1 session-fixation/CSRF-on-callback.
        - 302 → Google authorization endpoint (scope: openid email profile; hd hint if configured).
  2. Google authenticates user → 302 → /auth/callback?code&state
  3. ──GET /auth/callback──▶ auth.oidc:
        - verify state matches cookie; exchange code (+PKCE) for tokens; verify id_token (iss, aud,
          exp, nonce) via go-oidc verifier; extract email, sub, hd.
        - ALLOWLIST check: email domain == AUTH_GOOGLE_HD and/or email ∈ AUTH_ALLOWED_EMAILS → else 403.
        - upsert users row (by oidc_sub); CREATE a NEW server-side session (rotate id → anti-fixation);
          set opaque session cookie: HttpOnly, Secure, SameSite=Lax, Domain=hosting.example.com.
        - 302 → next (default /dashboard).
  4. Frontend route guard calls GET /api/me → {user} or 401 → redirect /login.
```

> **AuthProvider** abstraction (`auth.provider.go`) lets `oidc.go` (Google default; Keycloak/Authentik/Azure/GitHub
> via config) and `devauth.go` (no-auth / admin-password) be swapped by `AUTH_MODE`.

---

## 4. Deployment topology & the data-plane read strategy

### 4.1 Docker Compose services

```yaml
# deploy/docker-compose.yml (essential shape)
services:
  postgres:
    image: postgres:18
    environment: [POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB=kotoji]
    volumes: [pgdata:/var/lib/postgresql/data]
    healthcheck: pg_isready

  backend:
    build: { context: .., dockerfile: deploy/backend.Dockerfile }   # multi-stage; final image HAS `git`
    environment: [ ...full env matrix §6... , RUN_MODE=all ]         # all = api+auth+mcp+serve in one process
    volumes:
      - sites:/data/sites           # bare repos (SiteService writes; serve reads)
      - published:/data/published    # published worktrees (serve reads)
      - uploadtmp:/data/tmp          # zip staging (ephemeral)
    depends_on: { postgres: { condition: service_healthy } }
    ports: ["8080", "8081"]         # 8080 control API, 8081 data plane (see RUN_MODE)

  frontend:
    build: { context: .., dockerfile: deploy/frontend.Dockerfile }  # Next.js standalone
    environment: [NEXT_PUBLIC_API_BASE=/ (same-origin via proxy)]
    # No volume mounts; immutable.

volumes: { pgdata: {}, sites: {}, published: {}, uploadtmp: {} }
```

`backend.Dockerfile` final stage: `FROM gcr.io/distroless/base-debian12` *won't* have git; use
`alpine:3.x` + `apk add --no-cache git` (or `debian:stable-slim` + `apt-get install git`). The
**git binary must be present in the runtime image** because `git.GitService` shells out for writes.

### 4.2 Data plane: same binary or sibling?

**Decision: SAME Go binary, selectable run-mode (`RUN_MODE`), with `serve` runnable standalone too.**

- `RUN_MODE=all` → one process serves `:8080` (control) + `:8081` (data). Simplest self-host (the README
  `docker compose up` story). The data plane imports a **read-only sub-interface** of `SiteService`
  (`ReadOnlySiteService { ListFiles, ReadFile, OpenTree, HeadSHA }`) so it *cannot* mutate git even by mistake.
- `RUN_MODE=control` + `RUN_MODE=serve` → two containers sharing the `sites`/`published` volumes
  (control RW, serve RO mount). Lets you scale/restart the data plane independently; satisfies the spec's
  "配信は published を読むだけ／操作プレーンが落ちても公開ツールは生存" durability goal.

**Justification:** one codebase → one resolver, one security-header policy, dev/prod parity, no drift. The
run-mode flag gives the operational split without a second module. The RO sub-interface enforces invariant #1
at compile time when split.

### 4.3 How the data plane reads the tree

Three candidates were weighed:

| Strategy | Pros | Cons | Verdict |
|---|---|---|---|
| **(A) Per-published checkout dir** `/data/published/{uuid}` refreshed on publish/webhook | fastest serve (plain file I/O, `http.FileServer`-like, sendfile, ETag by mtime/SHA); survives if git layer is busy; trivial RO mount | extra disk (≈ working tree size, no `.git`); must keep in sync on publish | **CHOSEN for published** |
| **(B) `git archive`/`cat-file` on each request** | zero extra disk; always exactly the branch HEAD | per-request exec/process cost; concurrency vs writes; harder caching/range requests | rejected for hot path |
| **(C) go-git in-memory tree read** | no checkout dir | re-walks tree per request; memory churn for large sites; no sendfile | used only as a *fallback* |

**Final policy:**
- **Published** → strategy **(A)**: a checked-out worktree at `/data/published/{uuid}`, refreshed atomically on
  `Publish`/webhook. Serve = read-only filesystem walk under that dir. Disk cost is bounded and the README's
  "data plane is just reading the published tree" promise holds literally.
- **Preview branches** (`draft`, `feature-*`) → strategy **(C)** by default (go-git read of the branch tree,
  per-request), with an **optional bounded LRU of ephemeral checkouts** under `/data/preview-cache/{uuid}/{branch}`
  (capped count + TTL eviction) for heavy previews. Previews are lower-traffic, mostly the author's own browser,
  so per-request tree reads are acceptable and we avoid a checkout per branch per site (which would explode disk).

**Atomic publish refresh** (avoid serving a half-written tree): check out to a temp dir
`/data/published/.{uuid}.tmp`, then `rename()` over `/data/published/{uuid}` (atomic on same filesystem).
The serve layer opens files under a path it re-stats per request, so the swap is invisible.

---

## 5. On-disk storage layout under `/data`

```
/data/
├── sites/                          # bare repos — SiteService is the ONLY writer
│   └── {uuid}/
│       └── .git/                   # bare repo (HEAD, refs/heads/{published,draft,feature-*}, objects, config)
│                                    #   config has remote "origin" = GitHub mirror (when linked)
├── published/                      # checked-out published worktrees — serve reads, never writes
│   └── {uuid}/                     # exact contents of refs/heads/published at last publish
│       └── index.html, ...
├── preview-cache/                  # OPTIONAL bounded LRU of ephemeral preview checkouts (TTL-evicted)
│   └── {uuid}/{branch}/...
├── tmp/                            # zip upload staging; cleaned on success/error & on startup
│   └── {rand}.zip
└── backups/                        # OPTIONAL: periodic `git bundle` per repo (see §8.4 operability)
    └── {uuid}/{ts}.bundle
```

Notes:
- Storage path keys on **uuid**, so handle rename never moves bytes (spec invariant).
- `/data/sites/{uuid}/.git` is *bare*: no working tree in the repo dir → writes go via `git --work-tree`/index
  plumbing or a detached temp worktree, keeping the bare repo clean and lock-friendly.
- Quotas (§8.4) tracked per-uuid: sum of `objects` size + published worktree size.

---

## 6. Config / env matrix

> Backend parses into a typed `config.Config` and **fails fast** on missing-required / invalid values at boot.
> Frontend uses `NEXT_PUBLIC_*` only for values safe to ship to the browser.

### 6.1 Backend (`internal/config`)

| Env var | Default | Req | Meaning |
|---|---|---|---|
| `RUN_MODE` | `all` | – | `all` \| `control` \| `serve`. Selects which http servers boot. |
| `CONTROL_ADDR` | `:8080` | – | control plane (api/auth/mcp) listen addr. |
| `SERVE_ADDR` | `:8081` | – | data plane listen addr. |
| `HOSTING_BASE_DOMAIN` | `hosting.localhost` | ✓(prod) | base for `{handle}[--{branch}].<base>` parsing. |
| `CONTROL_BASE_URL` | `http://hosting.localhost:8080` | ✓ | external URL of control host (for OIDC redirect, cookies, links). |
| `DATABASE_URL` | – | ✓ | pgx DSN `postgres://user:pass@postgres:5432/kotoji?sslmode=...`. |
| `DB_MAX_CONNS` | `10` | – | pgxpool max. |
| `DATA_DIR` | `/data` | – | root for `sites/`, `published/`, `tmp/`. |
| `GIT_BIN` | `git` | – | path to git binary (for os/exec). |
| `AUTH_MODE` | `oidc` | – | `oidc` \| `password` \| `none`. |
| `AUTH_OIDC_ISSUER` | `https://accounts.google.com` | oidc | OIDC discovery issuer. |
| `AUTH_OIDC_CLIENT_ID` | – | oidc | OAuth client id. |
| `AUTH_OIDC_CLIENT_SECRET` | – | oidc | OAuth client secret. |
| `AUTH_OIDC_REDIRECT_URL` | `${CONTROL_BASE_URL}/auth/callback` | – | must match IdP config. |
| `AUTH_GOOGLE_HD` | (empty) | – | restrict to a Google Workspace domain (e.g. `necorox.com`). |
| `AUTH_ALLOWED_EMAILS` | (empty) | – | comma list allowlist (alt/extra to `hd`). |
| `AUTH_ADMIN_PASSWORD` | – | password | bcrypt-checked single-admin password (mode=password). |
| `AUTH_ADMIN_EMAIL` | `admin@kotoji.local` | password | identity for the password admin. |
| `SESSION_COOKIE_NAME` | `kotoji_session` | – | opaque session id cookie. |
| `SESSION_TTL` | `720h` | – | session lifetime (30d). |
| `SESSION_COOKIE_DOMAIN` | (derive from CONTROL_BASE_URL) | – | scope; **must NOT** be `.hosting.example.com` (see §8.1 isolation). |
| `CSRF_COOKIE_NAME` | `kotoji_csrf` | – | double-submit token cookie. |
| `MCP_ENABLED` | `true` | – | toggle MCP server. |
| `MCP_PATH` | `/mcp` | – | mount path. |
| `MCP_TOKEN_TTL` | `2160h` | – | default project-token lifetime (90d). |
| `GITHUB_MIRROR_ENABLED` | `false` | – | global toggle for mirror push. |
| `GITHUB_APP_TOKEN` / `GITHUB_PAT` | – | mirror | credential for push/repo create (prefer a scoped token). |
| `GITHUB_ORG` | – | mirror | org/owner for created repos. |
| `GITHUB_WEBHOOK_SECRET` | – | mirror | HMAC secret for `/api/webhooks/github`. |
| `MAX_UPLOAD_BYTES` | `52428800` (50MB) | – | hard cap on uploaded zip (pre-extract). |
| `ZIP_MAX_TOTAL_BYTES` | `209715200` (200MB) | – | cap on total *uncompressed* size. |
| `ZIP_MAX_FILES` | `2000` | – | max entries in a zip. |
| `ZIP_MAX_RATIO` | `100` | – | zip-bomb: max uncompressed/compressed ratio. |
| `ZIP_ALLOWED_EXT` | `.html,.htm,.css,.js,.mjs,.json,.svg,.png,.jpg,.jpeg,.gif,.webp,.ico,.woff,.woff2,.ttf,.txt,.md,.map,.xml,.csv,.wasm` | – | extension allowlist. |
| `SITE_QUOTA_BYTES` | `524288000` (500MB) | – | per-site disk quota. |
| `USER_SITE_QUOTA` | `50` | – | max sites per non-admin user. |
| `RATE_LIMIT_API_RPS` | `20` | – | per-session API rate limit. |
| `RATE_LIMIT_SERVE_RPS` | `100` | – | per-IP data-plane rate limit. |
| `CORS_ALLOWED_ORIGINS` | `${CONTROL_BASE_URL}` | – | for browser API calls (same-origin in prod via proxy). |
| `LOG_LEVEL` | `info` | – | slog level. |
| `LOG_FORMAT` | `json` | – | `json` \| `text`. |
| `HANDLE_MIN_LEN` / `HANDLE_MAX_LEN` | `2` / `40` | – | handle length bounds. |
| `PUBLISHED_PUBLIC` | `true` | – | published served without auth (set false for fully-private instance). |
| `TRUST_PROXY_HEADERS` | `true` | – | trust `X-Forwarded-*` (only behind the known proxy). |

### 6.2 Frontend (Next.js)

| Env var | Default | Meaning |
|---|---|---|
| `NEXT_PUBLIC_API_BASE` | `` (same-origin) | base for API calls; empty ⇒ relative `/api`. |
| `NEXT_PUBLIC_APP_NAME` | `kotoji` | branding. |
| `NEXT_PUBLIC_AUTH_MODE` | `oidc` | hides/shows login button vs dev banner. |
| `NEXT_PUBLIC_DEFAULT_THEME` | `system` | next-themes default. |
| `PORT` | `3000` | Next standalone listen port. |

---

## 7. Go ↔ Next API contract

**Source of truth: an OpenAPI 3.1 spec at `docs/contracts/openapi.yaml`, authored alongside the Go handlers.**

Flow:
1. **Write OpenAPI first** (or in lockstep) for every `/api/*` route — request/response schemas, error envelope,
   auth scheme. This is the single contract both sides honor.
2. **Backend:** keep handlers hand-written (chi), but **validate against the spec in CI** using a request/response
   conformance test (e.g. `kin-openapi` to load + validate sample payloads, and a golden-test that the registered
   routes ⊇ spec paths). Optionally generate Go server *interfaces* with `oapi-codegen` (types + stubs) and
   implement them — keeps Go DTOs spec-derived. Recommended: `oapi-codegen` for **types only**
   (`-generate types`), hand-write handlers → no framework lock-in, types stay in sync.
3. **Frontend:** generate a **typed TS client** from the same `openapi.yaml` into `frontend/lib/api/` (e.g.
   `openapi-typescript` for types + `openapi-fetch` for a typed client; or `orval` to emit TanStack Query hooks).
   The generated dir is git-ignored-from-edits (commit it, but never hand-edit; regenerate via `npm run gen:api`).
4. **CI gate:** a job runs `oapi-codegen` (Go types) and `openapi-typescript` (TS types) and **fails if the
   working tree changes** — i.e. generated artifacts are stale → drift is impossible to merge.

> **The two-language split's main cost is type drift; the OpenAPI-as-source-of-truth + CI staleness gate is the
> explicit mitigation.** MCP tool schemas live separately in `docs/contracts/mcp-tools.md` and are validated by
> the Go MCP SDK's own schema registration (Go structs → JSON schema), with a test asserting they match the doc.

Error envelope (uniform across `/api`):
```json
{ "error": { "code": "sha_mismatch", "message": "base commit is stale", "detail": {"expected":"<sha>","got":"<sha>"} } }
```
HTTP status mapping: `400` validation, `401` unauth, `403` forbidden, `404` not found, `409` sha_mismatch/handle_taken,
`413` upload too large, `422` reserved/invalid handle, `429` rate limited, `500` internal.

---

## 7.1 Canonical SQL DDL (metadata only — never file content)

> Goose migration `0001_init.sql`. sqlc generates type-safe accessors from `store/queries/*.sql`.
> Postgres 18. UUIDs via `gen_random_uuid()` (pgcrypto/pg builtin). Times `timestamptz`.

```sql
-- users: identity from AuthProvider (or the single password-admin)
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_sub      text UNIQUE,                 -- IdP subject; NULL for password-admin
    email         citext UNIQUE NOT NULL,      -- citext ⇒ case-insensitive uniqueness
    display_name  text NOT NULL DEFAULT '',
    is_admin      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

-- sites: 1 row = 1 git repo. uuid = storage/git identity (immutable). handle = mutable DNS name.
CREATE TABLE sites (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),     -- == /data/sites/{id}
    handle         citext UNIQUE NOT NULL,                         -- DNS-safe; validated in app + below
    owner_id       uuid NOT NULL REFERENCES users(id),
    visibility     text NOT NULL DEFAULT 'private'                 -- 'public'|'private' (published exposure)
                   CHECK (visibility IN ('public','private')),
    default_branch text NOT NULL DEFAULT 'draft',
    published_sha  text,                                           -- HEAD of published at last publish (NULL=never)
    published_at   timestamptz,
    github_repo    text,                                           -- "org/name" when mirror linked (UNIQUE below)
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz,                                    -- soft delete (§8.4)
    CONSTRAINT handle_format CHECK (handle ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$')
);
CREATE UNIQUE INDEX sites_github_repo_uniq ON sites (github_repo) WHERE github_repo IS NOT NULL;
CREATE INDEX sites_owner_idx ON sites (owner_id) WHERE deleted_at IS NULL;

-- handle history: old handle → current site, for rename redirects (301).
CREATE TABLE handle_aliases (
    old_handle  citext PRIMARY KEY,
    site_id     uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- membership / permissions (multi-user). owner is implicit admin of the site.
CREATE TABLE site_members (
    site_id  uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    user_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role     text NOT NULL DEFAULT 'editor'                        -- 'admin'|'editor'|'viewer'
             CHECK (role IN ('admin','editor','viewer')),
    PRIMARY KEY (site_id, user_id)
);

-- server-side sessions: cookie holds opaque id; row holds the truth.
CREATE TABLE sessions (
    id          text PRIMARY KEY,                                  -- opaque, high-entropy (rotated on login — §8.1)
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    user_agent  text,
    ip          inet
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expiry_idx ON sessions (expires_at);

-- MCP / API tokens: per-project scope (spec requirement).
CREATE TABLE access_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    site_id     uuid REFERENCES sites(id) ON DELETE CASCADE,       -- NULL ⇒ account-wide (discouraged)
    name        text NOT NULL,
    token_hash  bytea NOT NULL,                                    -- sha256 of the secret; secret shown once
    scopes      text[] NOT NULL DEFAULT '{read}',                  -- {read,write,publish}
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz,
    last_used_at timestamptz,
    revoked_at  timestamptz
);
CREATE INDEX tokens_lookup_idx ON access_tokens (token_hash) WHERE revoked_at IS NULL;

-- audit log: every mutation (who/what/how). append-only.
CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    actor_id   uuid REFERENCES users(id),
    site_id    uuid REFERENCES sites(id),
    action     text NOT NULL,            -- 'site.create','file.write','save','publish','rollback','token.create'...
    via        text NOT NULL,            -- 'ui'|'mcp'|'upload'|'webhook'|'admin'
    branch     text,
    commit_sha text,
    ip         inet,
    detail     jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_site_idx ON audit_log (site_id, at DESC);

-- reserved words: seedable from config but stored so admins can extend (spec: 管理者…予約語).
CREATE TABLE reserved_handles (
    handle text PRIMARY KEY
);
-- seeded: draft, preview, published, www, api, internal, host, admin, app, static, assets, mcp
```

---

## 8. Gap analysis / 考慮漏れ

> The user explicitly wants 考慮漏れ surfaced. Each item: the gap → recommended resolution. Items marked
> **[OPEN]** are decisions still owed by a human; the rest have a concrete recommendation that I'd implement.

### 8.1 Security gaps

1. **ZipSlip (path traversal on extract).** A zip entry named `../../etc/x` or `/abs/x` escapes the repo.
   → For every entry: reject names containing `..` segments, absolute paths, backslashes, NUL bytes, drive letters;
   `filepath.Clean` then assert the result stays within the dest via a prefix check on the *resolved* path;
   reject any entry whose mode is a **symlink or non-regular file** (symlinks are a second escape vector). Tests must
   include `../`, `..\\`, `a/../../b`, symlink, and unicode-normalization tricks.

2. **Zip bomb (decompression amplification).** A 1KB zip → 10GB on disk/RAM.
   → Enforce `ZIP_MAX_FILES`, `ZIP_MAX_TOTAL_BYTES` (running sum during extraction, abort on exceed), and
   `ZIP_MAX_RATIO` (uncompressed/compressed). Read each entry through an `io.LimitedReader` so a *lying* header
   can't blow the budget. Never trust `f.UncompressedSize64` alone.

3. **Extension allowlist vs hosting arbitrary JS.** We intentionally host JS/HTML (that's the product), so the
   allowlist is about *excluding executables/configs* (`.php`, `.sh`, `.exe`, `.htaccess`, dotfiles), not about
   sandboxing JS. The real JS containment is **CSP + subdomain isolation** (below), not the allowlist.

4. **CSP & content sniffing on served content.** Hosted pages can XSS *within their own origin*; the risk is
   them reaching the control plane or other sites.
   → Data plane sets on **every** response:
   `X-Content-Type-Options: nosniff`,
   `Content-Security-Policy: default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src *; frame-ancestors 'none'; base-uri 'self'; form-action 'self'`
   plus `Referrer-Policy: strict-origin-when-cross-origin`, `Cross-Origin-Opener-Policy: same-origin`,
   `X-Frame-Options: DENY` (legacy). **Caveat:** AI-built tools commonly inline `<script>` and call external
   APIs, so `'unsafe-inline'`/`'unsafe-eval'` in `script-src` and a permissive `connect-src` are *required for the
   product to work* — this is a deliberate, documented trade-off. Make CSP **per-site configurable** (a `csp`
   column / setting) so security-conscious sites can tighten it; ship a sane permissive default.
   **[OPEN]** exact default `connect-src` (fully open `*` vs an egress allowlist) — see SSRF below.

5. **SSRF / egress from hosted in-page fetch.** Hosted JS runs **in the visitor's browser**, so its `fetch`
   goes from the *client*, not the server — no server-side SSRF from page content. BUT: (a) a future API-proxy
   layer (spec's "含み") *would* introduce server-side egress and must allowlist destinations + block
   link-local/metadata IPs (`169.254.169.254`, RFC1918, `::1`); (b) the **webhook** and **mirror push** are the
   only current server egress and target fixed GitHub hosts — pin them. → Document that page `fetch` is
   client-side; gate any server-side fetch (future proxy) behind a destination allowlist + DNS-rebinding-safe IP
   checks. CSP `connect-src` limits where the *page* can talk only as defense-in-depth (and may break tools).

6. **Subdomain cookie / storage isolation.** Each site is its own origin (`a.hosting…` vs `b.hosting…`) so
   `localStorage`, `IndexedDB`, and Origin-scoped cookies are already isolated by the browser. The danger is a
   **too-broad session cookie**: if the control-plane session cookie were set on `Domain=.hosting.example.com`,
   every hosted site could read it.
   → Session cookie `Domain` is the **bare control host only** (`hosting.example.com`, *no* leading dot), never the
   wildcard parent. Data-plane responses set `Cache-Control` and never `Set-Cookie`. Document that operators must
   not host kotoji's control UI on a `*.hosting…` subdomain.
   **[OPEN]** If we ever issue *preview* cookies for private branches, they must be scoped to the exact preview
   host, not the parent — needs a per-host cookie design (below).

7. **Auth on private previews vs public published.** Published is public-by-config (`PUBLISHED_PUBLIC`), but
   `feature-*`/`draft` previews can leak unreleased content if served openly.
   → Data plane (§3e step 3): for non-published branches on a private site, require a **preview session**. Two
   options: (a) reuse the control session — but the cookie is scoped to the control host, not `*.hosting…`, so it
   won't be sent to the preview origin (good for isolation, bad for SSO). (b) issue a **short-lived, host-scoped,
   signed preview cookie** when an authenticated user opens a preview link from the dashboard. Recommend (b):
   `/api/sites/{h}/branches/{b}/preview-grant` mints a cookie scoped to `{h}--{b}.hosting…`. Published stays
   cookieless. **[OPEN]** confirm preview default = private (recommended) vs public.

8. **MCP token scope / leak.** A token leaked from a personal PC could touch all sites if account-wide.
   → Default tokens are **per-site** (`access_tokens.site_id` set). Store only `sha256(secret)`; show secret once.
   Enforce scope on *every* tool call (site_uuid ∈ token scope AND required perm ∈ scopes). Support revoke
   (`revoked_at`), `expires_at`, and `last_used_at` for stale detection. Account-wide tokens allowed but flagged
   in UI as higher-risk. Rate-limit MCP per token.

9. **Path traversal in file APIs (read/write).** `?path=../../other-site` or `..%2f` in REST/MCP.
   → All paths are **repo-relative**, validated by `handle.CleanRepoPath`: reject `..`, leading `/`, NUL,
   backslash, and any segment that's a reserved name; `path.Clean` then re-check no `..` remains. The git layer
   *additionally* operates via the index (blob writes), never via host `os.Open(join(root, userPath))`, so even a
   bypass can't escape the object store. Tests cover encoded/double-encoded traversal.

10. **CSRF on the JSON API.** Cookie-auth + JSON endpoints are CSRF-targets.
    → `SameSite=Lax` session cookie (blocks cross-site top-level POST in modern browsers) **plus** a
    **double-submit CSRF token**: backend sets a non-HttpOnly `kotoji_csrf` cookie; frontend echoes it in
    `X-CSRF-Token`; middleware requires the header to equal the cookie on all unsafe methods. The OIDC callback
    (a GET that mutates) is protected by the `state` parameter instead. MCP is bearer-token (not cookie) so it's
    CSRF-immune.

11. **Session fixation / rotation.** An attacker-set session id pre-login could be inherited.
    → Generate a **new** session id on successful login (don't reuse any pre-auth id); 128-bit+ entropy; store
    server-side; `expires_at` enforced; `last_seen_at` sliding refresh; logout deletes the row. The OIDC `state`
    + `nonce` + PKCE prevent login-CSRF and code-injection on the callback.

12. **Webhook spoofing.** `/api/webhooks/github` is unauthenticated by URL.
    → Verify `X-Hub-Signature-256` HMAC with `GITHUB_WEBHOOK_SECRET` in **constant time**; reject otherwise;
    rate-limit; ignore events for unknown repos. Treat the body as untrusted until verified.

13. **git CLI command injection.** Building git commands from user input (branch names, paths) via a shell.
    → **Never** use a shell string. `exec.CommandContext("git", args...)` with arg *arrays*; user values are
    separate argv elements (no interpolation). Validate branch names against `^[a-z0-9][a-z0-9/_-]*$` and reject
    leading `-` (option-injection: a path starting `-` could be read as a flag — prefix with `--` separator and
    use `git ... -- <path>`). Set `GIT_TERMINAL_PROMPT=0`, scrub env for the child.

14. **Credentials in git config / logs.** GitHub PAT could land in `remote.origin.url` or slog.
    → Store mirror creds out-of-band (env / a Postgres secret), inject via `GIT_ASKPASS`/credential helper or an
    ephemeral header, not in the persisted remote URL. slog redaction for token-shaped values; never log full
    request bodies of token-creation endpoints.

### 8.2 Concurrency & git gaps

1. **Parallel commits to one repo.** Two saves (Monaco + MCP) racing → corrupt index / lost update.
   → **Per-repo keyed mutex** (`git.lock`: `map[uuid]*sync.Mutex` guarded by a sync.Map) for in-process
   serialization, **plus** an OS `flock` on `/data/sites/{uuid}/.git/kotoji.lock` for the `RUN_MODE=control`
   multi-process case. All write methods acquire before touching the repo, with a `ctx` deadline to avoid deadlock.

2. **Base-SHA race (TOCTOU).** Client sends `baseSHA`, but HEAD advances between check and commit.
   → The base-SHA verification happens **inside** the per-repo lock, immediately before the commit, against live
   `HEAD`. So the check-and-commit is atomic. Mismatch → `409 sha_mismatch` with `expected`/`got`; client reloads
   and re-applies (Monaco shows a diff/merge prompt).

3. **Mirror-push conflicts (non-FF).** GitHub `published` diverged (someone pushed/merged there) → local push
   rejected.
   → Mirror push is **best-effort and never blocks the user save**; on non-FF it records a `mirror_diverged`
   flag on the site and surfaces a "needs sync" banner. Reconciliation = `FetchAndUpdate` (webhook path) which
   only fast-forwards; a true divergence requires explicit operator action (we do **not** auto-merge to avoid
   silently overwriting GitHub state). publish is server-authoritative; GitHub is the *backup/PR* surface.

4. **Webhook vs local-edit race.** Webhook fetch advances `published` while a publish is mid-flight.
   → Both go through the same per-repo lock and only **fast-forward**; whichever loses the lock re-checks HEAD and
   no-ops if already current. Non-FF from webhook is rejected + flagged, not force-applied (unless an explicit
   `FORCE` policy, off by default).

5. **Stale git lockfiles.** A crashed `git` leaves `index.lock` → all future writes fail.
   → On repo open / startup, detect a `.git/index.lock` older than a threshold with no holding process and remove
   it (logged as a warning + audit). Our own `kotoji.lock` flock is released by the OS on process death, so it
   self-heals.

6. **Publish atomicity vs serving.** Refreshing `/data/published/{uuid}` while it's being served → partial reads.
   → Build into `/data/published/.{uuid}.tmp`, then atomic `rename` over the live dir (§4.3). Serve re-stats per
   request; the swap is invisible.

### 8.3 Correctness gaps

1. **Handle rename redirects.** Old subdomain `old.hosting…` after rename to `new`.
   → On rename, insert into `handle_aliases(old_handle → site_id)`. Data-plane resolver: if `LookupByHandle`
   misses, check `handle_aliases`; if hit, `301` to the same path on the current handle's host. Control-plane
   links always use the live handle. Renaming *to* a handle that's in `handle_aliases` for the **same** site is
   fine; to one used by another site → `409`.

2. **Reserved words & case-insensitivity.** `Draft`, `API`, mixed-case collisions, IDN homoglyphs.
   → Handle stored as `citext` (case-insensitive unique). Validator lowercases input first; rejects if in
   `reserved_handles` (seeded list, admin-extendable); ASCII-only `[a-z0-9-]` (no IDN/punycode to avoid
   homoglyph spoofing); no leading/trailing/double hyphen (`--` is the branch separator — a handle containing
   `--` would be ambiguous, so **reject `--` in handles**). Length `[HANDLE_MIN_LEN, HANDLE_MAX_LEN]`.

3. **`--` ambiguity in resolution.** `my--tool.hosting…` — is it handle `my` branch `tool`, or handle `my--tool`?
   → Because handles forbid `--`, the **first** `--` always splits handle|branch. Branch names may themselves
   contain `-` but the resolver splits on the *first* `--` only; everything after is the branch (which may include
   `/` for `feature/x` if we allow slashes in branch → but subdomains can't contain `/`, so **preview branch names
   in URLs use `-` not `/`**; map `feature-x` URL ↔ `feature/x` ref, or just standardize on `feature-x` refs).
   **Recommendation:** standardize branch refs as `feature-{user}-{slug}` (no slashes) to keep URL↔ref 1:1.

4. **Collisions on create.** Two users create the same handle concurrently.
   → DB `UNIQUE(handle)` is the final arbiter; the create handler does an insert and maps the unique-violation to
   `409 handle_taken`. The git repo is created **after** the row commits (or in the same tx boundary with cleanup
   on failure) so we never have a repo without a row.

5. **index.html resolution & trailing slash.** `/`, `/foo`, `/foo/`, `/foo/index.html`.
   → Resolver rules: a request for a directory (path ends `/` or maps to a dir) serves `{dir}/index.html`; a
   request `/foo` where `/foo/` is a dir → `301` to `/foo/` (canonical); `/foo` where `foo.html` exists → serve
   it (optional "clean URL" mode, **[OPEN]** default on/off); root `/` → `/index.html`. Missing → custom `404`
   (serve `/404.html` if present, else default).

6. **MIME types & binary assets.** Wrong `Content-Type` breaks JS modules; `nosniff` makes it strict.
   → Map extension → MIME from a fixed table (cover `.mjs`→`text/javascript`, `.wasm`→`application/wasm`,
   `.json`→`application/json`, fonts, images). Serve binary as-is with correct type. Unknown extension (shouldn't
   exist due to allowlist) → `application/octet-stream` + `Content-Disposition: attachment` (never execute).

7. **ETag / caching.** → `ETag = blob SHA` (stable, content-addressed); `If-None-Match` → `304`. Published:
   `Cache-Control: public, max-age=60` (short, since publish changes content); previews: `no-store`.

8. **Empty project / first publish.** A site with no commits, or publish before any draft commit.
   → `CreateSite` seeds an initial empty commit on `draft` with a placeholder `index.html` ("This site is empty")
   so every site is immediately servable and has a base SHA. `Publish` with no draft commits → `422` "nothing to
   publish".

### 8.4 Operability gaps

1. **Disk quotas / runaway repos.** git history + assets grow unbounded.
   → Track per-site size (objects + published worktree) in a periodic job; enforce `SITE_QUOTA_BYTES` on write
   (reject `413` when exceeded) and `USER_SITE_QUOTA` on create. Run `git gc --auto` after N commits; a scheduled
   `git gc` + `git repack` job reclaims dangling objects from rollbacks/force operations.

2. **GC of dangling / orphaned repos.** A repo dir with no DB row (failed create, manual mess), or soft-deleted
   sites' bytes.
   → Soft delete sets `sites.deleted_at`; a reaper job, after a grace period (e.g. 30d), `git bundle`s the repo to
   `/data/backups/{uuid}/` then `rm -rf`s `/data/sites/{uuid}` and `/data/published/{uuid}`. A startup
   consistency check logs (does not auto-delete) repos on disk with no matching row.

3. **Backups.** Postgres + git both hold state.
   → Postgres: standard `pg_dump`/PITR (operator concern; document). git: the GitHub mirror *is* a backup when
   enabled; for non-mirror setups, the scheduled `git bundle` per repo to `/data/backups`. Document a restore
   runbook (repo bundle → `git clone`, DB rows must match uuids).

4. **Audit log.** Required for "who published/overwrote what" (multi-user + AI writers).
   → `audit_log` table (§7.1), appended on every mutation with `actor_id`, `via`, `branch`, `commit_sha`, `ip`.
   Admin UI lists per-site. Append-only (no update/delete grant for the app role).

5. **Rate limits.** AI clients can hammer MCP; uploads can DoS.
   → Per-session API limiter (`RATE_LIMIT_API_RPS`), per-IP data-plane limiter (`RATE_LIMIT_SERVE_RPS`),
   per-token MCP limiter, and a concurrency cap on zip extraction (a bounded worker pool) so big uploads can't
   exhaust CPU/disk in parallel.

6. **Observability.** → structured `slog` (JSON) with request-id propagation; per-request access logs (method,
   path, status, latency, site, actor); `/healthz` (process up) + `/readyz` (DB reachable, `/data` writable);
   optional Prometheus metrics (request counts/latency, git op durations, lock wait time, queue depth, mirror
   failures). Lock-wait-time is a key early-warning metric for repo contention.

7. **Migrations / startup ordering.** Backend may boot before Postgres is migrated.
   → Compose `depends_on: healthy`; backend runs `goose up` on boot (idempotent) behind a singleton advisory lock
   (`pg_advisory_lock`) so multiple backend replicas don't race migrations.

### 8.5 Product edge cases

1. **Single-file `index.html` with root-absolute paths.** AI output often references `/style.css`,
   `/assets/app.js`. Under per-subdomain hosting these resolve to the site root → **they actually work**, which is
   the whole point of subdomain-per-site (vs path-based hosting where `/style.css` would escape the prefix).
   → On the **path-based fallback** (`/host/{handle}/...`), root-absolute paths break. Mitigation: inject a
   `<base href="/host/{handle}/">` into served HTML *only in path-mode* (documented caveat: base-href injection
   can subtly change anchor/`fetch` resolution and is best-effort). Subdomain mode (the default) needs no
   injection. **[OPEN]** confirm we ship base-href injection for path-mode or just document the limitation.

2. **Large files.** A 200MB video in a repo bloats git and blows quotas/RAM on read.
   → Enforce a per-file size cap (`ZIP`/write path) and `SITE_QUOTA_BYTES`; for reads, stream (don't buffer whole
   file in `ReadFile` for serving — `OpenTree`/`fs.File` streams). Document that kotoji is for *tools*, not media
   hosting; large binaries should live elsewhere. **[OPEN]** whether to integrate git-LFS (recommend: no, out of
   scope for v1).

3. **Binary-vs-text in Monaco.** Editing a `.png` in Monaco is nonsense; diff of binary is noise.
   → File API returns an `is_binary` flag (null-byte heuristic + extension); the editor shows a read-only
   "binary asset" panel instead of Monaco; diffs skip binary with a "binary changed" marker.

4. **Concurrent editors on one branch.** Two humans editing the same file → second save hits `sha_mismatch`.
   → This is the intended optimistic-lock behavior; UX must make it recoverable (toast + reload + Monaco
   3-way/2-way diff). Future: soft presence indicators. Not a CRDT (out of scope).

5. **Publish while a preview link is open.** Visitor on `--draft` while author publishes.
   → Independent: draft and published are separate trees; the draft preview keeps serving draft. No action needed,
   but documented.

6. **MCP `create_site` handle clashes / generation.** AI invents a handle that's taken or reserved.
   → Same validator path; return a structured error with `suggestions` (append `-2`, etc.) so the AI can retry.

7. **Default branch & "save" target ambiguity.** Where does a bare `save` (no branch) commit?
   → `save` defaults to `sites.default_branch` (`draft`). MCP/REST may override with explicit `branch`. The
   non-engineer UI hides branches and always uses `draft` → publish.

### 8.6 Cross-cutting [OPEN] decisions owed by a human

- **[OPEN]** Default CSP `connect-src` (open `*` vs egress allowlist) — affects whether AI tools that call third-party
  APIs work out of the box. Recommend `*` default + per-site tightening.
- **[OPEN]** Preview default visibility (private recommended) and the preview-cookie design (host-scoped grant).
- **[OPEN]** "Clean URLs" (`/foo` → `foo.html`) on by default?
- **[OPEN]** Path-mode base-href injection: ship it, or document the limitation only?
- **[OPEN]** git-LFS / large-media policy (recommend: out of scope v1).
- **[OPEN]** Multi-replica backend: is horizontal scaling a v1 goal? (affects flock-vs-advisory-lock and the
  in-proc keyed mutex assumptions — current design supports it but adds the flock requirement).
- **[OPEN]** Branch ref naming: standardize on `feature-{user}-{slug}` (no slashes) to keep URL↔ref 1:1 — confirm.
```
