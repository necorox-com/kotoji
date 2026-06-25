// Package app is the composition root. It wires configuration, logging, the
// metadata store, the git-backed SiteService, the auth surface, the MCP server,
// the REST control plane, and the static data plane into the http.Handlers that
// cmd/kotojid serves per RUN_MODE.
//
// Every subsystem is constructed here from its package's exported constructor and
// joined behind small wiring adapters (this file + adapters.go) so the phase
// packages stay decoupled and individually testable. The composition root is the
// only place that knows about ALL of them at once.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	"github.com/necorox-com/kotoji/backend/internal/api"
	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/domaincfg"
	"github.com/necorox-com/kotoji/backend/internal/mcpserver"
	"github.com/necorox-com/kotoji/backend/internal/migrate"
	"github.com/necorox-com/kotoji/backend/internal/observability"
	"github.com/necorox-com/kotoji/backend/internal/oidccfg"
	"github.com/necorox-com/kotoji/backend/internal/ops"
	"github.com/necorox-com/kotoji/backend/internal/preview"
	"github.com/necorox-com/kotoji/backend/internal/ratelimit"
	"github.com/necorox-com/kotoji/backend/internal/resolve"
	"github.com/necorox-com/kotoji/backend/internal/secretbox"
	"github.com/necorox-com/kotoji/backend/internal/serve"
	"github.com/necorox-com/kotoji/backend/internal/site"
	"github.com/necorox-com/kotoji/backend/internal/tlsedge"
	"github.com/necorox-com/kotoji/backend/internal/webhook"
)

// sitesSubdir is the dir under DataDir that holds per-site git repos
// (CANONICAL §1: /data/sites/{uuid}). gitService.cfg.Root points here.
const sitesSubdir = "sites"

// App holds the wired dependencies shared across planes. It owns the resources
// that need lifecycle (the DB pool) so Close can release them on shutdown.
type App struct {
	cfg    config.Config
	logger *slog.Logger

	// store is the metadata pool-backed handle (nil only if no plane needs it,
	// which never happens for the supported run modes). It is also the readiness
	// checker for /readyz.
	store *db.Store
	// ready is the readiness checker shared by both planes' /readyz. It is the
	// *db.Store (DB ping) once the store is open; nil => always ready.
	ready observability.ReadinessChecker

	// auth is the assembled control-plane auth surface (sessions + CSRF + routes).
	auth *auth.Auth
	// siteSvc is the single git boundary every content op delegates to.
	siteSvc site.Service

	// domain resolves the EFFECTIVE base domain + control base URL with the
	// WordPress-style precedence (env OVERRIDES DB, DB over derived). When both
	// envs are set it is a pure static fast path (no per-request DB read); shared
	// by the data-plane resolver, /api/config, and the admin /api/admin/domain API.
	domain *domaincfg.Provider

	// oidc resolves the EFFECTIVE OIDC config (env > DB > derived) and owns the
	// RUNTIME-rebuildable OIDC provider (lazy discovery, cache keyed by the effective
	// issuer/client/secret/redirect, rebuilt on admin change). Shared by the auth
	// runtime seam, /api/config.authProviders, and the admin /api/admin/oidc API.
	// nil on the data-plane-only path (no control plane).
	oidc *oidccfg.Provider

	// opsScheduler runs the background operability jobs (reaper, gc, startup
	// consistency check). It is started ONLY when the control plane runs (single
	// replica owns the destructive reaper, decision #4); nil otherwise.
	opsScheduler *ops.Scheduler
}

