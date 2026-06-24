package api

import (
	"context"
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
		// Instance GitHub mirror config (write-only token; never echoed).
		r.Method(http.MethodGet, "/github", auth.RequireAdmin(http.HandlerFunc(s.adminGetGitHub)))
		r.Method(http.MethodPut, "/github", auth.RequireAdmin(http.HandlerFunc(s.adminPutGitHub)))
		// Instance domain/URL config (WordPress-style env > DB > derived; non-secret).
		r.Method(http.MethodGet, "/domain", auth.RequireAdmin(http.HandlerFunc(s.adminGetDomain)))
		r.Method(http.MethodPut, "/domain", auth.RequireAdmin(http.HandlerFunc(s.adminPutDomain)))
	})
}

// githubAdminConfig is the admin-screen view of the instance GitHub mirror
// config. It is SECRET-SAFE: the token and webhook secret are reduced to boolean
// "configured" flags and NEVER returned verbatim (LOCKED decision: the token is
// write-only over the API). The values fold DB-over-env so the admin sees the
// EFFECTIVE configuration.
type githubAdminConfig struct {
	Enabled          bool   `json:"enabled"`
	Org              string `json:"org"`
	TokenSet         bool   `json:"tokenSet"`
	WebhookSecretSet bool   `json:"webhookSecretSet"`
}

// adminGetGitHub GET /api/admin/github — return the effective (DB-over-env)
// GitHub mirror config with secrets reduced to "configured" booleans.
func (s *server) adminGetGitHub(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.effectiveGitHubAdminConfig(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// adminPutGitHub PUT /api/admin/github — persist a partial update. The body
// fields are all OPTIONAL (partial update): a nil field is left untouched. The
// token is WRITE-ONLY — an empty/absent token keeps the stored one; clearToken
// removes it. Returns the post-update secret-safe view.
func (s *server) adminPutGitHub(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled       *bool   `json:"enabled"`
		Org           *string `json:"org"`
		Token         *string `json:"token"`
		WebhookSecret *string `json:"webhookSecret"`
		ClearToken    bool    `json:"clearToken"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	if err := s.deps.Store.SetGitHubConfig(r.Context(), db.SetGitHubConfigInput{
		Enabled:       body.Enabled,
		Org:           body.Org,
		WebhookSecret: body.WebhookSecret,
		Token:         body.Token,
		ClearToken:    body.ClearToken,
	}); err != nil {
		writeServiceError(w, err)
		return
	}

	// Best-effort audit (no secret values in metadata — only "what changed").
	if actor, ok := auth.CurrentUser(r.Context()); ok && actor != nil {
		s.auditBestEffort(r.Context(), gen.InsertAuditParams{
			ActorUserID: uuidPtr(actor.UserID),
			Action:      "admin.github.config",
			Source:      gen.AuditSourceSystem,
			Metadata: auditMeta(map[string]any{
				"enabled_set":        body.Enabled != nil,
				"org_set":            body.Org != nil,
				"token_set":          body.Token != nil && *body.Token != "",
				"token_cleared":      body.ClearToken,
				"webhook_secret_set": body.WebhookSecret != nil,
			}),
		})
	}

	cfg, err := s.effectiveGitHubAdminConfig(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// effectiveGitHubAdminConfig folds the DB-stored config over the env defaults
// into the secret-safe admin view. DB overrides env on every axis; the env token
// counts toward TokenSet so an env-only deployment shows "token configured".
func (s *server) effectiveGitHubAdminConfig(ctx context.Context) (githubAdminConfig, error) {
	gh, err := s.deps.Store.GetGitHubConfig(ctx)
	if err != nil {
		return githubAdminConfig{}, err
	}

	envCfg := s.deps.Config.GitHub
	out := githubAdminConfig{
		Enabled:          envCfg.Enabled,
		Org:              envCfg.Org,
		TokenSet:         envCfg.Token != "",
		WebhookSecretSet: envCfg.WebhookSecret != "",
	}
	if gh.EnabledSet {
		out.Enabled = gh.Enabled
	}
	if gh.Org != "" {
		out.Org = gh.Org
	}
	if gh.TokenSet {
		out.TokenSet = true
	}
	if gh.WebhookSecret != "" {
		out.WebhookSecretSet = true
	}
	return out, nil
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
