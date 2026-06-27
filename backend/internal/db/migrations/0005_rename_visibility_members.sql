-- 0005_rename_visibility_members.sql — rename site_visibility VALUE 'internal' -> 'members'.
--
-- The middle visibility tier was relabeled in the UI to "Members"/メンバー, so the
-- backend enum VALUE follows. The two other values (public, private) are unchanged,
-- and the column default (private) is unaffected.
--
-- ALTER TYPE ... RENAME VALUE is a Postgres 10+ in-place rename: it rewrites the
-- enum label in pg_enum, so every EXISTING ROW that stored 'internal' now reads
-- 'members' automatically — no UPDATE over sites.visibility is needed. Unlike
-- ALTER TYPE ... ADD VALUE, RENAME VALUE is transaction-safe, so it runs inside
-- goose's default per-migration transaction (no NO-TRANSACTION annotation needed).

-- +goose Up
ALTER TYPE site_visibility RENAME VALUE 'internal' TO 'members';

-- +goose Down
-- Reverse rename. Symmetric and equally transaction-safe; carries existing rows back.
ALTER TYPE site_visibility RENAME VALUE 'members' TO 'internal';
