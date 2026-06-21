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
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	"github.com/necorox-com/kotoji/backend/internal/api"
	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/mcpserver"
	"github.com/necorox-com/kotoji/backend/internal/migrate"
	"github.com/necorox-com/kotoji/backend/internal/observability"
	"github.com/necorox-com/kotoji/backend/internal/ops"
	"github.com/necorox-com/kotoji/backend/internal/preview"
	"github.com/necorox-com/kotoji/backend/internal/ratelimit"
	"github.com/necorox-com/kotoji/backend/internal/resolve"
	"github.com/necorox-com/kotoji/backend/internal/serve"
	"github.com/necorox-com/kotoji/backend/internal/site"
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
	// the runner/adapter stay internal to the site package.
	a.siteSvc = site.NewProductionService(store, siteConfig(cfg))

	// Auth is only needed where the control plane runs.
	if cfg.ServesControl() {
		provider, perr := auth.ProviderFor(ctx, cfg) // OIDC does network discovery
		if perr != nil {
			store.Close()
			return nil, perr
		}
		// *db.Store satisfies auth.StoreDeps (SessionStore + WithTx).
		a.auth = auth.New(cfg, store, provider)

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
// (its own bounded value object). Root is DataDir/sites per CANONICAL §1.
func siteConfig(cfg config.Config) site.Config {
	return site.Config{
		Root:     filepath.Join(cfg.DataDir, sitesSubdir),
		GitBin:   cfg.GitBin,
		MirrorOn: cfg.GitHub.Enabled,
		Zip: site.ZipConfig{
			MaxUploadBytes:       cfg.Zip.MaxUploadBytes,
			MaxUncompressedBytes: cfg.Zip.MaxTotalBytes,
			MaxEntries:           cfg.Zip.MaxFiles,
			MaxCompressionRatio:  cfg.Zip.MaxRatio,
			AllowedExt:           cfg.Zip.AllowedExt,
		},
	}
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

	// MCP server (control plane only). Disabled => not mounted.
	var mcpHandler http.Handler
	if a.cfg.MCPEnabled {
		mcpHandler = mcpserver.New(mcpserver.FromConfig(a.cfg, a.siteSvc, a.store, a.logger))
	}

	apiHandler := api.NewRouter(api.Deps{
		Config:       a.cfg,
		Site:         a.siteSvc,
		Store:        a.store, // *db.Store satisfies api.MetaStore
		Auth:         api.WrapAuth(a.auth),
		Serve:        nil, // data plane runs on its own addr (run mode all|serve)
		MCP:          mcpHandler,
		PreviewGrant: a.previewSigner(), // shares ONE codec with the data-plane verifier
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

// ServeRouter builds the data-plane handler (Host-resolving static hosting) plus
// the health probes. The serve package owns resolution, preview authz, and the
// security/cache header set. Returns nil when the run mode does not serve data.
func (a *App) ServeRouter() http.Handler {
	if !a.cfg.ServesData() {
		return nil
	}

	res := resolve.NewResolver(resolve.Config{
		BaseDomain:         a.cfg.BaseDomain,
		ControlLabel:       "", // bare BaseDomain is the control host
		EnablePathFallback: true,
		// X-Forwarded-Host is trusted only behind the documented reverse proxy.
		TrustForwardedHost: a.cfg.TrustProxyHeaders,
	})

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

	h := serve.NewHandler(serve.Deps{
		Resolver: res,
		Trees:    tp,
		Authz:    az,
		Control:  nil, // pure data-plane mode (control runs on its own addr)
		Config: serve.HandlerConfig{
			Security: serve.DefaultSecurityHeaderConfig(),
			Cache:    serve.CacheConfig{}, // zero value: ETag ON + base-href injection ON
		},
	})

	// Health probes wrap the static handler so the serve plane is independently
	// monitorable; everything else falls through to the data-plane handler.
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
