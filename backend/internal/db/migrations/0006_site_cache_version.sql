-- 0006_site_cache_version.sql — per-site cache generation counter.
--
-- Adds sites.cache_version: a monotonically increasing integer the data plane
-- folds into the asset ETag (alongside the published commit SHA). An operator
-- "Clear cache" action (POST /api/sites/{handle}/cache/purge) bumps it, which
-- changes EVERY asset ETag for that site, forcing all clients to refetch fresh on
-- their next revalidation — WITHOUT requiring a new publish/commit.
--
-- NOT NULL DEFAULT 0 so every existing row gets a deterministic starting value and
-- the data plane never has to special-case a NULL. A plain ADD COLUMN with a
-- constant default is a metadata-only change in PostgreSQL 11+ (no table rewrite),
-- so it is transaction-safe and runs inside goose's default per-migration tx.

-- +goose Up
ALTER TABLE sites ADD COLUMN cache_version INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sites DROP COLUMN cache_version;