// New constructs the composition root: it opens the metadata store, builds the
// SiteService, selects + builds the auth provider, and assembles the auth surface.
// It performs network I/O (DB ping, and OIDC discovery when AUTH_MODE=oidc), so it
// takes a context. The caller owns Close.
//
// Run modes:
//   - control|all: needs the store, SiteService, and the full auth surface.
//   - serve:       needs the store + SiteService (read side) only; auth is skipped.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*App, error) {
	a := &App{cfg: cfg, logger: logger}

	// The metadata store backs every plane (sessions/metadata on control, the
	// uuid<->handle resolution + served-tree lookups on serve). config.validate
	// guarantees a DSN exists whenever a DB-backed plane runs.
	store, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	a.store = store
	a.ready = store // /readyz pings the DB

	// At-rest secret box: encrypts the DB-stored GitHub PAT. The key is resolved
	// from KOTOJI_SECRET_KEY (validated hex/base64 >=32 bytes) else derived via
	// sha256 over the same stable server seed the preview-grant key uses. A short/
	// undecodable env key silently falls back to the derived key (secretbox owns
	// that policy). Construction only fails on a non-32-byte resolved key, which the
	// resolver guarantees — so this is defensive, surfaced at boot rather than later.
	box, berr := secretbox.New(secretKey(cfg))
	if berr != nil {
		store.Close()
		return nil, berr
	}
	store.SetSecretBox(box)

	// Effective domain/URL provider (WordPress-style env > DB > derived). The store
	// is consulted ONLY for fields whose env var is unset; when both KOTOJI_BASE_DOMAIN
	// and KOTOJI_CONTROL_BASE_URL are set this never reads the DB (the live deployment
	// stays on the static fast path). Shared by the resolver, /api/config, and the
	// admin domain API; the admin PUT invalidates its cache.
	a.domain = domaincfg.New(domaincfg.Config{
		Store:             store,
		EnvBaseDomain:     cfg.BaseDomain,
		EnvBaseDomainSet:  cfg.BaseDomainEnvSet,
		EnvControlBaseURL: cfg.ControlBaseURL,
		EnvControlSet:     cfg.ControlBaseURLEnvSet,
	})

	// Effective OIDC provider (WordPress-style env > DB > derived) + the runtime,
	// rebuildable OIDC provider. The redirect derives from the effective control base
	// URL (a.domain); the builder (auth.NewOIDCBuilder) performs go-oidc discovery
	// lazily on first OIDC use and on each admin config change. It is wired on the
	// auth surface ONLY when KOTOJI_AUTH_MODE is NOT env-pinned (see below): an
	// env-pinned deployment keeps today's EAGER startup discovery + static provider.
	a.oidc = oidccfg.New(oidccfg.Config{
		Store:        store,
		Domain:       a.domain,
		Builder:      auth.NewOIDCBuilder(),
		Env:          cfg.OIDC,
		EnvSet:       cfg.OIDCEnvSet,
		EnvAuthModes: cfg.AuthModes,
	})

	// Provision the schema on boot so a fresh `docker compose up` needs zero manual
	// migration steps. Advisory-locked (safe across rolling restarts); disable with
	// KOTOJI_AUTO_MIGRATE=false to manage schema out of band.
	if cfg.AutoMigrate {
		if err := migrate.Run(ctx, cfg.DatabaseURL, logger); err != nil {
			store.Close()
			return nil, err
		}
	}

	// The production SiteService: dbStoreAdapter + os/exec git runner, rooted at
	// /data/sites. Built via the package's exported composition-root factory so
	// the runner/adapter stay internal to the site package. siteConfig closes over
	// the store so the mirror credential/enabled flag are resolved at git-call time
	// (DB overrides env), letting a runtime admin change apply without a restart.
	a.siteSvc = site.NewProductionService(store, siteConfig(cfg, store))

	// Auth is only needed where the control plane runs.
	if cfg.ServesControl() {
		// FAIL-CLOSED warning (decision #2). config.validate already refuses to boot
		// when oidc is enabled with neither an email allowlist NOR a domain allowlist,
		// so this is belt-and-suspenders: if a future code path (or a hand-built
		// Config) ever reaches here with both lists empty, log a loud warning that
		// EVERY OIDC sign-in will be denied at the provider gate (TestOIDC_Allowlist-
		// NoneConfigured pins the runtime denial). The break-glass password admin is
		// unaffected.
		if cfg.OIDCEnabled() && len(cfg.OIDC.AllowedEmails) == 0 && len(cfg.OIDC.AllowedDomains) == 0 {
			logger.Warn("oidc is enabled with NO allowed emails and NO allowed domains: " +
				"ALL OIDC sign-ins will be DENIED (fail-closed). Set KOTOJI_OIDC_ALLOWED_EMAILS " +
				"and/or KOTOJI_OIDC_ALLOWED_DOMAINS to permit accounts.")
		}

		// Build the SET of enabled providers (decision #1: oidc + password may be
		// enabled concurrently — break-glass). The store backs the password
		// provider's first-run DB credential (the hash set via /auth/setup takes
		// precedence over the env password); it is unused by the OIDC/dev providers.
		providers, perr := auth.ProvidersFor(ctx, cfg, store) // OIDC does network discovery
		if perr != nil {
			store.Close()
			return nil, perr
		}
		// *db.Store satisfies auth.StoreDeps (SessionStore + WithTx).
		a.auth = auth.NewWithProviders(cfg, store, providers...)
		// Surface the EFFECTIVE base domain + control base URL on /api/config (env >
		// DB > derived). On the env-set fast path the provider returns the static
		// values without a DB read; the dynamic path reflects the admin's DB setting.
		a.auth.SetDomainResolver(a.domain)

		// Wire the RUNTIME OIDC seam UNLESS KOTOJI_AUTH_MODE is env-pinned. When it is
		// pinned, oidc (if enabled) was already built EAGERLY at startup by ProvidersFor
		// (today's static fast path: discovery at boot, a static a.interactive provider)
		// — leaving the runtime seam off keeps that path byte-for-byte equivalent. When
		// AUTH_MODE is unset, the enabled set is { password always + oidc iff the admin
		// turned it on in the DB }, so the runtime seam owns the lazy/rebuildable OIDC
		// provider + the effective /api/config.authProviders.
		if !cfg.OIDCEnvSet.AuthMode {
			a.auth.SetOIDCRuntime(auth.NewOIDCRuntimeAdapter(a.oidc))
		}

		// Background operability scheduler (reaper + gc + startup consistency).
		// Built only on the control plane: the single replica that owns writes also
		// owns the destructive reaper (decision #4), so two data-plane replicas never
		// race a bundle+remove. It is started by StartBackground after boot.
		a.opsScheduler = ops.NewScheduler(a.buildOps(), logger)
	}

	return a, nil
}

