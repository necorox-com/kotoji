package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// MetaStore is the narrow slice of the metadata Store the REST API needs beyond
// what site.Service owns: per-site authz (membership role), member/token/audit
// management, and user lookup for the member-by-email path. *db.Store satisfies
// it directly (it embeds *gen.Queries). Keeping it an interface (not *db.Store)
// keeps the handlers unit-testable against an in-memory fake.
//
// site.Service is NOT membership-authz-aware (CANONICAL §1); role enforcement is
// the REST layer's job and reads through GetRole here.
type MetaStore interface {
	// ---- authz hot path ----
	GetRole(ctx context.Context, arg gen.GetRoleParams) (gen.SiteRole, error)

	// ---- members ----
	ListMembers(ctx context.Context, siteID uuid.UUID) ([]gen.ListMembersRow, error)
	AddMember(ctx context.Context, arg gen.AddMemberParams) error
	UpdateMemberRole(ctx context.Context, arg gen.UpdateMemberRoleParams) error
	RemoveMember(ctx context.Context, arg gen.RemoveMemberParams) error
	GetMember(ctx context.Context, arg gen.GetMemberParams) (gen.SiteMember, error)

	// ---- tokens ----
	CreateToken(ctx context.Context, arg gen.CreateTokenParams) (gen.CreateTokenRow, error)
	ListTokensForSite(ctx context.Context, siteID uuid.UUID) ([]gen.ListTokensForSiteRow, error)
	RevokeToken(ctx context.Context, arg gen.RevokeTokenParams) error

	// ---- site settings (owner-only patch) ----
	UpdateSiteSettings(ctx context.Context, arg gen.UpdateSiteSettingsParams) error

	// ---- users (member-by-email + admin) ----
	GetUserByEmail(ctx context.Context, email string) (gen.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (gen.User, error)
	SetUserAdminFlags(ctx context.Context, arg gen.SetUserAdminFlagsParams) error

	// ---- audit (best-effort append) ----
	InsertAudit(ctx context.Context, arg gen.InsertAuditParams) error

	// ---- history feed (admin/site activity) ----
	ListAuditForSite(ctx context.Context, arg gen.ListAuditForSiteParams) ([]gen.AuditLog, error)
}

// csrfGuard is the CSRF double-submit middleware the API mounts on its mutating
// subtree. *auth.CSRF satisfies it via its Middleware method.
type csrfGuard interface {
	Middleware(next http.Handler) http.Handler
}

// AuthSurface is the slice of the assembled auth package the API mounts: the
// session middleware (loads user->ctx, non-fatal), the CSRF guard, and the
// auth/identity route registration.
//
// It is satisfied by a thin adapter over *auth.Auth (see AuthAdapter) rather
// than by *auth.Auth directly, because auth.Auth.CSRF() returns the concrete
// *auth.CSRF — the adapter widens it to csrfGuard so the router stays mockable
// in tests without an OIDC provider or session store.
type AuthSurface interface {
	// Middleware returns the SessionAuth middleware (anonymous when no session).
	Middleware() func(http.Handler) http.Handler
	// CSRF exposes the double-submit guard for the mutating /api subtree.
	CSRF() csrfGuard
	// RegisterRoutes mounts /auth/* + /api/me + /api/config on r.
	RegisterRoutes(r chi.Router)
}

// Deps is the dependency-injection bundle for the REST API. The Integration
// phase constructs it in the composition root and calls NewRouter. Everything is
// behind interfaces (or already-constructed handlers) so the package is fully
// unit-testable against fakes.
type Deps struct {
	// Config supplies upload caps, handle bounds, base domain, cookie security.
	Config config.Config
	// Site is the single git boundary; every content/lifecycle op delegates here.
	Site site.Service
	// Store is the metadata side: authz roles, members, tokens, audit, users.
	Store MetaStore
	// Auth is the assembled auth surface (session middleware + CSRF + routes).
	// The router mounts its middleware + /auth + /api/me + /api/config.
	Auth AuthSurface
	// Serve, when non-nil, is the data-plane handler mounted for same-binary
	// mode. Pure control-plane deployments leave it nil (data plane is separate).
	Serve http.Handler
	// MCP, when non-nil, is the MCP Streamable-HTTP handler mounted at
	// Config.MCPPath on the control plane.
	MCP http.Handler
	// PreviewGrant, when non-nil, signs preview grants for the /preview-grant
	// endpoint with the SHARED preview secret the data plane verifies against
	// (routing-and-serving.md §8.1.2). nil disables the endpoint (404) — set when
	// the data plane runs elsewhere and previews are off on this control plane.
	PreviewGrant PreviewSigner
	// Logger is used for best-effort audit/internal logging.
	Logger *slog.Logger
}
