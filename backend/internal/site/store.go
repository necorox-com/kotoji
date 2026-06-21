package site

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Store is the MINIMAL metadata-persistence surface gitService needs, expressed
// in site-domain terms (no sqlc/pgx types) so gitService is decoupled and the
// dependency is trivially mockable in tests (memStore in fake.go / tests). The
// production *db.Store satisfies this via the adapter in storeadapter.go.
//
// git remains the source of truth for content/history; this Store only ever
// touches the Postgres metadata (uuid<->handle, owner, redirects, published
// pointer). All methods take ctx first and return error last.
type Store interface {
	// CreateSiteWithOwner inserts the site row AND the owner site_members row in one
	// transaction (the metadata half of CreateSite). The handle is pre-validated +
	// collision-checked by the caller; the DB UNIQUE is the final guard. It returns
	// the persisted record (with the DB-generated UUID + timestamps).
	CreateSiteWithOwner(ctx context.Context, in StoreCreateSite) (SiteRecord, error)

	// GetSiteByID loads a site by immutable UUID. Soft-deleted rows ARE returned
	// (the reaper/admin needs them); the caller filters on DeletedAt. ErrNoRows-like
	// misses are reported as found=false.
	GetSiteByID(ctx context.Context, id uuid.UUID) (SiteRecord, bool, error)

	// GetSiteByHandle resolves a CURRENT, live handle. Soft-deleted and old
	// (redirected) handles miss here (found=false).
	GetSiteByHandle(ctx context.Context, handle string) (SiteRecord, bool, error)

	// GetSiteByRedirect resolves a FORMER handle to its current live site (the data
	// plane uses this to emit a 301). found=false when no redirect maps it.
	GetSiteByRedirect(ctx context.Context, oldHandle string) (SiteRecord, bool, error)

	// ListSitesForUser returns every live site the user can see (owned OR member),
	// newest-activity first.
	ListSitesForUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]SiteRecord, error)

	// HandleIsTaken reports whether a candidate handle is unavailable for any reason
	// (live site, redirect, or reserved word) in a single query.
	HandleIsTaken(ctx context.Context, handle string) (bool, error)

	// RenameHandleWithRedirect performs the atomic rename: record old->site in
	// handle_redirects, update the live handle, and drop any stale rename-back
	// redirect — all in one transaction (CANONICAL §5.5).
	RenameHandleWithRedirect(ctx context.Context, id uuid.UUID, oldHandle, newHandle string) error

	// SoftDeleteSite stamps deleted_at (decision #3). The on-disk repo retention is
	// the SiteService's job.
	SoftDeleteSite(ctx context.Context, id uuid.UUID) error

	// SetPublished updates the published_commit_sha cache pointer + published_at,
	// called AFTER the git ref move succeeds. A nil sha clears the pointer.
	SetPublished(ctx context.Context, id uuid.UUID, sha *string) error

	// SetSiteRemote configures (nil clears) the GitHub mirror remote "owner/name".
	SetSiteRemote(ctx context.Context, id uuid.UUID, repo *string) error
}

// StoreCreateSite is the metadata-create payload (defaults already applied by the
// caller). It mirrors the sites columns the create flow sets.
type StoreCreateSite struct {
	Handle      string
	OwnerID     uuid.UUID
	Visibility  string // "public"|"internal"|"private"
	PublishMode string // "direct"|"request"
	GitHubRepo  *string
	WebRoot     string
	Description string
}

// SiteRecord is the persisted metadata row in site-domain terms. Nullable DB
// columns map to Go zero-values / nil pointers so gitService never imports pgtype.
type SiteRecord struct {
	ID            uuid.UUID
	Handle        string
	OwnerID       uuid.UUID
	Visibility    string
	DefaultBranch string
	PublishedSHA  *string
	PublishedAt   *time.Time
	PublishMode   string
	GitHubRepo    *string
	WebRoot       string
	Description   string
	DeletedAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// toSite converts a persisted record into the public Site domain struct, deriving
// HasPublished/PublishedSHA/GitHubRepo from the nullable columns.
func (r SiteRecord) toSite() Site {
	s := Site{
		ID:            r.ID,
		Handle:        Handle(r.Handle),
		OwnerID:       r.OwnerID,
		Visibility:    r.Visibility,
		DefaultBranch: BranchName(r.DefaultBranch),
		PublishMode:   r.PublishMode,
		WebRoot:       r.WebRoot,
		Description:   r.Description,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
	if r.GitHubRepo != nil {
		s.GitHubRepo = *r.GitHubRepo
	}
	if r.PublishedSHA != nil && *r.PublishedSHA != "" {
		s.HasPublished = true
		s.PublishedSHA = *r.PublishedSHA
	}
	if r.PublishedAt != nil {
		t := *r.PublishedAt
		s.PublishedAt = &t
	}
	return s
}
