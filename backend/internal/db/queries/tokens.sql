-- tokens.sql — per-site MCP/API tokens (hash-only storage).
-- Columns match the 0001_init `site_tokens` DDL. Plaintext is shown once at create
-- and never persisted; only token_hash (sha256, 32 bytes) + a 12-char prefix are stored.

-- name: CreateToken :one
-- Issue a token. scopes is a subset of {read,write,publish}; can_create_sites gates the
-- MCP create_site capability (default FALSE). Returns everything EXCEPT the hash.
INSERT INTO site_tokens (
    site_id, created_by, name, token_prefix, token_hash,
    scopes, can_create_sites, expires_at
)
VALUES (
    @site_id, @created_by, @name, @token_prefix, @token_hash,
    @scopes, @can_create_sites, sqlc.narg(expires_at)
)
RETURNING id, site_id, created_by, name, token_prefix, scopes,
          can_create_sites, created_at, last_used_at, expires_at, revoked_at;

-- name: GetTokenByPrefix :many
-- AUTH-BY-TOKEN HOT PATH (MCP/API). Narrow by indexed prefix among ACTIVE tokens; the
-- caller then constant-time compares token_hash to pick the exact row (prefix is not
-- unique by design). Joins the creating user so scope-capping can read their role/flags.
SELECT t.id, t.site_id, t.created_by, t.name, t.token_prefix, t.token_hash,
       t.scopes, t.can_create_sites, t.expires_at,
       u.is_active AS creator_active, u.can_create_sites AS creator_can_create_sites
FROM site_tokens t
JOIN users u ON u.id = t.created_by
WHERE t.token_prefix = @token_prefix
  AND t.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now())
  AND u.is_active = TRUE;

-- name: GetTokenByHash :one
-- Exact lookup by full hash (defense-in-depth / admin tooling). Active tokens only.
SELECT t.id, t.site_id, t.created_by, t.name, t.token_prefix, t.token_hash,
       t.scopes, t.can_create_sites, t.expires_at, t.revoked_at
FROM site_tokens t
JOIN users u ON u.id = t.created_by
WHERE t.token_hash = @token_hash
  AND t.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now())
  AND u.is_active = TRUE;

-- name: ListTokensForSite :many
-- Token management UI. NEVER returns the hash.
SELECT id, site_id, created_by, name, token_prefix, scopes, can_create_sites,
       created_at, last_used_at, expires_at, revoked_at
FROM site_tokens
WHERE site_id = @site_id
ORDER BY created_at DESC;

-- name: RevokeToken :exec
-- Soft revoke (instant). Scoped to the site to prevent cross-site revocation.
UPDATE site_tokens SET revoked_at = now()
WHERE id = @id AND site_id = @site_id AND revoked_at IS NULL;

-- name: TouchToken :exec
-- Throttled last_used_at update; never on every call.
UPDATE site_tokens SET last_used_at = now() WHERE id = @id;
