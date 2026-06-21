package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/ops"
)

// opsStore adapts *db.Store onto the ops.Store seam for the standalone ops command.
// It mirrors internal/app.opsStoreAdapter (which is unexported); duplicating the
// thin mapping here keeps cmd/kotoji-ops independent of the composition root.
type opsStore struct{ store *db.Store }

var _ ops.Store = opsStore{}

func (a opsStore) ListSitesPastGrace(ctx context.Context, cutoff time.Time) ([]ops.SoftDeletedSite, error) {
	rows, err := a.store.ListSitesForReaper(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make([]ops.SoftDeletedSite, 0, len(rows))
	for _, r := range rows {
		var deletedAt time.Time
		if r.DeletedAt.Valid {
			deletedAt = r.DeletedAt.Time
		}
		out = append(out, ops.SoftDeletedSite{ID: r.ID, Handle: r.Handle, DeletedAt: deletedAt})
	}
	return out, nil
}

func (a opsStore) HardDeleteSite(ctx context.Context, id uuid.UUID) error {
	return a.store.HardDeleteSite(ctx, id)
}

func (a opsStore) ListLiveSiteIDs(ctx context.Context) ([]uuid.UUID, error) {
	return a.store.ListLiveSiteIDs(ctx)
}

func (a opsStore) InsertSystemAudit(ctx context.Context, siteID uuid.UUID, action string, meta map[string]any) error {
	id := siteID
	b, err := json.Marshal(meta)
	if err != nil {
		b = []byte("{}")
	}
	return a.store.InsertAudit(ctx, gen.InsertAuditParams{
		SiteID:   &id,
		Action:   action,
		Source:   gen.AuditSourceSystem,
		Metadata: b,
	})
}
