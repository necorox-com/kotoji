// Command seed populates a LOCAL DEVELOPMENT database with the minimum rows needed
// to log in and start building. It is guarded: it refuses to run unless
// KOTOJI_ENV=development, so it can never touch a production database.
//
// What it seeds (idempotently):
//   - one admin user (KOTOJI_AUTH_ADMIN_EMAIL, is_admin=true, can_create_sites=true)
//   - a `dev`-provider identity for that admin (so AUTH_MODE=none/password can log
//     in immediately)
//
// Reserved handles are seeded by migration 0002 (ships in prod); this command only
// asserts they are present and warns if the migration has not been run. Sample SITES
// require the SiteService git-init (Phase 2) and are intentionally NOT created here —
// DB rows without the on-disk repo would violate the "git is authoritative" invariant
// (data-model.md §0).
//
// Re-running is safe: every write is an upsert / presence check (no-op if present).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/observability"
)

// seedTimeout bounds the whole seed run so a stuck DB can't hang CI/make.
const seedTimeout = 30 * time.Second

// devProvider is the pseudo-IdP key used by no-auth / admin-password modes so a
// seeded admin can authenticate without an external OIDC provider.
const devProvider = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := observability.NewLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	// HARD GUARD: never seed anything but a development database.
	if cfg.Env != config.EnvDevelopment {
		return fmt.Errorf("refusing to seed: KOTOJI_ENV=%q (only 'development' is allowed)", cfg.Env)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("KOTOJI_DATABASE_URL is required to seed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), seedTimeout)
	defer cancel()

	store, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer store.Close()

	if err := ensureReservedHandles(ctx, store, logger); err != nil {
		return err
	}
	if err := ensureAdminUser(ctx, store, cfg, logger); err != nil {
		return err
	}

	logger.Info("seed complete")
	return nil
}

// ensureReservedHandles asserts the migration 0002 baseline is present. It does not
// insert (the migration owns that); it warns loudly if the table is empty so a
// developer who forgot to migrate gets a clear signal.
func ensureReservedHandles(ctx context.Context, store *db.Store, logger *slog.Logger) error {
	rows, err := store.ListReserved(ctx)
	if err != nil {
		return fmt.Errorf("list reserved handles: %w", err)
	}
	if len(rows) == 0 {
		logger.Warn("reserved_handles is empty — run migrations (goose up) before seeding")
		return nil
	}
	// Cross-check against the Go baseline so a drift between the constant and the
	// seeded rows surfaces during dev seeding, not in production.
	seeded := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		seeded[r.Handle] = struct{}{}
	}
	var missing []string
	for _, h := range db.ReservedHandlesBaseline {
		if _, ok := seeded[h]; !ok {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		logger.Warn("reserved_handles missing baseline entries", slog.Any("missing", missing))
	}
	logger.Info("reserved handles present", slog.Int("count", len(rows)))
	return nil
}

// ensureAdminUser creates (or refreshes) the admin user and its dev identity in one
// transaction. Idempotent: UpsertUser matches on email, UpsertIdentity on (provider,
// subject). The admin gets is_admin=true so they can reach instance tooling.
func ensureAdminUser(ctx context.Context, store *db.Store, cfg config.Config, logger *slog.Logger) error {
	return store.WithTx(ctx, func(q *gen.Queries) error {
		user, err := q.UpsertUser(ctx, gen.UpsertUserParams{
			Email:       cfg.AdminEmail,
			DisplayName: "Admin",
			AvatarUrl:   nil,
		})
		if err != nil {
			return fmt.Errorf("upsert admin user: %w", err)
		}

		// UpsertUser does not set is_admin (that column is only stamped by an explicit
		// admin flow). Promote here, in the SAME transaction, so the dev admin actually
		// has instance powers. Using the bound `q` (not store.Pool()) is critical: a
		// separate pooled connection would block on the row lock this tx already holds,
		// self-deadlocking the seed.
		if !user.IsAdmin || !user.CanCreateSites {
			if err := q.SetUserAdminFlags(ctx, gen.SetUserAdminFlagsParams{
				ID:             user.ID,
				IsAdmin:        true,
				CanCreateSites: true,
			}); err != nil {
				return fmt.Errorf("promote admin: %w", err)
			}
		}

		// Stable dev subject keyed on the email so re-seeding maps to the same identity.
		subject := "dev-admin:" + cfg.AdminEmail
		email := cfg.AdminEmail
		if err := q.UpsertIdentity(ctx, gen.UpsertIdentityParams{
			UserID:      user.ID,
			Provider:    devProvider,
			Subject:     subject,
			EmailAtLink: &email,
		}); err != nil {
			return fmt.Errorf("upsert admin identity: %w", err)
		}

		logger.Info("admin user ensured",
			slog.String("email", cfg.AdminEmail),
			slog.String("id", user.ID.String()),
		)
		return nil
	})
}