// buildOps assembles the ops.Ops from config + the store + a production git runner
// and filesystem. The reaper + gc are enabled by default; grace + dirs are config-
// driven (guarded: the reaper only acts on PAST-GRACE soft-deleted rows).
func (a *App) buildOps() *ops.Ops {
	return ops.New(ops.Config{
		SitesDir:     filepath.Join(a.cfg.DataDir, sitesSubdir),
		BackupDir:    a.cfg.BackupDir,
		Grace:        a.cfg.SoftDeleteGrace,
		Interval:     a.cfg.OpsInterval,
		EnableReaper: true,
		EnableGC:     true,
	}, newOpsStoreAdapter(a.store), ops.NewExecGitRunner(a.cfg.GitBin), ops.OSFS{}, a.logger)
}

// StartBackground launches the background ops scheduler (no-op when not on the
// control plane). The entry point calls it after the HTTP servers start; the
// passed context is the server lifetime, so a shutdown signal drains the loop.
func (a *App) StartBackground(ctx context.Context) {
	if a.opsScheduler != nil {
		a.opsScheduler.Start(ctx)
	}
}

// siteConfig maps the env-derived config.Config onto the site package's Config
// (its own bounded value object). Root is DataDir/sites per CANONICAL §1. The
// store backs the DYNAMIC mirror credential/enabled providers so DB settings
// override env at git-call time (a nil store falls back to the static env fields,
// keeping the function usable in tests / env-only paths).
func siteConfig(cfg config.Config, store *db.Store) site.Config {
	sc := site.Config{
		Root:     filepath.Join(cfg.DataDir, sitesSubdir),
		GitBin:   cfg.GitBin,
		MirrorOn: cfg.GitHub.Enabled,
		// The instance-level mirror token (KOTOJI_GITHUB_APP_TOKEN/PAT) authenticates
		// push/fetch against github.com. It is injected per git call via an HTTP
		// header in the environment, never written to .git/config (see git_auth.go).
		// Static fields are the env fallback the dynamic providers below default to.
		GitHubToken: cfg.GitHub.Token,
		Zip: site.ZipConfig{
			MaxUploadBytes:       cfg.Zip.MaxUploadBytes,
			MaxUncompressedBytes: cfg.Zip.MaxTotalBytes,
			MaxEntries:           cfg.Zip.MaxFiles,
			// Per-entry uncompressed cap, now operator-configurable (M7,
			// KOTOJI_ZIP_MAX_ENTRY_BYTES). The site layer fills its 50MiB default when
			// this is zero, so an env-only/test path stays on the historical cap.
			MaxEntryUncompressed: cfg.Zip.MaxEntryBytes,
			MaxCompressionRatio:  cfg.Zip.MaxRatio,
			AllowedExt:           cfg.Zip.AllowedExt,
		},
		// Per-site disk quota: cumulative repo size is gated before each write so a
		// tenant cannot grow unbounded via repeated near-limit imports.
		SiteQuotaBytes: cfg.SiteQuotaBytes,
	}
	if store != nil {
		// MirrorToken: DB token (decrypted) when set, else the env token. Resolved per
		// git call so a runtime PAT change applies without a restart. A DB read error
		// degrades to the env token (fail safe). User defaults to x-access-token (the
		// site layer fills the default when empty).
		sc.MirrorToken = func(ctx context.Context) (token, user string) {
			if gh, err := store.GetGitHubConfig(ctx); err == nil && gh.TokenSet {
				return gh.Token, ""
			}
			return cfg.GitHub.Token, ""
		}
		// MirrorEnabled: DB flag overrides env; mirroring is only "on" when enabled
		// AND a token is present (so a half-configured instance does not attempt
		// doomed unauthenticated pushes). Mirrors the /api/config effective logic.
		sc.MirrorEnabled = func(ctx context.Context) bool {
			enabled := cfg.GitHub.Enabled
			tokenPresent := cfg.GitHub.Token != ""
			if gh, err := store.GetGitHubConfig(ctx); err == nil {
				if gh.EnabledSet {
					enabled = gh.Enabled
				}
				if gh.TokenSet {
					tokenPresent = true
				}
			}
			return enabled && tokenPresent
		}
	}
	return sc
}

