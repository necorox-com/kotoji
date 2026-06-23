# kotoji — MCP Server Design & Tool Contract

> Status: **locked design**, implementation-ready.
> Scope: the kotoji control-plane MCP server (Go). Defines transport, mounting,
> per-user token auth (membership-capped), scope enforcement, every tool's I/O
> schema + error behaviour + the `SiteService` method it delegates to, client
> config, safety guarantees, limits, Go type sketches and the test matrix.

This document is a **contract**. The MCP layer is a thin, authenticated adapter
over `SiteService` — it owns **no git logic**. Every mutation funnels through the
exact same `SiteService` interface used by the Zip-upload path and the Monaco
editor. If a behaviour is not described in [`docs/contracts/site-service.md`],
the MCP layer does not implement it; it delegates.

Related contracts:
- `docs/contracts/site-service.md` — the single git boundary (authoritative for git semantics, SHA model, branch rules).
- `docs/contracts/openapi.yaml` — the REST/OpenAPI control-plane API (per-user `/api/tokens` issuance lives here).
- `docs/contracts/data-model.md` — `sites`, `user_tokens`, `users` DDL.

---

## 1. Why MCP is "native" here, not bolted on

The three writers (Zip upload, Monaco, MCP) are peers. MCP is **not** a wrapper
around the REST API — both the REST handlers and the MCP tools call
`SiteService` directly. This avoids a second serialization hop and keeps a single
authorization model (token → identity + site scope) rather than re-deriving auth
from a forwarded HTTP session.

```
                Zip upload handler ─┐
                Monaco/REST handler ─┼──▶  SiteService  ──▶  git (CLI / go-git)
   MCP tool (this doc) ─────────────┘        (DI boundary, mockable)
```

The MCP layer's only extra responsibilities vs. REST are:
1. Translate the **bearer token** into `(userID, tokenID, scopes, canCreateSites)` —
   a token is owned by a USER, not bound to a site.
2. **Membership-cap every tool call**: resolve the named `site` (handle), read the
   user's membership role, and require `intersection(token.scopes, role scopes)`
   (§4). A token can only ever act within its user's memberships, and never beyond
   the user's own access.
3. Map `SiteService` errors into MCP tool-result errors (§6).

---

## 2. Transport & mounting

### 2.1 SDK

Official Go SDK: `github.com/modelcontextprotocol/go-sdk` (v1.x).
Packages used:
- `mcp` — `Server`, `AddTool[In,Out]`, `NewStreamableHTTPHandler`.
- `auth` — `RequireBearerToken`, `TokenVerifier`, `TokenInfo`.

Transport: **Streamable HTTP** (the SDK's `StreamableHTTPHandler`, which also
serves the SSE event stream for server→client notifications). We do **not** use
stdio: kotoji is remote/self-hosted and the client is the user's local AI
reaching a public endpoint.

### 2.2 Route & host

The MCP endpoint lives on the **control plane**, never the data plane.

| Concern | Value |
|---|---|
| Mount path | `POST/GET /mcp` (single endpoint; Streamable HTTP multiplexes) |
| Reachable host | `hosting.example.com/mcp` (the control-plane host) and `kotoji.localhost:8080/mcp` in dev |
| **Never** on | `*.hosting.example.com` (data plane is read-only static serving; it must not expose tooling) |
| NPM/proxy rule | bare host `/mcp` → backend; same upstream as `/api`, `/auth` |

The reserved-word list already blocks `mcp` as a handle, so a site can never
shadow `mcp.hosting.example.com`. (See identifier model in the spec.)

### 2.3 One server instance, per-request server resolution

`NewStreamableHTTPHandler` takes a `getServer func(*http.Request) *mcp.Server`.
kotoji uses a **single shared `*mcp.Server`** (tools are stateless; per-call
state comes from the token, not the server). The per-site authorization is
enforced **inside each tool** (resolve the named site → membership role →
effective scope), not by minting a server per site — the token identifies the
USER, and each tool names the site it targets.

```go
// internal/mcpserver/server.go
func New(svc site.Service, verifier auth.TokenVerifier, log *slog.Logger) http.Handler {
    s := mcp.NewServer(&mcp.Implementation{
        Name:    "kotoji",
        Version: buildinfo.Version,
    }, &mcp.ServerOptions{
        Instructions: mcpInstructions, // §9, shown to the client model
    })

    reg := &registry{svc: svc, log: log}
    reg.registerAll(s) // registers all 10 tools (§5)

    streamable := mcp.NewStreamableHTTPHandler(
        func(*http.Request) *mcp.Server { return s },
        &mcp.StreamableHTTPOptions{ /* defaults; stateless mode acceptable */ },
    )

    // auth.RequireBearerToken returns func(http.Handler) http.Handler.
    requireToken := auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{
        // No global Scopes here: scope is per-tool/per-site (see §4). We only
        // require *a valid token*; the tool decides what that token may do.
    })
    return requireToken(streamable)
}
```

### 2.4 Middleware order around the MCP handler

```
request-id → structured logging (slog) → recovery → CORS(scoped) →
RequireBearerToken(verifier) → StreamableHTTPHandler → tool dispatch
```

CORS for `/mcp`: MCP clients are not browsers, so `/mcp` does **not** get the
permissive browser CORS used for the dashboard. Allow only `Authorization`,
`Content-Type`, `Mcp-Session-Id`, `Mcp-Protocol-Version`; no credentials/cookies
(token auth, not cookie auth — this prevents CSRF-style misuse from a page the
user visits).

---

## 3. Authentication: per-user token (membership-capped)

### 3.1 How the client passes the token

Standard OAuth-style bearer:

```
Authorization: Bearer kotoji_pat_<base62-random>
```

Token format: `kotoji_pat_` prefix (greppable, lets us scan-detect leaks) +
≥160 bits of CSPRNG randomness, base62. **Only a SHA-256 hash is stored**; the
plaintext is shown exactly once at creation time in the dashboard. Prefix lets
the verifier fast-reject anything that isn't ours before a DB hit.

### 3.2 Token storage (`user_tokens`)

Authoritative DDL (mirrored in `docs/contracts/db.md` / migration `0004`):

