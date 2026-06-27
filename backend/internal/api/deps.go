package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
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

	// ---- tokens (per-USER; a token spans all the user's memberships) ----
	CreateUserToken(ctx context.Context, arg gen.CreateUserTokenParams) (gen.CreateUserTokenRow, error)
	ListUserTokens(ctx context.Context, userID uuid.UUID) ([]gen.ListUserTokensRow, error)
	RevokeUserToken(ctx context.Context, arg gen.RevokeUserTokenParams) error

	// ---- site settings (owner-only patch) ----
	UpdateSiteSettings(ctx context.Context, arg gen.UpdateSiteSettingsParams) error

	// BumpCacheVersion increments the per-site cache generation and returns the NEW
	// value. Backs the operator "Clear cache" action: the data plane folds
	// cache_version into the asset ETag, so a bump forces all clients to refetch
	// fresh on their next revalidation (no new commit required).
	BumpCacheVersion(ctx context.Context, id uuid.UUID) (int32, error)

	// ---- users (member-by-email + admin) ----
	GetUserByEmail(ctx context.Context, email string) (gen.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (gen.User, error)
	SetUserAdminFlags(ctx context.Context, arg gen.SetUserAdminFlagsParams) error

	// ---- instance GitHub mirror config (admin screen) ----
	// GetGitHubConfig reads the DB-stored mirror config (token decrypted, but the
	// admin handler NEVER echoes it — only a "configured" boolean). SetGitHubConfig
	// persists a partial update (token write-only). *db.Store satisfies both.
	GetGitHubConfig(ctx context.Context) (db.GitHubConfig, error)
	SetGitHubConfig(ctx context.Context, in db.SetGitHubConfigInput) error

	// ---- instance domain/URL config (admin screen) ----
	// SetDomainConfig persists a partial update of the runtime base domain /
	// control base URL (plain strings, not secret; empty value deletes the key,
	// reverting to env/derived). The GET path reads the EFFECTIVE value via the
	// Domain provider, not here. *db.Store satisfies it.
	SetDomainConfig(ctx context.Context, in db.SetDomainConfigInput) error

	// ---- instance OIDC config (admin screen) ----
	// SetOIDCConfig persists a partial update of the runtime OIDC config (client
	// secret encrypted + write-only; plain fields revert to env/derived on an empty
	// value). The GET path reads the EFFECTIVE value via the OIDC provider, not here.
	// *db.Store satisfies it.
	SetOIDCConfig(ctx context.Context, in db.SetOIDCConfigInput) error

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
	// Domain resolves the EFFECTIVE base domain + control base URL (env > DB >
	// derived) and exposes the env-locked flags + cache invalidation for the admin
	// /api/admin/domain surface. nil only in tests that do not exercise that route.
	Domain DomainConfigProvider
	// OIDC resolves the EFFECTIVE OIDC config (env > DB > derived) and owns the
	// rebuildable provider for the admin /api/admin/oidc surface. nil only in tests
	// that do not exercise that route.
	OIDC OIDCConfigProvider
	// Logger is used for best-effort audit/internal logging.
	Logger *slog.Logger
}

// DomainConfigProvider is the slice of the effective-domain provider the admin
// /api/admin/domain handlers need: read the effective base domain + control base
// URL with their sources, ask whether each field is env-locked, and invalidate
// the cache after a successful PUT. *domaincfg.Provider satisfies it; tests stub
// it. Returning the per-field source/locked lets the GET expose them to the GUI.
type DomainConfigProvider interface {
	// Resolve returns the effective base domain + control base URL for r (each with
	// its value, source "env"|"db"|"derived", and locked flag).
	Resolve(ctx context.Context, r *http.Request) DomainResolved
	// EnvBaseDomainLocked / EnvControlBaseURLLocked report whether the respective
	// field is pinned by the environment (env-set => read-only / reject writes).
	EnvBaseDomainLocked() bool
	EnvControlBaseURLLocked() bool
	// InvalidateCache drops the cached DB read so the next Resolve re-reads it
	// (called by the admin PUT on a successful persist).
	InvalidateCache()
}

// DomainResolved mirrors domaincfg.Resolved at the api boundary so the package
// does not import domaincfg directly (keeps the handler unit-testable against a
// stub). The adapter in the composition root maps the provider's type onto this.
type DomainResolved struct {
	BaseDomain     DomainEffective
	ControlBaseURL DomainEffective
}

// DomainEffective is one resolved setting: value + source + env-locked flag.
type DomainEffective struct {
	Value  string
	Source string // "env" | "db" | "derived"
	Locked bool
}

// OIDCConfigProvider is the slice of the effective-OIDC provider the admin
// /api/admin/oidc handlers need: read the effective config (env > DB > derived) with
// per-field sources/locked flags + the effective provider list, ask whether the
// auth-mode set is env-pinned, optionally pre-flight discovery at save time, and
// invalidate the caches after a successful PUT (so a runtime change applies without
// a restart). *oidccfg.Provider satisfies it via an adapter; tests stub it.
type OIDCConfigProvider interface {
	// Resolve returns the effective OIDC config for r (each field with its value,
	// source "env"|"db"|"derived", and locked flag). The client secret value is
	// carried server-side but the handler NEVER echoes it — only clientSecretSet.
	Resolve(ctx context.Context, r *http.Request) OIDCResolved
	// Providers returns the effective enabled auth-provider set for r (the admin GET
	// surfaces it so the GUI shows what login the change will produce).
	Providers(ctx context.Context, r *http.Request) []string
	// AuthModeEnvLocked reports whether KOTOJI_AUTH_MODE pins the provider set (so
	// the enabled toggle is read-only).
	AuthModeEnvLocked() bool
	// ValidateDiscovery pre-flights OIDC discovery for the effective config of r
	// (used at save time to 422 a bad issuer). A nil error means the issuer resolved.
	ValidateDiscovery(ctx context.Context, r *http.Request) error
	// InvalidateCache / InvalidateProvider drop the cached DB read / built provider
	// so the next request reflects the new config (called by the admin PUT).
	InvalidateCache()
	InvalidateProvider()
}

// OIDCResolved mirrors oidccfg.EffectiveOIDC at the api boundary so the package does
// not import oidccfg directly (keeps the handler unit-testable against a stub). The
// adapter in the composition root maps the provider's type onto this.
type OIDCResolved struct {
	Enabled         OIDCEffectiveBool
	Issuer          OIDCEffectiveString
	ClientID        OIDCEffectiveString
	ClientSecretSet bool   // whether a secret is configured (env or DB); never the value
	ClientSecretSrc string // "env" | "db" | "derived"
	ClientSecretLck bool   // env-locked
	RedirectURL     OIDCEffectiveString
	AllowedEmails   OIDCEffectiveList
	AllowedDomains  OIDCEffectiveList
	AdminEmails     OIDCEffectiveList
}

// OIDCEffectiveString / OIDCEffectiveList / OIDCEffectiveBool are one resolved
// setting at the api boundary: value + source + env-locked flag.
type OIDCEffectiveString struct {
	Value  string
	Source string
	Locked bool
}

type OIDCEffectiveList struct {
	Value  []string
	Source string
	Locked bool
}

type OIDCEffectiveBool struct {
	Value  bool
	Source string
	Locked bool
}
