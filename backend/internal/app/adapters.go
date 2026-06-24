package app

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/api"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/domaincfg"
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

// compile-time guarantee the adapter satisfies the api seam the admin
// /api/admin/domain handlers depend on.
var _ api.DomainConfigProvider = domainAdapter{}

// domainAdapter maps the concrete *domaincfg.Provider onto api.DomainConfigProvider
// (the api package's own type), translating domaincfg.Resolved -> api.DomainResolved
// so the api package never imports domaincfg. The composition root wraps the shared
// provider with this when building the control router.
type domainAdapter struct{ p *domaincfg.Provider }

// wrapDomain adapts the effective-domain provider for the api Deps.
func wrapDomain(p *domaincfg.Provider) api.DomainConfigProvider { return domainAdapter{p: p} }

func (d domainAdapter) Resolve(ctx context.Context, r *http.Request) api.DomainResolved {
	res := d.p.Resolve(ctx, r)
	return api.DomainResolved{
		BaseDomain:     toAPIEffective(res.BaseDomain),
		ControlBaseURL: toAPIEffective(res.ControlBaseURL),
	}
}

func (d domainAdapter) EnvBaseDomainLocked() bool     { return d.p.EnvBaseDomainLocked() }
func (d domainAdapter) EnvControlBaseURLLocked() bool { return d.p.EnvControlBaseURLLocked() }
func (d domainAdapter) InvalidateCache()              { d.p.InvalidateCache() }

// toAPIEffective maps one resolved setting onto the api boundary type.
func toAPIEffective(e domaincfg.Effective) api.DomainEffective {
	return api.DomainEffective{Value: e.Value, Source: string(e.Source), Locked: e.Locked}
}