```sql
CREATE TABLE user_tokens (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE, -- the human the token acts as
    name             TEXT        NOT NULL DEFAULT '',  -- human label, e.g. "claude-laptop"
    token_prefix     TEXT        NOT NULL,             -- first 12 chars of plaintext, for UI + lookup
    token_hash       BYTEA       NOT NULL,             -- sha256(plaintext), 32 bytes
    scopes           TEXT[]      NOT NULL DEFAULT '{read,write,publish}',
    can_create_sites BOOLEAN     NOT NULL DEFAULT FALSE, -- gates create_site; capped by users.can_create_sites
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at     TIMESTAMPTZ,
    expires_at       TIMESTAMPTZ,                       -- NULL = no expiry
    revoked_at       TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_user_tokens_hash   ON user_tokens (token_hash);
CREATE INDEX idx_user_tokens_prefix       ON user_tokens (token_prefix) WHERE revoked_at IS NULL;
CREATE INDEX idx_user_tokens_user_id      ON user_tokens (user_id);
```

A token is **owned by a user** and carries ONE scope set; it **automatically
covers every project the user is a member of**. There is no site binding on the
token. The **effective** capability on a given site is re-computed on EVERY
request as `intersection(token.scopes, the membership role's scopes)` — so
removing the user's membership (or downgrading the role) instantly limits the
token, and a token can **never exceed the user's own access** (§4). This
membership-capped model REPLACES the v1 per-project `site_tokens` (which was
dropped; existing per-project tokens were invalidated, users re-issue).

### 3.3 The `TokenVerifier`

The SDK's `auth.TokenVerifier` is:

```go
type TokenVerifier func(ctx context.Context, token string, req *http.Request) (*TokenInfo, error)
```

kotoji's implementation resolves the token to the OWNING USER (no site):

```go
// internal/mcpserver/verifier.go
type TokenInfo struct {
    UserID         uuid.UUID // the human the token acts as (user_tokens.user_id)
    TokenID        uuid.UUID
    Scopes         []string  // subset of {read,write,publish}
    CanCreateSites bool      // token flag AND'd with users.can_create_sites
}

func (v *Verifier) Verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
    if !strings.HasPrefix(token, tokenPrefix) { // "kotoji_pat_"
        return nil, fmt.Errorf("malformed token: %w", auth.ErrInvalidToken)
    }
    rows, err := v.q.GetUserTokenByPrefix(ctx, token[:12]) // active tokens sharing the prefix
    if err != nil {
        return nil, err // infra error → 500
    }
    sum := sha256.Sum256([]byte(token))
    // constant-time compare sum against each candidate hash; reject expired/revoked.
    // ... (match selected) ...
    canCreate := match.CanCreateSites && match.UserCanCreateSites // capped by the owner
    v.touch(match.ID) // best-effort last_used_at bump
    return &auth.TokenInfo{
        Scopes:     match.Scopes,
        Expiration: expiry(match.ExpiresAt),
        UserID:     match.UserID.String(),
        Extra:      map[string]any{kClaims: TokenInfo{UserID: match.UserID, TokenID: match.ID, Scopes: match.Scopes, CanCreateSites: canCreate}},
    }, nil
}
```

Notes:
- Verification failures **must** unwrap to `auth.ErrInvalidToken` so the SDK
  returns `401` with a `WWW-Authenticate` header (RFC 6750). Infra errors
  (DB down) return a plain error → `500`.
- We **do not** set global `Scopes` in `RequireBearerTokenOptions`. Scope is
  enforced **per call, per site**: a tool requiring `write` is allowed only if
  `write ∈ intersection(token.scopes, role scopes)` for the named site (§4.2).
- The verifier no longer carries a `SiteID`; the site comes from each tool's
  `site` argument and is authorized in the handler (§4.1).

### 3.4 From token to handler

Inside a tool handler, the SDK exposes the verified token via the request's
`Extra`:

```go
func principalFrom(req *mcp.CallToolRequest) (TokenInfo, bool) {
    info := req.GetExtra().TokenInfo // *auth.TokenInfo, set by RequireBearerToken
    if info == nil {
        return TokenInfo{}, false
    }
    c, ok := info.Extra[kClaims].(TokenInfo)
    return c, ok
}
```

---

## 4. Scope enforcement — a tool can NEVER act outside its project

This is the headline security property. Two independent guards, both in the
`registry` wrapper that decorates every tool handler:

### 4.1 Membership-cap (the hard wall)

The token identifies a **user**, not a site. Every content tool takes a `site`
(the project handle), and the wrapper **caps** the call to the user's membership:

1. Resolve the named `site` (current handle → site). A miss is `not_found`.
2. Read the user's **membership role** on that site (`site_members.GetRole`). If
   the user is **NOT a member**, return `not_found` — we never confirm a site's
   existence to a non-member (no existence leak).
3. Compute **effective scopes** = `intersection(token.scopes, roleScopes(role))`,
   where `owner/editor → {read,write,publish}` and `viewer → {read}` (CANONICAL §6).
4. The tool's required scope must be in the effective set, else `forbidden`.

This runs on **every call**, so a token can only ever act within its user's
memberships and can never exceed the user's access — removing a membership or
downgrading the role limits the token immediately, with no re-issue. A token of
user A naming a site A is not a member of is refused (`not_found`) before any
`SiteService` touch: this REPLACES the old "no tool takes a site selector / one
token = one site" pin. `SiteService` still resolves storage as
`/data/sites/{uuid}/.git` from the resolved site UUID.

`list_sites` returns the user's memberships (with role + effective scope per
site); `create_site` mints a NEW site (the user becomes owner) and is gated by
`token.can_create_sites AND users.can_create_sites` (§5.5).

### 4.2 Action scope check (token chain × membership role)

Every tool declares the scope it needs. The effective grant is the intersection
of the token's scopes and the membership role's scopes:

| scope | grants tools |
|---|---|
| `read` | `list_sites`, `list_files`, `read_file`, `get_diff`, `get_log` |
| `write` | everything in `read` + `write_file`, `save`, `rollback`, `create_branch` |
| `publish` | everything in `write` + `publish` |

`create_site` is NOT a `write`-scope gate; it is gated by `can_create_sites`
(token AND user), §5.5.

Token scopes are a superset chain (`publish ⊇ write ⊇ read`) enforced at issuance.
The role scopes are: `owner/editor → {read,write,publish}`, `viewer → {read}`.
A tool is allowed only when its required scope is in BOTH (e.g. a `write` token on
a site where the user is a `viewer` may only read).

### 4.3 Path confinement inside a site

Within the resolved site, `write_file`/`read_file` paths are still untrusted
input. The wrapper validates **before** `SiteService`:
- reject absolute paths, `..` segments, `.git/` prefix, NUL bytes, backslashes;
- normalize to a clean relative POSIX path;
- enforce the **extension allowlist** (same list as Zip upload:
  `.html .htm .css .js .mjs .json .svg .png .jpg .jpeg .gif .webp .ico .txt .md .woff .woff2 .map`);
- enforce max path length (255 bytes/segment, 1024 total).

