-- members.sql — per-site membership (authz) queries.
-- Columns match the 0001_init `site_members` DDL.

-- name: AddMember :exec
-- Add or update a member's role. Upsert keeps the call idempotent (re-grant safe).
INSERT INTO site_members (site_id, user_id, role, created_by)
VALUES (@site_id, @user_id, @role, sqlc.narg(created_by))
ON CONFLICT (site_id, user_id) DO UPDATE SET role = EXCLUDED.role;

-- name: AddOwnerMembership :exec
-- Stamp the creating user as owner in the same tx as CreateSite. Forces role=owner.
INSERT INTO site_members (site_id, user_id, role, created_by)
VALUES (@site_id, @user_id, 'owner', @user_id)
ON CONFLICT (site_id, user_id) DO UPDATE SET role = 'owner';

-- name: GetMember :one
-- Full membership row for (site, user).
SELECT site_id, user_id, role, created_at, created_by
FROM site_members
WHERE site_id = @site_id AND user_id = @user_id;

-- name: GetRole :one
-- AUTHZ HOT PATH: this user's role on this site; no row => no access.
SELECT role FROM site_members
WHERE site_id = @site_id AND user_id = @user_id;

-- name: ListMembers :many
-- Member management UI: every member of a site with their identity, newest first.
SELECT m.site_id, m.user_id, m.role, m.created_at, m.created_by,
       u.email, u.display_name, u.avatar_url
FROM site_members m
JOIN users u ON u.id = m.user_id
WHERE m.site_id = @site_id
ORDER BY m.created_at DESC;

-- name: UpdateMemberRole :exec
-- Change an existing member's role (owner-only action enforced above the Store).
UPDATE site_members SET role = @role
WHERE site_id = @site_id AND user_id = @user_id;

-- name: RemoveMember :exec
DELETE FROM site_members
WHERE site_id = @site_id AND user_id = @user_id;
