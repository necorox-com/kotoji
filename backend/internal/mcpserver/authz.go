package mcpserver

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// membershipQuerier is the narrow slice of the generated query surface the MCP
// authz guard needs to MEMBERSHIP-CAP a token to its user's access (CANONICAL §6).
// internal/db.Store (which embeds *gen.Queries) satisfies it; tests inject a fake.
// We depend on this interface — not the concrete Store — so the guard is fully
// unit-testable without a database (DI per project conventions).
type membershipQuerier interface {
	// GetRole is the AUTHZ HOT PATH: the user's role on a site. A no-rows error
	// (pgx.ErrNoRows) means NO membership -> the guard 404s (no existence leak).
	GetRole(ctx context.Context, arg gen.GetRoleParams) (gen.SiteRole, error)
	// ListSitesForUser returns every live site the user is a member of, with their
	// role — the membership list list_sites returns.
	ListSitesForUser(ctx context.Context, arg gen.ListSitesForUserParams) ([]gen.ListSitesForUserRow, error)
	// GetUserByID reads the owning user's account flags (create_site gate).
	GetUserByID(ctx context.Context, id uuid.UUID) (gen.User, error)
}

// compile-time guarantee: the real generated querier (and thus *db.Store) is a
// valid membershipQuerier.
var _ membershipQuerier = (gen.Querier)(nil)

// listSitesPageLimit bounds the membership list returned to list_sites. It is a
// generous cap (a user is rarely on thousands of projects); pagination over MCP
// is not a v1 requirement (mcp.md §5.1).
const listSitesPageLimit = 1000

// roleScopes maps a per-site membership role to the MCP scopes it grants
// (CANONICAL §6): owner/editor -> {read,write,publish}; viewer -> {read}. (Owner
// additionally manages members/settings, but those are not MCP scopes.) An
// unknown role grants nothing (fail closed).
func roleScopes(role gen.SiteRole) []scope {
	switch role {
	case gen.SiteRoleOwner, gen.SiteRoleEditor:
		return []scope{scopeRead, scopeWrite, scopePublish}
	case gen.SiteRoleViewer:
		return []scope{scopeRead}
	default:
		return nil
	}
}

// intersectScopes returns the EFFECTIVE scopes on a site: the intersection of the
// token's scopes and the scopes the membership role grants. This is the core of
// the membership-capped model — re-evaluated on EVERY call, so downgrading the
// user's role (or removing their membership) instantly limits the token, and the
// token can never exceed the user's own access (CANONICAL §6.2).
func intersectScopes(tokenScopes []string, role gen.SiteRole) []scope {
	granted := roleScopes(role)
	out := make([]scope, 0, len(granted))
	for _, sc := range granted {
		if slices.Contains(tokenScopes, string(sc)) {
			out = append(out, sc)
		}
	}
	return out
}

// authorized is the resolved per-call authorization context: the site the call
// targets, the user's role on it, and the effective scopes (token ∩ role).
type authorized struct {
	site   site.Site
	role   gen.SiteRole
	scopes []scope
}

// authorizeSite is the MEMBERSHIP-CAP gate run before every content tool. It:
//  1. resolves the site by handle (404 if missing),
//  2. reads the user's membership role (404 if NOT a member — no existence leak),
//  3. computes effective scopes = intersection(token.scopes, roleScopes(role)),
//  4. requires `needed` to be in the effective set (else forbidden).
//
// It returns (authorized, result, goErr) where exactly one of result/goErr is
// non-nil on failure (and both nil on success). result is a ready-to-return tool
// error; goErr is reserved for infrastructure failures (DB/git down) per the
// mapError contract.
func (r *registry) authorizeSite(ctx context.Context, c TokenInfo, handle string, needed scope) (authorized, *mcp.CallToolResult, error) {
	if handle == "" {
		return authorized{}, toolErr(codeValidation, "site: required", nil), nil
	}
	// 1. Resolve the CURRENT handle -> site. site.ErrNotFound (and any non-infra
	// resolution failure) is a not_found to a token caller — we never confirm a
	// site's existence to a non-member, and a bad handle simply matches nothing.
	// A genuine infra failure (ErrGit/disk) is surfaced as a Go error via mapError.
	s, err := r.svc.GetSiteByHandle(ctx, site.Handle(handle))
	if err != nil {
		if isSiteBusinessError(err) {
			return authorized{}, toolErr(codeNotFound, "site not found", nil), nil
		}
		res, gerr := r.mapError(err, "authorize_site")
		return authorized{}, res, gerr
	}

	// 2. Read the user's role on this site. No membership -> 404 (no existence leak).
	role, err := r.members.GetRole(ctx, gen.GetRoleParams{SiteID: s.ID, UserID: c.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return authorized{}, toolErr(codeNotFound, "site not found", nil), nil
		}
		// Infra failure reading membership: protocol error (logged, generic message).
		res, gerr := r.mapError(err, "authorize_site")
		return authorized{}, res, gerr
	}

	// 3. Effective scope = token.scopes ∩ roleScopes(role). Re-evaluated here on
	// EVERY call, so a downgrade/removal limits the token immediately.
	eff := intersectScopes(c.Scopes, role)

	// 4. The tool's needed scope must be in the effective set.
	if !slices.Contains(eff, needed) {
		return authorized{}, toolErr(codeForbidden,
			"your effective access on this site does not grant "+string(needed),
			map[string]any{"role": string(role), "effective_scopes": scopeStrings(eff)}), nil
	}

	return authorized{site: s, role: role, scopes: eff}, nil, nil
}

// isSiteBusinessError reports whether err is a site.Service BUSINESS error (a
// not-found / validation that a non-member must see as a plain 404), as opposed
// to an infrastructure failure (ErrGit / disk / unknown) which must surface as a
// Go protocol error. We collapse all business resolution failures to not_found
// to avoid leaking a site's existence.
func isSiteBusinessError(err error) bool {
	return errors.Is(err, site.ErrNotFound) ||
		errors.Is(err, site.ErrValidation) ||
		errors.Is(err, site.ErrReservedHandle)
}

// scopeStrings renders a scope slice as plain strings for an error detail / wire.
func scopeStrings(scopes []scope) []string {
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		out = append(out, string(sc))
	}
	return out
}