`SiteService` re-validates (defence in depth) — the MCP layer must not be the
only guard.

### 4.4 The decorator + per-site authorization

The guard does the TOKEN-level gates (principal recovery + per-token rate limit);
the per-SITE authorization happens inside each handler via `authorizeSite`, since
the target site comes from the tool's `site` argument:

```go
// guard: token-level gates only (no site needed yet).
func guard(r *registry, class toolClass, fn toolFn) mcp.ToolHandlerFor[In, Out] {
    return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
        claims, ok := principalFrom(req)
        if !ok { return toolErr(codeUnauthenticated, "invalid or missing token"), zero, nil }
        if !r.limits.Allow(claims.TokenID, class) { return toolErr(codeRateLimited, ...), zero, nil }
        return fn(withClaims(ctx, claims), claims, in)
    }
}

// authorizeSite: per-call membership cap (run first in each content handler).
func (r *registry) authorizeSite(ctx, c TokenInfo, handle string, need scope) (authorized, *CallToolResult, error) {
    s, err := r.svc.GetSiteByHandle(ctx, Handle(handle)) // miss → not_found
    role, err := r.members.GetRole(ctx, {s.ID, c.UserID}) // no membership → not_found
    eff := intersect(c.Scopes, roleScopes(role))          // owner/editor→rwp, viewer→r
    if !contains(eff, need) { return _, toolErr(codeForbidden, ...), nil }
    return authorized{site: s, role: role, scopes: eff}, nil, nil
}
```

---

## 5. Tool catalogue

Conventions for every tool below:
- **Every content tool takes a `site`** (the project handle), authorized per call
  against the token user's membership (§4.1). `list_sites` takes no selector (it
  enumerates the user's memberships); `create_site` takes a NEW `handle` (it mints
  a new site, not a selector over existing ones).
- `branch` is optional; default is the site's working branch (`draft`).
  `branch` must pass the same validation as handles minus reserved-word rules
  (branch names `published`/`draft`/`feature-*` are allowed; arbitrary names are
  validated as `[a-z0-9][a-z0-9/_-]{0,99}`). `published` is **read-only** to
  `write_file`/`save` (you reach it only via `publish`).
- Outputs are returned as the tool's **typed `Out`** (StructuredContent) **and** a
  compact human-readable `TextContent` summary, so both code-driven and
  chat-driven clients work.
- Errors: see §6. On a *business* error (stale SHA, not found, validation) we
  return a `CallToolResult` with `IsError=true` and a structured error body —
  **not** a Go `error` (a Go `error` is reserved for transport/infra failures).

The `SiteService` method names referenced below are defined in
`docs/contracts/site-service.md`. Signatures are shown in §7.

---

### 5.1 `list_sites`  — scope: `read`

Lists the projects the token's **user is a member of**, each with the user's
membership `role` and the `effective_scopes` (`token.scopes ∩ role scopes`) so the
model knows exactly what it may do where. Empty when the user has no memberships.

**Input**
```jsonc
{} // no arguments — enumerates the user's memberships
```

**Output**
```jsonc
{
  "sites": [
    {
      "uuid": "7f3a...",
      "handle": "expense-calc",
      "role": "owner",
      "effective_scopes": ["read", "write", "publish"],
      "published_url": "https://expense-calc.hosting.example.com",
      "draft_url": "https://expense-calc--draft.hosting.example.com",
      "default_branch": "draft",
      "is_published": true,
      "updated_at": "2026-06-21T09:12:00Z"
    }
  ]
}
```
**Delegates to** `members.ListSitesForUser(ctx, claims.UserID)` (membership join).
**Errors** — infra only (a non-member simply does not see the site).

---

### 5.2 `list_files`  — scope: `read`

**Input**
```jsonc
{
  "site": "expense-calc",   // required: project handle (from list_sites)
  "branch": "draft",        // optional, default = default working branch
  "path": "assets/",        // optional subtree filter, default = repo root
  "ref": "a1b2c3d"          // optional commit SHA to list at; default = branch tip
}
```

**Output**
```jsonc
{
  "branch": "draft",
  "commit": "a1b2c3d4...",            // resolved SHA the listing reflects
  "files": [
    { "path": "index.html", "size": 2048, "mode": "100644" },
    { "path": "assets/app.js", "size": 9210, "mode": "100644" }
  ]
}
```
Directories are implied by paths; we return a **flat file list** (non-engineer
clients and AIs both prefer full paths). `size` is bytes of the blob at `ref`.

**Delegates to** `svc.ListFiles(ctx, resolvedSiteID, branch, ref, pathPrefix)`
after `authorizeSite(site, read)`.
**Errors** — `not_found` (branch/ref), `validation` (bad path).

---

### 5.3 `read_file`  — scope: `read`

**Input**
```jsonc
{
  "site": "expense-calc",   // required: project handle (from list_sites)
  "path": "index.html",     // required
  "branch": "draft",        // optional
  "ref": "a1b2c3d"          // optional; default = branch tip
}
```

**Output**
```jsonc
{
  "path": "index.html",
  "branch": "draft",
  "commit": "a1b2c3d4...",  // the SHA this content came from — USE THIS as base_sha for write_file
  "blob_sha": "e5f6...",    // blob hash of this specific file (for fine-grained conflict checks)
  "encoding": "utf-8",      // or "base64" for binary
  "content": "<!doctype html>...",
  "size": 2048,
  "truncated": false        // true if file exceeded read limit (§10)
}
```

**Why we echo `commit`**: it is the value the model should pass back as
`base_sha` to `write_file`, making the optimistic-concurrency loop obvious to an
AI client: *read → edit → write_file(base_sha = the commit I read)*.

**Binary**: files whose extension is binary (images/fonts) return
`encoding:"base64"`. Files over the read limit return `truncated:true` and the
first N bytes; the model is told to use the editor for large binaries.

**Delegates to** `svc.ReadFile(ctx, resolvedSiteID, branch, ref, path)` after
`authorizeSite(site, read)`.
**Errors** — `not_found` (incl. non-membership), `validation`, `too_large` (only
if even truncation disabled for binary, §10).

---

### 5.4 `write_file`  — scope: `write`  — **base SHA REQUIRED**

The optimistic-locking core. Writes the file into the working tree of `branch`
**but does not commit** unless `commit:true` (convenience), mirroring the Monaco
flow where "save" = `write_file(commit:true)` or an explicit `save` call.

