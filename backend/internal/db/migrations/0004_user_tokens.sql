-- 0004_user_tokens.sql — re-architect MCP/API tokens from PER-PROJECT to PER-USER.
--
-- TODAY (0001): site_tokens was a per-project credential (site_id NOT NULL). A
-- token was bound to ONE site and no MCP tool took a site selector (pivot-proof
-- by construction).
--
-- NEW MODEL (CANONICAL §6, re-architecture): a token is OWNED BY A USER, carries
-- ONE scope set + can_create_sites, and AUTOMATICALLY covers every project the
-- user is a member of. The EFFECTIVE scope on a given site is re-evaluated on
-- every request as intersection(token.scopes, role_scopes(membership role)) — so
-- removing the membership (or downgrading the role) instantly limits the token,
-- and a token can NEVER exceed its user's own access (membership-capped authz
-- REPLACES the old site-pin guarantee).
--
-- Existing per-project tokens are INTENTIONALLY INVALIDATED: we DROP site_tokens
-- outright. Users re-issue under /api/tokens. This is acceptable (early product).
--
-- audit_log.token_id referenced site_tokens(id) ON DELETE SET NULL; the audit
-- trail must outlive both the old tokens and the table swap, so we drop the old
-- FK (NULLing nothing — historical token_ids stay as opaque values) and re-point
-- it at user_tokens(id) ON DELETE SET NULL so future MCP/API audit rows keep
-- their FK guarantee.

-- +goose Up

-- Re-point the audit FK BEFORE dropping site_tokens. We drop the old constraint
-- (the historical token_id values remain in place as plain UUIDs; they no longer
-- reference a live row, which is fine for an append-only trail) and re-add it
-- against the new table so new MCP-attributed audit rows keep ON DELETE SET NULL.
ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS audit_log_token_id_fkey;

-- Drop the per-project token table and its indexes (CASCADE removes the indexes
-- and any dependent objects). Existing per-project tokens are invalidated here.
DROP TABLE IF EXISTS site_tokens;

-- ============================ user_tokens ============================
-- Per-USER MCP/API token (hash-only storage). One token = one user = one scope
-- set, automatically scoped to all of that user's memberships. The EFFECTIVE
-- scope per site is computed at call time (intersection with the membership
-- role's scopes); this table never stores a site binding.
CREATE TABLE user_tokens (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,  -- the human the token acts as
    name             TEXT        NOT NULL DEFAULT '',   -- human label, e.g. "claude-laptop"
    token_prefix     TEXT        NOT NULL,              -- first 12 chars of plaintext (UI + lookup)
    token_hash       BYTEA       NOT NULL,              -- sha256(plaintext), 32 bytes
    scopes           TEXT[]      NOT NULL DEFAULT '{read,write,publish}',  -- subset of {read,write,publish}
    can_create_sites BOOLEAN     NOT NULL DEFAULT FALSE,  -- gates create_site over MCP (capped by users.can_create_sites)
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at     TIMESTAMPTZ,                        -- throttled update
    expires_at       TIMESTAMPTZ,                        -- NULL = no expiry
    revoked_at       TIMESTAMPTZ,                        -- NULL = active
    CONSTRAINT user_tokens_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT user_tokens_prefix_len CHECK (char_length(token_prefix) = 12),
    CONSTRAINT user_tokens_scopes_valid CHECK (scopes <@ ARRAY['read','write','publish']::text[])
);
CREATE UNIQUE INDEX uq_user_tokens_hash ON user_tokens (token_hash);
-- AUTH-BY-TOKEN HOT PATH: narrow by the indexed prefix among ACTIVE tokens.
CREATE INDEX idx_user_tokens_prefix  ON user_tokens (token_prefix) WHERE revoked_at IS NULL;
-- Token-management UI: list a user's own tokens.
CREATE INDEX idx_user_tokens_user_id ON user_tokens (user_id);

-- Existing audit rows still carry token_id values that referenced the now-dropped
-- site_tokens; user_tokens starts empty, so EVERY non-null token_id is orphaned.
-- NULL them before re-adding the FK (ADD CONSTRAINT validates existing rows, so a
-- dangling reference would abort the migration). The trail keeps actor_user_id +
-- metadata; only the (invalidated) token linkage is cleared.
UPDATE audit_log SET token_id = NULL WHERE token_id IS NOT NULL;

-- Re-point audit_log.token_id at the new table (ON DELETE SET NULL preserves the
-- trail when a token is hard-deleted; revocation is soft so this rarely fires).
ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_token_id_fkey
    FOREIGN KEY (token_id) REFERENCES user_tokens(id) ON DELETE SET NULL;

-- +goose Down
-- Reverse: drop the new FK + table and recreate the original per-project
-- site_tokens shape (and its FK) so a goose-down round-trips to the 0003 schema.
-- (Token rows are NOT restored — the data was intentionally invalidated on Up.)
ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS audit_log_token_id_fkey;
DROP TABLE IF EXISTS user_tokens;

CREATE TABLE site_tokens (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id          UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    created_by       UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name             TEXT        NOT NULL DEFAULT '',
    token_prefix     TEXT        NOT NULL,
    token_hash       BYTEA       NOT NULL,
    scopes           TEXT[]      NOT NULL DEFAULT '{read,write,publish}',
    can_create_sites BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at     TIMESTAMPTZ,
    expires_at       TIMESTAMPTZ,
    revoked_at       TIMESTAMPTZ,
    CONSTRAINT site_tokens_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT site_tokens_prefix_len CHECK (char_length(token_prefix) = 12),
    CONSTRAINT site_tokens_scopes_valid CHECK (scopes <@ ARRAY['read','write','publish']::text[])
);
CREATE UNIQUE INDEX uq_site_tokens_hash ON site_tokens (token_hash);
CREATE INDEX idx_site_tokens_prefix     ON site_tokens (token_prefix) WHERE revoked_at IS NULL;
CREATE INDEX idx_site_tokens_site_id    ON site_tokens (site_id);

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_token_id_fkey
    FOREIGN KEY (token_id) REFERENCES site_tokens(id) ON DELETE SET NULL;
