package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/serve"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// PreviewSigner mints a one-time signed preview grant (routing-and-serving.md
// §8.1.2). *serve.GrantAuthz satisfies it via SignGrant; keeping it an interface
// lets the API be wired with the SAME codec the data-plane verifier uses (one
// shared secret, no format drift) while staying mockable in tests.
type PreviewSigner interface {
	// SignGrant returns the signed "<uuid>:<branch>:<exp>:<sig>" token the data
	// plane accepts as ?kpt=.
	SignGrant(siteID uuid.UUID, branch string, exp time.Time) string
}

// previewGrantTTL bounds how long an issued one-time grant param stays valid
// before the visitor must re-request it from the dashboard. It is intentionally
// short: it is only the hand-off window; the data plane re-signs a longer-lived
// host-only cookie from it (serve.defaultPreviewCookieTTL).
const previewGrantTTL = 5 * time.Minute

// previewGrantResponse is the body returned by the preview-grant endpoint: the
// full preview URL (with the one-time ?kpt grant already appended) the dashboard
// opens, plus the bare grant token for clients that prefer to attach it
// themselves. (Ad-hoc body: the preview-grant route is a hardening addition not
// present in the frozen openapi.yaml, so it has no generated DTO.)
type previewGrantResponse struct {
	// PreviewUrl is "<scheme>://{handle}--{branch}.<baseDomain>/?kpt=<grant>".
	PreviewUrl string `json:"previewUrl"`
	// Grant is the raw one-time grant token (the value of the kpt query param).
	Grant string `json:"grant"`
	// Branch echoes the resolved preview branch.
	Branch string `json:"branch"`
	// ExpiresAt is the grant param's expiry (RFC3339).
	ExpiresAt time.Time `json:"expiresAt"`
}

// previewGrant POST /api/sites/{handle}/branches/{branch}/preview-grant — issues a
// signed preview grant for an authorized viewer so they can open a private preview
// (routing-and-serving.md §8: published is public; every non-published branch is
// auth-gated). Viewers+ may preview (CANONICAL §6.1: "View previews" is granted to
// owner|editor|viewer), so this requires only capRead.
//
// The grant is signed with the SHARED preview secret, so the data plane's
// serve.GrantAuthz verifier accepts it and re-issues the host-only kotoji_preview
// cookie. A grant is bound to {siteID, branch}: a grant for site A's draft is
// rejected by the data plane on site B (serve.GrantAuthz.Authorize → ErrForbidden).
func (s *server) previewGrant(w http.ResponseWriter, r *http.Request) {
	// capRead: viewers and above may view previews (CANONICAL §6.1).
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capRead)
	if !ok {
		return
	}

	// The signer is only wired when preview authz is enabled on the data plane. A
	// nil signer (pure control-plane deploy with previews disabled) returns 404 so
	// we never advertise a preview path we cannot honor.
	if s.deps.PreviewGrant == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "previews are not enabled", nil)
		return
	}

	branch := chi.URLParam(r, "branch")
	// Structural branch validation mirrors the resolver (CANONICAL §5.2): published
	// is NOT addressable via a grant (it is public, reached at the bare handle),
	// and the name must be host-safe so a clean preview subdomain exists.
	if branch == "" || branch == string(site.BranchPublished) {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "published is public and is not previewable", validationDetails{Field: "branch", Reason: "not a preview branch"})
		return
	}
	if err := site.ValidateBranchName(site.BranchName(branch)); err != nil {
		writeServiceError(w, err)
		return
	}

	// The branch must actually exist on the site, else we would issue a grant for a
	// non-existent preview. ServedTree returns ErrNotFound for an unknown branch.
	if _, err := s.deps.Site.ServedTree(r.Context(), ac.site.ID, site.BranchName(branch)); err != nil {
		writeServiceError(w, err)
		return
	}

	exp := time.Now().Add(previewGrantTTL)
	grant := s.deps.PreviewGrant.SignGrant(ac.site.ID, branch, exp)

	label := string(ac.site.Handle) + "--" + branch
	previewURL := s.urlFor(label) + "/?" + serve.PreviewGrantQueryParam + "=" + grant

	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "preview.grant",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"branch": branch}),
	})

	writeJSON(w, http.StatusOK, previewGrantResponse{
		PreviewUrl: previewURL,
		Grant:      grant,
		Branch:     branch,
		ExpiresAt:  exp,
	})
}