// secretKey resolves the 32-byte at-rest encryption key. KOTOJI_SECRET_KEY wins
// when it decodes to >=32 bytes; otherwise it is derived (sha256) over the same
// stable server seed the preview-grant key binds to (admin password | oidc secret
// | control base url | base domain), so an env-only instance keeps a stable key
// across restarts (sealed tokens stay decryptable).
func secretKey(cfg config.Config) []byte {
	return secretbox.ResolveKey(cfg.SecretKey, cfg.AdminPassword, cfg.OIDC.ClientSecret, cfg.ControlBaseURL, cfg.BaseDomain)
}

// ControlRouter builds the control-plane handler (REST API + /auth + /mcp) plus
// the health probes. The api package owns the full middleware chain (request-id,
// slog, recover, CORS, session-auth, CSRF) and the route tree, so this wraps it
// with the liveness/readiness probes that monitoring needs on every plane.
//
// Returns nil when the run mode does not serve the control plane.
func (a *App) ControlRouter() http.Handler {
	if !a.cfg.ServesControl() {
		return nil
	}

	// MCP server (control plane only). Disabled => not mounted. The one *db.Store
	// satisfies BOTH the token verifier surface and the membership-authz surface
	// (per-site role, the user's membership list, and the create-site user flag).
	var mcpHandler http.Handler
	if a.cfg.MCPEnabled {
		mcpHandler = mcpserver.New(mcpserver.FromConfig(a.cfg, a.siteSvc, a.store, a.store, a.logger))
	}

	apiHandler := api.NewRouter(api.Deps{
		Config:       a.cfg,
		Site:         a.siteSvc,
		Store:        a.store, // *db.Store satisfies api.MetaStore
		Auth:         api.WrapAuth(a.auth),
		Serve:        nil, // data plane runs on its own addr (run mode all|serve)
		MCP:          mcpHandler,
		PreviewGrant: a.previewSigner(), // shares ONE codec with the data-plane verifier
		Domain:       wrapDomain(a.domain),
		OIDC:         wrapOIDC(a.oidc),
		Logger:       a.logger,
	})

	// Health probes wrap the API handler. They are registered first so /healthz
	// and /readyz never fall through to the API's CORS/auth chain.
	r := chi.NewRouter()
	a.mountHealth(r)

	// GitHub webhook receiver (architecture.md §3f). Mounted OUTSIDE the API's
	// session-auth/CSRF chain — it authenticates by HMAC signature, not a cookie,
	// so it must not be CSRF-guarded. Only mounted when the mirror is enabled (the
	// secret is required then; config.validate enforces it).
	if a.cfg.GitHub.Enabled {
		wh := webhook.New(webhook.Deps{
			Secret:     a.cfg.GitHub.WebhookSecret,
			Site:       a.siteSvc,
			Store:      a.store, // *db.Store satisfies webhook.Store
			IsNotFound: db.IsNotFound,
			Logger:     a.logger,
		})
		r.Post("/api/webhooks/github", wh.ServeHTTP)
	}

	// Per-session/IP rate limiting on the control API (architecture.md §8.4.5).
	// Applied as an outer wrapper so it gates EVERY control request (auth + writes
	// + uploads) before the API's heavier middleware runs.
	if lim := a.controlLimiter(); lim != nil {
		r.Mount("/", lim(apiHandler))
	} else {
		r.Mount("/", apiHandler)
	}
	return r
}

