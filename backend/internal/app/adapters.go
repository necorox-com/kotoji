package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/serve"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// compile-time guarantee the adapter exposes the OPTIONAL redirect seam so the
// data plane's tree provider auto-enables former-handle 301s (it also satisfies
// the unexported siteResolver seam, which is asserted at the call site in app.go).
var _ serve.RedirectResolver = (*redirectingResolver)(nil)

// redirectingResolver adapts the SiteService read side into the seam the data
// plane's tree provider consumes, AND adds the former-handle -> current-handle
// resolution the frozen site.Service interface deliberately omits (CANONICAL §1
// exposes GetSiteByHandle, which 404s old handles; redirect handling is the HTTP
// layer's job per §5.5). It composes:
//
//   - site.Service for GetSiteByHandle + ServedTree (the siteResolver seam), and
//   - *db.Store.GetSiteByRedirect for ResolveRedirect (the OPTIONAL RedirectResolver
//     seam serve.NewServiceTreeProvider type-asserts for).
//
// Because it implements ResolveRedirect, serve enables 301s on the data plane
// automatically; without this adapter old handles would 404.
type redirectingResolver struct {
	svc   site.Service
	store *db.Store
}

// newRedirectingResolver wires the site service + metadata store into the data
// plane's tree-provider dependency.
func newRedirectingResolver(svc site.Service, store *db.Store) *redirectingResolver {
	return &redirectingResolver{svc: svc, store: store}
}

// GetSiteByHandle resolves a CURRENT handle to its site (delegates to the service;
// old handles miss here and are then tried via ResolveRedirect).
func (r *redirectingResolver) GetSiteByHandle(ctx context.Context, h site.Handle) (site.Site, error) {
	return r.svc.GetSiteByHandle(ctx, h)
}

// ServedTree returns the materialized served tree for (siteID, branch).
func (r *redirectingResolver) ServedTree(ctx context.Context, id uuid.UUID, branch site.BranchName) (site.TreeHandle, error) {
	return r.svc.ServedTree(ctx, id, branch)
}

// ResolveRedirect maps a FORMER handle to its current canonical handle so the data
// plane can emit a 301 (CANONICAL §5.5). (newHandle, true) on a hit; ("", false)
// when oldHandle is not a registered redirect. Errors are infra failures.
func (r *redirectingResolver) ResolveRedirect(ctx context.Context, oldHandle string) (string, bool, error) {
	rec, err := r.store.GetSiteByRedirect(ctx, oldHandle)
	if err != nil {
		if db.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return rec.Handle, true, nil
}
