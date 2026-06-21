package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// WithTx runs fn inside a single database transaction, passing it a *gen.Queries
// bound to that transaction. The transaction commits if fn returns nil and rolls
// back otherwise. A panic inside fn rolls back and re-panics so a buggy callback
// never leaves a transaction open.
//
// This is THE place atomic multi-query flows are expressed (e.g.
// CreateSiteWithOwner, RenameHandleWithRedirect, and the Phase 2 SiteService's
// "metadata + git in one logical op"). Nested/independent queries outside a tx use
// the embedded *gen.Queries on the Store directly.
func (s *Store) WithTx(ctx context.Context, fn func(q *gen.Queries) error) (err error) {
	tx, beginErr := s.pool.Begin(ctx)
	if beginErr != nil {
		return fmt.Errorf("db: begin tx: %w", beginErr)
	}

	// Guarantee the transaction is always resolved. On a panic we roll back and
	// re-panic; on a normal error path the deferred rollback is a no-op after an
	// explicit commit (pgx returns ErrTxClosed, which we deliberately ignore).
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("db: rollback: %w", rbErr))
			}
		}
	}()

	if err = fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		err = fmt.Errorf("db: commit: %w", commitErr)
		return err
	}
	return nil
}
