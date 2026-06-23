package mcpserver

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/cors"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// serverName / version identify this MCP server to connecting clients.
const (
	serverName    = "kotoji"
	serverVersion = "v1"
)

// mcpInstructions is the model-facing guidance shipped on connect (mcp.md §9).
// It is deliberately short and behavioural: pick a project, the optimistic-lock
// loop, and the static-only constraint.
const mcpInstructions = `This server hosts the web projects you are a member of. Every content tool takes
a "site" (the project handle); you can only act on projects you belong to, and only
within the scope your role on each project allows (a token can never exceed your own
access). To work:
1. list_sites -> see your projects and your effective scope on each.
2. read_file (site, path) -> note the returned "commit".
3. write_file (site, ..., base_sha = that "commit").
4. If you get a "conflict" error, the file changed underneath you: read_file
   again and redo your edit on the new content. Never retry blindly.
Saving commits and mirrors to backup; it does NOT make the change live.
Use "publish" to make the working branch live (this is the "go live" action).
Static files only: .html/.css/.js/images/fonts. No server code, no build step.`

// Deps is the dependency-injection bundle for the MCP server. The Integration
// phase constructs it and calls New. Everything the server needs is here, behind
// interfaces, so the whole package is unit-testable against fakes.
type Deps struct {
	// Service is the single git boundary; every tool delegates to it. REQUIRED.
	Service site.Service
	// Tokens is the token query surface for the verifier (internal/db.Store
	// satisfies it). REQUIRED.
	Tokens tokenQuerier
	// Members is the membership-authz surface the guard uses to cap a token to its
	// user's memberships: per-site role (GetRole), the user's membership list
	// (list_sites), and the user's account flags (create_site gate). internal/db.Store
	// satisfies it. REQUIRED (a nil Members fails every authz check closed).
	Members membershipQuerier
	// Limits bundles size caps + the rate Limiter. Zero value => spec defaults.
	Limits Limits
	// BaseDomain is the hosted-content base (e.g. "hosting.example.com") used to
	// compose published/preview URLs. Empty => URLs are composed without a domain.
	BaseDomain string
	// Scheme is the URL scheme for composed URLs ("https" in prod). Empty => https.
	Scheme string
	// CORSOrigins restricts the /mcp CORS allowlist. Empty => no cross-origin
	// (MCP clients are not browsers; cf. mcp.md §2.4).
	CORSOrigins []string
	// Logger is used for internal-error logging (never leaks detail to the model).
	Logger *slog.Logger
}

// FromConfig builds Deps from a parsed config.Config + the wired dependencies,
// filling the URL/CORS/limit fields from config. The Integration phase may use
// this or construct Deps directly. tokens AND members are both satisfied by the
// one *db.Store the composition root holds.
func FromConfig(cfg config.Config, svc site.Service, tokens tokenQuerier, members membershipQuerier, log *slog.Logger) Deps {
	scheme := "https"
	if !cfg.IsProduction() {
		scheme = "http"
	}
	return Deps{
		Service:     svc,
		Tokens:      tokens,
		Members:     members,
		Limits:      DefaultLimits(),
		BaseDomain:  cfg.BaseDomain,
		Scheme:      scheme,
		CORSOrigins: cfg.CORSAllowedOrigins,
		Logger:      log,
	}
}

// urlFor composes "<scheme>://<label>.<baseDomain>" for a host label. With no
// base domain it returns just the scheme+label (dev/test friendliness).
func (d Deps) urlFor(label string) string {
	scheme := d.Scheme
	if scheme == "" {
		scheme = "https"
	}
	if d.BaseDomain == "" {
		return scheme + "://" + label
	}
	return scheme + "://" + label + "." + d.BaseDomain
}

// New builds the MCP server and returns the Streamable HTTP http.Handler to mount
// at /mcp on the CONTROL plane only. The middleware chain (mcp.md §2.4) is:
//
//	scoped-CORS -> RequireBearerToken(verifier) -> StreamableHTTPHandler -> tool dispatch
//
// The handler is STATELESS (mcp.md Open Question #4 → stateless v1): tools are
// stateless and per-call state comes from the token, so the server scales
// horizontally with no sticky sessions.
func New(d Deps) http.Handler {
	limits := d.Limits.withDefaults()

	s := mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Version: serverVersion,
	}, &mcp.ServerOptions{
		Instructions: mcpInstructions,
		Logger:       d.Logger,
	})

	reg := &registry{
		svc:     d.Service,
		members: d.Members,
		limits:  limits,
		log:     d.Logger,
		cfg:     d,
	}
	reg.registerAll(s)

	// A single shared server: tools are stateless; per-call scoping comes from the
	// token, so there is no benefit to minting a server per site (mcp.md §2.3).
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, Logger: d.Logger},
	)

	// Bearer-token auth. No global Scopes here: scope is per-tool (mcp.md §3.3),
	// so the middleware only requires *a valid token* and the tools decide what
	// it may do.
	verifier := NewVerifier(d.Tokens)
	requireToken := auth.RequireBearerToken(verifier.Verify, &auth.RequireBearerTokenOptions{})

	// CORS for /mcp: MCP clients are not browsers, so we do NOT use the permissive
	// dashboard CORS. Allow only the MCP headers; no credentials/cookies (token
	// auth, not cookie auth — removes the CSRF surface, mcp.md §2.4 / guarantee #11).
	corsMW := cors.New(cors.Options{
		AllowedOrigins:   corsOrigins(d.CORSOrigins),
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodOptions, http.MethodDelete},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version"},
		AllowCredentials: false,
		MaxAge:           300,
	}).Handler

	return corsMW(requireToken(streamable))
}

// corsOrigins normalizes the configured origin allowlist; empty means deny all
// cross-origin (non-browser MCP clients send no Origin header and are unaffected).
func corsOrigins(origins []string) []string {
	out := make([]string, 0, len(origins))
	for _, o := range origins {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	if len(out) == 0 {
		// No origins configured: deny all cross-origin (non-browser clients send
		// no Origin header and are unaffected).
		return []string{}
	}
	return out
}
