package db

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migrationsDir is the goose SQL directory, relative to this test file.
const migrationsDir = "migrations"

// readMigration loads one migration file's contents, failing the test if missing
// or empty (the cheap "files exist & are non-empty" gate).
func readMigration(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(migrationsDir, name)
	b, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read %s", path)
	require.NotEmptyf(t, strings.TrimSpace(string(b)), "%s must be non-empty", path)
	return string(b)
}

// TestMigrationFilesExistAndNonEmpty asserts both migration files are present and
// have content. This is the lightweight guard runnable without Postgres.
func TestMigrationFilesExistAndNonEmpty(t *testing.T) {
	for _, name := range []string{"0001_init.sql", "0002_seed_reserved.sql", "0003_instance_settings.sql", "0004_user_tokens.sql"} {
		_ = readMigration(t, name)
	}
}

// TestMigrationGooseFraming checks each migration declares the goose Up/Down
// directives. Every StatementBegin must be balanced by a StatementEnd so goose's
// parser does not choke on function/trigger bodies.
func TestMigrationGooseFraming(t *testing.T) {
	for _, name := range []string{"0001_init.sql", "0002_seed_reserved.sql", "0003_instance_settings.sql", "0004_user_tokens.sql"} {
		src := readMigration(t, name)
		assert.Containsf(t, src, "-- +goose Up", "%s missing +goose Up", name)
		assert.Containsf(t, src, "-- +goose Down", "%s missing +goose Down", name)

		begins := strings.Count(src, "-- +goose StatementBegin")
		ends := strings.Count(src, "-- +goose StatementEnd")
		assert.Equalf(t, begins, ends,
			"%s: unbalanced StatementBegin(%d)/StatementEnd(%d)", name, begins, ends)
	}
}

// TestInitMigrationCoversSchema asserts the init migration creates every contracted
// table, enum, and the shared trigger function (CANONICAL §4). A missing object here
// means a contract regression even before Postgres runs the DDL.
func TestInitMigrationCoversSchema(t *testing.T) {
	src := strings.ToLower(readMigration(t, "0001_init.sql"))

	tables := []string{
		"users", "user_identities", "sessions", "sites", "site_members",
		"site_tokens", "handle_redirects", "reserved_handles", "audit_log",
	}
	for _, tbl := range tables {
		assert.Containsf(t, src, "create table "+tbl, "init must create table %s", tbl)
	}

	enums := []string{"site_role", "site_visibility", "audit_source"}
	for _, e := range enums {
		assert.Containsf(t, src, "create type "+e+" as enum", "init must create enum %s", e)
	}

	assert.Contains(t, src, "create extension if not exists citext", "init must enable citext")
	assert.Contains(t, src, "create or replace function set_updated_at", "init must define set_updated_at()")
}

// TestInitMigrationDownDropsEverything asserts the Down section drops every table
// (so a goose reset / test teardown is clean).
func TestInitMigrationDownDropsEverything(t *testing.T) {
	src := strings.ToLower(readMigration(t, "0001_init.sql"))
	_, down, found := strings.Cut(src, "-- +goose down")
	require.True(t, found, "init migration must have a +goose Down section")

	for _, tbl := range []string{
		"users", "user_identities", "sessions", "sites", "site_members",
		"site_tokens", "handle_redirects", "reserved_handles", "audit_log",
	} {
		assert.Containsf(t, down, "drop table if exists "+tbl, "Down must drop table %s", tbl)
	}
}

// TestUserTokensMigrationSwapsTokenTable asserts the 0004 re-architecture migration
// drops the per-project site_tokens table and creates the per-user user_tokens
// table (the canonical token model swap), keeping the audit FK consistent.
func TestUserTokensMigrationSwapsTokenTable(t *testing.T) {
	src := strings.ToLower(readMigration(t, "0004_user_tokens.sql"))

	up, down, found := strings.Cut(src, "-- +goose down")
	require.True(t, found, "0004 must have a +goose Down section")

	// Up: drop site_tokens, create user_tokens (with the canonical columns), and
	// re-point the audit FK at user_tokens.
	assert.Contains(t, up, "drop table if exists site_tokens", "Up must drop site_tokens")
	assert.Contains(t, up, "create table user_tokens", "Up must create user_tokens")
	assert.Contains(t, up, "user_id", "user_tokens must be keyed by user_id")
	assert.Contains(t, up, "references user_tokens(id) on delete set null",
		"Up must re-point audit_log.token_id at user_tokens (SET NULL)")
	assert.Contains(t, up, "octet_length(token_hash) = 32", "Up must keep the sha256 hash-len CHECK")
	assert.Contains(t, up, "char_length(token_prefix) = 12", "Up must keep the 12-char prefix CHECK")

	// Down: reverse cleanly back to the per-project site_tokens shape.
	assert.Contains(t, down, "drop table if exists user_tokens", "Down must drop user_tokens")
	assert.Contains(t, down, "create table site_tokens", "Down must recreate site_tokens")
}

// reservedSeedRe extracts the handles from the 0002 INSERT VALUES rows of the form
// ('handle', 'reason'). It anchors on the leading paren+quote so it does not also
// match the DELETE list (which has a different shape).
var reservedSeedRe = regexp.MustCompile(`\(\s*'([a-z0-9-]+)'\s*,\s*'[^']*'\s*\)`)

// TestReservedSeedMatchesGoBaseline guards the data-model.md §6 dual-maintenance
// hazard: the handles inserted by 0002_seed_reserved.sql MUST equal the Go
// ReservedHandlesBaseline constant. Drift fails the build.
func TestReservedSeedMatchesGoBaseline(t *testing.T) {
	src := readMigration(t, "0002_seed_reserved.sql")

	// Only scan the Up section's INSERT (avoid matching the Down DELETE list).
	up, _, found := strings.Cut(src, "-- +goose Down")
	require.True(t, found, "seed migration must have a +goose Down section")

	matches := reservedSeedRe.FindAllStringSubmatch(up, -1)
	require.NotEmpty(t, matches, "no reserved handles parsed from seed INSERT")

	seeded := make([]string, 0, len(matches))
	for _, m := range matches {
		seeded = append(seeded, m[1])
	}

	want := append([]string(nil), ReservedHandlesBaseline...)
	sort.Strings(want)
	sort.Strings(seeded)

	assert.Equal(t, want, seeded,
		"0002_seed_reserved.sql handles must match db.ReservedHandlesBaseline exactly")
}
