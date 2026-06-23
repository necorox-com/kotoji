-- tokens.sql — per-USER MCP/API tokens (hash-only storage).
-- Columns match the 0004 `user_tokens` DDL. A token is owned by a user and carries
-- ONE scope set; it automatically covers all of that user's memberships. The
-- effective scope on a given site is computed at call time (intersection with the
-- membership role's scopes), so this table never stores a site binding. Plaintext
-- is shown once at create and never persisted; only token_hash (sha256, 32 bytes)
-- + a 12-char prefix are stored.

-- name: CreateUserToken :one
-- Issue a token for a user. scopes is a subset of {read,write,publish};
-- can_create_sites gates the MCP create_site capability (default FALSE, and
-- additionally capped by users.can_create_sites at call time). Returns everything
-- EXCEPT the hash.
INSERT INTO user_tokens (
    user_id, name, token_prefix, token_hash,
    scopes, can_create_sites, expires_at
)
VALUES (
    @user_id, @name, @token_prefix, @token_hash,
    @scopes, @can_create_sites, sqlc.narg(expires_at)
)
RETURNING id, user_id, name, token_prefix, scopes,
          can_create_sites, created_at, last_used_at, expires_at, revoked_at;

-- name: GetUserTokenByPrefix :many
-- AUTH-BY-TOKEN HOT PATH (MCP/API). Narrow by indexed prefix among ACTIVE tokens; the
-- caller then constant-time compares token_hash to pick the exact row (prefix is not
-- unique by design). Joins the owning user so capability-capping can read their flags;
-- per-site role capping happens later (GetRole at call time) since a token now spans
-- ALL of the user's memberships.
SELECT t.id, t.user_id, t.name, t.token_prefix, t.token_hash,
       t.scopes, t.can_create_sites, t.expires_at,
       u.is_active AS user_active, u.can_create_sites AS user_can_create_sites
FROM user_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_prefix = @token_prefix
  AND t.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now())
  AND u.is_active = TRUE;

-- name: GetUserTokenByHash :one
-- Exact lookup by full hash (defense-in-depth / admin tooling). Active tokens only.
SELECT t.id, t.user_id, t.name, t.token_prefix, t.token_hash,
       t.scopes, t.can_create_sites, t.expires_at, t.revoked_at
FROM user_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = @token_hash
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

-- name: RevokeUserToken :exec
-- Soft revoke (instant). Scoped to the owner to prevent cross-user revocation.
UPDATE user_tokens SET revoked_at = now()
WHERE id = @id AND user_id = @user_id AND revoked_at IS NULL;

-- name: TouchUserToken :exec
-- Throttled last_used_at update; never on every call.
UPDATE user_tokens SET last_used_at = now() WHERE id = @id;