// previewSigner returns the shared-secret preview-grant signer for the control
// plane's /preview-grant endpoint, or nil if construction fails (then the endpoint
// 404s — fail-closed). It is the SAME serve.GrantAuthz type the data plane uses to
// verify, built from the SAME preview.Secret, so grants never drift in format.
func (a *App) previewSigner() api.PreviewSigner {
	gz, err := serve.NewGrantAuthz(serve.GrantAuthzConfig{
		Secret:       previewSecret(a.cfg),
		CookieSecure: a.cfg.CookieSecure,
	})
	if err != nil {
		a.logger.Error("preview signer init failed; preview-grant disabled", slog.Any("error", err))
		return nil
	}
	return gz
}

// controlLimiter builds the per-session/IP rate-limit middleware for the control
// plane, or nil when limiting is disabled (RPS <= 0). The key is the session
// cookie when present (so a logged-in user's quota follows them across IPs), else
// the client IP (anonymous/login traffic). Denied requests get the API's JSON
// 429 envelope.
func (a *App) controlLimiter() func(http.Handler) http.Handler {
	if a.cfg.RateLimitAPIRPS <= 0 {
		return nil
	}
	lim := ratelimit.New(ratelimit.Config{RPS: float64(a.cfg.RateLimitAPIRPS)})
	cookieName := a.cfg.SessionCookieName
	trust := a.cfg.TrustProxyHeaders
	keyer := func(r *http.Request) string {
		if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
			return "sess:" + c.Value
		}
		return "ip:" + ratelimit.ClientIP(r, trust)
	}
	return ratelimit.Middleware(lim, keyer, api.WriteRateLimited)
}

