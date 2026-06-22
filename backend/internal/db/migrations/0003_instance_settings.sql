-- 0003_instance_settings.sql — instance-level singleton/key-value settings.
--
-- A small key/value store for instance-wide configuration that must persist in
-- the database rather than the environment. The first consumer is the first-run
-- admin-password flow (key 'admin_password_hash'): when AUTH_MODE=password and no
-- env password is set, the bcrypt hash the admin chooses on first run lives here.
--
-- Shape: one row per setting key. updated_at is maintained by the shared
-- set_updated_at() trigger function (created in 0001) so writes stamp the time.

-- +goose Up
CREATE TABLE instance_settings (
    key        TEXT        PRIMARY KEY,                 -- e.g. 'admin_password_hash'
    value      TEXT        NOT NULL,                    -- opaque string (a bcrypt hash, a flag, ...)
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_instance_settings_updated_at BEFORE UPDATE ON instance_settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS instance_settings;
