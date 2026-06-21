// Package db is the metadata data-access layer. It wraps the sqlc-generated
// queries (internal/db/gen) behind a pgxpool-backed Store that the control plane
// (Phase 2/4) depends on. git remains the source of truth for content; this layer
// only ever touches the Postgres metadata store (data-model.md §0).
//
// DI / testability: callers depend on the Querier interface (re-exported from the
// generated package), not the concrete *Store, so a generated mock or a fake can
// be injected in tests. The Store adds the connection lifecycle (pool + ping),
// a transaction helper (tx.go), and a handful of domain-typed convenience methods
// that bundle the common multi-query flows Phase 2/4 need (e.g. atomic site create).
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// Querier is the full generated query surface. Re-exported here so consumers can
// depend on internal/db (not internal/db/gen) and mock a single interface. The
// generated *gen.Queries and *Store both satisfy it.
type Querier = gen.Querier

// pingTimeout bounds the initial connectivity check and the readiness probe so a
// dead/slow database fails fast instead of hanging the caller.
const pingTimeout = 5 * time.Second

// Store is the application-facing handle to the metadata database. It embeds the
// generated *gen.Queries (so every named query is available directly) and owns the
// underlying pgxpool for lifecycle (Close) and transactions (WithTx).
type Store struct {
	*gen.Queries
	pool *pgxpool.Pool
}

// compile-time guarantee: a *Store exposes the whole generated query surface.
var _ gen.Querier = (*Store)(nil)

// New opens a pgxpool against dsn, verifies connectivity with a ping, and returns
// a ready Store. The caller owns Close. dsn is a standard pgx connection string
// or URL (e.g. "postgres://user:pass@host:5432/db?sslmode=disable").
func New(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("db: empty DSN")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse DSN: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}

	// Fail fast on a dead database so boot does not silently proceed without
	// persistence. A bounded context guards against an unreachable host hanging.
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return &Store{
		Queries: gen.New(pool),
		pool:    pool,
	}, nil
}

// NewWithPool builds a Store over an already-constructed pool. Useful for tests
// and for callers that manage the pool's lifecycle themselves (it does NOT ping).
func NewWithPool(pool *pgxpool.Pool) *Store {
	return &Store{Queries: gen.New(pool), pool: pool}
}

// Pool exposes the underlying pool for advanced callers (e.g. goose advisory-lock
// migration on boot). Most code should use the query methods or WithTx instead.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases all pooled connections. Safe to call once during shutdown.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ready implements observability.ReadinessChecker: /readyz returns 503 while the
// database is unreachable. The bounded ping keeps the probe responsive.
func (s *Store) Ready(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := s.pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("db: not ready: %w", err)
	}
	return nil
}

// ---- domain-typed convenience wrappers (the flows Phase 2/4 reuse) ----

// CreateSiteWithOwner inserts a site and stamps the owner's site_members row in ONE
// transaction. This is the metadata half of SiteService.CreateSite; the git-init is
// layered on top by Phase 2 (which can pass its own tx via WithTx). The handle must
// already be validated + collision-checked by the caller. Returns the created site.
func (s *Store) CreateSiteWithOwner(ctx context.Context, arg gen.CreateSiteParams) (gen.Site, error) {
	var site gen.Site
	err := s.WithTx(ctx, func(q *gen.Queries) error {
		created, err := q.CreateSite(ctx, arg)
		if err != nil {
			return fmt.Errorf("create site: %w", err)
		}
		// Maintain an owner membership row so a single membership join answers authz
		// uniformly (data-model.md §1.5). created_by = owner = self at creation time.
		if err := q.AddOwnerMembership(ctx, gen.AddOwnerMembershipParams{
			SiteID: created.ID,
			UserID: created.OwnerID,
		}); err != nil {
			return fmt.Errorf("add owner membership: %w", err)
		}
		site = created
		return nil
	})
	if err != nil {
		return gen.Site{}, err
	}
	return site, nil
}

// RenameHandleWithRedirect performs the atomic rename: record old->site in
// handle_redirects, update the live handle, and drop any stale redirect that points
// the NEW handle back at this same site (rename-back). All in one transaction
// (CANONICAL §5.5). The caller validates newHandle and collision-checks beforehand.
func (s *Store) RenameHandleWithRedirect(ctx context.Context, id uuid.UUID, oldHandle, newHandle string) error {
	return s.WithTx(ctx, func(q *gen.Queries) error {
		// Rename-back: if newHandle is a redirect of THIS site, remove it first so the
		// live handle and the redirect set never both claim newHandle.
		if err := q.DeleteRedirect(ctx, gen.DeleteRedirectParams{
			OldHandle: newHandle,
			SiteID:    id,
		}); err != nil {
			return fmt.Errorf("clear rename-back redirect: %w", err)
		}
		if err := q.InsertRedirect(ctx, gen.InsertRedirectParams{
			OldHandle: oldHandle,
			SiteID:    id,
		}); err != nil {
			return fmt.Errorf("insert redirect: %w", err)
		}
		if err := q.RenameHandle(ctx, gen.RenameHandleParams{
			NewHandle: newHandle,
			ID:        id,
		}); err != nil {
			return fmt.Errorf("rename handle: %w", err)
		}
		return nil
	})
}

// IsNotFound reports whether err is pgx's no-rows sentinel, the canonical "row does
// not exist" signal from the :one queries. Callers map this to site.ErrNotFound.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