// newDataResolver builds the data-plane Host resolver shared by the pure-serve
// router, the combined (auto-TLS) router, and the on-demand TLS DecisionFunc. It
// is a single source of truth for the routing classification so the cert gate can
// never disagree with what the data plane will actually serve.
func (a *App) newDataResolver() resolve.Resolver {
	return resolve.NewResolver(resolve.Config{
		BaseDomain:   a.cfg.BaseDomain,
		ControlLabel: "", // bare BaseDomain is the control host
		// EnablePathFallback is a DEPRECATED no-op: serving is subdomain-only (M1).
		// The /host/{handle}/... path fallback was removed so untrusted project
		// content can never be served same-origin with the control plane. The field
		// is left set for documentation; the resolver ignores it.
		EnablePathFallback: false,
		// X-Forwarded-Host is trusted only behind the documented reverse proxy.
		TrustForwardedHost: a.cfg.TrustProxyHeaders,
		// Dynamic base domain: ONLY when KOTOJI_BASE_DOMAIN is unset. When set, leave
		// this nil so classifyHost uses the precomputed static suffix with no per-
		// request DB read (today's behavior on the live instance). When unset, the
		// resolver consults the effective value (DB > derived) each request.
		BaseDomainFunc: a.dynamicBaseDomainFunc(),
	})
}

// dataHandler builds the serve.Handler (Host-resolving static hosting). control is
// the optional same-binary control handler: nil for the pure data-plane router
// (control on its own addr) and the control router for the combined auto-TLS path,
// where a single listener fronts BOTH planes. res is injected so callers share one
// resolver instance.
func (a *App) dataHandler(res resolve.Resolver, control http.Handler) *serve.Handler {
	// The tree provider reads materialized served worktrees (no git on the request
	// path). The site service does not expose ResolveRedirect on its frozen
	// interface, so wrap it with a store-backed redirect resolver to enable
	// former-handle 301s on the data plane (CANONICAL §5.5).
	tp := serve.NewServiceTreeProvider(newRedirectingResolver(a.siteSvc, a.store), nil)

	// Preview authz: the signed-grant -> host-only-cookie flow shares ONE codec
	// with the control plane's preview-grant endpoint (same secret), so issued
	// cookies/grants never drift in format.
	az, err := serve.NewGrantAuthz(serve.GrantAuthzConfig{
		Secret:       previewSecret(a.cfg),
		CookieSecure: a.cfg.CookieSecure,
	})
	if err != nil {
		// Secret is derived non-empty below, so this cannot fail in practice; fall
		// back to fail-closed (DenyPreviewAuthz via nil) rather than panic.
		a.logger.Error("preview authz init failed; previews disabled", slog.Any("error", err))
		az = nil
	}

	return serve.NewHandler(serve.Deps{
		Resolver: res,
		Trees:    tp,
		Authz:    az,
		Control:  control, // nil => pure data-plane; non-nil => same-binary combined
		Config: serve.HandlerConfig{
			Security: serve.DefaultSecurityHeaderConfig(),
			Cache:    serve.CacheConfig{}, // zero value: ETag ON + base-href injection ON
		},
	})
}

// wrapDataRouter wraps a serve.Handler with the common middleware chain, per-IP
// rate limiting, and the health probes — the data-plane router envelope shared by
// ServeRouter and CombinedRouter.
func (a *App) wrapDataRouter(h *serve.Handler) http.Handler {
	r := chi.NewRouter()
	a.commonMiddleware(r)
	// Per-IP rate limiting on the data plane (architecture.md §8.4.5): a single
	// visitor cannot flood served content. Applied after the common chain so health
	// probes below stay limited too (they share the same IP budget — acceptable).
	if lim := a.serveLimiter(); lim != nil {
		r.Use(lim)
	}
	a.mountHealth(r)
	r.NotFound(h.ServeHTTP)
	r.MethodNotAllowed(h.ServeHTTP)
	return r
}

// ServeRouter builds the data-plane handler (Host-resolving static hosting) plus
// the health probes. The serve package owns resolution, preview authz, and the
// security/cache header set. Returns nil when the run mode does not serve data.
func (a *App) ServeRouter() http.Handler {
	if !a.cfg.ServesData() {
		return nil
	}
	// Pure data-plane: control runs on its own addr, so no same-binary control.
	return a.wrapDataRouter(a.dataHandler(a.newDataResolver(), nil))
}

