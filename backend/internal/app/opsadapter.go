package app

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

// auditMetaJSON marshals an audit metadata map to JSONB bytes; a marshal failure
// (impossible for plain maps) degrades to an empty object so audit never blocks.
func auditMetaJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// opsStoreAdapter maps the metadata *db.Store onto the ops.Store seam (ops-domain
// terms, no sqlc/pgtype leakage into the ops package). It lives in the composition
// root because it is the only place that knows both the generated queries and the
// ops contract.
type opsStoreAdapter struct{ store *db.Store }

// compile-time guarantee the adapter satisfies the ops dependency.
var _ ops.Store = (*opsStoreAdapter)(nil)

// newOpsStoreAdapter wires the metadata store into the ops jobs' store dependency.
func newOpsStoreAdapter(store *db.Store) *opsStoreAdapter { return &opsStoreAdapter{store: store} }

// ListSitesPastGrace returns soft-deleted sites older than cutoff (now - grace).
func (a *opsStoreAdapter) ListSitesPastGrace(ctx context.Context, cutoff time.Time) ([]ops.SoftDeletedSite, error) {
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

// HardDeleteSite purges the sites row (and cascades) after on-disk reclaim.
func (a *opsStoreAdapter) HardDeleteSite(ctx context.Context, id uuid.UUID) error {
	return a.store.HardDeleteSite(ctx, id)
}

// ListLiveSiteIDs returns the IDs of all non-deleted sites.
func (a *opsStoreAdapter) ListLiveSiteIDs(ctx context.Context) ([]uuid.UUID, error) {
	return a.store.ListLiveSiteIDs(ctx)
}

// InsertSystemAudit appends a system-sourced audit row (best-effort; the caller
// ignores the error so audit never blocks a reaper pass).
func (a *opsStoreAdapter) InsertSystemAudit(ctx context.Context, siteID uuid.UUID, action string, meta map[string]any) error {
	id := siteID
	return a.store.InsertAudit(ctx, gen.InsertAuditParams{
		SiteID:   &id,
		Action:   action,
		Source:   gen.AuditSourceSystem,
		Metadata: auditMetaJSON(meta),
	})
}
