package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// capability is one row of the role->capability matrix (CANONICAL §6.1). Each
// guarded endpoint declares the capability it requires; resolveAccess checks the
// caller's effective role grants it.
type capability int

const (
	// capRead: read files/history/diff/log + view previews. owner|editor|viewer.
	capRead capability = iota
	// capWrite: write/save/delete files, branches, upload, rollback. owner|editor.
	capWrite
	// capPublish: promote to published (direct mode). owner|editor (publish_mode
	// gating is applied at the publish handler, not here).
	capPublish
	// capOwner: rename, delete site, members, tokens, settings, mirror. owner only.
	capOwner
)

// roleRank orders the per-site roles so capability checks are a simple compare.
// owner(3) > editor(2) > viewer(1); 0 = no membership.
func roleRank(r gen.SiteRole) int {
	switch r {
	case gen.SiteRoleOwner:
		return 3
	case gen.SiteRoleEditor:
		return 2
	case gen.SiteRoleViewer:
		return 1
	default:
		return 0
	}
}

// minRankFor returns the lowest role rank that satisfies a capability.
func minRankFor(c capability) int {
	switch c {
	case capRead:
		return 1 // viewer
	case capWrite, capPublish:
		return 2 // editor
	case capOwner:
		return 3 // owner
	default:
		return 3
	}
}

// roleAllows reports whether a role satisfies a capability (CANONICAL §6.1).
func roleAllows(r gen.SiteRole, c capability) bool {
	return roleRank(r) >= minRankFor(c)
}

// access is the resolved authorization context for one site-scoped request: the
// site it targets, the caller, and the caller's effective role on it.
type access struct {
	site site.Site
	user *auth.SessionUser
	role gen.SiteRole
}

// resolveAccess loads the site by handle, the caller's session, and the caller's
// role on that site, then enforces the required capability. It writes the
// appropriate error (401/403/404) and returns ok=false on any failure, so each
// handler is a single `if !ok { return }` after the call.
//
// Ordering matters for information disclosure: an anonymous request gets 401; an
// authenticated non-member gets 404 (do not reveal a private site's existence);
// a member lacking the capability gets 403.
func (s *server) resolveAccess(w http.ResponseWriter, r *http.Request, handle string, cap capability) (access, bool) {
	user, ok := auth.CurrentUser(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "authentication required", nil)
		return access{}, false
	}

	// Resolve the CURRENT handle -> site (old handles 404 here; CANONICAL §5.5).
	st, err := s.deps.Site.GetSiteByHandle(r.Context(), site.Handle(handle))
	if err != nil {
		// Any resolution failure (incl. validation of a malformed handle) is a
		// 404 to a control-plane caller — we never confirm a site exists to a
		// non-member, and a bad handle simply matches nothing.
		writeError(w, http.StatusNotFound, codeNotFound, "site not found", nil)
		return access{}, false
	}

	role, rok := s.effectiveRole(r.Context(), st.ID, user)
	if !rok {
		// Authenticated but not a member: 404 (do not disclose existence).
		writeError(w, http.StatusNotFound, codeNotFound, "site not found", nil)
		return access{}, false
	}

	if !roleAllows(role, cap) {
		writeError(w, http.StatusForbidden, codeForbidden, "you do not have permission to do that", nil)
		return access{}, false
	}

	return access{site: st, user: user, role: role}, true
}

// effectiveRole returns the caller's role on a site. A real membership row wins;
// an instance admin with NO membership is granted a synthetic owner role so the
// admin tooling can operate on any site (CANONICAL §6: is_admin governs instance
// ops; here it also unblocks per-site admin actions through the standard path).
func (s *server) effectiveRole(ctx context.Context, siteID uuid.UUID, user *auth.SessionUser) (gen.SiteRole, bool) {
	role, err := s.deps.Store.GetRole(ctx, gen.GetRoleParams{SiteID: siteID, UserID: user.UserID})
	if err == nil {
		return role, true
	}
	if db.IsNotFound(err) {
		// Instance superusers may act on any site even without a membership row.
		if user.IsAdmin {
			return gen.SiteRoleOwner, true
		}
		return "", false
	}
	// A store error is treated as "no access" (fail-closed); it is rare and the
	// caller surfaces a 404, never a 500 that would leak a DB hiccup as access.
	if user.IsAdmin {
		return gen.SiteRoleOwner, true
	}
	return "", false
}
