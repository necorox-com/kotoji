// Package migrate runs the embedded goose migrations against Postgres on boot.
// The composition root calls Run before any plane starts, so a fresh
// `docker compose up` provisions the schema with zero manual steps. A Postgres
// session-level advisory lock serializes concurrent migrators (rolling restarts /
// multiple boots), which keeps it correct even though decision #4 targets a single
// replica.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	migrations "github.com/necorox-com/kotoji/backend/internal/db/migrations"
)

// advisoryLockKey is a fixed, application-chosen key for the boot-migration lock.
// The exact value is arbitrary; it only has to be stable across replicas.
const advisoryLockKey int64 = 0x6b6f746f6a69 // "kotoji" in ASCII hex

// migrationTimeout bounds the whole migrate operation so a wedged lock cannot
// hang boot indefinitely.
const migrationTimeout = 2 * time.Minute

// Run applies all pending migrations embedded in internal/db/migrations against
// dsn. goose uses database/sql (not pgx native), so this opens a short-lived
// *sql.DB via the pgx stdlib driver, holds a session advisory lock on a dedicated
// connection for the duration, applies migrations, then releases everything.
// It is idempotent: with no pending migrations it is a no-op.
func Run(ctx context.Context, dsn string, logger *slog.Logger) error {
	if dsn == "" {
		return fmt.Errorf("migrate: empty database URL")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("migrate: open: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()

	// Hold the advisory lock on its own connection so concurrent migrators block
	// here (the lock is session-scoped) rather than racing goose's own connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("migrate: acquire lock: %w", err)
	}
	// Release explicitly; closing conn would also drop the session lock, but being
	// explicit keeps the intent clear and frees it before the conn pool reaps.
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	}()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("migrate: dialect: %w", err)
	}

	before, _ := goose.GetDBVersionContext(ctx, db)
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("migrate: up: %w", err)
	}
	after, _ := goose.GetDBVersionContext(ctx, db)

	if after != before {
		logger.Info("database migrations applied", slog.Int64("from", before), slog.Int64("to", after))
	} else {
		logger.Info("database schema up to date", slog.Int64("version", after))
	}
	return nil
}
