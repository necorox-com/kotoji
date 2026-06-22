-- auth.sql — users, identities, and server-side sessions.
-- Columns match the 0001_init `users` / `user_identities` / `sessions` DDL.

-- name: UpsertUser :one
-- Match-or-create at OIDC callback (matched on email). Runs in a tx with UpsertIdentity.
-- Avatar only overwrites when the new value is non-NULL (don't clobber with empties).
INSERT INTO users (email, display_name, avatar_url)
VALUES (@email, @display_name, sqlc.narg(avatar_url))
ON CONFLICT (email) DO UPDATE
    SET display_name = EXCLUDED.display_name,
        avatar_url   = COALESCE(EXCLUDED.avatar_url, users.avatar_url),
        updated_at   = now()
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = @id;

-- name: SetUserAdminFlags :exec
-- Set the instance-superuser axis (is_admin) and the site-creation capability flag.
-- Admin-only action; used by the dev seed to promote the bootstrap admin and by the
-- admin screen (Phase 4) to toggle a user's instance powers.
UPDATE users
SET is_admin         = @is_admin,
    can_create_sites = @can_create_sites,
    updated_at        = now()
WHERE id = @id;

-- name: PromoteUserAdmin :exec
-- Promote a user to instance admin (is_admin=true) WITHOUT touching
-- can_create_sites. Used in PASSWORD mode only: the single admin IS the instance
-- admin, so first-run setup and every password login promote that user. NEVER
-- called for oidc/none users (they are governed by the admin screen instead).
UPDATE users
SET is_admin   = TRUE,
    updated_at = now()
WHERE id = @id;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = @email;

-- name: GetUserByIdentity :one
-- LOGIN HOT PATH: (provider, subject) -> the active user row. Match on the stable
-- OIDC `sub`, never email. Inactive (soft-disabled) users are rejected.
SELECT u.*
FROM user_identities i
JOIN users u ON u.id = i.user_id
WHERE i.provider = @provider AND i.subject = @subject
  AND u.is_active = TRUE;

-- name: UpsertIdentity :exec
-- Link (or refresh) a (provider, subject) -> user identity. Refreshes last_login_at.
INSERT INTO user_identities (user_id, provider, subject, email_at_link, last_login_at)
VALUES (@user_id, @provider, @subject, sqlc.narg(email_at_link), now())
ON CONFLICT (provider, subject) DO UPDATE
    SET last_login_at = now(),
        email_at_link = EXCLUDED.email_at_link;

-- name: ListIdentitiesForUser :many
-- Account screen: a user's linked IdP identities.
SELECT id, user_id, provider, subject, email_at_link, created_at, last_login_at
FROM user_identities
WHERE user_id = @user_id
ORDER BY created_at ASC;

-- name: CreateSession :one
-- Persist a new server-side session. The opaque id (cookie value) is app-generated.
INSERT INTO sessions (id, user_id, expires_at, user_agent, ip_addr)
VALUES (@id, @user_id, @expires_at, @user_agent, sqlc.narg(ip_addr))
RETURNING *;

-- name: GetSession :one
-- AUTH HOT PATH: validate the cookie on every request. Joins the user so middleware
-- gets identity in one round-trip; rejects expired sessions and inactive users.
SELECT s.id, s.user_id, s.created_at, s.expires_at, s.last_seen_at,
       u.email, u.display_name, u.avatar_url, u.is_admin, u.can_create_sites
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = @id
  AND s.expires_at > now()
  AND u.is_active = TRUE;

-- name: TouchSession :exec
-- Throttled by the caller (only when last_seen_at is stale enough).
UPDATE sessions SET last_seen_at = now() WHERE id = @id;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = @id;

-- name: DeleteUserSessions :exec
-- "Log out everywhere" / admin kill-switch.
DELETE FROM sessions WHERE user_id = @user_id;

-- name: DeleteExpiredSessions :execrows
-- Janitor (system). Index on expires_at keeps this cheap.
DELETE FROM sessions WHERE expires_at < now();
