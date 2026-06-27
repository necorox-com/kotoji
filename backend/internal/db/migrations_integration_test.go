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

	migrations "github.com/necorox-com/kotoji/backend/internal/db/migrations"
)

// embeddedMigrationsDir is the goose dir argument when reading from the embedded
// FS: the .sql files sit at the embed root, so the directory is ".". Using the
// embedded FS (the same one production's internal/migrate consumes) makes this
// test CWD-INDEPENDENT — it no longer depends on the test binary running with
// its working directory at internal/db just to resolve a relative "migrations"
// path, so `go test -tags=integration ./...` is stable from any CWD (e.g. CI).
const embeddedMigrationsDir = "."

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

	// Drive goose off the EMBEDDED migration FS (same source production uses) so
	// the round-trip does not depend on the process working directory. Without
	// this, goose's default osFS would resolve the relative "migrations" dir
	// against CWD and break under `go test -tags=integration ./...` from backend/.
	goose.SetBaseFS(migrations.FS)

	// Start from a known-clean state, then migrate fully up.
	require.NoError(t, goose.DownTo(sqlDB, embeddedMigrationsDir, 0), "reset to baseline")
	require.NoError(t, goose.Up(sqlDB, embeddedMigrationsDir), "goose up")

	// The reserved-handle seed (0002) must have populated the baseline rows.
	var count int
	require.NoError(t,
		sqlDB.QueryRow(`SELECT count(*) FROM reserved_handles`).Scan(&count))
	require.Equal(t, len(ReservedHandlesBaseline), count,
		"reserved_handles row count must match the Go baseline after 0002")

	// Core tables must exist and be queryable. After 0004 the token table is the
	// per-user user_tokens; the per-project site_tokens is dropped.
	for _, tbl := range []string{
		"users", "user_identities", "sessions", "sites", "site_members",
		"user_tokens", "handle_redirects", "reserved_handles", "audit_log",
		"instance_settings",
	} {
		_, err := sqlDB.Exec("SELECT 1 FROM " + tbl + " LIMIT 0")
		require.NoErrorf(t, err, "table %s must exist after up", tbl)
	}

	// The per-project site_tokens table must be GONE after the 0004 swap.
	_, err0 := sqlDB.Exec("SELECT 1 FROM site_tokens LIMIT 0")
	require.Error(t, err0, "site_tokens must NOT exist after the 0004 migration")

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
	require.NoError(t, goose.DownTo(sqlDB, embeddedMigrationsDir, 0), "goose down")
}

// TestSiteVisibilityRenameMembers proves the 0005 migration renamed the
// site_visibility enum VALUE 'internal' -> 'members': after a full goose up the
// enum carries 'members' (and NOT 'internal'), and the Down rename reverses it.
// A bare 'members'::site_visibility cast round-trips to confirm the label is a
// real, usable enum member (the rename also carries any existing 'internal' rows
// in place — see 0005_rename_visibility_members.sql).
func TestSiteVisibilityRenameMembers(t *testing.T) {
	sqlDB := openSQL(t)

	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetBaseFS(migrations.FS)

	require.NoError(t, goose.DownTo(sqlDB, embeddedMigrationsDir, 0), "reset to baseline")
	require.NoError(t, goose.Up(sqlDB, embeddedMigrationsDir), "goose up")

	// labels() reads the current ordered set of site_visibility enum labels.
	labels := func() []string {
		rows, err := sqlDB.Query(
			`SELECT e.enumlabel FROM pg_enum e
			   JOIN pg_type t ON t.oid = e.enumtypid
			  WHERE t.typname = 'site_visibility'
			  ORDER BY e.enumsortorder`)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var l string
			require.NoError(t, rows.Scan(&l))
			out = append(out, l)
		}
		require.NoError(t, rows.Err())
		return out
	}

	// After 0005 the middle tier is 'members'; 'public'/'private' are unchanged.
	require.Equal(t, []string{"public", "members", "private"}, labels(),
		"site_visibility must read [public, members, private] after 0005")

	// 'members' must be a usable enum value (round-trips through a cast).
	var got string
	require.NoError(t,
		sqlDB.QueryRow(`SELECT 'members'::site_visibility`).Scan(&got))
	require.Equal(t, "members", got)

	// The pre-rename label must be gone.
	require.NotContains(t, labels(), "internal",
		"the old 'internal' visibility label must not survive 0005")

	// Reversing only 0005 (Down) restores 'internal' — symmetric rename.
	require.NoError(t, goose.Down(sqlDB, embeddedMigrationsDir), "goose down one (0005)")
	require.Equal(t, []string{"public", "internal", "private"}, labels(),
		"0005 Down must rename 'members' back to 'internal'")

	require.NoError(t, goose.DownTo(sqlDB, embeddedMigrationsDir, 0), "goose down")
}

// ensure the pgx stdlib driver is linked (registers "pgx" for database/sql).
var _ = stdlib.GetDefaultDriver
