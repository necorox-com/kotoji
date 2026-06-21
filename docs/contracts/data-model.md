# kotoji — Data Model Contract (PostgreSQL)

> **Scope:** the authoritative relational schema for kotoji's **metadata** store.
> Raw SQL DDL (for [goose](https://github.com/pressly/goose) migrations) + [sqlc](https://sqlc.dev) query
> definitions (compiled against [pgx v5](https://github.com/jackc/pgx)). **No ORM. No Drizzle.**
>
> Status: design-locked draft. Companion contracts (referenced, not yet written):
> `docs/contracts/site-service.md`, `docs/contracts/api-openapi.yaml`, `docs/contracts/mcp-tools.md`.

---

## 0. The single most important rule: GIT is authoritative, DB is a directory

kotoji has **two stores** and they must never disagree about who owns what.

| Concern | Owner | Notes |
|---|---|---|
| File contents, file tree | **git** | `/data/sites/{uuid}/.git` |
| Branches & which exist | **git** | enumerated on demand via `git for-each-ref`; **NOT** mirrored in a `branches` table |
| Commit history, diffs, log | **git** | `git log` / `git diff`, never reconstructed from DB |
| What "draft" / "published" point at | **git refs** (`refs/heads/draft`, `refs/heads/published`) | source of truth |
| Which site a handle maps to | **DB** | git can't answer "who owns subdomain `expense-calc`" |
| Who may touch a site (authz) | **DB** | `site_members`, `site_tokens` |
| Identity / sessions | **DB** | OIDC subject → user, opaque session cookie |
| Rename redirects | **DB** | `handle_redirects` |
| Audit trail | **DB** | append-only `audit_log` |
| `published_commit_sha`, `default_branch` | **DB (cache pointer only)** | see §1.3 |

**Cache-pointer doctrine:** the DB stores *pointers into git* (`published_commit_sha`,
`default_branch`) purely so the **data plane can answer a request without shelling out to git on the
hot path**, and so the dashboard can show a "last published" badge cheaply. These are a **read-through
cache**: git is still authoritative. If the cache and git disagree, **git wins** and the row is
corrected by the SiteService on its next write. We deliberately do **not** mirror full branch/commit
state into Postgres — that is the classic dual-source-of-truth bug.

### Why NO `branches` table (decision + justification)

A `branches` table was considered and **rejected**:

1. **Dual source of truth.** Branches live in git refs. A table would have to be kept in lockstep
   with every `git branch`, `git push`, `git branch -d` (including ones that arrive via GitHub
   webhook pulls we did not initiate). Reconciliation bugs are inevitable.
2. **The data plane already needs git for content anyway.** Resolving `{handle}--{branch}` to bytes
   requires reading the git tree; confirming the branch exists is the same `git` round-trip
   (`git rev-parse --verify refs/heads/{branch}`), so a DB lookup saves nothing on that path.
3. **Branch list is a cold-path, control-plane concern** (rendering the branch switcher). One
   `git for-each-ref --format` per page load is cheap and always correct.

**Consequence / accepted cost:** we cannot do SQL like "all sites with an open `feature-*` branch"
without touching git. That is acceptable; such queries are admin/analytics, not hot path. If we ever
need them we add a **denormalized, explicitly-a-cache** `site_branch_cache` table refreshed by the
SiteService — never treated as authoritative. (Listed under Open Questions.)

The **only** branch pointers in the DB are `sites.default_branch` (a name, e.g. `draft`) and
`sites.published_commit_sha` (a cached SHA), justified above.

---

## 1. DDL

All DDL below is split into goose migrations (see §5). Presented here as one logical schema for
review. Target: **PostgreSQL 17/18**.

### 1.0 Extensions, enums, and shared constants

```sql
-- 00001_extensions_and_enums.up.sql

-- citext: case-insensitive handles & emails. Handles are DNS labels (always lowercased on
-- write), but citext gives us a defense-in-depth UNIQUE that is case-insensitive even if a
-- code path forgets to lowercase. Emails are naturally case-insensitive.
CREATE EXTENSION IF NOT EXISTS citext;

-- pgcrypto: gen_random_uuid() for UUID v4 PKs (also available natively in PG13+ but we make
-- the dependency explicit; we use gen_random_uuid()).
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Role of a user *on a specific site*. owner > editor > viewer.
--   owner  : full control incl. delete, rename, member mgmt, publish.
--   editor : save/commit, create branches, request publish, use Monaco/MCP write.
--   viewer : read files, read history, read preview. No writes.
CREATE TYPE site_role AS ENUM ('owner', 'editor', 'viewer');

-- Visibility of a site's *served content* (data plane). Authz on the control plane is always
-- enforced regardless; this governs anonymous access to the public URL.
--   public   : anyone with the URL can view served content (no login).
--   internal : served only to authenticated users of this kotoji instance.
--   private  : served only to site members (owner/editor/viewer).
CREATE TYPE site_visibility AS ENUM ('public', 'internal', 'private');

-- Which of the three "writers" produced an audited action. Matches the spec's three writers.
CREATE TYPE audit_source AS ENUM ('upload', 'editor', 'mcp', 'system');
--   upload : zip upload path
--   editor : Monaco / dashboard
--   mcp    : MCP tool call from an external AI client
--   system : webhook pulls, scheduled jobs, admin actions, migrations
```

> **Enum evolution note.** Postgres enums only allow `ADD VALUE` (no easy remove/reorder) and
> `ADD VALUE` cannot run inside a transaction block in older PGs. Our enums are small and stable.
> If churn appears (it shouldn't for `site_role`/`visibility`), we migrate that column to a
> `TEXT` + `CHECK (... IN (...))` constraint, which is trivially alterable. We start with native
> enums for type safety in sqlc-generated Go (`sqlc` maps them to Go typed string constants).

### 1.1 `users`

The human. Identity-provider-agnostic: a user can have multiple `user_identities` (e.g. Google
today, Keycloak tomorrow) linked to one `users` row.

```sql
-- 00002_users.up.sql

CREATE TABLE users (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         CITEXT      NOT NULL UNIQUE,         -- canonical contact; case-insensitive
    display_name  TEXT        NOT NULL DEFAULT '',     -- from OIDC `name` claim; may be empty
    avatar_url    TEXT,                                -- from OIDC `picture` claim; nullable
    is_admin      BOOLEAN     NOT NULL DEFAULT FALSE,  -- instance superuser (admin screen, quotas)
    is_active     BOOLEAN     NOT NULL DEFAULT TRUE,   -- soft disable; deactivated users can't log in
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Lookups by email happen at OIDC callback time (match-or-create) and in admin search.
-- UNIQUE on email already creates an index; no extra index needed.
```

**Rationale.**
- `email CITEXT UNIQUE` — email is the join key between the abstract IdP and a kotoji user; many
  IdPs return mixed-case. citext makes `Foo@x.com` == `foo@x.com`.
- `is_admin` is a coarse instance-level flag (distinct from per-site `site_role`). Admin screen,
  quota mgmt, reserved-word edits.
- `is_active` enables soft-disable without cascading deletes of authored content/audit history.
- No password column: passwords belong to the IdP. (The admin-password *dev mode* is an instance
  secret in `Config`, not a per-user credential — see Auth contract.)

### 1.2 `user_identities`

One row per (provider, subject) pair. This is the OIDC abstraction's DB footprint.

```sql
-- 00003_user_identities.up.sql

CREATE TABLE user_identities (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider       TEXT        NOT NULL,   -- AuthProvider key, e.g. 'google', 'keycloak', 'dev'
    subject        TEXT        NOT NULL,   -- OIDC `sub` claim: stable, opaque, provider-scoped
    email_at_link  CITEXT,                 -- email seen at link time (audit; may drift from users.email)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at  TIMESTAMPTZ,

    -- A given (provider, subject) maps to exactly one kotoji user.
    CONSTRAINT user_identities_provider_subject_key UNIQUE (provider, subject)
);

-- Hot path: every login resolves (provider, subject) -> user. UNIQUE above provides the index.
-- Reverse lookup (a user's linked identities) for the account screen:
CREATE INDEX idx_user_identities_user_id ON user_identities (user_id);
```

**Rationale.**
- **Match on `sub`, not email.** OIDC `sub` is the stable identifier; emails change. We store
  `email_at_link` only for audit/debug.
- `provider` is the `AuthProvider` registry key, not a hardcoded enum — new providers are config,
  not migrations. (`dev` is the no-auth/admin-password pseudo-provider.)
- `ON DELETE CASCADE` from `users`: deleting a user removes their identities. (Real-world we
  prefer `is_active=false`; hard delete is admin-only and rare.)

### 1.3 `sites`

The project. UUID is the immutable PK and the on-disk path (`/data/sites/{id}/.git`); `handle` is
the renameable DNS-safe public name.

```sql
-- 00004_sites.up.sql

CREATE TABLE sites (
    id                   UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
    handle               CITEXT          NOT NULL UNIQUE,   -- DNS-safe public name & GitHub repo name
    owner_id             UUID            NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    visibility           site_visibility NOT NULL DEFAULT 'private',
    default_branch       TEXT            NOT NULL DEFAULT 'draft',  -- working branch name (a git ref name)
    published_commit_sha TEXT,                              -- CACHE pointer into git; NULL = never published
    github_repo          TEXT,                              -- 'owner/name' for mirror push; NULL = no mirror
    description          TEXT            NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ     NOT NULL DEFAULT now(),

    -- Handle validation enforced at the DB edge as defense-in-depth. The application
    -- (Go validator, see §3) is the primary gate with friendly errors; this CHECK guarantees
    -- no malformed handle can ever land via any path (incl. manual SQL, future code).
    CONSTRAINT sites_handle_format CHECK (
        handle ~ '^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$'  -- DNS label: lower alnum + hyphen, no edge hyphen
    ),
    -- A published_commit_sha, if present, must look like a git object id (40 hex = SHA-1,
    -- 64 hex = SHA-256 to be forward-compatible with git's SHA-256 mode).
    CONSTRAINT sites_published_sha_format CHECK (
        published_commit_sha IS NULL OR published_commit_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
    )
);

-- Authoritative resolver index: handle -> site (data plane hot path). UNIQUE provides it.
-- List-by-owner (dashboard) and admin "all sites by owner":
CREATE INDEX idx_sites_owner_id ON sites (owner_id);
-- Recently-updated ordering on the dashboard:
CREATE INDEX idx_sites_updated_at ON sites (updated_at DESC);

-- Keep updated_at fresh on any row change (also bumped explicitly by SiteService on commit).
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_sites_updated_at
    BEFORE UPDATE ON sites
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
```

**Rationale.**
- `handle CITEXT UNIQUE` — case-insensitive uniqueness; subdomains/repos are case-folded anyway.
  The reserved-word block is enforced in app code (§3) because it needs friendly errors and the
  list lives in a Go constant; we *additionally* could add a `CHECK (handle NOT IN (...))` but a
  long literal list in a CHECK is unmaintainable — instead we add a separate `reserved_handles`
  table (§1.3a) so admins can edit it (spec mentions admin-managed 予約語).
- `owner_id ... ON DELETE RESTRICT` — you cannot delete a user who still owns sites; the admin must
  transfer ownership first. Protects against accidental orphaning of served content + git repos on
  disk. (Contrast with `is_active=false` for everyday "disable a person".)
- `default_branch TEXT` — the working branch *name*, default `draft`. It is a git ref name, not a
  FK to a branches table (none exists). Generalized so a site could default to a `feature-*`.
- `published_commit_sha` — **the only commit SHA in the DB**, explicitly a cache (see §0). Lets the
  data plane resolve `{handle}` → published tree, and lets the dashboard show "published @ abc123"
  without a git call. Updated transactionally by the SiteService at publish time; reconciled to git
  if ever stale.
- `github_repo` nullable — mirror-push is optional. Stored as `owner/name`; the actual remote URL
  + credentials are instance config / secrets, not per-row.
- We store **no `draft_commit_sha`**: the draft head is read from git on demand in the editor
  (it's already a git round-trip to load file contents), so caching it would add a stale-pointer
  surface for no hot-path win.

#### 1.3a `reserved_handles` (admin-editable reserved-word block)

```sql
-- 00005_reserved_handles.up.sql

CREATE TABLE reserved_handles (
    handle      CITEXT      PRIMARY KEY,
    reason      TEXT        NOT NULL DEFAULT '',   -- why reserved (shown to admin)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Seeded (see §6) with the locked reserved list. The Go validator loads this set (cached, refreshed
on admin edit) and also hardcodes the same baseline as a fallback constant (§3) so a fresh DB is
safe before the seed runs.

### 1.4 `sessions`

Server-side sessions. The cookie holds only an **opaque, high-entropy id**; everything else lives
here. (Spec: "server-side sessions stored in Postgres (cookie holds opaque session id)".)

```sql
-- 00006_sessions.up.sql

CREATE TABLE sessions (
    id           TEXT        PRIMARY KEY,            -- opaque random id (the cookie value), e.g. 32 bytes base64url
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,              -- absolute expiry; checked on every request
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),-- for idle-timeout / "active sessions" UI
    user_agent   TEXT        NOT NULL DEFAULT '',   -- audit: where this session lives
    ip_addr      INET                                -- audit: last-seen IP (nullable; behind proxy)
);

-- Validate-by-cookie on every authenticated request: PK lookup on id (already indexed).
-- Sweep expired sessions (janitor job) and "log out everywhere" (delete by user):
CREATE INDEX idx_sessions_user_id    ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);
```

**Rationale.**
- **Opaque id, not JWT.** Server-side sessions are revocable instantly (delete the row) — important
  for a multi-user instance where an admin must be able to kill a session now. The cookie is
  `HttpOnly; Secure; SameSite=Lax` (see Auth contract), value = `sessions.id`.
- `id TEXT PRIMARY KEY` generated by the app (CSPRNG, e.g. 256-bit base64url) — **not** a guessable
  UUID. We keep it `TEXT` so the encoding is the app's choice.
- `expires_at` absolute + `last_seen_at` for optional idle timeout and an "active sessions" screen.
- `ON DELETE CASCADE` from users so deactivating/deleting a user nukes their sessions.
- Expired-session GC: a periodic `DELETE FROM sessions WHERE expires_at < now()` (the `system`
  janitor); index on `expires_at` keeps it cheap.

### 1.5 `site_members`

Per-site authorization. Composite PK `(site_id, user_id)` — a user has at most one role per site.

```sql
-- 00007_site_members.up.sql

CREATE TABLE site_members (
    site_id    UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       site_role   NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID        REFERENCES users(id) ON DELETE SET NULL,  -- who granted (audit), nullable

    PRIMARY KEY (site_id, user_id)
);

-- Authz check is by (site_id, user_id) -> covered by the PK.
-- "Which sites can this user access" (dashboard incl. shared sites):
CREATE INDEX idx_site_members_user_id ON site_members (user_id);
```

**Rationale.**
- Composite PK `(site_id, user_id)` enforces one-role-per-user-per-site and is the natural authz
  lookup key.
- The **site owner** is denormalized on `sites.owner_id` for cheap "who owns this" and RESTRICT
  semantics; we **also** ensure an `owner` row exists in `site_members` (created in the same tx as
  the site) so a single authz query (`role >= editor?`) covers owner + members uniformly. The
  owner's `site_members.role` is `'owner'` and is kept in sync if ownership transfers.
- `ON DELETE CASCADE` on both FKs: removing a site or a user cleans up memberships.
- `created_by ... ON DELETE SET NULL`: keep the grant record even if the granting admin is later
  deleted.

### 1.6 `site_tokens`

Per-project MCP/API tokens. **We store only a hash** of the token; the plaintext is shown once at
creation and never again.

```sql
-- 00008_site_tokens.up.sql

CREATE TABLE site_tokens (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id      UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    created_by   UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,  -- the human acting via this token
    name         TEXT        NOT NULL DEFAULT '',     -- human label, e.g. "Claude on my laptop"
    token_prefix TEXT        NOT NULL,                -- first ~8 chars (e.g. 'ktj_a1b2') for UI display + fast lookup
    token_hash   BYTEA       NOT NULL,                -- SHA-256 of the full secret; constant-time compared
    scope        site_role   NOT NULL DEFAULT 'editor',  -- max role this token may exercise on the site
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,                          -- updated (throttled) on use
    expires_at   TIMESTAMPTZ,                          -- optional hard expiry; NULL = no expiry
    revoked_at   TIMESTAMPTZ,                          -- soft revoke; NULL = active

    CONSTRAINT site_tokens_hash_len CHECK (octet_length(token_hash) = 32)  -- SHA-256 = 32 bytes
);

-- Auth-by-token hot path: presented token -> prefix -> candidate rows -> constant-time hash compare.
-- (prefix is NOT unique on its own; collisions possible, hence we still hash-compare.)
CREATE INDEX idx_site_tokens_prefix ON site_tokens (token_prefix) WHERE revoked_at IS NULL;
-- Token management UI: list a site's tokens.
CREATE INDEX idx_site_tokens_site_id ON site_tokens (site_id);
-- Defense in depth: the same hash should never exist twice.
CREATE UNIQUE INDEX uq_site_tokens_hash ON site_tokens (token_hash);
```

**Rationale.**
- **Hash only, never plaintext.** Token format (app-side): `ktj_{site-short}_{random}`; we persist
  `token_prefix` (for display "ktj_a1b2…" and a cheap index lookup) and `token_hash =
  SHA-256(plaintext)`. Lookup: find candidates by prefix among non-revoked, then `subtle`
  constant-time compare the full hash. SHA-256 (not bcrypt) is correct here because the secret is
  **high-entropy random**, not a human password — fast verification, no rainbow-table risk.
- `scope site_role` — a token's max authority. The spec wants per-project scope; the *project* is
  already implied by `site_id`, and `scope` further bounds it to viewer/editor/owner. (`owner`-scope
  tokens are discouraged; default `editor` lets MCP write/save/publish but not delete the site.)
- `last_used_at` updated **throttled** (e.g. at most once/minute) to avoid a write on every MCP call.
- `expires_at` + `revoked_at` give both proactive expiry and instant revocation. The auth query
  filters `revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`.
- `created_by ON DELETE CASCADE` — a token acts *as* a human; if that human is deleted, their tokens
  die with them (no orphan credentials).

### 1.7 `handle_redirects`

When a site is renamed `old → new`, old subdomains/URLs must 301 to the new handle. This table maps
**former** handles to the current site.

```sql
-- 00009_handle_redirects.up.sql

CREATE TABLE handle_redirects (
    old_handle  CITEXT      PRIMARY KEY,                                  -- the freed-up former handle
    site_id     UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A former handle must itself be a valid DNS label (it was a real handle once).
    CONSTRAINT handle_redirects_format CHECK (
        old_handle ~ '^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$'
    )
);

-- Reverse lookup: all former names of a site (settings UI). Forward lookup is the PK.
CREATE INDEX idx_handle_redirects_site_id ON handle_redirects (site_id);
```

**Rationale & interaction with `sites.handle`.**
- **Uniqueness across the whole namespace.** A freed handle must not be (a) re-issued as a live
  `sites.handle`, nor (b) collide with another redirect. Postgres can't put a single UNIQUE across
  two tables, so the **application** enforces, in one transaction at create/rename time, that a
  candidate handle exists in *neither* `sites.handle` *nor* `handle_redirects.old_handle` (nor
  `reserved_handles`). The resolver checks `sites` first, then `handle_redirects`.
- **Rename flow (atomic, in SiteService):** `INSERT handle_redirects(old_handle=OLD, site_id)` +
  `UPDATE sites SET handle=NEW`. If the *new* handle is itself an old redirect of the *same* site
  (rename back), delete that stale redirect row.
- `ON DELETE CASCADE` — deleting the site frees all its former handles.
- Redirects are forever (cheap rows); an admin can prune. We do **not** auto-expire to avoid
  breaking external links.

### 1.8 `audit_log`

Append-only trail of who did what, from which writer. Powers the "who changed this" view and is a
security primitive (AI write access + multi-user).

```sql
-- 00010_audit_log.up.sql

CREATE TABLE audit_log (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,  -- monotonic; cheap append
    actor_id    UUID         REFERENCES users(id) ON DELETE SET NULL,  -- human actor; NULL for pure system
    site_id     UUID         REFERENCES sites(id) ON DELETE SET NULL,  -- target site; NULL for instance-level
    action      TEXT         NOT NULL,            -- verb, e.g. 'site.create','file.write','publish','rollback'
    source      audit_source NOT NULL,            -- which writer/origin: upload/editor/mcp/system
    token_id    UUID         REFERENCES site_tokens(id) ON DELETE SET NULL,  -- if via a token (MCP/API)
    commit_sha  TEXT,                             -- resulting git commit, if the action made one
    metadata    JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- action-specific detail (paths, branch, ip, base_sha…)
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Per-site activity feed (most-recent-first) is the dominant read.
CREATE INDEX idx_audit_log_site_created ON audit_log (site_id, created_at DESC);
-- Admin "what did this user do":
CREATE INDEX idx_audit_log_actor_created ON audit_log (actor_id, created_at DESC);
-- Ad-hoc structured queries on metadata (e.g. metadata->>'branch'):
CREATE INDEX idx_audit_log_metadata ON audit_log USING gin (metadata);
```

**Rationale.**
- `BIGINT IDENTITY` PK — append-only, monotonic, smaller/faster than UUID for a high-write log; no
  natural need to reference these rows by id elsewhere.
- **All FKs `ON DELETE SET NULL`.** The audit trail must **survive** deletion of the user, site, or
  token it references — that's the whole point of an audit log. We keep `commit_sha` and `metadata`
  so the record remains meaningful even after the referenced rows vanish. (`site_id` SET NULL means
  we should also denormalize the handle into `metadata.handle` at write time so post-delete records
  are still readable.)
- `action TEXT` (not enum) — verbs grow freely; a constrained enum here would force a migration per
  new audited action. Conventions documented in the SiteService/MCP contracts.
- `source audit_source` enum — the three writers + system, exactly per spec; small and stable.
- `metadata JSONB` + GIN index — flexible per-action payload (`{"paths":[...], "branch":"draft",
  "base_sha":"...", "bytes":1234, "ip":"…"}`) without schema churn.
- `commit_sha` denormalized so the activity feed can deep-link to a git commit without re-deriving.

---

## 2. Entity relationship summary

```
users ──< user_identities           (1 user : N IdP identities)         CASCADE
users ──< sessions                  (1 user : N sessions)               CASCADE
users ──1 sites.owner_id            (owner)                             RESTRICT  (transfer first)
users ──< site_members >── sites    (M:N membership, role)              CASCADE / CASCADE
users ──< site_tokens >── sites     (token created_by human, per site)  CASCADE / CASCADE
sites ──< handle_redirects          (former handles -> current site)    CASCADE
users/sites/site_tokens ──< audit_log   (all SET NULL; log outlives refs)

reserved_handles : standalone admin-edited blocklist (no FKs)
```

**ON DELETE matrix (at a glance):**

| FK | Behavior | Why |
|---|---|---|
| `user_identities.user_id → users` | CASCADE | identities are meaningless without the user |
| `sessions.user_id → users` | CASCADE | kill sessions with the user |
| `sites.owner_id → users` | **RESTRICT** | never orphan a site/git repo; force ownership transfer |
| `site_members.{site,user}` | CASCADE | membership is a pure join |
| `site_members.created_by → users` | SET NULL | keep grant record |
| `site_tokens.site_id → sites` | CASCADE | tokens die with the site |
| `site_tokens.created_by → users` | CASCADE | no orphan credentials |
| `handle_redirects.site_id → sites` | CASCADE | free former handles on delete |
| `audit_log.* ` | **SET NULL** | audit must survive referenced-row deletion |

> **Note on deleting a site:** removing the `sites` row does **not** remove the on-disk git repo at
> `/data/sites/{id}/.git` — that is the SiteService's job (it owns all filesystem effects). The
> documented delete flow is: SiteService archives/removes the repo, *then* deletes the row in the
> same operation. The DB cascade only handles relational cleanup.

---

## 3. Handle validation rules + reserved-words constant

Validation is enforced in **three layers** (defense in depth):

1. **Go validator (primary)** — friendly errors, runs before any DB write.
2. **DB CHECK** on `sites.handle` / `handle_redirects.old_handle` — format guarantee.
3. **`reserved_handles` table + Go fallback constant** — admin-editable blocklist + safe default.

### 3.1 Rules

| Rule | Value | Enforced by |
|---|---|---|
| Allowed chars | `^[a-z0-9-]+$` (lowercase letters, digits, hyphen) | Go + DB CHECK |
| Must start/end alphanumeric (no leading/trailing hyphen) | regex `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$` | Go + DB CHECK |
| No consecutive hyphens `--` | reject `--` substring | **Go only** |
| Length | min **3**, max **63** (DNS label limit) | Go (CHECK caps at 63 via regex) |
| Case | input lowercased before store; uniqueness via citext | Go + citext |
| Reserved words | not in blocklist | Go (table + constant) |
| Uniqueness | not an existing `sites.handle`, `handle_redirects.old_handle`, or reserved | Go (tx) + UNIQUE |

> **Why `--` is Go-only:** `{handle}--{branch}` uses `--` as the branch separator in the data-plane
> Host parser. A handle containing `--` would be ambiguous against a preview subdomain. The DB CHECK
> regex *permits* internal hyphens (DNS allows them); we forbid the **double** hyphen specifically in
> app code, tied to the URL scheme. (Documented as a coupling between handle rules and the URL
> contract.)

### 3.2 Reserved words — single source of truth

Locked baseline (from spec), expressed as a **Go constant** that also seeds `reserved_handles`:

```go
// backend/internal/handle/reserved.go
package handle

// ReservedHandles is the locked baseline blocklist. It is (a) used as a fallback when the
// reserved_handles table is empty/unreachable, and (b) the seed source for that table.
// Admins may ADD to the DB table at runtime; they cannot remove these baseline entries via UI.
var ReservedHandles = map[string]struct{}{
    "draft":     {}, // branch/preview keyword
    "preview":   {},
    "published": {},
    "www":       {},
    "api":       {}, // control-plane path prefix
    "internal":  {},
    "host":      {}, // path-based fallback prefix /host/{handle}/
    "admin":     {},
    "app":       {},
    "static":    {},
    "assets":    {},
    "mcp":       {}, // MCP endpoint prefix
}
```

```go
// Validate returns nil if h is a structurally valid, non-reserved handle.
// reservedFromDB is the admin-editable set loaded (and cached) from reserved_handles;
// it is unioned with the baseline ReservedHandles constant.
func Validate(h string, reservedFromDB map[string]struct{}) error
```

Corresponding SQL constant block (the goose seed mirrors the Go constant — keep in sync; a
`go test` asserts they match, see §6):

```sql
-- seed (see 90001_seed_reserved_handles in §6)
INSERT INTO reserved_handles (handle, reason) VALUES
    ('draft','branch/preview keyword'),
    ('preview','branch/preview keyword'),
    ('published','branch/preview keyword'),
    ('www','infra'),
    ('api','control-plane path prefix'),
    ('internal','infra'),
    ('host','path-based fallback prefix'),
    ('admin','reserved'),
    ('app','reserved'),
    ('static','reserved'),
    ('assets','reserved'),
    ('mcp','MCP endpoint prefix')
ON CONFLICT (handle) DO NOTHING;
```

---

## 4. sqlc — representative queries (hot paths)

**sqlc config** (`backend/sqlc.yaml`): engine `postgresql`, driver `pgx/v5`, `emit_pointers_for_null_types: true`,
enums emitted as Go typed string constants, `citext → string`, `jsonb → []byte` (or a typed wrapper),
`inet → netip.Addr`. Queries live in `backend/internal/db/queries/*.sql`; generated code in
`backend/internal/db/gen`.

```sql
-- backend/internal/db/queries/sites.sql

-- name: ResolveHandle :one
-- DATA-PLANE HOT PATH. Resolve a live handle to the row the resolver needs to serve content.
-- Returns the cached published pointer + visibility so the data plane can answer without git
-- on the metadata side (it still reads the git tree for bytes).
SELECT id, handle, visibility, default_branch, published_commit_sha
FROM sites
WHERE handle = sqlc.arg(handle);

-- name: ResolveHandleRedirect :one
-- Resolver fallback: a former handle -> the current site's live handle (issue 301).
SELECT s.id, s.handle AS current_handle
FROM handle_redirects r
JOIN sites s ON s.id = r.site_id
WHERE r.old_handle = sqlc.arg(old_handle);

-- name: ListSitesForUser :many
-- DASHBOARD HOT PATH. Every site the user can see (owned OR member), newest activity first,
-- with the viewer's effective role. owner_id rows are surfaced even if no explicit member row,
-- but we maintain an owner member row so a single membership join suffices.
SELECT s.id, s.handle, s.visibility, s.default_branch, s.published_commit_sha,
       s.description, s.updated_at, m.role
FROM sites s
JOIN site_members m ON m.site_id = s.id AND m.user_id = sqlc.arg(user_id)
ORDER BY s.updated_at DESC
LIMIT  sqlc.arg(lim)
OFFSET sqlc.arg(off);

-- name: GetSiteForMember :one
-- Authz + detail load: site row + this user's role; absent row => no access (404/403).
SELECT s.*, m.role
FROM sites s
JOIN site_members m ON m.site_id = s.id AND m.user_id = sqlc.arg(user_id)
WHERE s.id = sqlc.arg(site_id);

-- name: CreateSite :one
-- Create a site (UUID generated by DB). Must run in the SAME tx as: inserting the owner
-- site_members row (CreateOwnerMembership below) and the SiteService git-init. Handle
-- pre-validated + collision-checked (sites/redirects/reserved) by the app before this call.
INSERT INTO sites (handle, owner_id, visibility, default_branch, description, github_repo)
VALUES (
    sqlc.arg(handle),
    sqlc.arg(owner_id),
    sqlc.narg(visibility),       -- nullable arg -> falls back to column default if NULL? no: pass explicit
    COALESCE(sqlc.narg(default_branch), 'draft'),
    COALESCE(sqlc.narg(description), ''),
    sqlc.narg(github_repo)
)
RETURNING *;

-- name: SetPublishedCommit :exec
-- Update the cache pointer at publish time (called by SiteService AFTER git ref move succeeds).
UPDATE sites
SET published_commit_sha = sqlc.arg(published_commit_sha),
    updated_at = now()
WHERE id = sqlc.arg(site_id);

-- name: RenameSiteHandle :exec
-- Step of the atomic rename (the redirect INSERT is a separate query in the same tx).
UPDATE sites SET handle = sqlc.arg(new_handle), updated_at = now()
WHERE id = sqlc.arg(site_id);

-- name: InsertHandleRedirect :exec
INSERT INTO handle_redirects (old_handle, site_id)
VALUES (sqlc.arg(old_handle), sqlc.arg(site_id))
ON CONFLICT (old_handle) DO UPDATE SET site_id = EXCLUDED.site_id;

-- name: HandleIsTaken :one
-- One-shot collision check across live handles, redirects, and reserved words.
SELECT EXISTS (
    SELECT 1 FROM sites            WHERE handle     = sqlc.arg(handle)
    UNION ALL
    SELECT 1 FROM handle_redirects WHERE old_handle = sqlc.arg(handle)
    UNION ALL
    SELECT 1 FROM reserved_handles WHERE handle     = sqlc.arg(handle)
) AS taken;
```

```sql
-- backend/internal/db/queries/members.sql

-- name: CreateOwnerMembership :exec
INSERT INTO site_members (site_id, user_id, role, created_by)
VALUES (sqlc.arg(site_id), sqlc.arg(user_id), 'owner', sqlc.arg(user_id))
ON CONFLICT (site_id, user_id) DO UPDATE SET role = 'owner';

-- name: UpsertMember :exec
INSERT INTO site_members (site_id, user_id, role, created_by)
VALUES (sqlc.arg(site_id), sqlc.arg(user_id), sqlc.arg(role), sqlc.arg(created_by))
ON CONFLICT (site_id, user_id) DO UPDATE SET role = EXCLUDED.role;

-- name: GetMemberRole :one
SELECT role FROM site_members
WHERE site_id = sqlc.arg(site_id) AND user_id = sqlc.arg(user_id);

-- name: RemoveMember :exec
DELETE FROM site_members
WHERE site_id = sqlc.arg(site_id) AND user_id = sqlc.arg(user_id);
```

```sql
-- backend/internal/db/queries/auth.sql

-- name: GetIdentity :one
-- LOGIN HOT PATH: (provider, subject) -> the user row.
SELECT u.*
FROM user_identities i
JOIN users u ON u.id = i.user_id
WHERE i.provider = sqlc.arg(provider) AND i.subject = sqlc.arg(subject)
  AND u.is_active = TRUE;

-- name: UpsertUserByEmail :one
-- Match-or-create at OIDC callback; run in tx with LinkIdentity.
INSERT INTO users (email, display_name, avatar_url)
VALUES (sqlc.arg(email), sqlc.arg(display_name), sqlc.narg(avatar_url))
ON CONFLICT (email) DO UPDATE
    SET display_name = EXCLUDED.display_name,
        avatar_url   = COALESCE(EXCLUDED.avatar_url, users.avatar_url),
        updated_at   = now()
RETURNING *;

-- name: LinkIdentity :exec
INSERT INTO user_identities (user_id, provider, subject, email_at_link, last_login_at)
VALUES (sqlc.arg(user_id), sqlc.arg(provider), sqlc.arg(subject), sqlc.narg(email_at_link), now())
ON CONFLICT (provider, subject) DO UPDATE
    SET last_login_at = now(), email_at_link = EXCLUDED.email_at_link;

-- name: CreateSession :one
INSERT INTO sessions (id, user_id, expires_at, user_agent, ip_addr)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(expires_at), sqlc.arg(user_agent), sqlc.narg(ip_addr))
RETURNING *;

-- name: GetSession :one
-- AUTH HOT PATH: validate cookie on every authenticated request. Joins the user so middleware
-- gets identity in one round-trip; rejects expired sessions and inactive users.
SELECT s.id, s.user_id, s.expires_at,
       u.email, u.display_name, u.avatar_url, u.is_admin
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = sqlc.arg(id)
  AND s.expires_at > now()
  AND u.is_active = TRUE;

-- name: TouchSession :exec
-- Throttled by caller (only if last_seen_at is stale enough).
UPDATE sessions SET last_seen_at = now() WHERE id = sqlc.arg(id);

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = sqlc.arg(id);

-- name: DeleteUserSessions :exec
-- "Log out everywhere" / admin kill-switch.
DELETE FROM sessions WHERE user_id = sqlc.arg(user_id);

-- name: DeleteExpiredSessions :execrows
-- Janitor (system).
DELETE FROM sessions WHERE expires_at < now();
```

```sql
-- backend/internal/db/queries/tokens.sql

-- name: ListTokenCandidatesByPrefix :many
-- AUTH-BY-TOKEN HOT PATH (MCP/API). Narrow by indexed prefix among active tokens; the caller
-- then constant-time compares token_hash to pick the exact row. Returns authz essentials.
SELECT t.id, t.site_id, t.created_by, t.token_hash, t.scope
FROM site_tokens t
JOIN users u ON u.id = t.created_by
WHERE t.token_prefix = sqlc.arg(token_prefix)
  AND t.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now())
  AND u.is_active = TRUE;

-- name: CreateToken :one
INSERT INTO site_tokens (site_id, created_by, name, token_prefix, token_hash, scope, expires_at)
VALUES (sqlc.arg(site_id), sqlc.arg(created_by), sqlc.arg(name),
        sqlc.arg(token_prefix), sqlc.arg(token_hash), sqlc.arg(scope), sqlc.narg(expires_at))
RETURNING id, site_id, name, token_prefix, scope, created_at, expires_at;

-- name: TouchToken :exec
-- Throttled last_used_at update; never on every call.
UPDATE site_tokens SET last_used_at = now() WHERE id = sqlc.arg(id);

-- name: RevokeToken :exec
UPDATE site_tokens SET revoked_at = now() WHERE id = sqlc.arg(id) AND site_id = sqlc.arg(site_id);

-- name: ListSiteTokens :many
-- Token management UI (never returns the hash).
SELECT id, name, token_prefix, scope, created_at, last_used_at, expires_at, revoked_at
FROM site_tokens
WHERE site_id = sqlc.arg(site_id)
ORDER BY created_at DESC;
```

```sql
-- backend/internal/db/queries/audit.sql

-- name: AppendAudit :exec
INSERT INTO audit_log (actor_id, site_id, action, source, token_id, commit_sha, metadata)
VALUES (sqlc.narg(actor_id), sqlc.narg(site_id), sqlc.arg(action), sqlc.arg(source),
        sqlc.narg(token_id), sqlc.narg(commit_sha), sqlc.arg(metadata));

-- name: ListSiteAudit :many
-- Per-site activity feed (most recent first), keyset by id for stable pagination.
SELECT id, actor_id, action, source, commit_sha, metadata, created_at
FROM audit_log
WHERE site_id = sqlc.arg(site_id)
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id))
ORDER BY id DESC
LIMIT sqlc.arg(lim);

-- name: ListReservedHandles :many
SELECT handle, reason FROM reserved_handles ORDER BY handle;
```

### 4.1 Optimistic concurrency (base-SHA) — where it lives

The spec's optimistic lock (write/save carries a **base commit SHA**, rejected on mismatch) is a
**git concern, enforced by the SiteService**, *not* a DB row version. There is no `version`/`xmin`
gymnastics here: the SiteService reads the current branch head, compares to the caller's `base_sha`,
and refuses (`409 Conflict`) on mismatch before committing. The DB only **records** the attempt/outcome
in `audit_log.metadata.base_sha`. This keeps git as the single source of truth for content state.

---

## 5. Migrations (goose)

- **Tool:** `pressly/goose` v3, SQL-file migrations (no Go migrations needed for schema).
- **Location:** `backend/internal/db/migrations/NNNNN_name.sql` with `-- +goose Up` / `-- +goose Down`
  sections. (Shown above as separate `.up.sql` for readability; the repo uses goose's single-file
  Up/Down convention.)
- **Numbering:** zero-padded sequential (`00001`…). Sequential (not timestamp) because this is a
  single-team OSS repo; sequential numbers read cleanly in PRs. Switch to timestamps only if
  parallel-branch migration collisions become real.
- **Statements needing care:** `CREATE TYPE ... AS ENUM` and `CREATE EXTENSION` are fine in a tx.
  Any future `ALTER TYPE ... ADD VALUE` must be flagged `-- +goose NO TRANSACTION` on PG versions
  that forbid it mid-tx.
- **Execution:** `goose up` runs automatically on backend container start (entrypoint), gated by an
  advisory lock so concurrent replicas don't race:
  `SELECT pg_advisory_lock(<const>)` around the migrate step (goose supports this via its lock).
- **Down migrations:** authored for every Up (drop in reverse dependency order), but production runs
  forward-only; downs exist for local dev/test teardown.

**Migration manifest (initial):**

| # | File | Contents |
|---|---|---|
| 00001 | `extensions_and_enums` | citext, pgcrypto; enums `site_role`, `site_visibility`, `audit_source`; `set_updated_at()` fn |
| 00002 | `users` | `users` |
| 00003 | `user_identities` | `user_identities` + indexes |
| 00004 | `sites` | `sites` + checks/indexes/trigger |
| 00005 | `reserved_handles` | `reserved_handles` |
| 00006 | `sessions` | `sessions` + indexes |
| 00007 | `site_members` | `site_members` + index |
| 00008 | `site_tokens` | `site_tokens` + indexes |
| 00009 | `handle_redirects` | `handle_redirects` + index |
| 00010 | `audit_log` | `audit_log` + indexes (incl. GIN) |
| 90001 | `seed_reserved_handles` | idempotent reserved-word seed (§3.2) — **schema seed, ships in prod** |

> Reserved-handle seed is `90001` (high number) and `ON CONFLICT DO NOTHING`, so it's a real
> migration that ships everywhere — distinct from *dev* seed data (§6) which never runs in prod.

---

## 6. Local-dev seed plan

Dev seed is **separate from migrations** and must never touch prod.

- **Mechanism:** a Go program `backend/cmd/seed/main.go` (run via `make seed` / a compose `seed`
  profile), idempotent, guarded by `if cfg.Env != "development" { log.Fatal("refusing to seed non-dev") }`.
  We use Go (not a raw `.sql`) because seeding a site also requires the **SiteService to git-init**
  `/data/sites/{uuid}/.git` and write starter files — DB rows alone would be inconsistent with §0.
- **What it creates:**
  1. **Admin user** `admin@example.test` (`is_admin=true`) + a `dev`-provider `user_identities`
     row (so no-auth/admin-password mode can log in immediately).
  2. **Two regular users** `alice@example.test`, `bob@example.test`.
  3. **Reserved handles** — already seeded by migration `90001`; the seed asserts presence.
  4. **Sample sites** via the real `SiteService.CreateSite` path (so git repos exist on disk):
     - `expense-calc` (owner alice, visibility internal) — has `draft` + `published`, a sample
       `index.html`, one `feature-tweak` branch, `published_commit_sha` set.
     - `team-links` (owner bob, visibility private) — alice added as `editor`.
     - `status-board` (owner admin, visibility public) — published.
  5. **One site token** per sample site (plaintext printed once to stdout for MCP testing).
  6. **A renamed site**: create `old-name`, rename to `new-name`, producing a `handle_redirects`
     row to exercise the redirect resolver.
  7. **Audit rows** generated naturally by the above operations (no hand-inserted audit).
- **Reset:** `make db-reset` = `goose reset` (drop) → `goose up` → `make seed`, plus wiping
  `/data/sites/*` so git and DB start consistent.
- **Consistency test:** a `go test` asserts the Go `ReservedHandles` constant equals the rows from
  `ListReservedHandles` after `90001` (guards the §3.2 dual-maintenance hazard).

---

## 7. Authoritative-store summary (restating the contract)

| Question the system asks | Answered from | Mechanism |
|---|---|---|
| "What bytes for `foo.hosting.example.com`?" | **git** | resolve handle (DB) → site uuid → git tree of `published` |
| "Which branches exist for site X?" | **git** | `git for-each-ref` (no DB table) |
| "Is `foo` taken?" | **DB** | `HandleIsTaken` across sites/redirects/reserved |
| "Can user U write site X?" | **DB** | `site_members.role` / `site_tokens.scope` |
| "Is this save safe (no clobber)?" | **git** | SiteService compares caller `base_sha` to branch head |
| "What did the AI change yesterday?" | **DB** | `audit_log` (+ git commit it points to) |
| "Where is published right now?" | **git** (DB caches) | git ref is truth; `published_commit_sha` is a read-through cache |

---

## 8. Open questions / gaps (考慮漏れ)

1. **`site_branch_cache` for cross-site branch queries.** We deliberately have no `branches` table.
   If product later needs "list all open `feature-*` across sites" or per-branch preview metadata
   (title, last build time), do we add an *explicitly-cache* table refreshed by the SiteService, or
   keep paying the git cost? Decision deferred until a real use case appears.
2. **Quotas.** Spec mentions admin quotas (per-user site count, per-site repo size). Not modeled
   here. Likely a `quotas` table or columns on `users` (`max_sites`, `max_repo_bytes`) + a
   `sites.repo_bytes` cache (updated by SiteService after git gc). Needs its own contract.
3. **`sites.repo_bytes` / file-count cache.** Dashboard "size" badge and zip-bomb post-checks would
   benefit from a cached size, but it's a git-derived cache (stale-pointer risk) — include only if
   the dashboard actually needs it without a git call.
4. **Per-branch visibility / preview auth.** `visibility` is per-site. Do preview URLs of `private`
   sites need their own access rule (e.g. a `feature-*` preview shared via unguessable token)? Not
   modeled; may need a `branch_preview_tokens` table or extend `site_tokens.scope`.
5. **GitHub linkage detail.** `sites.github_repo` is just `owner/name`. The mirror remote URL,
   installation id, webhook secret, and "last pulled SHA" are not modeled — push/webhook is a later
   feature. Likely a `site_github` 1:1 table when that lands (keep secrets out of `sites`).
6. **Soft-delete vs hard-delete for sites.** We currently hard-delete (`sites` row + git repo). A
   `deleted_at` tombstone (with a grace period before disk reclaim) would be safer for non-engineers
   who delete by accident. Trade-off: handle stays reserved during grace. Undecided.
7. **`audit_log` retention / partitioning.** Append-only log grows unbounded. No retention policy or
   monthly partitioning (`PARTITION BY RANGE (created_at)`) defined yet; fine at small scale, revisit
   if a busy instance accumulates millions of rows.
8. **Email uniqueness vs multi-IdP edge case.** We match identities to users by email
   (`UpsertUserByEmail` + `LinkIdentity`). If two *different* IdPs return the *same* email for what
   are truly different humans (rare in single-org use), they'd merge into one `users` row. Acceptable
   for internal/company use (the spec's scope) but documented as a known assumption.
9. **`token_prefix` collision under load.** Prefix is non-unique by design (we hash-compare). Length
   (~8 chars from a base32/62 space) makes collisions negligible, but the candidate-list query could
   theoretically return >1 row; the constant-time compare handles it. Confirm prefix length in the
   token-format contract.
10. **citext + index collation.** citext relies on the DB's default collation for case folding;
    non-ASCII handles are already excluded by the format CHECK (ASCII-only), so this is safe for
    handles. Flagged only so we don't later widen handle charset without revisiting.
