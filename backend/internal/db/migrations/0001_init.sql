-- 0001_init.sql — the COMPLETE kotoji metadata schema.
--
-- This single migration is the canonical artifact (CANONICAL.md §4). It creates
-- all extensions, enums, the shared updated_at trigger function, all 9 tables,
-- their indexes, FKs, and CHECK constraints exactly per the frozen DDL.
--
-- git is the source of truth for content; this schema stores only metadata +
-- cache pointers (data-model.md §0). Target: PostgreSQL 18 (17 compatible).
--
-- NOTE on goose statement framing: each CREATE FUNCTION / CREATE TRIGGER body
-- that contains semicolons is wrapped in its own StatementBegin/End block so
-- goose does not split it on the inner ';'. Plain DDL statements (CREATE TABLE,
-- CREATE INDEX, CREATE TYPE, CREATE EXTENSION) are split by goose on ';' and do
-- NOT need StatementBegin/End.

-- +goose Up

-- ============================ extensions ============================
CREATE EXTENSION IF NOT EXISTS citext;    -- case-insensitive handles & emails
CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid()

-- ============================ enums ============================
-- Per-site role. owner > editor > viewer. (CANONICAL §4 / consistency #1.10.)
CREATE TYPE site_role AS ENUM ('owner', 'editor', 'viewer');

-- Served-content visibility (data plane). 3-valued (consistency #1.4).
--   public   : anyone with the URL (subject to instance PUBLISHED_PUBLIC cap)
--   internal : any authenticated user of this kotoji instance
--   private  : only site members (owner/editor/viewer)
CREATE TYPE site_visibility AS ENUM ('public', 'internal', 'private');

-- Which writer/origin produced an audited action (consistency #1.12).
--   upload : zip upload path
--   editor : Monaco / dashboard
--   mcp    : MCP tool call
--   system : webhook pulls, scheduled jobs, admin actions, migrations
CREATE TYPE audit_source AS ENUM ('upload', 'editor', 'mcp', 'system');

-- ============================ shared trigger fn ============================
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- ============================ users ============================
CREATE TABLE users (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email            CITEXT      NOT NULL UNIQUE,
    display_name     TEXT        NOT NULL DEFAULT '',
    avatar_url       TEXT,
    is_admin         BOOLEAN     NOT NULL DEFAULT FALSE,  -- instance superuser (separate axis)
    can_create_sites BOOLEAN     NOT NULL DEFAULT TRUE,   -- may this user create sites? (decision #2/#8)
    is_active        BOOLEAN     NOT NULL DEFAULT TRUE,    -- soft disable
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_users_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================ user_identities ============================
CREATE TABLE user_identities (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider      TEXT        NOT NULL,   -- AuthProvider key: 'google','keycloak','dev',...
    subject       TEXT        NOT NULL,   -- OIDC `sub`: stable, opaque, provider-scoped
    email_at_link CITEXT,                 -- email seen at link time (audit)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    CONSTRAINT user_identities_provider_subject_key UNIQUE (provider, subject)
);
CREATE INDEX idx_user_identities_user_id ON user_identities (user_id);

-- ============================ sessions ============================
CREATE TABLE sessions (
    id           TEXT        PRIMARY KEY,   -- opaque CSPRNG id (the __Host- cookie value)
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_agent   TEXT        NOT NULL DEFAULT '',
    ip_addr      INET
);
CREATE INDEX idx_sessions_user_id    ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

-- ============================ sites ============================
CREATE TABLE sites (
    id                   UUID            PRIMARY KEY DEFAULT gen_random_uuid(),  -- == /data/sites/{id}
    handle               CITEXT          NOT NULL UNIQUE,
    owner_id             UUID            NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    visibility           site_visibility NOT NULL DEFAULT 'private',
    default_branch       TEXT            NOT NULL DEFAULT 'draft',
    published_commit_sha TEXT,                       -- CACHE pointer into git; NULL = never published
    published_at         TIMESTAMPTZ,                -- last publish time (dashboard badge)
    publish_mode         TEXT            NOT NULL DEFAULT 'direct'   -- decision #6
                         CHECK (publish_mode IN ('direct', 'request')),
    github_repo          TEXT,                       -- 'owner/name' for mirror; NULL = no mirror
    web_root             TEXT            NOT NULL DEFAULT '',        -- served subdir; '' = repo root (v1)
    description          TEXT            NOT NULL DEFAULT '',
    deleted_at           TIMESTAMPTZ,                -- soft delete (decision #3); 30-day grace
    created_at           TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ     NOT NULL DEFAULT now(),

    -- DNS-label format (defense in depth; Go validator is the friendly primary gate).
    CONSTRAINT sites_handle_format CHECK (
        handle ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
    ),
    -- git object id: 40 hex (SHA-1) or 64 hex (SHA-256 forward-compat).
    CONSTRAINT sites_published_sha_format CHECK (
        published_commit_sha IS NULL OR published_commit_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
    )
);
CREATE UNIQUE INDEX idx_sites_github_repo ON sites (github_repo) WHERE github_repo IS NOT NULL;
CREATE INDEX idx_sites_owner_id   ON sites (owner_id);
CREATE INDEX idx_sites_updated_at ON sites (updated_at DESC);
-- handle resolution should ignore soft-deleted sites on the hot path:
CREATE INDEX idx_sites_handle_live ON sites (handle) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_sites_updated_at BEFORE UPDATE ON sites
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================ site_members ============================
CREATE TABLE site_members (
    site_id    UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       site_role   NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID        REFERENCES users(id) ON DELETE SET NULL,  -- who granted (audit)
    PRIMARY KEY (site_id, user_id)
);
CREATE INDEX idx_site_members_user_id ON site_members (user_id);

-- ============================ site_tokens ============================
-- Per-project MCP/API token. (consistency #1.2 canonical shape.)
CREATE TABLE site_tokens (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id          UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,  -- one token = one site
    created_by       UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,  -- the human it acts as
    name             TEXT        NOT NULL DEFAULT '',   -- human label, e.g. "claude-laptop"
    token_prefix     TEXT        NOT NULL,              -- first 12 chars of plaintext (UI + lookup)
    token_hash       BYTEA       NOT NULL,              -- sha256(plaintext), 32 bytes
    scopes           TEXT[]      NOT NULL DEFAULT '{read,write,publish}',  -- subset of {read,write,publish}
    can_create_sites BOOLEAN     NOT NULL DEFAULT FALSE,  -- gates create_site over MCP (decision #2/#8)
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at     TIMESTAMPTZ,                        -- throttled update
    expires_at       TIMESTAMPTZ,                        -- NULL = no expiry
    revoked_at       TIMESTAMPTZ,                        -- NULL = active
    CONSTRAINT site_tokens_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT site_tokens_prefix_len CHECK (char_length(token_prefix) = 12),
    CONSTRAINT site_tokens_scopes_valid CHECK (scopes <@ ARRAY['read','write','publish']::text[])
);
CREATE UNIQUE INDEX uq_site_tokens_hash   ON site_tokens (token_hash);
CREATE INDEX idx_site_tokens_prefix       ON site_tokens (token_prefix) WHERE revoked_at IS NULL;
CREATE INDEX idx_site_tokens_site_id      ON site_tokens (site_id);

-- ============================ handle_redirects ============================
CREATE TABLE handle_redirects (
    old_handle CITEXT      PRIMARY KEY,                                  -- the freed former handle
    site_id    UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,  -- -> current site
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT handle_redirects_format CHECK (
        old_handle ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
    )
);
CREATE INDEX idx_handle_redirects_site_id ON handle_redirects (site_id);

-- ============================ reserved_handles ============================
-- Admin-editable blocklist; baseline seeded by 0002 (§5). Go constant is the fallback.
CREATE TABLE reserved_handles (
    handle     CITEXT      PRIMARY KEY,
    reason     TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ============================ audit_log ============================
-- Append-only. ALL FKs ON DELETE SET NULL so the trail outlives referenced rows.
CREATE TABLE audit_log (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_user_id UUID         REFERENCES users(id)       ON DELETE SET NULL,  -- human actor; NULL for system
    site_id       UUID         REFERENCES sites(id)       ON DELETE SET NULL,  -- target; NULL for instance-level
    token_id      UUID         REFERENCES site_tokens(id) ON DELETE SET NULL,  -- if via a token
    action        TEXT         NOT NULL,        -- 'site.create','file.write','publish','rollback',...
    source        audit_source NOT NULL,        -- upload|editor|mcp|system
    commit_sha    TEXT,                         -- resulting git commit, if any
    metadata      JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- {paths,branch,base_sha,handle,ip,kind,...}
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_log_site_created  ON audit_log (site_id, created_at DESC);
CREATE INDEX idx_audit_log_actor_created ON audit_log (actor_user_id, created_at DESC);
CREATE INDEX idx_audit_log_metadata      ON audit_log USING gin (metadata);

-- +goose Down
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS reserved_handles;
DROP TABLE IF EXISTS handle_redirects;
DROP TABLE IF EXISTS site_tokens;
DROP TABLE IF EXISTS site_members;
DROP TABLE IF EXISTS sites;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS user_identities;
DROP TABLE IF EXISTS users;
DROP TYPE IF EXISTS audit_source;
DROP TYPE IF EXISTS site_visibility;
DROP TYPE IF EXISTS site_role;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS set_updated_at();
-- +goose StatementEnd
DROP EXTENSION IF EXISTS pgcrypto;
DROP EXTENSION IF EXISTS citext;
