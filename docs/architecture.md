# kotoji — Architecture (implementation-level)

> Status: SHIPPED & deployed. This document describes the live system: the Go package layout,
> interface signatures, SQL DDL, request flows, deployment topology, the env matrix, the Go↔Next
> API contract, and the gap / 考慮漏れ analysis (most items resolved — see [§8](#8-gap-analysis--考慮漏れ)).
>
> Companion documents:
> - `docs/contracts/CANONICAL.md` — **the law** (per-user token model §6, frozen DDL §4, error
>   taxonomy §3). On any conflict, CANONICAL.md wins and this doc is read for explanation only.
> - `docs/contracts/openapi.yaml` — the current REST contract (source of truth for `/api/*`).
> - `docs/contracts/mcp.md` — MCP tool schemas (per-user token + `site` selector model).
>
> Where this doc and CANONICAL.md disagree, CANONICAL.md is authoritative.

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
| **site.Service** | The single Go interface (`internal/site`) that touches git. The DI boundary. The only writer to `/data/sites`. |

**Hard invariants (enforced in code, asserted in tests):**

1. *Nothing* writes to `/data/sites/**` except `site.Service`. The data plane and DB layer are **read-only** w.r.t. git.
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
              │   Edge proxy (Traefik / NPM /     │   TLS terminates here.
              │   Caddy / nginx).                 │   Proxy is "dumb": routes by Host only,
              │   wildcard *.example.com          │   holds NO authority over {handle}/{branch}.
              │   + cert example.com              │   See §4.4: the base compose is proxy-LESS;
              └───┬───────────────────────────┬───┘   the opt-in edge overlay ships Traefik.
                  │                           │
   Host = example.com                  Host = *.example.com
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
                      │              │   site.Service   │  ← DI seam   │
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
                    ┌─────────────▼──┐   ┌────▼───────────────────────────┐
                    │  PostgreSQL 18 │   │  /data/sites/{uuid}/.git        │
                    │ users, sites,  │   │  (1 repo per site)              │
                    │ sessions,      │   │  + /data/sites/{uuid}/served/   │
                    │ tokens, audit  │   │    {branch}/  (RO worktrees)    │
                    │ (metadata only)│   │  + /data/tmp (uploads)          │
                    └────────────────┘   └─────────────┬───────────────────┘
                                                        │ mirror push / fetch
                                                        ▼
                                              GitHub remote (backup,
                                              PR delegation, webhook)
```

### 1.1 Proxy routing rules (the only thing the proxy knows)

The proxy is configured **once** and never again per-site. Two routes (the opt-in Traefik edge
overlay, `deploy/docker-compose.edge.yml`, encodes exactly these as labels — see §4.4):

**A. `example.com` (bare control host)** — *path-based* split, all to internal services:

| Path prefix | Upstream | Notes |
|---|---|---|
| `/api/` | backend `:8080` | REST JSON API (incl. `/api/webhooks/github`) |
| `/auth/` | backend `:8080` | OIDC login/callback/logout + first-run `/auth/setup` |
| `/mcp` | backend `:8080` | MCP SSE/Streamable HTTP. Long-lived; disable proxy buffering, 7d read timeout. |
| `/healthz`, `/readyz` | backend `:8080` | liveness/readiness |
| everything else (`/`, `/_next/`, …) | frontend `:3000` | Next.js standalone server |

**B. `*.example.com` (wildcard, every site + preview)** — *single* upstream:

| Match | Upstream | Notes |
|---|---|---|
| `*` | backend data-plane `:8081` | The Go `serve` mode. It re-parses the `Host` header itself. |

> The proxy passes `Host` unchanged. The data plane is the authority for `{handle}` / `{branch}`
> resolution — see [§3e](#3e-data-plane-get). The base host derives from `KOTOJI_BASE_DOMAIN`.

> **Path-based fallback** (`/host/{handle}/...` and `/host/{handle}--{branch}/...`) is served by
> the *same* resolver abstraction (`resolve.Resolver`), so a proxy that can't do wildcards still works.

---

## 2. Repo layout (monorepo)

```
kotoji/
├── README.md  LICENSE  .gitignore
├── docs/
│   ├── architecture.md            ← this file
│   └── contracts/
│       ├── CANONICAL.md           ← the law (token model, frozen DDL, error taxonomy)
│       ├── openapi.yaml           ← Go↔Next REST contract (source of truth)
│       └── mcp.md                 ← MCP tool JSON schemas (per-user token + site selector)
│
├── backend/                       ← Go 1.24+, single module (go.mod: github.com/necorox-com/kotoji/backend)
│   ├── go.mod  go.sum
│   ├── sqlc.yaml                  ← sqlc config (engine: postgresql, sql package: pgx/v5)
│   ├── cmd/
│   │   └── kotojid/main.go        ← the server. env KOTOJI_RUN_MODE selects sub-servers.
│   ├── internal/
│   │   ├── config/                ← typed Config struct + env parsing + validation
│   │   │   └── config.go          ←   all vars are KOTOJI_*-prefixed (§6)
│   │   ├── app/                   ← composition root: wires DI, runs boot migrations, builds http.Servers
│   │   │   └── app.go             ←   constructs site.Service, db.Store, secretbox, auth, ops; mounts routers
│   │   ├── api/                   ← REST handlers (control plane). chi router.
│   │   │   ├── router.go          ←   mounts /api/*; middleware chain
│   │   │   ├── sites.go files.go branches.go publish.go history.go members.go upload.go preview.go
│   │   │   ├── tokens.go          ←   PER-USER tokens: /api/tokens (list/create/revoke)
│   │   │   ├── admin.go           ←   /api/admin/* incl. GET|PUT /api/admin/github (instance mirror config)
│   │   │   ├── authz.go           ←   role→capability gate (CANONICAL §6)
│   │   │   └── errors.go          ←   typed → JSON error envelope
│   │   ├── auth/                  ← AuthProvider abstraction + session store + first-run setup
│   │   │   ├── provider.go        ←   AuthProvider interface
│   │   │   ├── oidc.go            ←   go-oidc/v3 + x/oauth2 impl (Google default)
│   │   │   ├── devauth.go         ←   no-auth (dev) + PasswordProvider (DB-hash-then-env)
│   │   │   ├── handlers.go        ←   /auth/login|callback|logout, /auth/setup, /api/me, /api/config
│   │   │   ├── csrf.go            ←   double-submit CSRF guard
│   │   │   └── session.go         ←   server-side session (Postgres-backed)
│   │   ├── site/                  ← THE DI BOUNDARY: site.Service interface + git impl, ONE package
│   │   │   ├── service.go         ←   type Service interface { ... } (CANONICAL §1)
│   │   │   ├── git_service.go     ←   production impl (git CLI via os/exec; go-git for reads)
│   │   │   ├── git_auth.go        ←   GitHub push/fetch auth: env-injected http.<base>.extraHeader
│   │   │   ├── mirror.go          ←   remote add / mirror push / fetch
│   │   │   ├── gitrunner.go       ←   safe os/exec wrapper (no shell, arg arrays, ctx, env)
│   │   │   ├── lock.go            ←   per-repo keyed mutex (in-proc) + flock (cross-proc)
│   │   │   ├── handle.go path.go mime.go zip.go errors.go  ← validation, MIME table, zip guards, taxonomy
│   │   │   ├── fake.go            ←   in-memory Service for table-driven tests of api/mcp/serve
│   │   │   └── prod.go            ←   NewProductionService composition-root factory
│   │   ├── resolve/               ← Host → {handle,branch} resolver (data plane)
│   │   ├── serve/                 ← DATA PLANE http.Handler: security headers, read-only file serve
│   │   ├── preview/               ← signed preview-grant (host-scoped kotoji_preview cookie)
│   │   ├── mcpserver/             ← MCP server (official Go SDK), HTTP/SSE
│   │   │   ├── server.go registry.go ← transport + tool registration
│   │   │   ├── tools.go           ←   list_sites, read_file, write_file, save, publish, ... (take a `site`)
│   │   │   ├── verifier.go        ←   bearer token → user + scopes (per-user token)
│   │   │   └── authz.go           ←   MEMBERSHIP-CAP: effective = token.scopes ∩ roleScopes(membership)
│   │   ├── db/                    ← DB access (sqlc-generated + thin wrappers)
│   │   │   ├── gen/               ←   sqlc OUTPUT (queries.sql.go, models.go) — generated
│   │   │   ├── store.go           ←   *db.Store: pgx pool, WithTx, GitHub config (secretbox), settings
│   │   │   └── migrations/        ←   goose .sql files (embed.go embeds them)
│   │   │       └── 0001_init.sql 0002_seed_reserved.sql 0003_instance_settings.sql 0004_user_tokens.sql
│   │   ├── migrate/               ← boot-time goose runner (advisory-locked)
│   │   ├── secretbox/             ← AES-256-GCM at-rest encryption for the DB-stored GitHub PAT
│   │   ├── ops/                   ← background scheduler: soft-delete reaper + git gc
│   │   ├── ratelimit/             ← per-session / per-IP / per-token limiters
│   │   ├── openapi/               ← spec-derived Go DTOs (oapi-codegen, types only)
│   │   ├── webhook/               ← GitHub webhook receiver (HMAC verify → fetch → redeploy)
│   │   └── observability/         ← slog setup, request-id, recover
│
├── frontend/                      ← Next.js 16 App Router, React 19, TS strict
│   ├── package.json  tsconfig.json  next.config.ts
│   ├── src/
│   │   ├── app/
│   │   │   ├── icon.svg            ← kotoji-tōrō lantern (favicon + brand mark)
│   │   │   ├── (auth)/login/page.tsx              ← login + first-run setup-form
│   │   │   ├── (app)/dashboard/page.tsx
│   │   │   ├── (app)/sites/new/page.tsx
│   │   │   ├── (app)/sites/[handle]/page.tsx      ← project detail (tree+Monaco+branches+publish+history)
│   │   │   ├── (app)/sites/[handle]/settings/page.tsx ← per-project settings
│   │   │   ├── (app)/settings/page.tsx            ← INSTANCE settings: GitHub config + MCP guide + tokens
│   │   │   └── (app)/admin/page.tsx
│   │   ├── components/            ← atomic design: atoms / molecules / organisms / templates / ui
│   │   ├── lib/api/               ← GENERATED TS client (from openapi.yaml) + TanStack Query hooks
│   │   ├── i18n/  messages/       ← next-intl (ja default + en)
│   │   └── hooks/
│
└── deploy/
    ├── docker-compose.yml         ← backend (KOTOJI_RUN_MODE=all) + frontend + postgres. PROXY-LESS base.
    ├── docker-compose.dev.yml     ← dev overlay (live reload, dev-auth)
    ├── docker-compose.edge.yml    ← OPT-IN Traefik v3 edge overlay (turnkey TLS) — see §4.4
    ├── backend.Dockerfile         ← multi-stage; final stage INCLUDES git binary
    ├── frontend.Dockerfile        ← Next.js standalone
    ├── .env.example
    └── npm/                       ← sample NPM/Caddy/nginx configs (for an existing shared edge)
```

### 2.1 Why this split

- `site.Service` (the interface in `internal/site`) is the single git boundary; handlers depend on the
  interface, never the impl. `site.fake` enables table-driven tests of `api`, `mcpserver`, and `serve`
  with **no real git**.
- `serve` reads via a narrow read-only path (see §4) and is wired so it can run in-process (`KOTOJI_RUN_MODE=all`)
  or as a sibling binary (`KOTOJI_RUN_MODE=serve`) unchanged.
- `db/gen` is the sqlc generated output and must never be edited by hand; `db/store.go` wraps it with domain
  types and owns the at-rest secret box used to encrypt the GitHub PAT.

---

## 2.2 The site.Service interface (the contract handlers code against)

> **The frozen interface is `site.Service` in `internal/site` — see [CANONICAL.md §1](contracts/CANONICAL.md).**
> What follows is a faithful summary; on any field/signature difference CANONICAL.md wins. All methods take
> `ctx` first and return typed errors from `site/errors.go` (the taxonomy is CANONICAL §3). **No method
> exposes a raw file path** — paths are repo-relative, validated, and never reach `os.*` without cleaning
> (see [§8 path traversal](#83-correctness-gaps)).

```go
package site

// Service is the ONLY component that touches git on disk. Every writer (zip upload,
// Monaco editor, MCP tools) and the data-plane read side funnel through this interface.
type Service interface {
    // --- site lifecycle (git repo init AND metadata, one tx) ---
    CreateSite(ctx context.Context, in CreateSiteInput) (Site, error)       // init repo + seed draft + owner row
    GetSite(ctx context.Context, id uuid.UUID) (Site, error)
    GetSiteByHandle(ctx context.Context, h Handle) (Site, error)            // CURRENT handle only (old → resolver 301)
    ListSites(ctx context.Context, ownerID uuid.UUID) ([]Site, error)
    RenameHandle(ctx context.Context, id uuid.UUID, newHandle Handle) (Site, error)
    DeleteSite(ctx context.Context, id uuid.UUID, actor Actor) error        // SOFT (deleted_at); reaper later (§8.4)

    // --- branches ---
    ListBranches(ctx context.Context, id uuid.UUID) ([]Branch, error)
    CreateBranch(ctx context.Context, id uuid.UUID, name BranchName, from string) (Branch, error)
    DeleteBranch(ctx context.Context, id uuid.UUID, name BranchName) error  // refuses published/draft

    // --- read (never mutate) ---
    ListFiles(ctx context.Context, in ListFilesInput) ([]FileEntry, ResolvedRef, error)
    ReadFile(ctx context.Context, id uuid.UUID, branch BranchName, ref, path string) (FileContent, error)

    // --- write (git CLI; per-repo lock; baseSHA REQUIRED, empty => ErrValidation) ---
    WriteFile(ctx context.Context, in WriteFileInput) (CommitInfo, error)
    DeleteFile(ctx context.Context, id uuid.UUID, branch BranchName, path, baseSHA string, actor Actor) (CommitInfo, error)
    ImportZip(ctx context.Context, id uuid.UUID, branch BranchName, src ZipSource, baseSHA string, actor Actor) (CommitInfo, error)
    Commit(ctx context.Context, in CommitInput) (CommitInfo, error)         // "save" the staged working set
    Publish(ctx context.Context, in PublishInput) (CommitInfo, error)       // draft→published fast-fwd/merge
    Rollback(ctx context.Context, id uuid.UUID, branch BranchName, toSHA, baseSHA string, actor Actor) (CommitInfo, error)

    // --- history / diff ---
    GetDiff(ctx context.Context, in DiffOptions) (DiffResult, error)
    GetLog(ctx context.Context, in LogOptions) ([]CommitInfo, error)

    // --- github mirror ---
    SetRemote(ctx context.Context, id uuid.UUID, url string) error
    MirrorPush(ctx context.Context, id uuid.UUID, branches ...BranchName) error  // best-effort; never fails the save
    FetchAndUpdate(ctx context.Context, id uuid.UUID, branch BranchName) (CommitInfo, error) // webhook pull (FF only)

    // --- data-plane read side ---
    ServedTree(ctx context.Context, id uuid.UUID, branch BranchName) (TreeHandle, error)
}

// Site identity is the bare uuid.UUID (NOT a SiteRef wrapper). BaseSHA is REQUIRED on
// every write (empty => ErrValidation, never "force"); a mismatch => ErrConflict.
type Actor struct {           // git author = real identity for audit
    UserID uuid.UUID; Name, Email string
    Via    WriteSource        // upload|editor|mcp|system
    TokenID *uuid.UUID         // set when acting via a per-user token (MCP/API)
}
```

> **Authz boundary:** `site.Service` is NOT membership-authz-aware — it trusts the `uuid` it is given.
> Session→role and token→membership+scope checks are enforced *above* it (REST/MCP middleware). An MCP
> token is **per-user**: the MCP layer resolves the named `site`, reads the user's membership role, and caps
> the call to `intersection(token.scopes, roleScopes(membership))` before passing the resolved uuid down
> (CANONICAL §6.2).

The production impl `site.gitService` (built via `site.NewProductionService`) wraps the **git CLI via
`os/exec`** (arg arrays, never a shell string) for write fidelity, and uses **go-git** for pure reads.

---

## 3. Request flows

### 3a. Upload zip → commit → serve

```
Browser ──POST /api/sites/{handle}/branches/{branch}/import (multipart: file=.zip, baseSHA) ──▶ api.upload
  1. auth middleware: resolve session → user; authz: user can write this site? (role ≥ editor)
  2. CSRF check (double-submit header; see §8.1).
  3. Stream upload with a hard KOTOJI_MAX_UPLOAD_BYTES cap on the io.Copy.
  4. Open as archive/zip; run ZIP GUARDS before extracting (site/zip.go, see §8.1):
        - file count ≤ KOTOJI_ZIP_MAX_FILES
        - per-file declared+actual uncompressed size, running total ≤ KOTOJI_ZIP_MAX_TOTAL_BYTES
        - compression ratio guard (zip-bomb)
        - each name: reject absolute, '..', backslash, NUL, symlink entries
        - extension allowlist (KOTOJI_ZIP_ALLOWED_EXT)
  5. Service.ImportZip(id, branch, src, baseSHA, actor):
        - acquire per-repo lock
        - verify branch HEAD == baseSHA (empty baseSHA allowed ONLY on the initial seed) → else ErrConflict
        - write blobs into a fresh tree REPLACING it, commit on the branch
        - release lock
  6. Best-effort MirrorPush(id, branch) (failure does not fail the request — §8.2).
  7. audit insert (source=upload); return {commit, files[]}.
Later: visiting {handle}--draft.example.com → data plane reads draft tree → 200.
```

### 3b. Monaco save → POST backend → site.Service commit → mirror push

```
Editor (frontend) holds: handle, branch, path, content, baseSHA (commit SHA when file was opened).
Browser ──PUT /api/sites/{handle}/branches/{branch}/file?path=... (JSON: content, baseSHA) ──▶ api.files
  1. auth + authz(write) + CSRF.
  2. handle→uuid lookup (db.Store). path validated by site/path rules.
  3. Service.WriteFile{...baseSHA...} (Commit=true) or Service.Commit (multi-file "save"):
        - per-repo lock
        - branch HEAD == baseSHA ? no → return 409 ErrConflict (client must reload+merge)
        - stage content, commit (author = user identity), get newSHA
        - unlock
  4. best-effort MirrorPush(id, branch).  audit insert (source=editor).
  5. 200 {commit: newSHA}. Frontend updates its baseSHA to newSHA.
```

> "Save also pushes" = interpretation #1: the **server is the source of truth**; save commits locally and
> *mirror-pushes* to GitHub for backup/external diff. **push ≠ publish.**

### 3c. MCP write → commit

```
Personal-PC AI (Claude) ──MCP over HTTP/SSE──▶ backend mcpserver (/mcp)
  1. Transport auth: bearer token in header → verifier looks up the PER-USER token
     (user_tokens, hash + prefix) → {user_id, token.scopes, can_create_sites}. The token is
     owned by a USER and spans all of that user's memberships (no per-project binding).
  2. Tool call write_file{site, path, content, base_sha, branch?}:  ('site' = a project HANDLE)
        - authz.authorizeSite: GetSiteByHandle(site) → 404 if missing; GetRole(user,site) →
          404 if NOT a member (no existence leak). EFFECTIVE scope = token.scopes ∩ roleScopes(role),
          re-evaluated PER CALL. 'write' must be in the effective set → else forbidden.
        - SAME path as Monaco: Service.WriteFile (base_sha required) → Commit.
  3. save{site,...} → Commit. publish{site,...} → Publish. list_sites returns the user's
     memberships + effective scope per site. create_site (no 'site') mints a NEW project owned
     by the user, gated by BOTH token.can_create_sites AND users.can_create_sites; no token is
     ever minted over MCP.
  4. audit insert (actor=token's user, token_id set, source=mcp). best-effort MirrorPush.
```

MCP tools and REST handlers both call the *same* `site.Service` methods — the funnel invariant. The
per-user, membership-capped model means removing a membership (or downgrading the role) **instantly**
limits the token, and a token can never exceed its owner's own access. See CANONICAL §6.2 and `mcp.md`.

### 3d. Publish (draft → published)

```
Browser ──POST /api/sites/{handle}/publish (JSON: from=draft, baseSHA) ──▶ api.publish
  1. auth + authz(publish perm — owner always; editor when publish_mode=direct).
  2. Service.Publish({From: "draft", BaseSHA: ...}):
        - per-repo lock
        - verify draft HEAD == baseSHA
        - fast-forward published → draft  (or merge if published diverged via GitHub; §8.2)
        - refresh the served worktree to published HEAD  (see §4)
        - unlock
  3. update published_commit_sha + published_at. best-effort MirrorPush(published).
  4. 200. {handle}.example.com now serves the new tree.
Request-publish variant (publish_mode=request, non-owner): opens/updates a GitHub PR via mirror,
  does NOT auto-publish. Merge on GitHub → webhook (§3f) advances published.
```

### 3e. Data-plane GET

```
Visitor ──GET https://expense-calc--draft.example.com/reports/q1.html──▶ proxy ──▶ serve :8081
  1. resolve.Resolver.Resolve(Host="expense-calc--draft.example.com"):
        - strip the configured base domain (KOTOJI_BASE_DOMAIN) → label = "expense-calc--draft"
        - split on the FIRST "--": handle="expense-calc", branch="draft"  (no "--" ⇒ branch="published")
        - validate handle (reuse handle validator); reject reserved/invalid → 404 (not 400, no info leak)
  2. db.Store handle→uuid lookup → uuid, visibility (public|internal|private), branch policy.
  3. AUTHZ: if branch != "published" AND site is private → require a valid host-scoped preview-grant
        cookie (internal/preview), else 401/redirect to control-plane login (§8.1). published is
        public when KOTOJI_PUBLISHED_PUBLIC=true.
  4. Service.ServedTree(uuid, branch) → an immutable served worktree root  (materialized under
        /data/sites/{uuid}/served/{branch} for every branch, refreshed lazily if missing/stale — §4).
  5. Path resolution: clean URL path → repo-relative; directory ⇒ append "index.html";
        trailing-slash + index.html rules (§8.3). 404 page if missing.
  6. MIME by extension allowlist; SECURITY HEADERS injected (CSP, nosniff, frame-ancestors, etc. §8.1).
  7. Stream file (read-only). ETag = blob SHA; cache headers per branch (published: cacheable; preview: no-store).
```

### 3f. GitHub mirror push + webhook redeploy

```
Save/Publish ──▶ site.MirrorPush ──▶ GitHub remote (per-site repo)   [backup + external diff]
  Push/fetch AUTH (site/git_auth.go): mirroring is "on" only when ENABLED and a TOKEN is present.
  The token is resolved PER git call (DB-stored token, decrypted via secretbox, OVERRIDES the env
  KOTOJI_GITHUB_APP_TOKEN/PAT) so a runtime admin change applies without a restart. It is injected as an
  `Authorization: Basic base64("x-access-token:<token>")` via git's config-via-ENVIRONMENT
  (GIT_CONFIG_* → http.https://github.com.extraHeader), scoped to github.com. The token NEVER touches
  .git/config or argv, so it cannot leak through *GitError.Args or a process listing.

Maintainer merges a PR into `published` on GitHub
  ──▶ GitHub webhook POST /api/webhooks/github (X-Hub-Signature-256)
  1. webhook.github: verify HMAC with the webhook secret (DB github_webhook_secret OR env
     KOTOJI_GITHUB_WEBHOOK_SECRET), constant-time. reject else. body untrusted until verified.
  2. map repo → site uuid (by github_repo). ignore non-published ref pushes (configurable).
  3. Service.FetchAndUpdate(uuid, "published"):
        - per-repo lock; git fetch origin; fast-forward local published → origin/published
        - refresh the served worktree.   (reject non-FF, never force — §8.2)
  4. update published_commit_sha. audit(source=system). 200 quickly.
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
        - ALLOWLIST check: email domain == KOTOJI_AUTH_GOOGLE_HD and/or email ∈ KOTOJI_AUTH_ALLOWED_EMAILS → else 403.
        - upsert users row + user_identities (by provider/subject); CREATE a NEW server-side session (rotate id → anti-fixation);
          set opaque __Host- session cookie: HttpOnly, Secure, SameSite=Lax, host-only Domain (the bare
          control host, NEVER the wildcard parent — §8.1 isolation).
        - 302 → next (default /dashboard).
  4. Frontend route guard calls GET /api/me → {user} or 401 → redirect /login.
```

> **AuthProvider** abstraction (`auth/provider.go`) lets `oidc.go` (Google default; any OIDC issuer
> via config) and `devauth.go` (no-auth dev / single-admin password) be swapped by `KOTOJI_AUTH_MODE`.

**First-run admin setup (password mode).** `KOTOJI_AUTH_ADMIN_PASSWORD` is now **OPTIONAL**. When it is
empty AND no DB hash exists, the instance is in the *setupRequired* state: `GET /api/config` reports
`setupRequired:true` and the SPA renders a `/auth/setup` first-run screen. `POST /auth/setup` (live ONLY
during first run — it 409s once a credential exists) bcrypt-hashes the chosen password into
`instance_settings('admin_password_hash')`, ensures the admin user row exists, **promotes it to is_admin**,
and logs the admin straight in. The `PasswordProvider` verifies the **DB hash first, then the env password**.
Every successful password login (and the setup itself) (re)asserts `is_admin` on that single admin — never
for oidc/none users, whose powers come solely from the admin screen (`PATCH /api/admin/users/{id}/flags`).

### 3h. Instance Settings page (`/settings`)

A single authenticated page (`frontend/src/app/(app)/settings/page.tsx`), reachable from the avatar menu and
sidebar, with three sections:

- **(A) GitHub連携 — admin only** (rendered when `me.user.isAdmin`). The instance GitHub mirror config:
  enable toggle, org, **write-only** PAT, and **write-only** webhook secret, persisted via
  `GET|PUT /api/admin/github`. The PAT/secret are never echoed back (reduced to "configured" booleans); the
  PAT is stored AES-256-GCM-encrypted (§6.1 note, `internal/secretbox`). The effective view folds DB-over-env.
- **(B) MCP / API トークン — everyone.** The user's OWN per-user tokens (`/api/tokens`): issue (show-once
  plaintext) / list / revoke. A requested `canCreateSites` is capped by the user's own account flag.
- **(C) MCP 接続ガイド — everyone.** A read-only tutorial for pointing an AI client at this instance's `/mcp`
  with one of the above tokens.

> **Branding:** the favicon and brand mark are the **kotoji-tōrō** lantern as an SVG (`frontend/src/app/icon.svg`).

---

## 4. Deployment topology & the data-plane read strategy

### 4.1 Docker Compose services

The **base `deploy/docker-compose.yml` is proxy-less** (see §4.4): `postgres` + `backend`
(`KOTOJI_RUN_MODE=all`) + `frontend`. It binds 8080/8081/3000 so it drops cleanly behind any existing
shared edge (NPM / Caddy / nginx).

```yaml
# deploy/docker-compose.yml (essential shape)
services:
  postgres:
    image: postgres:18-alpine
    environment: [POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB=kotoji]
    # postgres:18 stores data in a major-version subdir — mount the volume at the PARENT.
    volumes: [pgdata:/var/lib/postgresql]
    healthcheck: pg_isready   # gates backend start so boot migrations don't race

  backend:
    build: { context: ../backend, dockerfile: ../deploy/backend.Dockerfile }  # multi-stage; final image HAS `git`
    environment: [ ...full env matrix §6... , KOTOJI_RUN_MODE=all ]   # all = api+auth+mcp+serve in one process
    depends_on: { postgres: { condition: service_healthy } }
    volumes:
      - kotojidata:/data        # one volume: sites/ (1 repo per site) + served worktrees + tmp staging
    ports: ["8080:8080", "8081:8081"]   # 8080 control API, 8081 data plane (see KOTOJI_RUN_MODE)
    # On boot the backend runs the embedded goose migrations (KOTOJI_AUTO_MIGRATE=true, default).

  frontend:
    build: { context: ../frontend, dockerfile: ../deploy/frontend.Dockerfile }  # Next.js standalone
    environment: [NEXT_PUBLIC_API_BASE_URL=  (empty ⇒ same-origin /api via proxy)]
    ports: ["3000:3000"]   # No volume mounts; immutable.

volumes: { pgdata: {}, kotojidata: {} }
```

`backend.Dockerfile` final stage uses an image that **has the git binary** (alpine + `apk add git`),
because `site.gitService` shells out for writes. Schema is provisioned at boot by `internal/migrate`
(goose, advisory-locked — §8.4.7), so a fresh `docker compose up` needs zero manual migration steps;
set `KOTOJI_AUTO_MIGRATE=false` to manage schema out of band.

### 4.2 Data plane: same binary or sibling?

**Decision: SAME Go binary, selectable run-mode (`KOTOJI_RUN_MODE`), with `serve` runnable standalone too.**

- `KOTOJI_RUN_MODE=all` → one process serves `:8080` (control) + `:8081` (data). Simplest self-host (the
  `docker compose up` story). The data plane reads via the narrow `ServedTree`/read path so it *cannot*
  mutate git even by mistake.
- `KOTOJI_RUN_MODE=control` + `KOTOJI_RUN_MODE=serve` → two containers sharing the `kotojidata` volume
  (control RW, serve RO). Lets you scale/restart the data plane independently; satisfies the durability goal
  "配信は published を読むだけ／操作プレーンが落ちても公開ツールは生存".

**Justification:** one codebase → one resolver, one security-header policy, dev/prod parity, no drift. The
run-mode flag gives the operational split without a second module.

### 4.3 How the data plane reads the tree

Three candidates were weighed:

| Strategy | Pros | Cons | Verdict |
|---|---|---|---|
| **(A) Per-branch served checkout dir** `/data/sites/{uuid}/served/{branch}` refreshed on publish/webhook/read | fastest serve (plain file I/O, `http.FileServer`-like, sendfile, ETag by mtime/SHA); survives if git layer is busy; trivial RO mount | extra disk (≈ working tree size, no `.git`); must keep in sync on write | **CHOSEN** |
| **(B) `git archive`/`cat-file` on each request** | zero extra disk; always exactly the branch HEAD | per-request exec/process cost; concurrency vs writes; harder caching/range requests | rejected for hot path |
| **(C) go-git in-memory tree read** | no checkout dir | re-walks tree per request; memory churn for large sites; no sendfile | rejected |

**Final policy:**
- **Every served branch** (`published`, `draft`, `feature-*`) → strategy **(A)**: `ServedTree` materializes a
  read-only worktree at **`/data/sites/{uuid}/served/{branch}`**, refreshed (lazily, if missing or stale) on
  read and eagerly on `Publish`/webhook. Serve = read-only filesystem walk under that dir. Disk cost is bounded
  and the README's "data plane is just reading the tree" promise holds literally. (CANONICAL §1 `ServedTree`.)
- `published` is materialized under `served/published`; previews (`draft`, `feature-*`) under `served/{branch}`.
  Deleting a branch drops its `served/{branch}` dir.

**Atomic refresh** (avoid serving a half-written tree): check out to a temp dir, then `rename()`
over the live `served/{branch}` dir (atomic on same filesystem). The serve layer opens files under a path it
re-stats per request, so the swap is invisible.

### 4.4 Proxy topology: proxy-less base + opt-in Traefik edge overlay

The **base compose is proxy-less** — it binds the host ports and is meant to sit behind an operator's
existing shared edge (NPM / Caddy / nginx). `deploy/npm/` ships sample configs for that case.

For a turnkey single-host self-host, an **opt-in overlay** adds Traefik:

```
docker compose -f docker-compose.yml -f docker-compose.edge.yml up -d
```

`deploy/docker-compose.edge.yml` ADDS one **Traefik v3** service and merges routing *labels* onto the
unchanged backend/frontend services (and `!reset`s their host port bindings, since Traefik fronts
everything). The labels encode exactly the §1.1 routing, keyed off `KOTOJI_BASE_DOMAIN`:

- `HostRegexp` `^[^.]+\.${KOTOJI_BASE_DOMAIN_REGEX}$` (≥1 label before the bare host) → backend `:8081`
  (data/serve plane; priority 30). The regexp deliberately never matches the bare control host.
- `Host(KOTOJI_BASE_DOMAIN) && PathPrefix(/api|/auth|/mcp|/healthz|/readyz)` → backend `:8080` (control; prio 20).
- `Host(KOTOJI_BASE_DOMAIN)` bare → frontend `:3000` (UI; prio 10, catch-all).

**TLS:** with `KOTOJI_ACME_EMAIL` unset the overlay is HTTP-only (great for localhost/dev). Set it and the
`letsencrypt` DNS-01 resolver issues a **wildcard** cert for `KOTOJI_BASE_DOMAIN` + `*.KOTOJI_BASE_DOMAIN`
(required for the per-site hosts) and redirects web→websecure. DNS-01 needs a provider token —
`KOTOJI_ACME_DNS_PROVIDER` (default `cloudflare`) + the provider token env (e.g. `KOTOJI_CF_DNS_API_TOKEN`).

### 4.5 kotoji-native on-demand TLS (third deploy mode — `KOTOJI_TLS_MODE=auto`)

For a single-host self-host where the admin wants HTTPS with **just DNS** — no external proxy, no wildcard
cert, no DNS-01 token, no ACME secret in env — kotoji can **terminate TLS itself**. A third opt-in overlay
flips it on:

```
docker compose -f docker-compose.yml -f docker-compose.tls.yml up -d   # MUTUALLY EXCLUSIVE with edge.yml
```

`deploy/docker-compose.tls.yml` publishes `80:80` + `443:443` and sets `KOTOJI_TLS_MODE=auto`. The backend
(`internal/tlsedge`, CertMagic on-demand) then:

- binds **`:443`** with the **single combined Host-routing handler** (`app.CombinedRouter` — `serve.Handler`
  with its same-binary `Control` hook wired to the control router, so the resolver dispatches the control host
  → control plane and every project host → data plane). Requires `KOTOJI_RUN_MODE=all`.
- binds **`:80`** to solve ACME **HTTP-01** challenges and **301-redirect** everything else to https.
- issues a **per-host** cert **on the fly** at handshake time via TLS-ALPN-01 / HTTP-01 (NO DNS-01, NO wildcard).

**Abuse prevention (DecisionFunc).** On-demand issuance is gated: kotoji obtains a cert for host `H` **only if**
`H` is the effective control host **OR** the resolve layer classifies `H` to an **existing** hosted site /
preview (reusing the live `resolve` classifier + an indexed site-exists lookup). Unknown hosts are **refused**
with **no issuance attempt** (so an attacker cannot make kotoji burn ACME rate limit on arbitrary names); a DB
error refuses too (**fail-closed**). The gate is pure + bounded (one classify, one indexed lookup) since it runs
inside the handshake.

**CA + storage.** Let's Encrypt **production** by default, with a **staging** toggle (`KOTOJI_TLS_CA=staging`) for
safe testing. The ACME account email (`KOTOJI_ACME_EMAIL`) is **optional** (settable later via env/UI). Issued
certs/keys + the ACME account persist under **`${KOTOJI_DATA_DIR}/certmagic`** (the existing data volume) so they
survive restarts.

> **Default is `off`** — the live Cloudflare-fronted deployment (kotoji speaks HTTP behind CF / the Traefik
> overlay) is **unchanged**. `off` mode starts nothing new; `:8080`/`:8081` stay plain HTTP. The three modes are
> mutually exclusive edges: **own proxy (base)** · **Traefik overlay (`edge.yml`)** · **kotoji-native TLS (`tls.yml`)**.

---

## 5. On-disk storage layout under `/data`

```
/data/
├── sites/                          # one repo per site — site.Service is the ONLY writer
│   └── {uuid}/
│       ├── .git/                   # repo (HEAD, refs/heads/{published,draft,feature-*}, objects, config)
│       │                            #   config has remote "origin" = GitHub mirror (when linked)
│       ├── index.html, ...         # the repo's own working tree (writes stage from here)
│       └── served/                 # materialized read-only worktrees — serve reads, never writes
│           ├── published/          # contents of refs/heads/published at last publish
│           │   └── index.html, ...
│           └── {branch}/...        # per-branch preview worktree (draft, feature-*), refreshed on read
├── tmp/                            # zip upload staging; cleaned on success/error & on startup
│   └── {rand}.zip
└── backups/                        # OPTIONAL: periodic `git bundle` per repo (see §8.4 operability)
    └── {uuid}/{ts}.bundle
```

Notes:
- Storage path keys on **uuid**, so handle rename never moves bytes (spec invariant).
- `/data/sites/{uuid}/` is a normal (non-bare) git repo: its working tree is the staging area for writes
  (the index), and serve never touches it — serve reads only the immutable `served/{branch}` checkouts.
- Quotas (§8.4) tracked per-uuid: sum of `objects` size + served-worktree size.

---

## 6. Config / env matrix

> Backend parses into a typed `config.Config` and **fails fast** on missing-required / invalid values at boot.
> Frontend uses `NEXT_PUBLIC_*` only for values safe to ship to the browser.

### 6.1 Backend (`internal/config`)

> All backend vars are **`KOTOJI_`-prefixed** and parsed into a typed `config.Config` that fails fast in
> production. The "Req" column: ✓ = required (in prod); `oidc`/`password`/`mirror` = required only when that
> mode/feature is active.

| Env var | Default | Req | Meaning |
|---|---|---|---|
| `KOTOJI_ENV` | `development` | – | `development` \| `production`. Gates how strict Load is. |
| `KOTOJI_RUN_MODE` | `all` | – | `all` \| `control` \| `serve`. Selects which http servers boot. |
| `KOTOJI_CONTROL_ADDR` | `:8080` | – | control plane (api/auth/mcp) listen addr. |
| `KOTOJI_SERVE_ADDR` | `:8081` | – | data plane listen addr. |
| `KOTOJI_TLS_MODE` | `off` | – | `off` (plain HTTP behind your own proxy / Traefik overlay) \| `auto` (kotoji terminates TLS itself, §4.5). `auto` requires `KOTOJI_RUN_MODE=all`. |
| `KOTOJI_TLS_CA` | `prod` | – | `prod` (Let's Encrypt production) \| `staging` (untrusted test certs, generous rate limits). On-demand TLS only. |
| `KOTOJI_ACME_EMAIL` | (empty) | – | OPTIONAL Let's Encrypt account email for on-demand TLS (settable later via env/UI). Shared name with the edge overlay. |
| `KOTOJI_TLS_ADDR` | `:443` | – | HTTPS listen addr in `auto` mode (combined control+data handler). |
| `KOTOJI_TLS_HTTP_ADDR` | `:80` | – | ACME HTTP-01 + HTTPS-redirect listen addr in `auto` mode. |
| `KOTOJI_BASE_DOMAIN` | `hosting.localhost` | ✓(prod) | base for `{handle}[--{branch}].<base>` parsing. |
| `KOTOJI_CONTROL_BASE_URL` | `http://hosting.localhost:8080` | ✓(prod) | external URL of control host (OIDC redirect, cookies, links). |
| `KOTOJI_DATABASE_URL` | – | ✓ | pgx DSN `postgres://user:pass@postgres:5432/kotoji?sslmode=...`. |
| `KOTOJI_DB_MAX_CONNS` | `10` | – | pgxpool max. |
| `KOTOJI_AUTO_MIGRATE` | `true` | – | run embedded goose migrations on boot (advisory-locked). Set `false` to manage schema out of band. |
| `KOTOJI_DATA_DIR` | `/data` | – | root for repos, served worktrees, tmp, backups. |
| `KOTOJI_GIT_BIN` | `git` | – | path to git binary (for os/exec). |
| `KOTOJI_SECRET_KEY` | (empty) | – | **at-rest key** (hex/base64, ≥32 bytes) for AES-256-GCM encryption of the DB-stored GitHub PAT. Optional: when unset a stable key is *derived* from a server seed (so env-only deploys still decrypt across restarts). |
| `KOTOJI_AUTH_MODE` | `oidc` | – | `oidc` \| `password` \| `none` (`none` rejected in prod). |
| `KOTOJI_AUTH_OIDC_ISSUER` | `https://accounts.google.com` | oidc | OIDC discovery issuer. |
| `KOTOJI_AUTH_OIDC_CLIENT_ID` | – | oidc | OAuth client id. |
| `KOTOJI_AUTH_OIDC_CLIENT_SECRET` | – | oidc | OAuth client secret. |
| `KOTOJI_AUTH_OIDC_REDIRECT_URL` | `${CONTROL_BASE_URL}/auth/callback` | – | must match IdP config. |
| `KOTOJI_AUTH_GOOGLE_HD` | (empty) | oidc* | restrict to a Workspace domain (e.g. `example.com`). *One of HD / ALLOWED_EMAILS required for oidc. |
| `KOTOJI_AUTH_ALLOWED_EMAILS` | (empty) | oidc* | comma list allowlist (alt/extra to `HD`). |
| `KOTOJI_AUTH_ADMIN_PASSWORD` | (empty) | **OPTIONAL** | single-admin password (mode=password). **Now optional**: empty ⇒ first-run `/auth/setup` sets it (bcrypt hash in `instance_settings`). When provided, min 8 chars; PasswordProvider checks DB hash then this. |
| `KOTOJI_AUTH_ADMIN_EMAIL` | `admin@kotoji.local` | – | identity for the password admin. |
| `KOTOJI_SESSION_COOKIE_NAME` | `kotoji_session` | – | opaque session id cookie (`__Host-` prefix when Secure). |
| `KOTOJI_SESSION_TTL` | `720h` | – | session lifetime (30d). |
| `KOTOJI_SESSION_COOKIE_DOMAIN` | (derive from CONTROL_BASE_URL host) | – | scope; **must NOT** be the wildcard parent `.<base>` (see §8.1 isolation). |
| `KOTOJI_COOKIE_SECURE` | `true` in prod | – | Secure attribute on cookies (off by default in dev/http). |
| `KOTOJI_CSRF_COOKIE_NAME` | `kotoji_csrf` | – | double-submit token cookie. |
| `KOTOJI_MCP_ENABLED` | `true` | – | toggle MCP server. |
| `KOTOJI_MCP_PATH` | `/mcp` | – | mount path. |
| `KOTOJI_MCP_TOKEN_TTL` | `2160h` | – | default token lifetime (90d). |
| `KOTOJI_GITHUB_MIRROR_ENABLED` | `false` | – | **env bootstrap** toggle for mirror push (DB `github_mirror_enabled` overrides). |
| `KOTOJI_GITHUB_APP_TOKEN` / `KOTOJI_GITHUB_PAT` | – | mirror | env credential for push/repo create (the DB-stored, encrypted token overrides). |
| `KOTOJI_GITHUB_ORG` | – | mirror | org/owner for created repos (DB `github_org` overrides). |
| `KOTOJI_GITHUB_WEBHOOK_SECRET` | – | mirror | HMAC secret for `/api/webhooks/github` (DB `github_webhook_secret` overrides). |
| `KOTOJI_MAX_UPLOAD_BYTES` | `52428800` (50MB) | – | hard cap on uploaded zip (pre-extract). |
| `KOTOJI_ZIP_MAX_TOTAL_BYTES` | `209715200` (200MB) | – | cap on total *uncompressed* size. |
| `KOTOJI_ZIP_MAX_FILES` | `2000` | – | max entries in a zip. |
| `KOTOJI_ZIP_MAX_RATIO` | `100` | – | zip-bomb: max uncompressed/compressed ratio. |
| `KOTOJI_ZIP_ALLOWED_EXT` | `.html,.htm,.css,.js,.mjs,.json,.svg,.png,.jpg,.jpeg,.gif,.webp,.ico,.woff,.woff2,.ttf,.txt,.md,.map,.xml,.csv,.wasm` | – | extension allowlist. |
| `KOTOJI_SITE_QUOTA_BYTES` | `524288000` (500MB) | – | per-site disk quota. |
| `KOTOJI_USER_SITE_QUOTA` | `50` | – | max sites per non-admin user. |
| `KOTOJI_RATE_LIMIT_API_RPS` | `20` | – | per-session API rate limit. |
| `KOTOJI_RATE_LIMIT_SERVE_RPS` | `100` | – | per-IP data-plane rate limit. |
| `KOTOJI_SOFT_DELETE_GRACE` | `720h` | – | retention before the reaper bundles+reclaims a soft-deleted site (30d). |
| `KOTOJI_BACKUP_DIR` | `${DATA_DIR}/backups` | – | where `git bundle` backups are written before disk reclaim. |
| `KOTOJI_OPS_INTERVAL` | `1h` | – | background ops scheduler tick (reaper + gc). |
| `KOTOJI_CORS_ALLOWED_ORIGINS` | `${CONTROL_BASE_URL}` | – | for browser API calls (same-origin in prod via proxy). |
| `KOTOJI_LOG_LEVEL` | `info` | – | slog level. |
| `KOTOJI_LOG_FORMAT` | `json` | – | `json` \| `text`. |
| `KOTOJI_HANDLE_MIN_LEN` / `KOTOJI_HANDLE_MAX_LEN` | `2` / `40` | – | resolver handle length bounds (create-time min is 3 per CANONICAL §5.1). |
| `KOTOJI_PUBLISHED_PUBLIC` | `true` | – | published served without auth (set false for fully-private instance). |
| `KOTOJI_TRUST_PROXY_HEADERS` | `true` | – | trust `X-Forwarded-*` (only behind the known proxy). |

> **GitHub mirror config is DB-or-env (DB overrides).** The admin saves it via the GUI (instance Settings
> page → `PUT /api/admin/github`) into `instance_settings`; the PAT is stored AES-256-GCM-encrypted
> (`internal/secretbox`, key from `KOTOJI_SECRET_KEY` or derived) and never echoed back. The env
> `KOTOJI_GITHUB_*` vars are only a bootstrap fallback.

### 6.2 Edge overlay (`deploy/docker-compose.edge.yml`, opt-in — §4.4)

| Env var | Default | Meaning |
|---|---|---|
| `KOTOJI_ACME_EMAIL` | (empty) | Let's Encrypt account email. Empty ⇒ HTTP-only edge (no TLS, no redirect). Set ⇒ DNS-01 wildcard cert + web→websecure redirect. |
| `KOTOJI_ACME_DNS_PROVIDER` | `cloudflare` | lego DNS-01 provider for the wildcard challenge. |
| `KOTOJI_CF_DNS_API_TOKEN` | (empty) | Cloudflare DNS API token (when provider=cloudflare). Other providers: pass their token env via your own `.env`. |
| `KOTOJI_BASE_DOMAIN_REGEX` | `hosting\.localhost` | the base domain with literal dots escaped (Go regexp) for the hosted-site `HostRegexp` router. Set alongside `KOTOJI_BASE_DOMAIN` (e.g. `example\.com`). |
| `KOTOJI_TRAEFIK_DASHBOARD` | `false` | opt-in insecure local Traefik dashboard. |

### 6.3 Frontend (Next.js)

| Env var | Default | Meaning |
|---|---|---|
| `NEXT_PUBLIC_API_BASE_URL` | `` (same-origin) | base for API calls; empty ⇒ relative `/api`. |
| `NEXT_PUBLIC_APP_NAME` | `kotoji` | branding. |
| `NEXT_PUBLIC_AUTH_MODE` | `oidc` | hides/shows login button vs dev banner; drives the first-run setup form in password mode. |
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
> explicit mitigation.** MCP tool schemas live separately in `docs/contracts/mcp.md` and are validated by the
> Go MCP SDK's own schema registration (Go structs → JSON schema), with a test asserting they match the doc.

Error envelope (uniform across `/api`, mirrored by MCP tool errors — CANONICAL §3):
```json
{ "error": { "code": "conflict", "message": "base commit is stale",
             "details": {"expected":"<sha>","actual":"<sha>","changedPaths":["index.html"]} } }
```
Stable machine `code` enum and HTTP mapping (CANONICAL §3): `unauthenticated`→401, `forbidden`→403,
`validation`→422, `conflict`/`handle_taken`/`publish_conflict`/`branch_exists`/`nothing_to_commit`→409,
`not_found`→404, `too_large`/`quota_exceeded`→413, `unsupported_media_type`→415, `rate_limited`→429,
`internal`→500. (Malformed bodies that never reach the Service → 400 `validation`.)

---

## 7.1 SQL DDL (metadata only — never file content)

> **The complete, frozen DDL is [CANONICAL.md §4](contracts/CANONICAL.md).** Do not duplicate it here; the
> migrations under `backend/internal/db/migrations/` are the live artifact, applied at boot by
> `internal/migrate` (goose, advisory-locked — §8.4.7). sqlc generates type-safe accessors into `db/gen`.
> Postgres 18. UUIDs via `gen_random_uuid()`. Times `timestamptz`. `citext` for handles/emails.

The migration chain (the schema as it actually is):

| Migration | What it establishes |
|---|---|
| `0001_init.sql` | The core schema: `users` (with `is_admin` + `can_create_sites`), `user_identities` (provider/subject), `sessions`, `sites` (3-valued `site_visibility` enum public\|internal\|private, `publish_mode`, `web_root`, soft-delete `deleted_at`), `site_members` (`site_role` enum **owner\|editor\|viewer**), `handle_redirects`, `reserved_handles`, `audit_log` (`audit_source` enum upload\|editor\|mcp\|system). |
| `0002_seed_reserved.sql` | Seeds the reserved-handle blocklist (draft, preview, published, www, api, internal, host, admin, app, static, assets, mcp). |
| `0003_instance_settings.sql` | `instance_settings(key,value)` — the instance key/value store. Holds the first-run admin-password **bcrypt hash** (`admin_password_hash`) AND the GitHub mirror config: `github_mirror_enabled`, `github_org`, `github_webhook_secret`, and `github_token` (**AES-256-GCM-encrypted** via `internal/secretbox`). |
| `0004_user_tokens.sql` | Re-architects MCP/API tokens from per-project to **per-user**: DROPS `site_tokens` and creates `user_tokens` (no `site_id` — owned by a `user_id`, carries `scopes` + `can_create_sites`, hash + 12-char prefix). Re-points `audit_log.token_id` at `user_tokens`. Existing per-project tokens are intentionally invalidated; users re-issue under `/api/tokens`. |

> **Token model (CANONICAL §6).** A `user_tokens` row is owned by a user and automatically covers ALL of
> that user's `site_members` memberships. The *effective* scope on a given site is
> `intersection(token.scopes, roleScopes(membership role))`, **re-evaluated per request** — so removing the
> membership or downgrading the role instantly limits the token, and a token can never exceed its owner's own
> access. There is no per-project token binding anymore.

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
   → Enforce `KOTOJI_ZIP_MAX_FILES`, `KOTOJI_ZIP_MAX_TOTAL_BYTES` (running sum during extraction, abort on
   exceed), and `KOTOJI_ZIP_MAX_RATIO` (uncompressed/compressed). Read each entry through an `io.LimitedReader` so a *lying* header
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

6. **Subdomain cookie / storage isolation.** Each site is its own origin (`a.example.com` vs `b.example.com`)
   so `localStorage`, `IndexedDB`, and Origin-scoped cookies are already isolated by the browser. The danger is
   a **too-broad session cookie**: if the control-plane session cookie were set on `Domain=.example.com`, every
   hosted site could read it.
   → The session cookie uses the `__Host-` prefix (host-only by definition, no `Domain`) when Secure; its
   `Domain` derives from `KOTOJI_CONTROL_BASE_URL`'s host and **must never** be the wildcard parent
   `.<KOTOJI_BASE_DOMAIN>` (config rejects/derives accordingly). Data-plane responses set `Cache-Control` and
   never `Set-Cookie`. Operators must not host kotoji's control UI on a `*.<base>` subdomain.
   **[OPEN]** If we ever issue *preview* cookies for private branches, they must be scoped to the exact preview
   host, not the parent — needs a per-host cookie design (below).

7. **Auth on private previews vs public published.** Published is public-by-config (`KOTOJI_PUBLISHED_PUBLIC`), but
   `feature-*`/`draft` previews can leak unreleased content if served openly.
   → **[DONE.]** Data plane (§3e step 3): for non-published branches on a private site, require a **host-scoped,
   signed preview-grant cookie** (`internal/preview`). `POST /api/sites/{h}/branches/{b}/preview-grant` mints a
   short-lived `kotoji_preview` cookie scoped to the exact `{h}--{b}.<base>` preview host (no `Domain`, so it is
   never sent to the control host or sibling sites). Published stays cookieless. The control session is NOT
   reused for previews (it is host-only to the control host — good isolation).

8. **MCP token scope / leak.** A token leaked from a personal PC could touch sites beyond the user's access.
   → **[DONE — re-architected to per-user, membership-capped.]** Tokens are **per-user** (`user_tokens`, no
   site binding; the old per-project `site_tokens` was DROPPED in migration 0004). A token automatically
   covers all the user's memberships, and on every tool call the effective capability is
   `intersection(token.scopes, roleScopes(membership role))`, re-evaluated live — so a token can **never
   exceed its owner's own access**, and removing/downgrading the membership limits it instantly. A
   non-member targeting a site gets `not_found` (no existence leak). Store only `sha256(secret)` + a 12-char
   prefix; show the secret once. Revoke (`revoked_at`), `expires_at`, `last_used_at` supported; MCP is
   rate-limited per token. Format: `kotoji_pat_<base62>`, ≥160 bits.

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
    → Verify `X-Hub-Signature-256` HMAC with the webhook secret (DB `github_webhook_secret`, else env
    `KOTOJI_GITHUB_WEBHOOK_SECRET`) in **constant time**; reject otherwise; rate-limit; ignore events for
    unknown repos. Treat the body as untrusted until verified.

13. **git CLI command injection.** Building git commands from user input (branch names, paths) via a shell.
    → **Never** use a shell string. `exec.CommandContext("git", args...)` with arg *arrays*; user values are
    separate argv elements (no interpolation). Validate branch names against `^[a-z0-9][a-z0-9/_-]*$` and reject
    leading `-` (option-injection: a path starting `-` could be read as a flag — prefix with `--` separator and
    use `git ... -- <path>`). Set `GIT_TERMINAL_PROMPT=0`, scrub env for the child.

14. **Credentials in git config / logs.** GitHub PAT could land in `remote.origin.url` or slog.
    → **[DONE.]** The mirror PAT is stored **out-of-band and encrypted**: in `instance_settings.github_token`,
    AES-256-GCM-sealed by `internal/secretbox` (key from `KOTOJI_SECRET_KEY` or derived), never echoed by the
    admin API. It is injected **per git invocation through the ENVIRONMENT** (`GIT_CONFIG_*` →
    `http.https://github.com.extraHeader: Authorization: Basic …`, scoped to github.com), so it never touches
    `.git/config`, the remote URL, or argv — and therefore cannot leak through `*GitError.Args` or a process
    listing (`site/git_auth.go`). Token-creation request bodies are never logged.

### 8.2 Concurrency & git gaps

1. **Parallel commits to one repo.** Two saves (Monaco + MCP) racing → corrupt index / lost update.
   → **Per-repo keyed mutex** (`git.lock`: `map[uuid]*sync.Mutex` guarded by a sync.Map) for in-process
   serialization, **plus** an OS `flock` on `/data/sites/{uuid}/.git/kotoji.lock` for the `KOTOJI_RUN_MODE=control`
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

6. **Publish atomicity vs serving.** Refreshing `/data/sites/{uuid}/served/{branch}` while it's being served →
   partial reads. → Build into a sibling temp dir, then atomic `rename` over the live `served/{branch}` dir
   (§4.3). Serve re-stats per request; the swap is invisible.

### 8.3 Correctness gaps

1. **Handle rename redirects.** Old subdomain `old.<base>` after rename to `new`.
   → **[DONE.]** On rename, insert into `handle_redirects(old_handle → site_id)`. Data-plane resolver: if the
   live handle lookup misses, check `handle_redirects`; if hit, `301` to the same path on the current handle's
   host (preserving branch+path+query). Control-plane links always use the live handle, and `GetSiteByHandle`
   404s old handles (confirmed asymmetry). Renaming *back* to a stale redirect of the **same** site deletes
   that redirect row; to one used by another site → `409`.

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
   → Track per-site size (objects + published worktree) in a periodic job; enforce `KOTOJI_SITE_QUOTA_BYTES` on
   write (reject `413` when exceeded) and `KOTOJI_USER_SITE_QUOTA` on create. Run `git gc --auto` after N commits; a scheduled
   `git gc` + `git repack` job reclaims dangling objects from rollbacks/force operations.

2. **GC of dangling / orphaned repos.** A repo dir with no DB row (failed create, manual mess), or soft-deleted
   sites' bytes.
   → Soft delete sets `sites.deleted_at`; a reaper job, after a grace period (e.g. 30d), `git bundle`s the repo to
   `/data/backups/{uuid}/` then `rm -rf`s `/data/sites/{uuid}` (which includes its `served/` worktrees). A startup
   consistency check logs (does not auto-delete) repos on disk with no matching row.

3. **Backups.** Postgres + git both hold state.
   → Postgres: standard `pg_dump`/PITR (operator concern; document). git: the GitHub mirror *is* a backup when
   enabled; for non-mirror setups, the scheduled `git bundle` per repo to `/data/backups`. Document a restore
   runbook (repo bundle → `git clone`, DB rows must match uuids).

4. **Audit log.** Required for "who published/overwrote what" (multi-user + AI writers).
   → `audit_log` table (CANONICAL §4), appended on every mutation with `actor_user_id`, `token_id` (when via a
   per-user token), `source` (upload\|editor\|mcp\|system), `commit_sha`, and a `metadata` jsonb (branch, paths,
   ip, …). The admin activity feed is `GET /api/admin/sites/{handle}/audit`. Append-only (all FKs ON DELETE SET
   NULL so the trail outlives referenced rows).

5. **Rate limits.** AI clients can hammer MCP; uploads can DoS.
   → Per-session API limiter (`KOTOJI_RATE_LIMIT_API_RPS`), per-IP data-plane limiter
   (`KOTOJI_RATE_LIMIT_SERVE_RPS`, `internal/ratelimit`),
   per-token MCP limiter, and a concurrency cap on zip extraction (a bounded worker pool) so big uploads can't
   exhaust CPU/disk in parallel.

6. **Observability.** → structured `slog` (JSON) with request-id propagation; per-request access logs (method,
   path, status, latency, site, actor); `/healthz` (process up) + `/readyz` (DB reachable, `/data` writable);
   optional Prometheus metrics (request counts/latency, git op durations, lock wait time, queue depth, mirror
   failures). Lock-wait-time is a key early-warning metric for repo contention.

7. **Migrations / startup ordering.** Backend may boot before Postgres is migrated.
   → **[DONE.]** Compose `depends_on: { postgres: healthy }`; the composition root runs the embedded goose
   migrations on boot (idempotent, `KOTOJI_AUTO_MIGRATE=true` default) behind a session-level
   `pg_advisory_lock` (`internal/migrate`) so rolling restarts / multiple boots don't race migrations. Set
   `KOTOJI_AUTO_MIGRATE=false` to manage schema out of band.

### 8.5 Product edge cases

1. **Single-file `index.html` with root-absolute paths.** AI output often references `/style.css`,
   `/assets/app.js`. Under per-subdomain hosting these resolve to the site root → **they actually work**, which is
   the whole point of subdomain-per-site (vs path-based hosting where `/style.css` would escape the prefix).
   → On the **path-based fallback** (`/host/{handle}/...`), root-absolute paths break. Mitigation: inject a
   `<base href="/host/{handle}/">` into served HTML *only in path-mode* (documented caveat: base-href injection
   can subtly change anchor/`fetch` resolution and is best-effort). Subdomain mode (the default) needs no
   injection. **[OPEN]** confirm we ship base-href injection for path-mode or just document the limitation.

2. **Large files.** A 200MB video in a repo bloats git and blows quotas/RAM on read.
   → Enforce a per-file size cap (zip/write path) and `KOTOJI_SITE_QUOTA_BYTES`; for reads, stream (don't buffer whole
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

### 8.6 Cross-cutting decisions — now resolved (CANONICAL §9 locked the 8)

- **[DONE]** Preview default visibility = private; preview-cookie design = host-scoped signed grant
  (`internal/preview`, §8.1.7).
- **[DONE]** Branch ref naming standardized on `feature-{user}-{slug}` (no slashes) to keep URL↔ref 1:1
  (CANONICAL §5.2).
- **[DONE]** Single backend replica for v1 (CANONICAL decision #4): in-proc per-site keyed mutex + `flock`; the
  lock seam is interfaced so a `pg_advisory_xact_lock` impl can drop in for future HA.
- Remaining design-rationale notes (not blockers): default CSP `connect-src` (permissive default + per-site
  tightening), "clean URLs", path-mode base-href injection, and git-LFS (out of scope v1).
```