**Input**
```jsonc
{
  "site": "expense-calc",        // required: project handle (from list_sites)
  "path": "index.html",          // required
  "content": "<!doctype html>...",// required
  "encoding": "utf-8",           // "utf-8" (default) | "base64"
  "base_sha": "a1b2c3d4...",     // REQUIRED: the branch-tip commit the edit is based on
  "branch": "draft",             // optional; "published" is REJECTED (validation error)
  "commit": true,                // optional, default true: create a commit after staging
  "message": "edit index.html"   // optional commit message; default auto-generated
}
```

**`base_sha` is mandatory** — there is **no** "force"/"overwrite" flag in v1.
The server compares `base_sha` to the current tip of `branch`:

- **Match** → proceed (stage + optional commit).
- **Mismatch (stale)** → reject with `conflict` (§6), returning the current tip
  and a diff hint so the client can re-read and retry:

```jsonc
{
  "error": {
    "code": "conflict",
    "message": "branch 'draft' moved since base_sha; re-read and retry",
    "details": {
      "base_sha": "a1b2c3d4...",   // what the client sent
      "current_sha": "z9y8x7...",  // actual tip now
      "changed_paths": ["index.html"] // files that differ between the two
    }
  }
}
```

**Success Output**
```jsonc
{
  "path": "index.html",
  "branch": "draft",
  "committed": true,
  "commit": "b2c3d4e5...",   // new tip; becomes the next base_sha
  "blob_sha": "f7a8...",
  "pushed": true,            // mirror-push to GitHub remote succeeded (best-effort; see note)
  "bytes_written": 2051
}
```

**Mirror-push semantics**: per the spec, save = commit + mirror-push to GitHub.
`pushed` reflects the mirror result. A **mirror-push failure does NOT fail the
tool** — the local commit is the source of truth (the server is authoritative).
We return `committed:true, pushed:false` plus a `warnings:["mirror push failed: ..."]`
array so the AI knows backup lagged but the save itself succeeded. (Failing the
tool on push failure would make GitHub availability a hard dependency of saving,
which violates "server is the source of truth".)

**Concurrency detail**: staging + commit + tip-check happen under a
**per-site write lock** inside `SiteService` (one repo can't take two concurrent
commits). The `base_sha` check is performed **while holding the lock**, so the
check-then-commit is atomic — no TOCTOU window between "tip matches" and "commit".

**Delegates to** `svc.WriteFile(ctx, WriteRequest{...})` (single call that does
stage+optional-commit+push under lock).
**Errors** — `conflict` (stale base_sha), `validation` (path/ext/encoding/empty
base_sha/branch=published), `not_found` (branch), `too_large` (§10).

---

### 5.5 `create_site`  — capability: `can_create_sites`

Creates a brand-new repo + DB row. It does NOT take a `site` selector (it mints a
new one via `handle`), and it is gated by **capability**, not the read/write chain:

