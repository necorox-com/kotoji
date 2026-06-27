package site

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// dbStoreAdapter adapts the production *db.Store (sqlc + pgx) to the site.Store
// interface, translating between pgtype/gen types and the site-domain SiteRecord.
// This is the only file in the package that imports internal/db; gitService stays
// decoupled from the persistence types, satisfying the DI/mockability contract.
type dbStoreAdapter struct {
	s *db.Store
}

// NewStore wraps a *db.Store as a site.Store. The production composition root
// (internal/app) builds the *db.Store and passes it here.
func NewStore(s *db.Store) Store {
	return &dbStoreAdapter{s: s}
}

// compile-time guarantee the adapter satisfies the interface.
var _ Store = (*dbStoreAdapter)(nil)

func (a *dbStoreAdapter) CreateSiteWithOwner(ctx context.Context, in StoreCreateSite) (SiteRecord, error) {
	row, err := a.s.CreateSiteWithOwner(ctx, gen.CreateSiteParams{
		Handle:        in.Handle,
		OwnerID:       in.OwnerID,
		Visibility:    gen.SiteVisibility(in.Visibility),
		DefaultBranch: string(BranchDraft),
		PublishMode:   in.PublishMode,
		GithubRepo:    in.GitHubRepo,
		WebRoot:       in.WebRoot,
		Description:   in.Description,
	})
	if err != nil {
		return SiteRecord{}, err
	}
	return siteRecordFromGen(row), nil
}

func (a *dbStoreAdapter) GetSiteByID(ctx context.Context, id uuid.UUID) (SiteRecord, bool, error) {
	row, err := a.s.GetSiteByID(ctx, id)
	if err != nil {
		if db.IsNotFound(err) {
			return SiteRecord{}, false, nil
		}
		return SiteRecord{}, false, err
	}
	return siteRecordFromGen(row), true, nil
}

func (a *dbStoreAdapter) GetSiteByHandle(ctx context.Context, handle string) (SiteRecord, bool, error) {
	row, err := a.s.GetSiteByHandle(ctx, handle)
	if err != nil {
		if db.IsNotFound(err) {
			return SiteRecord{}, false, nil
		}
		return SiteRecord{}, false, err
	}
	return siteRecordFromGen(row), true, nil
}

func (a *dbStoreAdapter) GetSiteByRedirect(ctx context.Context, oldHandle string) (SiteRecord, bool, error) {
	row, err := a.s.GetSiteByRedirect(ctx, oldHandle)
	if err != nil {
		if db.IsNotFound(err) {
			return SiteRecord{}, false, nil
		}
		return SiteRecord{}, false, err
	}
	return siteRecordFromGen(row), true, nil
}

func (a *dbStoreAdapter) ListSitesForUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]SiteRecord, error) {
	rows, err := a.s.ListSitesForUser(ctx, gen.ListSitesForUserParams{
		UserID: userID,
		Off:    int32(offset),
		Lim:    int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]SiteRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, SiteRecord{
			ID:            r.ID,
			Handle:        r.Handle,
			OwnerID:       r.OwnerID,
			Visibility:    string(r.Visibility),
			DefaultBranch: r.DefaultBranch,
			PublishedSHA:  r.PublishedCommitSha,
			PublishedAt:   nullableTime(r.PublishedAt),
			PublishMode:   r.PublishMode,
			GitHubRepo:    r.GithubRepo,
			WebRoot:       r.WebRoot,
			Description:   r.Description,
			CreatedAt:     r.CreatedAt.Time,
			UpdatedAt:     r.UpdatedAt.Time,
		})
	}
	return out, nil
}

func (a *dbStoreAdapter) HandleIsTaken(ctx context.Context, handle string) (bool, error) {
	return a.s.HandleIsTaken(ctx, handle)
}

func (a *dbStoreAdapter) RenameHandleWithRedirect(ctx context.Context, id uuid.UUID, oldHandle, newHandle string) error {
	return a.s.RenameHandleWithRedirect(ctx, id, oldHandle, newHandle)
}

func (a *dbStoreAdapter) SoftDeleteSite(ctx context.Context, id uuid.UUID) error {
	return a.s.SoftDeleteSite(ctx, id)
}

func (a *dbStoreAdapter) SetPublished(ctx context.Context, id uuid.UUID, sha *string) error {
	return a.s.SetPublished(ctx, gen.SetPublishedParams{PublishedCommitSha: sha, ID: id})
}

func (a *dbStoreAdapter) SetSiteRemote(ctx context.Context, id uuid.UUID, repo *string) error {
	return a.s.SetSiteRemote(ctx, gen.SetSiteRemoteParams{GithubRepo: repo, ID: id})
}

// siteRecordFromGen converts a generated gen.Site row to the domain SiteRecord.
func siteRecordFromGen(r gen.Site) SiteRecord {
	return SiteRecord{
		ID:            r.ID,
		Handle:        r.Handle,
		OwnerID:       r.OwnerID,
		Visibility:    string(r.Visibility),
		DefaultBranch: r.DefaultBranch,
		PublishedSHA:  r.PublishedCommitSha,
		PublishedAt:   nullableTime(r.PublishedAt),
		PublishMode:   r.PublishMode,
		GitHubRepo:    r.GithubRepo,
		WebRoot:       r.WebRoot,
		Description:   r.Description,
		CacheVersion:  int64(r.CacheVersion),
		DeletedAt:     nullableTime(r.DeletedAt),
		CreatedAt:     r.CreatedAt.Time,
		UpdatedAt:     r.UpdatedAt.Time,
	}
}

// nullableTime maps a pgtype.Timestamptz to a *time.Time (nil when NULL).
func nullableTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
