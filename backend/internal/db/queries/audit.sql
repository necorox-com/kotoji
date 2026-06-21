-- audit.sql — append-only audit trail.
-- Columns match the 0001_init `audit_log` DDL. All FK columns are nullable so the trail
-- survives deletion of the referenced user/site/token (ON DELETE SET NULL).

-- name: InsertAudit :exec
-- Append one audited action. actor/site/token are nullable (system actions, instance-level
-- events, non-token paths). metadata denormalizes context (handle, paths, branch, ip, ...)
-- so the row stays meaningful after referenced rows vanish.
INSERT INTO audit_log (
    actor_user_id, site_id, token_id, action, source, commit_sha, metadata
)
VALUES (
    sqlc.narg(actor_user_id),
    sqlc.narg(site_id),
    sqlc.narg(token_id),
    @action,
    @source,
    sqlc.narg(commit_sha),
    @metadata
);

-- name: ListAuditForSite :many
-- Per-site activity feed (most recent first), keyset-paginated by id for stability.
-- A NULL before_id returns the newest page.
SELECT id, actor_user_id, site_id, token_id, action, source, commit_sha, metadata, created_at
FROM audit_log
WHERE site_id = @site_id
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id))
ORDER BY id DESC
LIMIT @lim;

-- name: ListAuditForActor :many
-- Admin "what did this user do", newest first, keyset-paginated.
SELECT id, actor_user_id, site_id, token_id, action, source, commit_sha, metadata, created_at
FROM audit_log
WHERE actor_user_id = @actor_user_id
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id))
ORDER BY id DESC
LIMIT @lim;
