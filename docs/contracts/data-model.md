# kotoji — Data Model Contract (PostgreSQL)

> **Scope:** the authoritative relational schema for kotoji's **metadata** store.
> Raw SQL DDL (for [goose](https://github.com/pressly/goose) migrations) + [sqlc](https://sqlc.dev) query
> definitions (compiled against [pgx v5](https://github.com/jackc/pgx)). **No ORM. No Drizzle.**
>
> Status: **shipped & live.** This doc is detail/rationale; on any conflict
> [`CANONICAL.md`](./CANONICAL.md) §4 (the frozen DDL) **wins**. Companion contracts:
> [`openapi.yaml`](./openapi.yaml) (REST), [`mcp.md`](./mcp.md) (MCP tools).
>
> The real schema ships as four goose migrations under
> `backend/internal/db/migrations/` — `0001_init.sql`, `0002_seed_reserved.sql`,
> `0003_instance_settings.sql`, `0004_user_tokens.sql` — applied automatically at boot
> (see §5). The per-table split below is editorial; the canonical artifact is those files.

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
| Who may touch a site (authz) | **DB** | `site_members` (per-site role) + `user_tokens` (per-user, membership-capped) |
| Identity / sessions | **DB** | OIDC subject → user, opaque session cookie |
| Rename redirects | **DB** | `handle_redirects` |
| Audit trail | **DB** | append-only `audit_log` |
| Instance config & secrets | **DB** | `instance_settings` (admin password hash, GitHub mirror config incl. encrypted PAT) |
| `published_commit_sha`, `published_at`, `default_branch` | **DB (cache pointer only)** | see §1.3 |

**Cache-pointer doctrine:** the DB stores *pointers into git* (`published_commit_sha`,
`default_branch`) purely so the **data plane can answer a request without shelling out to git on the
hot path**, and so the dashboard can show a "last published" badge cheaply. These are a **read-through
cache**: git is still authoritative. If the cache and git disagree, **git wins** and the row is
corrected by the SiteService on its next write. We deliberately do **not** mirror full branch/commit
state into Postgres — that is the classic dual-source-of-truth bug.

**One nuance on `instance_settings` (added since v1):** that table is the **authoritative** home for a
small set of *instance configuration* — the admin password hash (first-run setup) and the GitHub mirror
config (feature flag, org, webhook secret, and the **encrypted** push token). That is config, not
content: it has no git counterpart, so there is no dual-source concern. Per-row `sites.github_repo`
(the `owner/name` to mirror to) still lives on the site; the instance-wide *credentials/flags* live in
`instance_settings` and override the `KOTOJI_GITHUB_*` env at the effective-config layer (§1.9).

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

The DDL ships as goose migrations (see §5). The whole schema is created by `0001_init.sql` (one
file — the per-table split below is for review); `0002_seed_reserved.sql` seeds the reserved-word
blocklist; `0003_instance_settings.sql` adds the key/value `instance_settings` store; and
`0004_user_tokens.sql` **drops the per-project `site_tokens`** and replaces it with the per-USER
`user_tokens` table (re-pointing the audit FK). Target: **PostgreSQL 18 (17 compatible)**.

### 1.0 Extensions, enums, and shared trigger fn

```sql
-- 0001_init.sql

-- citext: case-insensitive handles & emails. Handles are DNS labels (always lowercased on
-- write), but citext gives us a defense-in-depth UNIQUE that is case-insensitive even if a
-- code path forgets to lowercase. Emails are naturally case-insensitive.
CREATE EXTENSION IF NOT EXISTS citext;

-- pgcrypto: gen_random_uuid() for UUID v4 PKs.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Role of a user *on a specific site*. owner > editor > viewer.
--   owner  : full control incl. delete, rename, member mgmt, publish, token issuance.
--   editor : save/commit, create/delete branches, publish (under publish_mode='direct'),
--            upload, rollback, Monaco/MCP write.
--   viewer : read files, read history, read previews (incl. drafts). No writes.
CREATE TYPE site_role AS ENUM ('owner', 'editor', 'viewer');

-- Visibility of a site's *served content* (data plane). Authz on the control plane is always
-- enforced regardless; this governs anonymous access to the public URL.
--   public   : anyone with the URL can view served content (subject to instance cap).
--   members  : served only to authenticated users of this kotoji instance.
--   private  : served only to site members (owner/editor/viewer).
CREATE TYPE site_visibility AS ENUM ('public', 'members', 'private');

-- Which of the writers produced an audited action.
CREATE TYPE audit_source AS ENUM ('upload', 'editor', 'mcp', 'system');
--   upload : zip upload path
--   editor : Monaco / dashboard
--   mcp    : MCP tool call from an external AI client
--   system : webhook pulls, scheduled jobs, admin actions, migrations

-- Shared updated_at trigger fn, attached to users, sites, and instance_settings.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
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
CREATE TABLE users (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email            CITEXT      NOT NULL UNIQUE,         -- canonical contact; case-insensitive
    display_name     TEXT        NOT NULL DEFAULT '',     -- from OIDC `name` claim; may be empty
    avatar_url       TEXT,                                -- from OIDC `picture` claim; nullable
    is_admin         BOOLEAN     NOT NULL DEFAULT FALSE,  -- instance superuser (admin screen, GitHub config, quotas)
    can_create_sites BOOLEAN     NOT NULL DEFAULT TRUE,   -- may this user create sites? (decision #2/#8)
    is_active        BOOLEAN     NOT NULL DEFAULT TRUE,   -- soft disable; deactivated users can't log in
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_users_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Lookups by email happen at OIDC callback time (match-or-create) and in admin search.
-- UNIQUE on email already creates an index; no extra index needed.
```

**Rationale.**
- `email CITEXT UNIQUE` — email is the join key between the abstract IdP and a kotoji user; many
  IdPs return mixed-case. citext makes `Foo@x.com` == `foo@x.com`.
- `is_admin` is a coarse instance-level flag (distinct from per-site `site_role`). It governs the
  admin screen, **GitHub-mirror config in `instance_settings`**, quotas, and reserved-word edits.
  The single-admin **password/setup** path (§1.8, auth contract) **promotes** that user to
  `is_admin = true`.
- `can_create_sites` is the per-user gate on site creation (decision #2/#8). It is the **upper
  bound** for an MCP token's own `can_create_sites` (§1.6 / §6).
- `is_active` enables soft-disable without cascading deletes of authored content/audit history.
- No password column: passwords belong to the IdP. The single-admin password (mode=`password`) is
  an **instance** secret — a bcrypt hash in `instance_settings` (§1.8), not a per-user credential.

### 1.2 `user_identities`

One row per (provider, subject) pair. This is the OIDC abstraction's DB footprint.

```sql
CREATE TABLE user_identities (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider      TEXT        NOT NULL,   -- AuthProvider key, e.g. 'google', 'keycloak', 'dev'
    subject       TEXT        NOT NULL,   -- OIDC `sub` claim: stable, opaque, provider-scoped
    email_at_link CITEXT,                 -- email seen at link time (audit; may drift from users.email)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,

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
CREATE TABLE sites (
    id                   UUID            PRIMARY KEY DEFAULT gen_random_uuid(),  -- == /data/sites/{id}
    handle               CITEXT          NOT NULL UNIQUE,   -- DNS-safe public name & GitHub repo name
    owner_id             UUID            NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    visibility           site_visibility NOT NULL DEFAULT 'private',
    default_branch       TEXT            NOT NULL DEFAULT 'draft',  -- working branch name (a git ref name)
    published_commit_sha TEXT,                              -- CACHE pointer into git; NULL = never published
    published_at         TIMESTAMPTZ,                       -- last publish time (dashboard "published @" badge)
    publish_mode         TEXT            NOT NULL DEFAULT 'direct'   -- decision #6
                         CHECK (publish_mode IN ('direct', 'request')),
    github_repo          TEXT,                              -- 'owner/name' for mirror push; NULL = no mirror
    web_root             TEXT            NOT NULL DEFAULT '', -- served subdir; '' = repo root (v1)
    description          TEXT            NOT NULL DEFAULT '',
    deleted_at           TIMESTAMPTZ,                       -- soft delete (decision #3); 30-day grace
    created_at           TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ     NOT NULL DEFAULT now(),

    -- Handle validation enforced at the DB edge as defense-in-depth. The application
    -- (Go validator, see §3) is the primary gate with friendly errors; this CHECK guarantees
    -- no malformed handle can ever land via any path (incl. manual SQL, future code).
    CONSTRAINT sites_handle_format CHECK (
        handle ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'  -- DNS label: lower alnum + hyphen, no edge hyphen
    ),
    -- A published_commit_sha, if present, must look like a git object id (40 hex = SHA-1,
    -- 64 hex = SHA-256 to be forward-compatible with git's SHA-256 mode).
    CONSTRAINT sites_published_sha_format CHECK (
        published_commit_sha IS NULL OR published_commit_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
    )
);

-- One site may mirror to a given GitHub repo at most once (partial unique on non-null).
CREATE UNIQUE INDEX idx_sites_github_repo ON sites (github_repo) WHERE github_repo IS NOT NULL;
-- List-by-owner (dashboard) and admin "all sites by owner":
CREATE INDEX idx_sites_owner_id   ON sites (owner_id);
-- Recently-updated ordering on the dashboard:
CREATE INDEX idx_sites_updated_at ON sites (updated_at DESC);
-- Handle resolution ignores soft-deleted sites on the hot path:
CREATE INDEX idx_sites_handle_live ON sites (handle) WHERE deleted_at IS NULL;

CREATE TRIGGER trg_sites_updated_at BEFORE UPDATE ON sites
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
```

**Rationale.**
- `handle CITEXT UNIQUE` — case-insensitive uniqueness; subdomains/repos are case-folded anyway.
  The reserved-word block is enforced in app code (§3) because it needs friendly errors and the
  list lives in a Go constant; we *additionally* keep a `reserved_handles` table (§1.7) so admins
  can extend it.
- `owner_id ... ON DELETE RESTRICT` — you cannot delete a user who still owns sites; the admin must
  transfer ownership first. Protects against accidental orphaning of served content + git repos on
  disk. (Contrast with `is_active=false` for everyday "disable a person".)
- `default_branch TEXT` — the working branch *name*, default `draft`. A git ref name, not a FK to a
  branches table (none exists).
- `published_commit_sha` + `published_at` — **the only commit SHA in the DB**, explicitly a cache
  (see §0). Lets the data plane resolve `{handle}` → published tree, and lets the dashboard show
  "published @ abc123" with a timestamp, without a git call. Updated transactionally by the
  SiteService at publish time; reconciled to git if ever stale.
- `publish_mode` (decision #6) — `direct` (default; small-team: editors publish directly) or
  `request` (non-owner publishes route through a GitHub PR).
- `github_repo` nullable — mirror push is optional. Stored as `owner/name`; the actual remote
  credentials live in `instance_settings` (§1.9), **not** per-row.
- `web_root` — served subdir (`''` = repo root in v1).
- `deleted_at` (decision #3) — **soft delete**: the handle stays reserved during a 30-day grace; a
  reaper `git bundle`s the repo to `/data/backups/{uuid}` then reclaims disk.
- We store **no `draft_commit_sha`**: the draft head is read from git on demand in the editor.

### 1.4 `sessions`

Server-side sessions. The cookie holds only an **opaque, high-entropy id**; everything else lives
here.

```sql
CREATE TABLE sessions (
    id           TEXT        PRIMARY KEY,            -- opaque CSPRNG id (the __Host- cookie value)
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
  `__Host-`-prefixed, `HttpOnly; Secure; SameSite=Lax` (see Auth contract), value = `sessions.id`.
- `id TEXT PRIMARY KEY` generated by the app (CSPRNG) — **not** a guessable UUID. We keep it `TEXT`
  so the encoding is the app's choice.
- `expires_at` absolute + `last_seen_at` for optional idle timeout and an "active sessions" screen.
- `ON DELETE CASCADE` from users so deactivating/deleting a user nukes their sessions.
- Expired-session GC: a periodic `DELETE FROM sessions WHERE expires_at < now()` (the `system`
  janitor); index on `expires_at` keeps it cheap.

### 1.5 `site_members`

Per-site authorization. Composite PK `(site_id, user_id)` — a user has at most one role per site.

```sql
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
  lookup key. It is also the **per-request cap** for a per-user MCP token (§1.6 / §6.2).
- The **site owner** is denormalized on `sites.owner_id` for cheap "who owns this" and RESTRICT
  semantics; we **also** ensure an `owner` row exists in `site_members` (created in the same tx as
  the site) so a single authz query (`role >= editor?`) covers owner + members uniformly.
- `ON DELETE CASCADE` on both FKs: removing a site or a user cleans up memberships. (Removing a
  membership also instantly narrows any of that user's tokens on that site — §6.2.)
- `created_by ... ON DELETE SET NULL`: keep the grant record even if the granting admin is later
  deleted.

### 1.6 `user_tokens` (per-USER MCP/API tokens)

> **Migration note.** v1 had a per-project `site_tokens` table (one token bound to one `site_id`,
> no MCP site selector). Migration `0004_user_tokens.sql` **DROPPED `site_tokens`** and replaced it
> with `user_tokens` below. Existing per-project tokens were intentionally invalidated; users
> re-issue under `/api/tokens` (the per-site `/api/sites/{handle}/tokens` route was removed). The
> `audit_log.token_id` FK was re-pointed at this table (§1.8 / §4.2).

A token is **owned by a user** and automatically covers **every** project that user is a member of.
We store only a **hash** of the secret; the plaintext is shown once at creation and never again.

```sql
CREATE TABLE user_tokens (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,  -- the human it acts as
    name             TEXT        NOT NULL DEFAULT '',   -- human label, e.g. "claude-laptop"
    token_prefix     TEXT        NOT NULL,              -- first 12 chars of plaintext (UI display + fast lookup)
    token_hash       BYTEA       NOT NULL,              -- sha256(plaintext), 32 bytes; constant-time compared
    scopes           TEXT[]      NOT NULL DEFAULT '{read,write,publish}',  -- subset of {read,write,publish}
    can_create_sites BOOLEAN     NOT NULL DEFAULT FALSE,  -- gates create_site over MCP; capped by users.can_create_sites
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at     TIMESTAMPTZ,                        -- updated (throttled) on use
    expires_at       TIMESTAMPTZ,                        -- optional hard expiry; NULL = no expiry
    revoked_at       TIMESTAMPTZ,                        -- soft revoke; NULL = active
    CONSTRAINT user_tokens_hash_len    CHECK (octet_length(token_hash) = 32),     -- SHA-256 = 32 bytes
    CONSTRAINT user_tokens_prefix_len  CHECK (char_length(token_prefix) = 12),    -- fixed 12-char prefix
    CONSTRAINT user_tokens_scopes_valid CHECK (scopes <@ ARRAY['read','write','publish']::text[])
);

-- Defense in depth: the same hash should never exist twice.
CREATE UNIQUE INDEX uq_user_tokens_hash  ON user_tokens (token_hash);
-- Auth-by-token hot path: presented token -> prefix -> candidate rows -> constant-time hash compare.
CREATE INDEX idx_user_tokens_prefix      ON user_tokens (token_prefix) WHERE revoked_at IS NULL;
-- Token management UI: list a user's own tokens.
CREATE INDEX idx_user_tokens_user_id     ON user_tokens (user_id);
```

**Rationale.**
- **Per-USER, not per-project (the load-bearing change).** A token is no longer bound to one site.
  It carries ONE `scopes` set and `can_create_sites`, and automatically covers ALL of the owning
  user's memberships. The **effective** capability on a given site is computed **per request** as
  `intersection(token.scopes, role_scopes(membership role))` (§6.2). Removing the user's membership
  (or downgrading the role) **instantly** limits the token — no re-issue — and a token can **never
  exceed its user's own access**. A non-member's token cannot reach the site at all (→ `not_found`,
  no existence leak).
- **Hash only, never plaintext.** Token format (app-side): `kotoji_pat_<base62>`, ≥160 bits CSPRNG.
  We persist a fixed **12-char** `token_prefix` (display "kotoji_pat_…" + cheap index lookup) and
  `token_hash = SHA-256(plaintext)`. Lookup: find candidates by prefix among non-revoked, then
  `subtle` constant-time compare the full hash. SHA-256 (not bcrypt) is correct here because the
  secret is **high-entropy random**, not a human password — fast verification, no rainbow-table risk.
- `scopes TEXT[]` with the chain `read ⊂ write ⊂ publish` and a CHECK that the array is a subset of
  the three valid values. This is the token's *max* authority; the membership role caps it further.
- `can_create_sites` (default FALSE) gates `create_site` over MCP. It is additionally **capped by
  `users.can_create_sites`** at call time (decision #2/#8). The new site is owned by the user, so
  the same token immediately covers it.
- `last_used_at` updated **throttled** (e.g. at most once/minute) to avoid a write on every MCP call.
- `expires_at` + `revoked_at` give both proactive expiry and instant revocation. The auth query
  filters `revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())` AND `users.is_active`.
- `user_id ON DELETE CASCADE` — a token acts *as* a human; if that human is deleted, their tokens
  die with them (no orphan credentials).

### 1.7 `handle_redirects`

When a site is renamed `old → new`, old subdomains/URLs must 301 to the new handle. This table maps
**former** handles to the current site.

```sql
CREATE TABLE handle_redirects (
    old_handle CITEXT      PRIMARY KEY,                                  -- the freed-up former handle
    site_id    UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A former handle must itself be a valid DNS label (it was a real handle once).
    CONSTRAINT handle_redirects_format CHECK (
        old_handle ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
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
  `reserved_handles`). The resolver checks live `sites` first, then `handle_redirects`.
- **Rename flow (atomic, in SiteService):** `INSERT handle_redirects(old_handle=OLD, site_id)` +
  `UPDATE sites SET handle=NEW`. If the *new* handle is itself an old redirect of the *same* site
  (rename back), delete that stale redirect row. Files do NOT move (path is UUID-keyed).
- **Resolver vs control API asymmetry:** the data plane follows redirects and emits `301`; the
  control API (`GetSiteByHandle`) 404s old handles.
- `ON DELETE CASCADE` — deleting the site frees all its former handles.
- Redirects are permanent (cheap rows); an admin can prune. We do **not** auto-expire to avoid
  breaking external links.

### 1.8 `instance_settings` (admin password hash + GitHub mirror config)

> **Added by migration `0003_instance_settings.sql`.**

A small **key/value** store for instance-wide configuration that must persist in the database rather
than the environment. One row per setting key.

```sql
CREATE TABLE instance_settings (
    key        TEXT        PRIMARY KEY,                 -- e.g. 'admin_password_hash', 'github_token'
    value      TEXT        NOT NULL,                    -- opaque string (a bcrypt hash, a flag, ciphertext, ...)
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_instance_settings_updated_at BEFORE UPDATE ON instance_settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
```

**Defined keys** (the Go store layer owns these as exported constants):

| Key | Value encoding | Purpose |
|---|---|---|
| `admin_password_hash` | **bcrypt** hash | First-run single-admin password (mode=`password`). |
| `github_mirror_enabled` | `"true"` / `"false"` | Feature flag for the GitHub mirror. Absent ⇒ fall back to env. |
| `github_org` | plaintext | Org/owner for created/mirror repos. |
| `github_webhook_secret` | plaintext | HMAC secret for `/api/webhooks/github` (never echoed over the API). |
| `github_token` | **AES-256-GCM ciphertext** | The push PAT/app token, encrypted at rest (§1.9). |

**Rationale.**
- **First-run admin setup (changed since v1).** `AUTH_MODE=password` no longer *requires*
  `KOTOJI_AUTH_ADMIN_PASSWORD` in the env. If the env password is empty, the first run serves an
  `/auth/setup` screen; the password the admin chooses is bcrypt-hashed and stored under
  `admin_password_hash`. The `PasswordProvider` verifies the **DB hash first**, then the env value.
  The single-admin password/setup user is **promoted to `is_admin = true`**.
- **GitHub mirror config via GUI.** An admin configures the mirror at runtime from the Instance
  Settings page (`/settings`). The DB values **override** the `KOTOJI_GITHUB_*` env at the
  effective-config layer (§1.9). The webhook secret and the (encrypted) token never leave the
  server in API responses.
- **Why key/value, not columns.** The set is tiny and grows slowly; a generic K/V table avoids a
  migration per new instance flag and keeps the secret-handling logic in one place. The token is the
  only encrypted value; everything else is a flag/string.

### 1.9 GitHub-token encryption at rest (`internal/secretbox`)

The `github_token` value is **never** stored in plaintext. It is sealed with **AES-256-GCM** by
`internal/secretbox` before it lands in `instance_settings.value`, and opened on read.

- **Key resolution (LOCKED order):** an explicit `KOTOJI_SECRET_KEY` (hex or base64, ≥32 bytes; the
  first 32 bytes are used) wins; otherwise the 32-byte key is **derived** deterministically via
  SHA-256 over a stable server seed (admin-password-hash | OIDC secret | control base URL | base
  domain). The derived path means an env-only deployment with no `KOTOJI_SECRET_KEY` still gets a
  stable key across restarts, so tokens sealed before a restart remain decryptable.
- **Blob layout (pre-base64):** `versionByte(1) || nonce(12) || ciphertext+tag`; a fresh CSPRNG
  nonce per seal (never reused). The stored value is the standard-base64 of that blob.
- **Graceful failure (LOCKED policy):** `Open` never panics or errors. On any failure — wrong/rotated
  key, truncated/tampered ciphertext, bad base64 — it returns "not configured". A rotated
  `KOTOJI_SECRET_KEY` therefore degrades to "admin re-enters the token", never a crash.
- **No-box guard:** if no secret box is wired, the store **refuses** to persist a token (so a
  misconfigured instance never writes a plaintext credential), and reads report the token as unset.

### 1.10 `reserved_handles` (admin-extendable reserved-word block)

```sql
CREATE TABLE reserved_handles (
    handle     CITEXT      PRIMARY KEY,
    reason     TEXT        NOT NULL DEFAULT '',   -- why reserved (shown to admin)
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Seeded by migration `0002_seed_reserved.sql` (§3.2) with the locked baseline. The Go validator loads
this set (cached, refreshed on admin edit) and unions it with a hardcoded baseline constant (§3) so a
fresh DB is safe before the seed runs. Admins may **add** entries at runtime; they cannot remove the
baseline via UI.

### 1.11 `audit_log`

Append-only trail of who did what, from which writer. Powers the "who changed this" view and is a
security primitive (AI write access + multi-user).

```sql
CREATE TABLE audit_log (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,  -- monotonic; cheap append
    actor_user_id UUID         REFERENCES users(id)       ON DELETE SET NULL,  -- human actor; NULL for pure system
    site_id       UUID         REFERENCES sites(id)       ON DELETE SET NULL,  -- target site; NULL for instance-level
    token_id      UUID         REFERENCES user_tokens(id) ON DELETE SET NULL,  -- if via a token (re-pointed in 0004)
    action        TEXT         NOT NULL,            -- verb, e.g. 'site.create','file.write','publish','rollback'
    source        audit_source NOT NULL,            -- which writer/origin: upload/editor/mcp/system
    commit_sha    TEXT,                             -- resulting git commit, if the action made one
    metadata      JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- action-specific detail (paths, branch, base_sha, handle, ip…)
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Per-site activity feed (most-recent-first) is the dominant read.
CREATE INDEX idx_audit_log_site_created  ON audit_log (site_id, created_at DESC);
-- Admin "what did this user do":
CREATE INDEX idx_audit_log_actor_created ON audit_log (actor_user_id, created_at DESC);
-- Ad-hoc structured queries on metadata (e.g. metadata->>'branch'):
CREATE INDEX idx_audit_log_metadata      ON audit_log USING gin (metadata);
```

**Rationale.**
- `BIGINT IDENTITY` PK — append-only, monotonic, smaller/faster than UUID for a high-write log; no
  natural need to reference these rows by id elsewhere.
- **All FKs `ON DELETE SET NULL`.** The audit trail must **survive** deletion of the user, site, or
  token it references — that's the whole point of an audit log. We keep `commit_sha` and `metadata`
  so the record remains meaningful even after the referenced rows vanish. (`site_id` SET NULL means
  we denormalize the handle into `metadata.handle` at write time so post-delete records are still
  readable.)
- **`token_id → user_tokens` (re-pointed in `0004`).** It originally referenced `site_tokens`; when
  that table was dropped, `0004` cleared dangling `token_id`s, then re-added the FK against
  `user_tokens(id) ON DELETE SET NULL` so future MCP/API audit rows keep their FK guarantee.
- `action TEXT` (not enum) — verbs grow freely; a constrained enum here would force a migration per
  new audited action. Conventions documented in the SiteService/MCP contracts.
- `source audit_source` enum — the writers + system, small and stable. (`via` mapping: `ui`/`monaco`
  → `editor`; `webhook`/`github`/`admin` → `system`, finer distinction in `metadata.kind`.)
- `metadata JSONB` + GIN index — flexible per-action payload (`{"paths":[...], "branch":"draft",
  "base_sha":"...", "handle":"…", "ip":"…"}`) without schema churn.
- `commit_sha` denormalized so the activity feed can deep-link to a git commit without re-deriving.

---

## 2. Entity relationship summary

```
users ──< user_identities           (1 user : N IdP identities)            CASCADE
users ──< sessions                  (1 user : N sessions)                  CASCADE
users ──1 sites.owner_id            (owner)                                RESTRICT  (transfer first)
users ──< site_members >── sites    (M:N membership, role)                 CASCADE / CASCADE
users ──< user_tokens               (1 user : N per-user tokens)           CASCADE
sites ──< handle_redirects          (former handles -> current site)       CASCADE
users/sites/user_tokens ──< audit_log   (all SET NULL; log outlives refs)

reserved_handles  : standalone admin-extendable blocklist (no FKs)
instance_settings : standalone key/value (admin password hash + GitHub config; no FKs)
```

> **Authz is two tables now:** `site_members` (per-site role) AND `user_tokens` (per-user secret).
> A token never carries a `site_id`; it is bound only to its `user_id`, and its reach on any site is
> capped by the owner's `site_members` row for that site, re-evaluated per request (§6.2).

**ON DELETE matrix (at a glance):**

| FK | Behavior | Why |
|---|---|---|
| `user_identities.user_id → users` | CASCADE | identities are meaningless without the user |
| `sessions.user_id → users` | CASCADE | kill sessions with the user |
| `sites.owner_id → users` | **RESTRICT** | never orphan a site/git repo; force ownership transfer |
| `site_members.site_id → sites` | CASCADE | membership is a pure join |
| `site_members.user_id → users` | CASCADE | membership is a pure join |
| `site_members.created_by → users` | SET NULL | keep grant record |
| `user_tokens.user_id → users` | CASCADE | no orphan credentials (a token dies with its owner) |
| `handle_redirects.site_id → sites` | CASCADE | free former handles on delete |
| `audit_log.actor_user_id → users` | **SET NULL** | audit must survive referenced-row deletion |
| `audit_log.site_id → sites` | **SET NULL** | audit must survive (denormalize handle into metadata) |
| `audit_log.token_id → user_tokens` | **SET NULL** | audit must survive (re-pointed in `0004`) |

> **Note on deleting a site:** removing the `sites` row does **not** remove the on-disk git repo at
> `/data/sites/{id}/.git`. Deletion is **soft** (decision #3): `DeleteSite` sets `sites.deleted_at`,
> the handle stays reserved for a 30-day grace, and a reaper `git bundle`s the repo to
> `/data/backups/{uuid}` then `rm -rf`s it. The DB cascade only handles relational cleanup.

---

## 3. Handle validation rules + reserved-words constant

Validation is enforced in **three layers** (defense in depth):

1. **Go validator (primary)** — friendly errors, runs before any DB write.
2. **DB CHECK** on `sites.handle` / `handle_redirects.old_handle` — format guarantee.
3. **`reserved_handles` table + Go fallback constant** — admin-extendable blocklist + safe default.

### 3.1 Rules

| Rule | Value | Enforced by |
|---|---|---|
| Allowed chars | `^[a-z0-9-]+$` (lowercase letters, digits, hyphen), ASCII only | Go + DB CHECK |
| Must start/end alphanumeric (no leading/trailing hyphen) | regex `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` | Go + DB CHECK |
| No consecutive hyphens `--` | reject `--` substring | **Go only** |
| Length (create) | min **3**, max **63** (DNS label limit) | Go (CHECK caps at 63 via regex) |
| Length (resolver) | accepts **1–63** so already-created short handles still resolve | Go |
| Case | input lowercased before store; uniqueness via citext | Go + citext |
| Reserved words | not in blocklist | Go (table ∪ constant) |
| Uniqueness | not an existing `sites.handle`, `handle_redirects.old_handle`, or reserved | Go (tx) + UNIQUE |

> **Why `--` is Go-only:** `{handle}--{branch}` uses `--` as the branch separator in the data-plane
> Host parser. A handle containing `--` would be ambiguous against a preview subdomain. The DB CHECK
> regex *permits* internal hyphens (DNS allows them); we forbid the **double** hyphen specifically in
> app code, tied to the URL scheme.

### 3.2 Reserved words — single source of truth

Locked baseline, expressed as a **Go constant** that also seeds `reserved_handles`:

```go
// backend/internal/handle/reserved.go
package handle

// ReservedHandles is the locked baseline blocklist: (a) the fallback when the reserved_handles
// table is empty/unreachable, and (b) the seed source for that table (0002_seed_reserved.sql).
// Admins may ADD to the DB table at runtime; they cannot remove these baseline entries via UI.
var ReservedHandles = []string{
    "draft", "preview", "published", "www", "api", "internal",
    "host", "admin", "app", "static", "assets", "mcp",
}
```

Corresponding seed (goose `0002_seed_reserved.sql`; a `go test` asserts it matches the Go constant):

```sql
-- +goose Up
INSERT INTO reserved_handles (handle, reason) VALUES
    ('draft',     'branch/preview keyword'),
    ('preview',   'branch/preview keyword'),
    ('published', 'branch/preview keyword'),
    ('www',       'infra'),
    ('api',       'control-plane path prefix'),
    ('internal',  'infra'),
    ('host',      'path-based fallback prefix'),
    ('admin',     'reserved'),
    ('app',       'reserved'),
    ('static',    'reserved'),
    ('assets',    'reserved'),
    ('mcp',       'MCP endpoint prefix')
ON CONFLICT (handle) DO NOTHING;
```

---

## 4. sqlc — representative queries (hot paths)

**sqlc config** (`backend/sqlc.yaml`): engine `postgresql`, driver `pgx/v5`, enums emitted as Go
typed string constants, `citext → string`, `jsonb → []byte`, `inet → netip.Addr`. Queries live in
`backend/internal/db/queries/*.sql`; generated code in `backend/internal/db/gen`. The store
(`internal/db/store.go`) embeds `*gen.Queries`. Query names below are the real ones in the repo.

```sql
-- backend/internal/db/queries/sites.sql

-- name: GetSiteByHandle :one
-- DATA-PLANE + CONTROL-API HOT PATH. Resolve a *current, live* handle to its full site row.
-- Excludes soft-deleted sites (a deleted handle 404s); old handles do NOT match here (that is
-- GetSiteByRedirect's job). The cached published pointer lets the data plane answer without git
-- on the metadata side (it still reads the git tree for bytes).
SELECT * FROM sites
WHERE handle = @handle AND deleted_at IS NULL;

-- name: GetSiteByRedirect :one
-- Resolver fallback: a former handle -> the current (live) site row (data plane issues 301).
SELECT s.*
FROM handle_redirects r
JOIN sites s ON s.id = r.site_id
WHERE r.old_handle = @old_handle AND s.deleted_at IS NULL;

-- name: ListSitesForUser :many
-- DASHBOARD HOT PATH. Every live site the user can see (owner row maintained as a member),
-- newest activity first, with the viewer's effective role.
SELECT s.id, s.handle, s.owner_id, s.visibility, s.default_branch,
       s.published_commit_sha, s.published_at, s.publish_mode, s.github_repo,
       s.web_root, s.description, s.created_at, s.updated_at, m.role
FROM sites s
JOIN site_members m ON m.site_id = s.id AND m.user_id = @user_id
WHERE s.deleted_at IS NULL
ORDER BY s.updated_at DESC
LIMIT @lim OFFSET @off;

-- name: CreateSite :one
-- Create a site (UUID generated by DB). Runs in the SAME tx as AddOwnerMembership and the
-- SiteService git-init. Handle pre-validated + collision-checked by the app before this call.
INSERT INTO sites (
    handle, owner_id, visibility, default_branch,
    publish_mode, github_repo, web_root, description
)
VALUES (@handle, @owner_id, @visibility, @default_branch,
        @publish_mode, sqlc.narg(github_repo), @web_root, @description)
RETURNING *;

-- name: SetPublished :exec
-- Update the cache pointer + timestamp at publish time (called AFTER the git ref move succeeds).
UPDATE sites
SET published_commit_sha = @published_commit_sha, published_at = now(), updated_at = now()
WHERE id = @id;

-- name: RenameHandle :exec
-- Step of the atomic rename (the redirect INSERT is InsertRedirect in the same tx).
UPDATE sites SET handle = @new_handle, updated_at = now() WHERE id = @id;

-- name: SoftDeleteSite :exec
-- Decision #3: stamp deleted_at; the handle stays reserved during the grace window and the
-- on-disk repo retention/reaping is the SiteService's job.
UPDATE sites SET deleted_at = now(), updated_at = now() WHERE id = @id AND deleted_at IS NULL;

-- name: InsertRedirect :exec
INSERT INTO handle_redirects (old_handle, site_id)
VALUES (@old_handle, @site_id)
ON CONFLICT (old_handle) DO UPDATE SET site_id = EXCLUDED.site_id;

-- name: HandleIsTaken :one
-- One-shot collision check across live handles, redirects, and reserved words.
SELECT EXISTS (
    SELECT 1 FROM sites            s WHERE s.handle     = @handle AND s.deleted_at IS NULL
    UNION ALL
    SELECT 1 FROM handle_redirects r WHERE r.old_handle = @handle
    UNION ALL
    SELECT 1 FROM reserved_handles h WHERE h.handle     = @handle
) AS taken;
```

```sql
-- backend/internal/db/queries/members.sql

-- name: AddOwnerMembership :exec
-- Stamp the creating user as owner in the same tx as CreateSite. Forces role=owner.
INSERT INTO site_members (site_id, user_id, role, created_by)
VALUES (@site_id, @user_id, 'owner', @user_id)
ON CONFLICT (site_id, user_id) DO UPDATE SET role = 'owner';

-- name: AddMember :exec
-- Add or update a member's role (re-grant safe).
INSERT INTO site_members (site_id, user_id, role, created_by)
VALUES (@site_id, @user_id, @role, sqlc.narg(created_by))
ON CONFLICT (site_id, user_id) DO UPDATE SET role = EXCLUDED.role;

-- name: GetRole :one
-- AUTHZ HOT PATH (also the per-request cap for a per-user token, §6.2): the user's role on a
-- site; no row => no access.
SELECT role FROM site_members WHERE site_id = @site_id AND user_id = @user_id;

-- name: RemoveMember :exec
DELETE FROM site_members WHERE site_id = @site_id AND user_id = @user_id;
```

```sql
-- backend/internal/db/queries/auth.sql

-- name: GetUserByIdentity :one
-- LOGIN HOT PATH: (provider, subject) -> the active user row. Match on the stable OIDC `sub`,
-- never email. Inactive (soft-disabled) users are rejected.
SELECT u.*
FROM user_identities i
JOIN users u ON u.id = i.user_id
WHERE i.provider = @provider AND i.subject = @subject AND u.is_active = TRUE;

-- name: UpsertUser :one
-- Match-or-create at OIDC callback (matched on email); run in tx with UpsertIdentity.
INSERT INTO users (email, display_name, avatar_url)
VALUES (@email, @display_name, sqlc.narg(avatar_url))
ON CONFLICT (email) DO UPDATE
    SET display_name = EXCLUDED.display_name,
        avatar_url   = COALESCE(EXCLUDED.avatar_url, users.avatar_url),
        updated_at   = now()
RETURNING *;

-- name: PromoteUserAdmin :exec
-- PASSWORD mode only: the single admin IS the instance admin, so first-run setup and every
-- password login promote that user to is_admin=true (without touching can_create_sites).
UPDATE users SET is_admin = TRUE, updated_at = now() WHERE id = @id;

-- name: GetSession :one
-- AUTH HOT PATH: validate cookie on every authenticated request. Joins the user so middleware
-- gets identity (incl. is_admin / can_create_sites) in one round-trip; rejects expired sessions
-- and inactive users.
SELECT s.id, s.user_id, s.created_at, s.expires_at, s.last_seen_at,
       u.email, u.display_name, u.avatar_url, u.is_admin, u.can_create_sites
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = @id AND s.expires_at > now() AND u.is_active = TRUE;

-- name: DeleteExpiredSessions :execrows
-- Janitor (system).
DELETE FROM sessions WHERE expires_at < now();
```

```sql
-- backend/internal/db/queries/tokens.sql  (per-USER tokens; see §1.6)

-- name: CreateUserToken :one
-- Issue a token for a user. scopes ⊆ {read,write,publish}; can_create_sites gates the MCP
-- create_site capability (default FALSE, additionally capped by users.can_create_sites at call
-- time). Returns everything EXCEPT the hash.
INSERT INTO user_tokens (user_id, name, token_prefix, token_hash, scopes, can_create_sites, expires_at)
VALUES (@user_id, @name, @token_prefix, @token_hash, @scopes, @can_create_sites, sqlc.narg(expires_at))
RETURNING id, user_id, name, token_prefix, scopes, can_create_sites,
          created_at, last_used_at, expires_at, revoked_at;

-- name: GetUserTokenByPrefix :many
-- AUTH-BY-TOKEN HOT PATH (MCP/API). Narrow by the indexed prefix among ACTIVE tokens; the caller
-- then constant-time compares token_hash (prefix is non-unique by design). Joins the owning user
-- so capability-capping can read their flags; per-site role capping happens LATER (GetRole at
-- call time) because a token now spans ALL of the user's memberships.
SELECT t.id, t.user_id, t.name, t.token_prefix, t.token_hash,
       t.scopes, t.can_create_sites, t.expires_at,
       u.is_active AS user_active, u.can_create_sites AS user_can_create_sites
FROM user_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_prefix = @token_prefix
  AND t.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now())
  AND u.is_active = TRUE;

-- name: ListUserTokens :many
-- Token-management UI: a user's own tokens. NEVER returns the hash.
SELECT id, user_id, name, token_prefix, scopes, can_create_sites,
       created_at, last_used_at, expires_at, revoked_at
FROM user_tokens
WHERE user_id = @user_id
ORDER BY created_at DESC;

-- name: TouchUserToken :exec
-- Throttled last_used_at update; never on every call.
UPDATE user_tokens SET last_used_at = now() WHERE id = @id;

-- name: RevokeUserToken :exec
-- Soft revoke (instant). Scoped to the owner to prevent cross-user revocation.
UPDATE user_tokens SET revoked_at = now() WHERE id = @id AND user_id = @user_id AND revoked_at IS NULL;
```

```sql
-- backend/internal/db/queries/instance_settings.sql  (§1.8 / §1.9)

-- name: GetInstanceSetting :one
-- Read one setting value by key. pgx.ErrNoRows => the key is unset (found=false at the store).
SELECT value FROM instance_settings WHERE key = @key;

-- name: SetInstanceSetting :exec
-- Upsert one setting value. The updated_at trigger stamps the time on UPDATE.
INSERT INTO instance_settings (key, value)
VALUES (@key, @value)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;

-- name: DeleteInstanceSetting :exec
-- Remove one setting by key (idempotent). Used to CLEAR a stored secret (the encrypted GitHub
-- token) so it reverts to the env fallback.
DELETE FROM instance_settings WHERE key = @key;
```

> The store layer (`internal/db/store.go`) wraps these with `GetGitHubConfig` / `SetGitHubConfig`,
> which seal/open the `github_token` value through `internal/secretbox` (§1.9) and refuse to persist
> a token when no secret box is wired.

```sql
-- backend/internal/db/queries/audit.sql

-- name: InsertAudit :exec
INSERT INTO audit_log (actor_user_id, site_id, token_id, action, source, commit_sha, metadata)
VALUES (sqlc.narg(actor_user_id), sqlc.narg(site_id), sqlc.narg(token_id),
        @action, @source, sqlc.narg(commit_sha), @metadata);

-- name: ListAuditForSite :many
-- Per-site activity feed (most recent first), keyset by id for stable pagination.
SELECT id, actor_user_id, site_id, token_id, action, source, commit_sha, metadata, created_at
FROM audit_log
WHERE site_id = @site_id
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id))
ORDER BY id DESC
LIMIT @lim;
```

### 4.1 Optimistic concurrency (base-SHA) — where it lives

The optimistic lock (write/save carries a **base commit SHA**, rejected on mismatch) is a **git
concern, enforced by the SiteService**, *not* a DB row version. There is no `version`/`xmin`
gymnastics here: the SiteService reads the current branch head, compares to the caller's `base_sha`,
and refuses (`409 Conflict`) on mismatch before committing. The DB only **records** the
attempt/outcome in `audit_log.metadata.base_sha`. This keeps git as the single source of truth for
content state.

---

## 5. Migrations (goose)

- **Tool:** `pressly/goose` v3, SQL-file migrations.
- **Location:** `backend/internal/db/migrations/NNNN_name.sql` with `-- +goose Up` / `-- +goose Down`
  sections, embedded into the binary (`embed.go`).
- **Numbering:** zero-padded sequential (`0001`…). Sequential (not timestamp) because this is a
  single-team OSS repo; sequential numbers read cleanly in PRs.
- **Statements needing care:** `CREATE FUNCTION` / `CREATE TRIGGER` bodies containing semicolons are
  wrapped in `-- +goose StatementBegin` / `-- +goose StatementEnd` so goose does not split on the
  inner `;`. Any future `ALTER TYPE ... ADD VALUE` must be flagged `-- +goose NO TRANSACTION` on PG
  versions that forbid it mid-tx.
- **Execution (boot-time, advisory-locked).** `internal/migrate` runs the embedded goose migrations
  automatically on backend start, gated by `KOTOJI_AUTO_MIGRATE` (default **true**) and serialized
  by a Postgres **advisory lock** so concurrent replicas don't race. Set `KOTOJI_AUTO_MIGRATE=false`
  to manage migrations out of band.
- **Down migrations:** authored for every Up (drop in reverse dependency order); production runs
  forward-only. Note `0004`'s Down recreates the original `site_tokens` shape (schema only — the
  invalidated token rows are **not** restored).

**Migration manifest (current):**

| # | File | Contents |
|---|---|---|
| 0001 | `init` | citext, pgcrypto; enums `site_role`/`site_visibility`/`audit_source`; `set_updated_at()`; tables `users`, `user_identities`, `sessions`, `sites`, `site_members`, `site_tokens` (v1), `handle_redirects`, `reserved_handles`, `audit_log` + all indexes/checks/triggers |
| 0002 | `seed_reserved` | idempotent reserved-word seed (§3.2) — schema seed, ships in prod |
| 0003 | `instance_settings` | `instance_settings` key/value table + updated_at trigger (§1.8) |
| 0004 | `user_tokens` | **DROP `site_tokens`**; CREATE per-user `user_tokens`; re-point `audit_log.token_id` FK → `user_tokens` (§1.6) |

> `0001` still *creates* `site_tokens` and `0004` drops it — the live schema therefore has
> `user_tokens`, **not** `site_tokens`. CANONICAL §4 folds this into a single logical `0001_init`
> showing `user_tokens` directly (with a note that it was established by `0004`).

---

## 6. Authorization model (per-site role × per-user token)

Two **orthogonal** axes, plus the instance superuser flag:

- **Per-site role** (`site_members.role`): `owner | editor | viewer`. See the capability matrix in
  CANONICAL §6.1.
- **Token/API scope** (`user_tokens.scopes`): subset of `{read, write, publish}`, chain
  `read ⊂ write ⊂ publish`. A token is owned by a **user** (`user_tokens.user_id`) and automatically
  covers ALL of that user's memberships — **no per-project binding**.
- **Instance superuser**: `users.is_admin` (admin screen, GitHub config, quotas, reserved-word
  edits; not a site role).
- **Site-creation capability**: `users.can_create_sites` AND `user_tokens.can_create_sites`.

### 6.1 Effective scope = token ∩ role, re-evaluated per request

On a given site, a token's **effective** capability is the **intersection** of:
1. the token's `scopes`, AND
2. the scopes the user's **membership role** grants on THAT site (`owner/editor → {read,write,
   publish}`, `viewer → {read}`), read via `GetRole` on **every** call.

Because step 2 is re-evaluated per request, removing the user's membership (or downgrading the role)
**instantly** limits the token — no re-issue — and a token can **never exceed the user's own access**.
Concretely:
- `read` scope ⇒ read tools only.
- `write` scope ⇒ read + write/save/rollback/create_branch — only where the user is owner/editor.
- `publish` scope ⇒ write + publish — only where the user may publish (owner, or editor under
  `publish_mode='direct'`).
- A user who is **not a member** of the named site is refused with `not_found` (no existence leak).
- `create_site` over MCP requires `user_tokens.can_create_sites = true` AND `users.can_create_sites
  = true`; the new site is owned by the user, so the same token immediately covers it.
- A token can never exceed `owner`; "delete site / manage members / issue tokens" are not grantable
  to any token in v1.

MCP tools take a **`site` (handle) selector**; `list_sites` returns the caller's memberships; an
unknown/non-member site → `not_found`. Enforcement lives in the REST/MCP middleware — the
`site.Service` is not membership-authz-aware. (See `mcp.md` and CANONICAL §6.2.)

---

## 7. Authoritative-store summary (restating the contract)

| Question the system asks | Answered from | Mechanism |
|---|---|---|
| "What bytes for `foo.hosting.example.com`?" | **git** | resolve handle (DB) → site uuid → git tree of `published` |
| "Which branches exist for site X?" | **git** | `git for-each-ref` (no DB table) |
| "Is `foo` taken?" | **DB** | `HandleIsTaken` across sites/redirects/reserved |
| "Can user U write site X?" | **DB** | `site_members.role` (and `user_tokens.scopes` ∩ role for token calls) |
| "Is this save safe (no clobber)?" | **git** | SiteService compares caller `base_sha` to branch head |
| "What did the AI change yesterday?" | **DB** | `audit_log` (+ the git commit it points to) |
| "Where is published right now?" | **git** (DB caches) | git ref is truth; `published_commit_sha`/`published_at` is a read-through cache |
| "Is the GitHub mirror on, and with what creds?" | **DB** | `instance_settings` (flag/org/webhook secret + encrypted token), env as fallback |
| "What's the admin password?" | **DB** | `instance_settings.admin_password_hash` (bcrypt; first-run setup), env as fallback |

---

## 8. Open questions / gaps (考慮漏れ)

1. **`site_branch_cache` for cross-site branch queries.** We deliberately have no `branches` table.
   If product later needs "list all open `feature-*` across sites" or per-branch preview metadata,
   add an *explicitly-cache* table refreshed by the SiteService. Deferred until a real use case.
2. **Quotas.** Admin quotas (per-user site count, per-site repo size) are not modeled. Likely a
   `quotas` table or columns on `users` (`max_sites`, `max_repo_bytes`) + a `sites.repo_bytes` cache.
3. **`sites.repo_bytes` / file-count cache.** A git-derived cache (stale-pointer risk) — include only
   if the dashboard needs a size badge without a git call.
4. **Per-branch visibility / preview auth.** `visibility` is per-site. Preview access uses a signed
   preview-grant → host-only `kotoji_preview` cookie (decision #7); a per-branch share token is not
   modeled.
5. **GitHub linkage detail.** `sites.github_repo` is `owner/name`; the instance-wide credentials/flags
   now live in `instance_settings` (§1.8). Per-site overrides, installation ids, and "last pulled
   SHA" are still not modeled (mirror is a single-org-token model in v1).
6. **Token rotation / per-token site restriction.** A per-user token covers all the user's
   memberships with no opt-out. If a user wants a token scoped to a subset of their projects, that is
   not expressible today (would need a `user_token_sites` allowlist join).
7. **`audit_log` retention / partitioning.** Append-only log grows unbounded. No retention policy or
   monthly partitioning defined yet; fine at small scale, revisit at millions of rows.
8. **Email uniqueness vs multi-IdP edge case.** We match identities to users by email. Two different
   IdPs returning the same email for different humans would merge into one `users` row. Acceptable for
   single-org use; documented as a known assumption.
9. **`instance_settings` secret rotation.** Rotating `KOTOJI_SECRET_KEY` makes the stored
   `github_token` ciphertext undecryptable; `secretbox.Open` degrades to "not configured" (no crash)
   and the admin re-enters the PAT in `/settings`. A key-versioning scheme (the `versionByte` is
   reserved for it) is not yet implemented.
10. **citext + index collation.** citext relies on the DB's default collation for case folding;
    non-ASCII handles are excluded by the ASCII-only format CHECK, so this is safe for handles.