// CombinedRouter builds the SINGLE Host-routing handler that fronts BOTH planes in
// one listener — the handler kotoji-native auto-TLS serves on :443. It is the
// serve.Handler with its same-binary Control hook wired to the control router, so
// the resolver dispatches the control host to the API/auth/MCP plane and every
// project host to the data plane. Returns nil unless the run mode serves both
// planes (RUN_MODE=all); auto-TLS is gated on that mode too.
func (a *App) CombinedRouter() http.Handler {
	if !a.cfg.ServesData() || !a.cfg.ServesControl() {
		return nil
	}
	return a.wrapDataRouter(a.dataHandler(a.newDataResolver(), a.ControlRouter()))
}

// NewTLSEngine builds the kotoji-native on-demand TLS engine for the combined
// handler, or returns (nil, nil) when auto-TLS is not enabled (KOTOJI_TLS_MODE!=
// auto or run mode != all) so the caller starts NOTHING new. The DecisionFunc
// gates issuance to the effective control host OR an EXISTING hosted site/preview,
// reusing the live resolver + a store-backed existence lookup (fail-closed on a DB
// error). ca is injected so a test can target a local pebble CA; the production
// caller passes the zero value (the config-selected CA is applied below) — see the
// (ca CertAuthority) form via NewTLSEngineWithCA for tests.
func (a *App) NewTLSEngine() (*tlsedge.Engine, error) {
	return a.NewTLSEngineWithCA(a.prodCA())
}

// NewTLSEngineWithCA is NewTLSEngine with the ACME CA injected (the constructor
// seam the integration test uses to point CertMagic at pebble). Returns (nil, nil)
// when auto-TLS is disabled.
func (a *App) NewTLSEngineWithCA(ca tlsedge.CA) (*tlsedge.Engine, error) {
	if !a.cfg.ServesTLS() {
		return nil, nil
	}
	combined := a.CombinedRouter()
	if combined == nil {
		// ServesTLS already implies RUN_MODE=all (config.validate), so this is
		// defensive; refuse rather than start a listener with no handler.
		return nil, errTLSNoCombinedHandler
	}
	decider, err := tlsedge.NewDecider(a.effectiveControlHost, a.newDataResolver(), a.siteExists)
	if err != nil {
		return nil, err
	}
	return tlsedge.New(tlsedge.Config{
		Handler:    combined,
		Decider:    decider,
		StorageDir: a.cfg.CertMagicStorageDir,
		CA:         ca,
		Email:      a.cfg.ACMEEmail,
		TLSAddr:    a.cfg.TLSAddr,
		HTTPAddr:   a.cfg.TLSHTTPAddr,
		Logger:     a.logger,
	})
}

// errTLSNoCombinedHandler is returned when auto-TLS is enabled but the combined
// handler could not be built (would only happen on a run-mode that config.validate
// already rejects, so it is defensive).
var errTLSNoCombinedHandler = errors.New("app: auto-TLS enabled but combined handler unavailable (requires RUN_MODE=all)")

// prodCA maps KOTOJI_TLS_CA onto the Let's Encrypt production/staging directory.
// The staging directory is selected for safe testing (KOTOJI_TLS_CA=staging).
func (a *App) prodCA() tlsedge.CA {
	return tlsedge.LetsEncryptCA(a.cfg.TLSStaging())
}

// effectiveControlHost returns the EFFECTIVE control host (bare hostname, no port)
// for the on-demand TLS gate. It resolves via the domaincfg provider with no
// request (env-locked fast path returns the static host; the dynamic path returns
// the cached DB/derived value). Empty only on a fresh, wholly-unconfigured install.
func (a *App) effectiveControlHost() string {
	if a.domain == nil {
		return a.cfg.ControlHost
	}
	return a.domain.Resolve(context.Background(), nil).ControlHost
}

