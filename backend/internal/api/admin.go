package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// adminAuditDefaultLimit / adminAuditMaxLimit bound the admin activity feed page.
const (
	adminAuditDefaultLimit = 50
	adminAuditMaxLimit     = 200
)

// mountAdmin registers the instance-admin (is_admin) surface under /api/admin.
// These endpoints are OUTSIDE the frozen openapi.yaml path set (which covers the
// per-site control plane); they back the admin screen described in CANONICAL §6
// (instance superuser axis: quotas, user flags, audit). Each is guarded by the
// auth.RequireAdmin wrapper so non-admins get 401/403.
func (s *server) mountAdmin(r chi.Router) {
	r.Route("/api/admin", func(r chi.Router) {
		// RequireAdmin returns 401 anonymous / 403 non-admin before the handler.
		r.Method(http.MethodGet, "/sites/{handle}/audit", auth.RequireAdmin(http.HandlerFunc(s.adminSiteAudit)))
		r.Method(http.MethodPatch, "/users/{userId}/flags", auth.RequireAdmin(http.HandlerFunc(s.adminSetUserFlags)))
	})
}

// adminSiteAudit GET /api/admin/sites/{handle}/audit — per-site activity feed
// (admin only), keyset-paginated by `before` (an audit row id, exclusive).
func (s *server) adminSiteAudit(w http.ResponseWriter, r *http.Request) {
	// Admin is already enforced by RequireAdmin; resolve the site by handle.
	st, err := s.deps.Site.GetSiteByHandle(r.Context(), site.Handle(chi.URLParam(r, "handle")))
	if err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "site not found", nil)
		return
	}

	limit := int32(adminAuditDefaultLimit)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, perr := parseInt32(v); perr == nil && n > 0 {
			limit = clampInt32(n, 1, adminAuditMaxLimit)
		}
	}
	var before *int64
	if v := r.URL.Query().Get("before"); v != "" {
		if n, perr := parseInt64(v); perr == nil {
			before = &n
		}
	}

	siteID := st.ID
	rows, lerr := s.deps.Store.ListAuditForSite(r.Context(), gen.ListAuditForSiteParams{
		SiteID:   &siteID,
		BeforeID: before,
		Lim:      limit,
	})
	if lerr != nil {
		writeServiceError(w, lerr)
		return
	}

	out := make([]auditEntry, 0, len(rows))
	for _, a := range rows {
		out = append(out, toAuditEntry(a))
	}
	writeJSON(w, http.StatusOK, struct {
		Audit []auditEntry `json:"audit"`
	}{Audit: out})
}

// adminSetUserFlags PATCH /api/admin/users/{userId}/flags — toggle a user's
// instance powers (is_admin, can_create_sites). Admin only.
func (s *server) adminSetUserFlags(w http.ResponseWriter, r *http.Request) {
	userID, perr := uuid.Parse(chi.URLParam(r, "userId"))
	if perr != nil {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid user id", validationDetails{Field: "userId", Reason: "must be a uuid"})
		return
	}

	var body struct {
		IsAdmin        *bool `json:"isAdmin"`
		CanCreateSites *bool `json:"canCreateSites"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// Read the current flags so a partial PATCH only changes the supplied fields.
	cur, gerr := s.deps.Store.GetUserByID(r.Context(), userID)
	if gerr != nil {
		if db.IsNotFound(gerr) {
			writeError(w, http.StatusNotFound, codeNotFound, "user not found", nil)
			return
		}
		writeServiceError(w, gerr)
		return
	}
	isAdmin := cur.IsAdmin
	if body.IsAdmin != nil {
		isAdmin = *body.IsAdmin
	}
	canCreate := cur.CanCreateSites
	if body.CanCreateSites != nil {
		canCreate = *body.CanCreateSites
	}

	if err := s.deps.Store.SetUserAdminFlags(r.Context(), gen.SetUserAdminFlagsParams{
		IsAdmin:        isAdmin,
		CanCreateSites: canCreate,
		ID:             userID,
	}); err != nil {
		writeServiceError(w, err)
		return
	}

	actor, _ := auth.CurrentUser(r.Context())
	if actor != nil {
		s.auditBestEffort(r.Context(), gen.InsertAuditParams{
			ActorUserID: uuidPtr(actor.UserID),
			Action:      "admin.user.flags",
			Source:      gen.AuditSourceSystem,
			Metadata:    auditMeta(map[string]any{"user_id": userID.String(), "is_admin": isAdmin, "can_create_sites": canCreate}),
		})
	}

	writeJSON(w, http.StatusOK, struct {
		UserID         uuid.UUID `json:"userId"`
		IsAdmin        bool      `json:"isAdmin"`
		CanCreateSites bool      `json:"canCreateSites"`
	}{UserID: userID, IsAdmin: isAdmin, CanCreateSites: canCreate})
}

// auditEntry is the wire shape of one admin activity-feed row. It is admin-only
// and not part of the public openapi contract, so it lives here.
type auditEntry struct {
	ID        int64          `json:"id"`
	ActorID   *uuid.UUID     `json:"actorId"`
	SiteID    *uuid.UUID     `json:"siteId"`
	TokenID   *uuid.UUID     `json:"tokenId"`
	Action    string         `json:"action"`
	Source    string         `json:"source"`
	CommitSha *string        `json:"commitSha"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt time.Time      `json:"createdAt"`
}

// toAuditEntry maps a generated AuditLog row to the wire shape, decoding the
// JSONB metadata back into a map (best-effort; empty on parse failure).
func toAuditEntry(a gen.AuditLog) auditEntry {
	meta := map[string]any{}
	if len(a.Metadata) > 0 {
		_ = json.Unmarshal(a.Metadata, &meta)
	}
	return auditEntry{
		ID:        a.ID,
		ActorID:   a.ActorUserID,
		SiteID:    a.SiteID,
		TokenID:   a.TokenID,
		Action:    a.Action,
		Source:    string(a.Source),
		CommitSha: a.CommitSha,
		Metadata:  meta,
		CreatedAt: a.CreatedAt.Time,
	}
}
