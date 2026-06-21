// Package api is the kotoji control-plane REST surface. It mounts the hand-written
// chi handlers (CANONICAL §9 decision #1: spec-derived DTOs in internal/openapi,
// hand-written handlers) for sites, files, branches, publish, history, members,
// tokens, and admin; chains them onto the shared middleware (request-id, slog,
// recover, CORS, session-auth, CSRF); and composes /auth/*, /mcp, and the data
// plane into one http.Handler.
//
// Boundaries (DI): handlers depend on site.Service (the single git boundary) and
// the narrow MetaStore (authz/members/tokens/audit) — never on git or pgx
// directly — so the whole package is unit-testable against fakes (FakeService +
// an in-memory store).
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"

	"github.com/necorox-com/kotoji/backend/internal/observability"
)

// server bundles the dependencies behind the handler methods. It is unexported;
// callers construct it via NewRouter.
type server struct {
	deps Deps
}

// NewRouter assembles the control-plane HTTP handler from deps and returns it.
// The Integration phase calls this in the composition root:
//
//	h := api.NewRouter(api.Deps{
//	        Config: cfg, Site: siteSvc, Store: store,
//	        Auth: api.WrapAuth(authSvc), Serve: serveHandler, MCP: mcpHandler,
//	        Logger: logger,
//	})
//
// The returned handler:
//   - applies the shared middleware chain to every request,
//   - mounts /auth/* + /api/me + /api/config via the auth surface (public-safe),
//   - mounts the guarded /api/* resource tree (session-auth + CSRF + role gates),
//   - mounts /mcp (if MCP != nil) and the data plane (if Serve != nil).
func NewRouter(deps Deps) http.Handler {
	s := &server{deps: deps}
	return s.handler()
}

// handler builds the chi router with the full middleware chain and route tree.
func (s *server) handler() http.Handler {
	r := chi.NewRouter()

	// ---- shared middleware chain (order is load-bearing) ----
	// request-id -> slog access log -> panic recovery -> CORS -> session-auth.
	r.Use(observability.RequestID)
	if s.deps.Logger != nil {
		r.Use(observability.RequestLogger(s.deps.Logger))
		r.Use(observability.Recoverer(s.deps.Logger))
	}
	r.Use(s.corsMiddleware())
	// SessionAuth loads the user onto the context (NON-fatal: anonymous if absent)
	// so /api/config stays public while protected routes enforce presence.
	r.Use(s.deps.Auth.Middleware())

	// ---- auth + identity (public-safe; /api/config has security: []) ----
	s.deps.Auth.RegisterRoutes(r)

	// ---- guarded REST resource tree ----
	// The mutating /api subtree additionally requires a valid CSRF double-submit
	// token (bearer-token requests are exempt inside the guard).
	r.Group(func(gr chi.Router) {
		gr.Use(s.deps.Auth.CSRF().Middleware)
		s.mountAPI(gr)
	})

	// ---- MCP (control plane only) ----
	if s.deps.MCP != nil {
		r.Mount(s.deps.Config.MCPPath, s.deps.MCP)
	}

	// ---- data plane (same-binary mode) ----
	// Mounted last as the catch-all so it only sees requests that did not match a
	// control-plane route. Pure control deployments leave Serve nil.
	if s.deps.Serve != nil {
		r.NotFound(s.deps.Serve.ServeHTTP)
		r.MethodNotAllowed(s.deps.Serve.ServeHTTP)
	}

	return r
}

// mountAPI registers the guarded /api/* resource routes. Each handler resolves
// access (role->capability, CANONICAL §6) before touching the Service/Store.
func (s *server) mountAPI(r chi.Router) {
	r.Route("/api/sites", func(r chi.Router) {
		r.Get("/", s.listSites)
		r.Post("/", s.createSite)

		r.Route("/{handle}", func(r chi.Router) {
			r.Get("/", s.getSite)
			r.Patch("/", s.updateSite)
			r.Delete("/", s.deleteSite)
			r.Post("/rename", s.renameSite)

			// members
			r.Get("/members", s.listMembers)
			r.Post("/members", s.addMember)
			r.Patch("/members/{userId}", s.updateMemberRole)
			r.Delete("/members/{userId}", s.removeMember)

			// tokens
			r.Get("/tokens", s.listTokens)
			r.Post("/tokens", s.createToken)
			r.Delete("/tokens/{tokenId}", s.revokeToken)

			// branches
			r.Get("/branches", s.listBranches)
			r.Post("/branches", s.createBranch)
			r.Delete("/branches/{branch}", s.deleteBranch)

			// preview grant: a signed grant a viewer+ uses to open a private
			// preview (routing-and-serving.md §8.1.2).
			r.Post("/branches/{branch}/preview-grant", s.previewGrant)

			// files (scoped under a branch)
			r.Get("/branches/{branch}/files", s.listFiles)
			r.Get("/branches/{branch}/file", s.readFile)
			r.Put("/branches/{branch}/file", s.writeFile)
			r.Delete("/branches/{branch}/file", s.deleteFile)
			r.Post("/branches/{branch}/import", s.importZip)

			// history
			r.Post("/branches/{branch}/commit", s.commit)
			r.Get("/branches/{branch}/log", s.getLog)
			r.Post("/branches/{branch}/rollback", s.rollback)
			r.Get("/diff", s.getDiff)

			// publish
			r.Post("/publish", s.publish)
		})
	})

	// instance admin (is_admin only).
	s.mountAdmin(r)
}

// corsMiddleware builds the CORS handler from the configured allowlist. Cookie
// auth requires AllowCredentials, so a wildcard origin is never used; the
// allowlist comes from config. X-CSRF-Token is exposed so the SPA can echo it.
func (s *server) corsMiddleware() func(http.Handler) http.Handler {
	return cors.Handler(cors.Options{
		AllowedOrigins:   s.deps.Config.CORSAllowedOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Request-Id"},
		ExposedHeaders:   []string{"X-Request-Id"},
		AllowCredentials: true,
		MaxAge:           300,
	})
}