// siteExists reports whether handle maps to a servable site on THIS instance — a
// CURRENT live site OR a FORMER handle that 301-redirects to one. It is the
// existence half of the on-demand TLS DecisionFunc; an error is propagated so the
// gate fails CLOSED (refuses issuance) on a DB blip. Both lookups are indexed, so
// it is bounded for the handshake hot path.
func (a *App) siteExists(ctx context.Context, handle string) (bool, error) {
	if a.store == nil {
		return false, errors.New("app: no metadata store for site-exists lookup")
	}
	// Current live handle (the common case).
	if _, err := a.store.GetSiteByHandle(ctx, handle); err == nil {
		return true, nil
	} else if !db.IsNotFound(err) {
		return false, err // real error => fail closed upstream
	}
	// Former handle still served via a 301 redirect: issue a cert so the old host
	// can redirect over HTTPS.
	if _, err := a.store.GetSiteByRedirect(ctx, handle); err == nil {
		return true, nil
	} else if !db.IsNotFound(err) {
		return false, err
	}
	return false, nil
}

// dynamicBaseDomainFunc returns the per-request effective-base-domain resolver for
// the data-plane resolver, or NIL when KOTOJI_BASE_DOMAIN is set (the static fast
// path — the resolver then uses its precomputed suffix with zero DB reads). When
// the env is unset, the returned func consults the effective provider (DB cached
// in memory > derived from the request Host) so a runtime admin change applies.
func (a *App) dynamicBaseDomainFunc() func(r *http.Request) string {
	if a.cfg.BaseDomainEnvSet || a.domain == nil {
		return nil
	}
	return func(r *http.Request) string {
		return a.domain.Resolve(r.Context(), r).BaseDomain.Value
	}
}

// serveLimiter builds the per-IP rate-limit middleware for the data plane, or nil
// when disabled (RPS <= 0). A denied request gets a plain 429 (data-plane
// responses are not JSON-enveloped; they are static content).
func (a *App) serveLimiter() func(http.Handler) http.Handler {
	if a.cfg.RateLimitServeRPS <= 0 {
		return nil
	}
	lim := ratelimit.New(ratelimit.Config{RPS: float64(a.cfg.RateLimitServeRPS)})
	trust := a.cfg.TrustProxyHeaders
	keyer := func(r *http.Request) string { return ratelimit.ClientIP(r, trust) }
	denied := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}
	return ratelimit.Middleware(lim, keyer, denied)
}

// previewSecret derives the HMAC key for preview grants via the shared
// internal/preview codec so the data plane (this file's serve.GrantAuthz) and the
// control plane (api's /preview-grant endpoint) use ONE identical key — grants
// minted by the API are accepted by the data-plane verifier without format drift.
func previewSecret(cfg config.Config) []byte {
	return preview.Secret(cfg.AdminPassword, cfg.OIDC.ClientSecret, cfg.ControlBaseURL, cfg.BaseDomain)
}

// commonMiddleware applies the cross-cutting chain (request-id -> slog -> recover)
// used by the data-plane wrapper. The control plane gets this chain from the api
// package's own router, so it is applied here only on the serve wrapper.
func (a *App) commonMiddleware(r chi.Router) {
	r.Use(observability.RequestID)
	r.Use(observability.RequestLogger(a.logger))
	r.Use(observability.Recoverer(a.logger))
}

// mountHealth attaches the liveness/readiness probes. Both planes expose them so
// each is independently monitorable.
func (a *App) mountHealth(r chi.Router) {
	r.Get("/healthz", observability.Health)
	r.Get("/readyz", observability.Ready(a.ready))
}

// Logger exposes the app logger for the entry point's lifecycle logging.
func (a *App) Logger() *slog.Logger { return a.logger }

// Config exposes the validated config (listen addresses, run mode).
func (a *App) Config() config.Config { return a.cfg }

// Close releases held resources (the background ops scheduler, then the DB pool).
// Safe to call once during shutdown. The scheduler is stopped first so an in-flight
// reaper pass cannot touch a closing pool.
func (a *App) Close(_ context.Context) error {
	if a.opsScheduler != nil {
		a.opsScheduler.Stop()
	}
	if a.store != nil {
		a.store.Close()
	}
	return nil
}
