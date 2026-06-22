//go:build integration

// These tests require a live PostgreSQL instance (with the citext + pgcrypto
// extensions available) reachable via KOTOJI_DATABASE_URL. They are excluded from
// the default `go test ./...` fast path and run only under `-tags=integration`.
//
//	KOTOJI_DATABASE_URL=postgres://kotoji:kotoji@localhost:5432/kotoji_test?sslmode=disable \
//	    go test -tags=integration ./internal/db/...
package db

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// openSQL returns a database/sql handle for goose (goose drives migrations over the
// stdlib interface, not pgxpool).
func openSQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("KOTOJI_DATABASE_URL")
	if dsn == "" {
		t.Skip("KOTOJI_DATABASE_URL not set; skipping live migration test")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx), "database must be reachable")
	return db
}

// TestMigrationsUpDownRoundTrip proves the goose migrations apply cleanly to a real
// database and tear down cleanly — the full up → assert seeded → down round-trip.
func TestMigrationsUpDownRoundTrip(t *testing.T) {
	sqlDB := openSQL(t)

	require.NoError(t, goose.SetDialect("postgres"))

	// Start from a known-clean state, then migrate fully up.
	require.NoError(t, goose.DownTo(sqlDB, migrationsDir, 0), "reset to baseline")
	require.NoError(t, goose.Up(sqlDB, migrationsDir), "goose up")

	// The reserved-handle seed (0002) must have populated the baseline rows.
	var count int
	require.NoError(t,
		sqlDB.QueryRow(`SELECT count(*) FROM reserved_handles`).Scan(&count))
	require.Equal(t, len(ReservedHandlesBaseline), count,
		"reserved_handles row count must match the Go baseline after 0002")

	// Core tables must exist and be queryable.
	for _, tbl := range []string{
		"users", "user_identities", "sessions", "sites", "site_members",
		"site_tokens", "handle_redirects", "reserved_handles", "audit_log",
		"instance_settings",
	} {
		_, err := sqlDB.Exec("SELECT 1 FROM " + tbl + " LIMIT 0")
		require.NoErrorf(t, err, "table %s must exist after up", tbl)
	}

	// The instance_settings key/value upsert round-trips (first-run admin hash path).
	_, err := sqlDB.Exec(`INSERT INTO instance_settings (key, value) VALUES ('admin_password_hash', 'h1')`)
	require.NoError(t, err)
	_, err = sqlDB.Exec(`INSERT INTO instance_settings (key, value) VALUES ('admin_password_hash', 'h2')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	require.NoError(t, err)
	var v string
	require.NoError(t, sqlDB.QueryRow(`SELECT value FROM instance_settings WHERE key = 'admin_password_hash'`).Scan(&v))
	require.Equal(t, "h2", v, "upsert must overwrite the existing value")

	// Tear all the way down; every object must drop without error.
	require.NoError(t, goose.DownTo(sqlDB, migrationsDir, 0), "goose down")
}

// ensure the pgx stdlib driver is linked (registers "pgx" for database/sql).
var _ = stdlib.GetDefaultDriver
