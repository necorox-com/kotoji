-- instance_settings.sql — instance-level key/value settings.
-- Columns match the 0003_instance_settings DDL.

-- name: GetInstanceSetting :one
-- Read one setting value by key. pgx.ErrNoRows => the key is unset (found=false
-- at the store layer). Used by the first-run admin-password flow.
SELECT value FROM instance_settings WHERE key = @key;

-- name: SetInstanceSetting :exec
-- Upsert one setting value (insert or overwrite). The updated_at trigger stamps
-- the time on UPDATE; the INSERT default covers first write.
INSERT INTO instance_settings (key, value)
VALUES (@key, @value)
ON CONFLICT (key) DO UPDATE
    SET value = EXCLUDED.value;

-- name: DeleteInstanceSetting :exec
-- Remove one setting by key (idempotent: no-op when absent). Used to CLEAR a
-- stored secret (the encrypted GitHub token) so it reverts to the env fallback.
DELETE FROM instance_settings WHERE key = @key;
