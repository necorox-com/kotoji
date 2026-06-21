-- reserved.sql — admin-editable reserved-handle blocklist.
-- Columns match the 0001_init `reserved_handles` DDL.

-- name: IsReserved :one
-- Returns TRUE if the candidate handle is on the blocklist (case-insensitive via citext).
SELECT EXISTS (
    SELECT 1 FROM reserved_handles WHERE handle = @handle
) AS reserved;

-- name: ListReserved :many
-- The full blocklist (admin UI + the §6 consistency test asserting it matches the Go const).
SELECT handle, reason, created_at
FROM reserved_handles
ORDER BY handle;

-- name: AddReserved :exec
-- Admins may ADD entries at runtime (baseline entries are protected above the Store).
INSERT INTO reserved_handles (handle, reason)
VALUES (@handle, @reason)
ON CONFLICT (handle) DO UPDATE SET reason = EXCLUDED.reason;

-- name: RemoveReserved :exec
-- Remove a non-baseline reserved handle (baseline protection is enforced in app code).
DELETE FROM reserved_handles WHERE handle = @handle;
