-- 0002_seed_reserved.sql — seed the reserved_handles blocklist baseline.
--
-- This is a SCHEMA seed (ships in prod, idempotent ON CONFLICT DO NOTHING),
-- distinct from the dev-only data seed in cmd/seed. The exact list mirrors the
-- CANONICAL §5 ReservedHandles Go constant; a go test asserts they stay in sync.

-- +goose Up
INSERT INTO reserved_handles (handle, reason) VALUES
    ('draft',     'branch/preview keyword'),
    ('preview',   'branch/preview keyword'),
    ('published', 'branch/preview keyword'),
    ('www',       'infra'),
    ('api',       'control-plane path prefix'),
    ('internal',  'infra'),
    ('host',      'path-based fallback prefix'),
    ('admin',     'reserved'),
    ('app',       'reserved'),
    ('static',    'reserved'),
    ('assets',    'reserved'),
    ('mcp',       'MCP endpoint prefix')
ON CONFLICT (handle) DO NOTHING;

-- +goose Down
DELETE FROM reserved_handles WHERE handle IN
    ('draft','preview','published','www','api','internal','host','admin','app','static','assets','mcp');