- Requires `user_tokens.can_create_sites` **AND** `users.can_create_sites`
  (both re-checked at call time; the token flag is AND'd with the user flag).
  Default token `can_create_sites = false`. Rationale: a token that can spawn
  arbitrary new projects is a privilege-escalation vector, so it is opt-in at
  issuance and still capped by the user's account flag.
- The new site is owned by `claims.UserID`. Because the token is per-user and
  automatically covers all the user's memberships, the SAME token immediately
  works on the new site (the user is its owner) — no new token needed.

**Input**
```jsonc
{
  "handle": "invoice-tool",      // required; validated (lowercase [a-z0-9-], len, reserved-words, unique)
  "init": "empty",               // "empty" | "template"
  "template": "blank-html",      // optional, when init=template
  "private": true                // optional; mirror to a private GitHub repo (default true)
}
```

**Output**
```jsonc
{
  "uuid": "9a8b...",
  "handle": "invoice-tool",
  "draft_url": "https://invoice-tool--draft.hosting.example.com",
  "default_branch": "draft",
  "token_hint": "your existing token already covers this new site (you are its owner)"
}
```
Note: `create_site` does **not** mint a new token — your existing per-user token
already covers the new site (you are its owner).

**Delegates to** `svc.CreateSite(ctx, CreateRequest{Handle, OwnerID, Init, Template})`.
**Errors** — `forbidden` (capability off / scope), `validation` (handle rules),
`conflict` (handle taken), `quota_exceeded` (per-user site quota).

---

### 5.6 `save`  — scope: `write`

Explicit commit of whatever is currently staged/working on `branch`. In the
common MCP flow `write_file(commit:true)` already commits, so `save` exists for:
(a) committing multiple `write_file(commit:false)` calls as one logical change,
(b) parity with the Monaco "Save" button semantics.

**Input**
```jsonc
{
  "site": "expense-calc",      // required: project handle (from list_sites)
  "branch": "draft",           // optional; "published" rejected
  "base_sha": "a1b2c3d4...",   // REQUIRED: optimistic lock, same rules as write_file
  "message": "tidy up layout"  // optional
}
```

**Output**
```jsonc
{
  "branch": "draft",
  "commit": "c3d4e5f6...",     // new tip (== base_sha if nothing to commit, with no_op:true)
  "no_op": false,              // true when working tree was clean
  "pushed": true,
  "files_changed": 3
}
```

**Delegates to** `svc.Commit(ctx, CommitRequest{SiteID, Branch, BaseSHA, Message})`.
**Errors** — `conflict` (stale base_sha), `validation` (branch=published),
`not_found`.

---

### 5.7 `publish`  — scope: `publish`

Promotes `draft` (or a named source branch) into `published`. This is the
"go live" action; non-engineers see it as a button, AIs call this tool.

**Input**
```jsonc
{
  "site": "expense-calc",      // required: project handle (from list_sites)
  "from": "draft",             // optional source branch, default = default working branch
  "base_sha": "a1b2c3d4...",   // REQUIRED: SHA of `from` the publish is intended for
  "message": "publish: v1.2"   // optional
}
```

`base_sha` here guards against publishing a stale snapshot: the client asserts
"publish the `from` branch as I last saw it". If `from` advanced past `base_sha`,
the tool returns `conflict` (so an AI can't accidentally ship a version newer
than the one it reviewed). The promotion itself is a fast-forward/merge of `from`
into `published`.

**Output**
```jsonc
{
  "published_commit": "d4e5f6a7...",
  "published_url": "https://expense-calc.hosting.example.com",
  "from": "draft",
  "from_commit": "a1b2c3d4...",
  "pushed": true,              // published branch mirror-pushed to GitHub
  "redeploy": "live"           // data plane serves the new published tree (no extra step; serving reads git)
}
```

**Delegates to** `svc.Publish(ctx, PublishRequest{SiteID, From, BaseSHA, Message})`.
The DB `sites.is_published` / `published_commit` columns are updated **inside**
`SiteService` within the same logical operation.
**Errors** — `conflict` (stale base_sha), `not_found` (source branch), `forbidden`
(scope), `validation`.

> **Note on GitHub-PR publish**: per the locked design, a *human* GitHub-merge to
> `published` → webhook → server pull is a separate, server-side path (REST/webhook
> handler), **not** an MCP tool. `publish` here is the direct server-authoritative
> promotion. Both end at `SiteService.Publish`-equivalent state.

---

### 5.8 `get_diff`  — scope: `read`

**Input**
```jsonc
{
  "site": "expense-calc",       // required: project handle (from list_sites)
  // Two modes:
  //  (a) compare two refs:
  "from": "published",          // ref or branch
  "to": "draft",                // ref or branch
  //  (b) single commit vs its parent (set "commit" instead of from/to):
  "commit": "a1b2c3d",
  // common:
  "path": "index.html",         // optional path filter
  "context_lines": 3,           // optional, default 3
  "format": "unified"           // "unified" (default) | "name-status"
}
```

**Output**
```jsonc
{
  "from": "published", "to": "draft",
  "from_commit": "z9...", "to_commit": "a1...",
  "files": [
    {
      "path": "index.html",
      "status": "modified",       // added|modified|deleted|renamed
      "old_path": null,           // set for renames
      "additions": 12, "deletions": 4,
      "patch": "@@ -1,4 +1,12 @@\n..."   // omitted in name-status format
    }
  ],
  "truncated": false              // patches over total limit are dropped (§10)
}
```

**Delegates to** `svc.Diff(ctx, DiffRequest{SiteID, From, To, Path, Context, Format})`.
**Errors** — `not_found` (ref), `validation`.

---

### 5.9 `get_log`  — scope: `read`

**Input**
```jsonc
{
  "site": "expense-calc",       // required: project handle (from list_sites)
  "branch": "draft",            // optional, default = default working branch
  "path": "assets/app.js",      // optional; history of one file
  "limit": 20,                  // optional, default 20, max 100
  "before": "f6a7..."           // optional cursor: return commits before this SHA (pagination)
}
```

**Output**
```jsonc
{
  "branch": "draft",
  "commits": [
    {
      "sha": "a1b2c3d4...",
      "short": "a1b2c3d",
      "author": "alice@example.com",   // git author (mapped from the writer's identity)
      "via": "mcp",                    // mcp|monaco|upload|github — provenance tag (§ note)
      "message": "edit index.html",
      "committed_at": "2026-06-21T09:12:00Z",
      "files_changed": 1
    }
  ],
  "next_before": "z9y8..."             // cursor for the next page, null if end
}
```

**Provenance (`via`)**: `SiteService` writes a git trailer (`Kotoji-Via: mcp`)
on every commit it makes, so history is auditable by writer. This is how an admin
can answer "what did the AI change?".

**Delegates to** `svc.Log(ctx, LogRequest{SiteID, Branch, Path, Limit, Before})`.
**Errors** — `not_found` (branch), `validation` (limit out of range).

---

### 5.10 `rollback`  — scope: `write`

Reverts `branch` to a previous state. **Implemented as a forward commit**
(`revert`-style new commit that restores the tree of `to_sha`), never a
destructive history rewrite — so rollback is itself undoable and the optimistic
SHA model still holds.

**Input**
```jsonc
{
  "site": "expense-calc",       // required: project handle (from list_sites)
  "branch": "draft",            // optional; "published" rejected (publish from a rolled-back draft instead)
  "to_sha": "a1b2c3d4...",      // required: the commit whose tree we restore
  "base_sha": "f6a7b8c9...",    // REQUIRED: current branch tip the client believes it is reverting from
  "message": "rollback to a1b2c3d"
}
```

**Output**
```jsonc
{
  "branch": "draft",
  "commit": "g8h9i0...",        // new tip restoring to_sha's tree
  "restored_from": "a1b2c3d4...",
  "pushed": true
}
```

`base_sha` guards the same optimistic-lock invariant; `to_sha` must be an
ancestor reachable on the branch (rejected otherwise).

**Delegates to** `svc.Rollback(ctx, RollbackRequest{SiteID, Branch, ToSHA, BaseSHA, Message})`.
**Errors** — `conflict` (stale base_sha), `not_found` (to_sha unreachable),
`validation` (branch=published).

---

## 6. Error model

MCP distinguishes **transport/protocol errors** (the Go `error` return →
JSON-RPC error) from **tool errors** (`CallToolResult.IsError = true` with a
payload the model can read and act on). kotoji uses:

| Situation | How returned |
|---|---|
| Bad/missing/expired/revoked token | `401` from `RequireBearerToken` (never reaches a tool) |
| Token lacks required scope | tool result `IsError`, code `forbidden` |
| Validation (bad path, ext, missing base_sha, branch=published, bad handle) | tool result `IsError`, code `validation` |
| Optimistic-lock mismatch | tool result `IsError`, code `conflict` (+ current_sha) |
| Site/branch/ref/file not found | tool result `IsError`, code `not_found` |
| Handle/site already exists | tool result `IsError`, code `conflict` |
| Over a size/count/rate limit | tool result `IsError`, code `too_large` / `rate_limited` / `quota_exceeded` |
| git/DB/disk/internal failure | Go `error` return → JSON-RPC error, `500`-class; details logged, not leaked |

Why business errors are `IsError` results, not Go errors: the model needs the
**structured detail** (e.g. `current_sha` on conflict) to self-correct. A
JSON-RPC error loses that affordance in many clients. Internal failures are Go
errors so they surface as protocol errors and get retried/escalated, not fed to
the model as "fixable".

Canonical error body (inside `StructuredContent` and summarized in text):
```jsonc
{
  "error": {
    "code": "conflict",                  // stable machine string (enum below)
    "message": "human-readable, safe to show",
    "details": { /* code-specific, e.g. current_sha */ },
    "retryable": false                   // hint: conflict=re-read+retry; rate_limited=backoff
  }
}
```

Error code enum (stable contract):
`unauthenticated | forbidden | validation | conflict | not_found | too_large | rate_limited | quota_exceeded | internal`

---

## 7. Go type sketches

### 7.1 SiteService methods the MCP layer calls

Authoritative interface in `docs/contracts/site-service.md`; the MCP-relevant
subset (all take `ctx` first; all mutations carry `BaseSHA`):

```go
package site

type Service interface {
    GetSite(ctx context.Context, siteID uuid.UUID) (*Site, error)
    ListFiles(ctx context.Context, siteID uuid.UUID, branch, ref, pathPrefix string) ([]FileEntry, ResolvedRef, error)
    ReadFile(ctx context.Context, siteID uuid.UUID, branch, ref, path string) (*Blob, error)

    WriteFile(ctx context.Context, r WriteRequest) (*WriteResult, error)
    Commit(ctx context.Context, r CommitRequest) (*CommitResult, error)
    Publish(ctx context.Context, r PublishRequest) (*PublishResult, error)
    Rollback(ctx context.Context, r RollbackRequest) (*CommitResult, error)

    CreateSite(ctx context.Context, r CreateRequest) (*Site, error)

    Diff(ctx context.Context, r DiffRequest) (*DiffResult, error)
    Log(ctx context.Context, r LogRequest) (*LogResult, error)
}

type WriteRequest struct {
    SiteID   uuid.UUID
    Branch   string      // "" → default working branch; "published" rejected
    Path     string
    Content  []byte
    BaseSHA  string      // REQUIRED; service rejects "" and mismatches under lock
    Commit   bool
    Message  string
    AuthorID uuid.UUID   // from claims.UserID; becomes git author + Kotoji-Via trailer
    Via      WriteSource // WriteSourceMCP
}

// ErrConflict carries the live tip so the tool can return current_sha.
type ErrConflict struct {
    Branch     string
    BaseSHA    string
    CurrentSHA string
    Changed    []string
}
func (e *ErrConflict) Error() string { return "optimistic lock conflict on " + e.Branch }
```

`SiteService` returns typed sentinel errors (`ErrConflict`, `ErrNotFound`,
`ErrValidation`, `ErrQuota`) that the MCP layer maps to §6 codes via
`errors.As`.

### 7.2 Tool arg/result structs (jsonschema-tagged for auto schema)

The SDK infers the JSON Schema from these structs (descriptions via the
`jsonschema` tag). Pointers mark optional fields; required fields are non-pointer.

```go
package mcpserver

// ---- read_file ----
type ReadFileArgs struct {
    Site   string  `json:"site" jsonschema:"project handle (from list_sites) to read from"`
    Path   string  `json:"path" jsonschema:"file path relative to the site root, POSIX style"`
    Branch *string `json:"branch,omitempty" jsonschema:"branch name; defaults to the working branch (draft)"`
    Ref    *string `json:"ref,omitempty" jsonschema:"commit SHA to read at; defaults to the branch tip"`
}
type ReadFileResult struct {
    Path     string `json:"path"`
    Branch   string `json:"branch"`
    Commit   string `json:"commit"`   // pass this back as base_sha to write_file
    BlobSHA  string `json:"blob_sha"`
    Encoding string `json:"encoding"` // "utf-8" | "base64"
    Content  string `json:"content"`
    Size     int64  `json:"size"`
    Truncated bool  `json:"truncated"`
}

// ---- write_file ----
type WriteFileArgs struct {
    Site     string  `json:"site" jsonschema:"project handle (from list_sites) to write to"`
    Path     string  `json:"path" jsonschema:"file path relative to the site root"`
    Content  string  `json:"content" jsonschema:"full new file contents"`
    Encoding *string `json:"encoding,omitempty" jsonschema:"utf-8 (default) or base64 for binary"`
    BaseSHA  string  `json:"base_sha" jsonschema:"REQUIRED commit SHA the edit is based on (from read_file.commit). Stale value is rejected with a conflict error."`
    Branch   *string `json:"branch,omitempty" jsonschema:"target branch; 'published' is not writable"`
    Commit   *bool   `json:"commit,omitempty" jsonschema:"create a commit after writing; default true"`
    Message  *string `json:"message,omitempty" jsonschema:"commit message"`
}
type WriteFileResult struct {
    Path         string   `json:"path"`
    Branch       string   `json:"branch"`
    Committed    bool     `json:"committed"`
    Commit       string   `json:"commit"`
    BlobSHA      string   `json:"blob_sha"`
    Pushed       bool     `json:"pushed"`
    BytesWritten int      `json:"bytes_written"`
    Warnings     []string `json:"warnings,omitempty"`
}

// ---- publish ----
type PublishArgs struct {
    Site    string  `json:"site" jsonschema:"project handle (from list_sites) to publish"`
    From    *string `json:"from,omitempty" jsonschema:"source branch to publish; default working branch"`
    BaseSHA string  `json:"base_sha" jsonschema:"REQUIRED tip SHA of the source branch you intend to publish"`
    Message *string `json:"message,omitempty"`
}
type PublishResult struct {
    PublishedCommit string `json:"published_commit"`
    PublishedURL    string `json:"published_url"`
    From            string `json:"from"`
    FromCommit      string `json:"from_commit"`
    Pushed          bool   `json:"pushed"`
}
// (list_sites, list_files, create_site, save, get_diff, get_log, rollback
//  follow the same pattern; see §5 for their JSON shapes.)
```

### 7.3 Registration

```go
func (r *registry) registerAll(s *mcp.Server) {
    addTool(s, r, "read_file",  "Read a file from a site.",                scopeRead,    r.readFile)
    addTool(s, r, "write_file", "Write a file (requires base_sha).",       scopeWrite,   r.writeFile)
    addTool(s, r, "publish",    "Promote a branch to the live site.",      scopePublish, r.publish)
    // ...all 10...
}

// addTool wraps with the scope/site guard (§4.4) then registers via the SDK.
func addTool[In, Out any](
    s *mcp.Server, r *registry, name, desc string, sc scope,
    fn func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error),
) {
    guarded := guardTyped(sc, fn) // injects claims, checks scope, then calls fn
    mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, guarded)
}
```

---

## 8. Example client config (end-user's local AI → remote kotoji)

The user issues a personal token in the kotoji dashboard
(`Settings ▸ MCP / API tokens ▸ New`), copies it once, and configures their
local client to reach the **remote** MCP endpoint. One token covers ALL the
user's projects (it auto-spans their memberships), so a single MCP server entry
is enough — the client picks a project per call via the `site` selector.

### 8.1 Claude Desktop / Claude Code (remote Streamable HTTP)

```jsonc
// claude_desktop_config.json  (or .mcp.json for Claude Code)
{
  "mcpServers": {
    "kotoji-expense-calc": {
      "type": "http",
      "url": "https://hosting.example.com/mcp",
      "headers": {
        "Authorization": "Bearer kotoji_pat_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
      }
    }
  }
}
```

A single entry is enough for **all** your projects: the token is per-user and
auto-covers every project you're a member of. Choose the target project per call
with the `site` (handle) selector; `list_sites` enumerates the projects this
token can reach. (Name the entry just `kotoji` rather than per-site.)

### 8.2 Claude Code CLI one-liner

```bash
claude mcp add --transport http kotoji-expense-calc \
  https://hosting.example.com/mcp \
  --header "Authorization: Bearer kotoji_pat_XXXX..."
```

### 8.3 Local dev

```jsonc
{
  "mcpServers": {
    "kotoji-dev": {
      "type": "http",
      "url": "http://kotoji.localhost:8080/mcp",
      "headers": { "Authorization": "Bearer kotoji_pat_DEVTOKEN..." }
    }
  }
}
```

In `dev/no-auth` mode the verifier accepts a fixed dev token bound to a seeded
dev site so the connection works with zero setup.

---

## 9. Server instructions (model-facing)

`ServerOptions.Instructions` ships guidance the client model sees on connect.
Keep it short and behavioural:

```
This server hosts web projects for one user. Every tool takes a `site` (project
handle); you can only reach projects you're a member of. Call list_sites first to
see them. To edit safely:
1. read_file → note the returned `commit`.
2. write_file with the same `site` and base_sha = that `commit`.
3. If you get a `conflict` error, the file changed underneath you: read_file
   again and redo your edit on the new content. Never retry blindly.
Saving commits and mirrors to backup; it does NOT make the change live.
Use `publish` to make the working branch live (this is the "go live" action).
Static files only: .html/.css/.js/images/fonts. No server code, no build step.
```

---

## 10. Idempotency & limits

### 10.1 Idempotency

- **Reads** (`list_*`, `read_file`, `get_diff`, `get_log`) are naturally idempotent.
- **`write_file` / `save` / `publish` / `rollback`** are idempotent **per
  `base_sha`**: the first call advances the tip; a retried call with the *same
  now-stale* `base_sha` returns `conflict` (not a duplicate commit). A retry
  with the *new* tip + identical content is a `no_op` commit avoided by content
  hashing (`save` returns `no_op:true` when the tree is unchanged). This makes
  client retries safe without an explicit idempotency key.
- **`create_site`** is idempotent on `handle`: a second call with the same handle
  returns `conflict` (handle taken), never a duplicate repo.

### 10.2 Size limits (constants, configurable via env)

| Limit | Default | Env |
|---|---|---|
| Single file write (`write_file.content`) | 5 MiB | `KOTOJI_MCP_MAX_FILE_BYTES` |
| Single file read (returned inline) | 1 MiB (then `truncated:true`) | `KOTOJI_MCP_MAX_READ_BYTES` |
| `get_diff` total patch bytes | 1 MiB (then `truncated:true`, patches dropped) | `KOTOJI_MCP_MAX_DIFF_BYTES` |
| `list_files` max entries | 5000 | `KOTOJI_MCP_MAX_LIST` |
| `get_log.limit` | default 20, max 100 | — |
| Request body (whole JSON-RPC msg) | 8 MiB | `KOTOJI_MCP_MAX_BODY_BYTES` |

These mirror the Zip-upload guards so MCP is not a bypass around the
upload-path's zip-bomb/size protections. Binary files larger than the read limit
return `too_large` with guidance to use the editor (we won't stream multi-MB
base64 through chat).

### 10.3 Rate limiting

Token-bucket **per `token_id`** (not per IP — a token is the unit of trust):

| Class | Default rate |
|---|---|
| Read tools | 120 / min, burst 30 |
| Write tools (`write_file`,`save`,`rollback`) | 30 / min, burst 10 |
| `publish` | 6 / min, burst 3 |
| `create_site` | 3 / min, burst 3 |

Over-limit → `rate_limited` tool error with `retryable:true` and a `retry_after`
seconds hint in `details`. Limiter is an interface (`Limiter.Allow(tokenID,
class) (bool, retryAfter)`) so tests inject a deterministic fake and prod can
swap an in-memory bucket for a Postgres/Redis-backed one.

---

## 11. Safety guarantees (summary checklist)

1. **Per-user, membership-capped token.** A token is owned by a user and spans all
   their projects; every content tool takes a `site` selector. The effective scope
   on a site is `token.scopes ∩ the membership role's scopes`, re-evaluated per
   request, so a token can **never exceed its owner's own access** and a non-member
   target returns `not_found` (no existence leak). Removing/downgrading a
   membership limits the token instantly — no re-issue.
2. **No credential minting over MCP.** `create_site` (when enabled) returns a
   site but never a token; tokens are issued only in the authenticated dashboard.
3. **`create_site` off by default** (`user_tokens.can_create_sites`, also capped by
   `users.can_create_sites`) — prevents privilege escalation from "edit my
   projects" to "spawn projects".
4. **Optimistic locking everywhere it mutates.** `base_sha` is required on
   `write_file`/`save`/`publish`/`rollback`; the check is atomic under a per-site
   write lock (no TOCTOU).
5. **No destructive history.** `rollback` is a forward revert commit; no
   force-push, no history rewrite reachable via MCP.
6. **`published` is not writable** via MCP content tools; it is reachable only
   through `publish` (and the server-side GitHub-merge webhook path).
7. **Path & extension confinement** inside the site, same allowlist as upload;
   `.git/`, `..`, absolute paths rejected; `SiteService` re-validates (defence in
   depth).
8. **Token at rest = hash only** (`sha256`); plaintext shown once; prefix lets us
   leak-scan; revocation + expiry honoured on every call.
9. **Mirror-push failures don't fail saves** (server is source of truth) but are
   surfaced as warnings — backup never becomes a hard dependency.
10. **MCP is control-plane only**, never on `*.hosting.example.com`; the data
    plane stays read-only static serving with strict CSP.
11. **No cookies on `/mcp`** — bearer-token only, so a malicious page can't
    drive the MCP endpoint via the user's session (no CSRF surface).
12. **Internal errors are never leaked** to the model (Go-error path → generic
    `internal`; details only in server logs with request-id).

---

## 12. Test matrix (TDD)

`SiteService` is mocked at the interface for all unit tests; integration tests
run a real temp git repo + ephemeral Postgres (testcontainers or `pg-tmp`).

### 12.1 Unit — verifier & auth (mock sqlc querier)
- valid token → `TokenInfo` with correct `UserID`/`TokenID`/`Scopes`/`CanCreateSites` (no `SiteID`).
- `can_create_sites` capped by the owning user's flag (AND of both).
- malformed prefix / unknown hash / revoked / expired / inactive owner → unwraps to `ErrInvalidToken`.
- DB error → non-`ErrInvalidToken` error (maps to 500, not 401).
- `last_used_at` touch is async and never blocks/fails the request.

### 12.2 Unit — membership-cap authz (mock SiteService + membership querier)
- table-driven authz matrix: each (token scope × membership role) → effective
  scope = intersection; a tool requiring a scope outside it → `forbidden`.
- **token(write) + user is viewer on the site → write denied, read ok**.
- **non-membership**: a user NOT a member of the named site → `not_found` on every
  tool (no existence leak).
- **pivot attempt**: token of user A naming a site A is not a member of → `not_found`
  before any SiteService touch (membership-capped, replaces the old site-pin).
- `list_sites` returns only the user's memberships (with effective scope per site).
- `create_site` requires `token.can_create_sites AND users.can_create_sites` → else `forbidden`.

### 12.3 Unit — per-tool behaviour (mock SiteService)
- `read_file` echoes `commit` as the value usable for `base_sha`; base64 path for binary; `truncated` at the read limit.
- `write_file`: missing `base_sha` → `validation`; `branch:"published"` → `validation`; mismatched `base_sha` → maps `ErrConflict` to `conflict` with `current_sha`/`changed_paths`; success returns new `commit`.
- `write_file` mirror-push failure → `committed:true, pushed:false`, warning present, **no Go error**.
- `save` clean tree → `no_op:true`.
- `publish` stale `base_sha` → `conflict`; success updates DB `is_published` (assert mock call).
- `rollback` unreachable `to_sha` → `not_found`; success returns forward commit.
- `get_diff` two-ref vs single-commit modes; `name-status` omits patches; over-limit → `truncated`.
- `get_log` pagination via `before`; `limit` > 100 → `validation`; `via` provenance present.
- `list_sites` returns exactly the pinned site; deleted site → `not_found`.
- path validation: `..`, absolute, `.git/`, NUL, disallowed extension → `validation` (one test per case).

### 12.4 Unit — limits & rate limiting (fake Limiter)
- file > max bytes → `too_large`; body > max → rejected.
- limiter denies → `rate_limited` with `retry_after`; read vs write vs publish classes use the right buckets.
- idempotent retry with stale `base_sha` → `conflict` (no duplicate commit on mock).

### 12.5 Integration — real git + real SDK transport
- spin the MCP HTTP handler; connect with the SDK client over Streamable HTTP using a real seeded token.
- end-to-end optimistic loop: `read_file` → `write_file(base_sha)` → second `write_file` with the old `base_sha` → `conflict`; re-read → succeed.
- `write_file(commit:true)` then `get_log` shows the commit with `via:"mcp"` and the `Kotoji-Via` trailer.
- `publish` makes the published tree match draft (read it back via the data-plane resolver in a test harness).
- two concurrent `write_file` calls on the same branch: exactly one wins, the other gets `conflict` (exercises the per-site lock).
- 401 path: missing/garbage `Authorization` header → handler returns 401 with `WWW-Authenticate` before any tool runs.

### 12.6 Security regression tests
- token of user A cannot reach a site A is not a member of (attempt every tool naming that site → `not_found` before any SiteService touch).
- membership downgrade limits the token immediately (write token → user downgraded to viewer → write `forbidden`, read still ok; no re-issue).
- revoked token mid-session: a connection that authenticated, then the token is revoked, fails on the next tool call (verifier hit per call, not cached indefinitely).

---

## 13. Open questions / gaps (考慮漏れ)

1. **Multi-site / account-wide tokens — RESOLVED (per-user model).** A token is
   now owned by a user and automatically covers all of that user's memberships;
   content tools take a `site` argument validated against the user's membership at
   call time (membership-capped). The former pivot risk is closed by the
   membership cap (a token can never act outside its user's memberships and never
   exceed the user's role-derived access), not by removing the site argument.

2. **`create_site` over MCP at all.** Disabled by default (capability flag). Is
   "the AI creates a new project autonomously" a desired flow or an anti-feature
   for non-engineers? Need a product decision on whether the flag is ever exposed
   in the dashboard, and if so, how the resulting site's first token is issued
   (since we refuse to mint tokens over MCP).

3. **Token verification on every call vs. session caching.** Current design hits
   the DB per tool call (so revocation is near-immediate). Under heavy AI loops
   this is N queries. Acceptable? Or cache `TokenInfo` for e.g. 30s with a
   revocation epoch? Caching weakens instant-revoke. Recommend per-call for v1,
   revisit with metrics.

4. **Streamable HTTP session statefulness.** The SDK supports stateful (with
   `Mcp-Session-Id`) and stateless modes. Stateless is simpler and scales
   horizontally trivially (no sticky sessions), but loses server→client
   notifications (e.g. "someone else published, your draft changed"). Decide:
   stateless v1, or stateful for push notifications? This affects the
   `StreamableHTTPOptions` and whether we need session affinity at the proxy.

5. **Binary writes via base64.** `write_file` accepts base64 for images/fonts,
   but pushing a multi-MB image through an AI chat turn is wasteful and bumps the
   8 MiB body limit. Should binaries be **upload-only** (Zip/dashboard) and MCP
   `write_file` be text-only? Leaning yes; needs confirming.

6. **Branch creation over MCP.** No tool currently *creates* a branch
   (`feature-*`). `write_file`/`save` assume the branch exists. Do AIs need
   `create_branch` / does `write_file(branch:"feature-x")` auto-create on first
   write? Auto-create is convenient but lets an AI spawn arbitrary branches
   (each gets a preview URL → resource use). Needs a decision + possibly a
   `create_branch` tool with its own limits.

7. **`git author` identity for MCP commits.** We set author = token's
   `UserID`'s email. But the *machine/agent* identity (which laptop, which model)
   is also useful for audit. Currently captured loosely via `Kotoji-Via: mcp`.
   Consider a richer trailer (`Kotoji-Token: <token-name>`), but never leak the
   token value into git history.

8. **Concurrent same-branch edits from MCP + Monaco simultaneously.** The
   per-site write lock serializes commits, and `base_sha` catches staleness, but
   the UX of "the AI just changed your draft while you were typing in Monaco" is
   unspecified. Likely a frontend concern (live-reload + conflict toast), but the
   MCP `conflict` contract must stay stable for the UI to rely on.

9. **MCP resources/prompts.** This doc defines only **tools**. The MCP spec also
   has *resources* (could expose files as readable resources) and *prompts*.
   Out of scope for v1, but listing here so it's a conscious omission, not an
   oversight.

10. **OAuth flow for MCP (RFC 9728 / dynamic client registration).** We use
    static bearer tokens. The SDK's `RequireBearerTokenOptions.ResourceMetadataURL`
    enables the discovery flow some clients prefer. Static tokens are simpler and
    fit "copy from dashboard"; revisit if a client mandates the OAuth dance.
