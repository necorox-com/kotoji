package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

// listMembers GET /api/sites/{handle}/members — members + roles. Read members
// requires at least read access; the management ops require owner.
func (s *server) listMembers(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capRead)
	if !ok {
		return
	}
	rows, err := s.deps.Store.ListMembers(r.Context(), ac.site.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]openapi.Member, 0, len(rows))
	for _, m := range rows {
		out = append(out, openapi.Member{
			UserId:      m.UserID,
			Email:       openapiEmail(m.Email),
			DisplayName: m.DisplayName,
			Role:        openapi.SiteRole(m.Role),
			CreatedAt:   tsToTimePtr(m.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Members []openapi.Member `json:"members"`
	}{Members: out})
}

// addMember POST /api/sites/{handle}/members — add/upsert a member by email
// (owner only). The email must resolve to an existing user (no invitations v1).
func (s *server) addMember(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capOwner)
	if !ok {
		return
	}
	var body openapi.AddMemberJSONRequestBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validRole(body.Role) {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid role", validationDetails{Field: "role", Reason: "must be owner|editor|viewer"})
		return
	}

	target, err := s.deps.Store.GetUserByEmail(r.Context(), string(body.Email))
	if err != nil {
		if db.IsNotFound(err) {
			// No user with that email exists yet (must sign in once first).
			writeError(w, http.StatusUnprocessableEntity, codeValidation, "no user with that email has signed in yet", validationDetails{Field: "email", Reason: "unknown user"})
			return
		}
		writeServiceError(w, err)
		return
	}

	createdBy := ac.user.UserID
	if err := s.deps.Store.AddMember(r.Context(), gen.AddMemberParams{
		SiteID:    ac.site.ID,
		UserID:    target.ID,
		Role:      gen.SiteRole(body.Role),
		CreatedBy: &createdBy,
	}); err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "member.add",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"user_id": target.ID.String(), "role": string(body.Role)}),
	})
	writeJSON(w, http.StatusOK, openapi.Member{
		UserId:      target.ID,
		Email:       openapiEmail(target.Email),
		DisplayName: target.DisplayName,
		Role:        openapi.SiteRole(body.Role),
	})
}

// updateMemberRole PATCH /api/sites/{handle}/members/{userId} — owner only.
func (s *server) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capOwner)
	if !ok {
		return
	}
	userID, perr := uuid.Parse(chi.URLParam(r, "userId"))
	if perr != nil {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid user id", validationDetails{Field: "userId", Reason: "must be a uuid"})
		return
	}
	var body openapi.UpdateMemberRoleJSONRequestBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validRole(body.Role) {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid role", validationDetails{Field: "role", Reason: "must be owner|editor|viewer"})
		return
	}

	// Guard against demoting the last owner: if the target is currently the only
	// owner and the new role is not owner, refuse (409) so a site never becomes
	// ownerless (mirrors the removeMember sole-owner rule).
	if body.Role != openapi.SiteRoleOwner {
		if soleOwner, err := s.isSoleOwner(r.Context(), ac.site.ID, userID); err == nil && soleOwner {
			writeError(w, http.StatusConflict, codeConflict, "cannot demote the sole owner", nil)
			return
		}
	}

	// Ensure the member exists before updating (UpdateMemberRole is a no-op UPDATE
	// otherwise, which would silently 200 a non-member).
	if _, err := s.deps.Store.GetMember(r.Context(), gen.GetMemberParams{SiteID: ac.site.ID, UserID: userID}); err != nil {
		if db.IsNotFound(err) {
			writeError(w, http.StatusNotFound, codeNotFound, "member not found", nil)
			return
		}
		writeServiceError(w, err)
		return
	}

	if err := s.deps.Store.UpdateMemberRole(r.Context(), gen.UpdateMemberRoleParams{
		Role:   gen.SiteRole(body.Role),
		SiteID: ac.site.ID,
		UserID: userID,
	}); err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "member.role",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"user_id": userID.String(), "role": string(body.Role)}),
	})

	target, err := s.deps.Store.GetUserByID(r.Context(), userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, openapi.Member{
		UserId:      userID,
		Email:       openapiEmail(target.Email),
		DisplayName: target.DisplayName,
		Role:        body.Role,
	})
}

// removeMember DELETE /api/sites/{handle}/members/{userId} — owner only; refuses
// to remove the sole owner (409).
func (s *server) removeMember(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capOwner)
	if !ok {
		return
	}
	userID, perr := uuid.Parse(chi.URLParam(r, "userId"))
	if perr != nil {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid user id", validationDetails{Field: "userId", Reason: "must be a uuid"})
		return
	}

	if soleOwner, err := s.isSoleOwner(r.Context(), ac.site.ID, userID); err == nil && soleOwner {
		writeError(w, http.StatusConflict, codeConflict, "cannot remove the sole owner", nil)
		return
	}

	if err := s.deps.Store.RemoveMember(r.Context(), gen.RemoveMemberParams{SiteID: ac.site.ID, UserID: userID}); err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "member.remove",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"user_id": userID.String()}),
	})
	w.WriteHeader(http.StatusNoContent)
}

// isSoleOwner reports whether userID is the only owner of the site. Used to keep
// a site from ever becoming ownerless via remove/demote. A store error returns
// (false, err) so callers can choose to ignore it (fail-open on the guard only —
// the worst case is a removable-then-recoverable ownerless site, never a crash).
func (s *server) isSoleOwner(ctx context.Context, siteID, userID uuid.UUID) (bool, error) {
	rows, err := s.deps.Store.ListMembers(ctx, siteID)
	if err != nil {
		return false, err
	}
	owners := 0
	targetIsOwner := false
	for _, m := range rows {
		if m.Role == gen.SiteRoleOwner {
			owners++
			if m.UserID == userID {
				targetIsOwner = true
			}
		}
	}
	return targetIsOwner && owners == 1, nil
}

// validRole reports whether r is one of the three canonical site roles.
func validRole(r openapi.SiteRole) bool {
	switch r {
	case openapi.SiteRoleOwner, openapi.SiteRoleEditor, openapi.SiteRoleViewer:
		return true
	default:
		return false
	}
}

// openapiEmail adapts a plain string to the generated email type.
func openapiEmail(s string) openapi_types.Email { return openapi_types.Email(s) }
