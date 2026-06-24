package api

import (
	"errors"
	"net/http"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/domaincfg"
)

// domainAdminConfig is the admin-screen view of the instance domain/URL config
// (WordPress-style env > DB > derived). Unlike the GitHub config these values are
// NOT secret, so they are returned verbatim. Each carries its source and a locked
// flag so the GUI can render an editable field or a read-only "set via
// environment / locked" field.
type domainAdminConfig struct {
	BaseDomain           string `json:"baseDomain"`
	ControlBaseURL       string `json:"controlBaseURL"`
	BaseDomainSource     string `json:"baseDomainSource"`     // "env" | "db" | "derived"
	ControlBaseURLSource string `json:"controlBaseURLSource"` // "env" | "db" | "derived"
	BaseDomainLocked     bool   `json:"baseDomainLocked"`     // env-set => read-only
	ControlBaseURLLocked bool   `json:"controlBaseURLLocked"` // env-set => read-only
}

// adminGetDomain GET /api/admin/domain — return the EFFECTIVE base domain +
// control base URL with their sources and env-locked flags. The effective values
// are derived from the request when neither env nor DB is set (fresh install), so
// the admin sees the same values the data plane / auth use right now.
func (s *server) adminGetDomain(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.effectiveDomainAdminConfig(r))
}

// adminPutDomain PUT /api/admin/domain — persist a partial update of the runtime
// base domain / control base URL. Body fields are OPTIONAL (partial update): a nil
// field is left untouched. A field whose env var is SET (locked) is REJECTED with
// a 409 (do not silently no-op). Values are validated (hostname / absolute http(s)
// URL) before persisting; the control base URL is normalized (trailing slash
// trimmed). On success the effective cache is invalidated and the same view is
// returned.
func (s *server) adminPutDomain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseDomain     *string `json:"baseDomain"`
		ControlBaseURL *string `json:"controlBaseURL"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// Build the partial write, validating each supplied field and rejecting writes
	// to an env-locked field with a 409 (LOCKED decision: clear failure, not a no-op).
	var in db.SetDomainConfigInput

	// For each field: an env-locked field rejects the write (409). A non-empty
	// value is VALIDATED then persisted. An EMPTY string is a deliberate REVERT —
	// it deletes the DB key so the field falls back to env/derived (no validation).
	if body.BaseDomain != nil {
		if s.deps.Domain.EnvBaseDomainLocked() {
			writeError(w, http.StatusConflict, codeConflict, "baseDomain is locked by the environment (KOTOJI_BASE_DOMAIN is set)", validationDetails{Field: "baseDomain", Reason: "locked by environment"})
			return
		}
		if *body.BaseDomain != "" {
			if err := domaincfg.ValidateBaseDomain(*body.BaseDomain); err != nil {
				writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid base domain", validationDetails{Field: "baseDomain", Reason: validationReason(err)})
				return
			}
		}
		in.BaseDomain = body.BaseDomain
	}

	if body.ControlBaseURL != nil {
		if s.deps.Domain.EnvControlBaseURLLocked() {
			writeError(w, http.StatusConflict, codeConflict, "controlBaseURL is locked by the environment (KOTOJI_CONTROL_BASE_URL is set)", validationDetails{Field: "controlBaseURL", Reason: "locked by environment"})
			return
		}
		if *body.ControlBaseURL == "" {
			// Empty -> revert to env/derived (delete the key).
			in.ControlBaseURL = body.ControlBaseURL
		} else {
			if err := domaincfg.ValidateControlBaseURL(*body.ControlBaseURL); err != nil {
				writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid control base URL", validationDetails{Field: "controlBaseURL", Reason: validationReason(err)})
				return
			}
			// Normalize away a trailing slash so derived links (base + "/auth/callback")
			// never double a slash. Stored as the canonical origin.
			normalized := domaincfg.NormalizeControlBaseURL(*body.ControlBaseURL)
			in.ControlBaseURL = &normalized
		}
	}

	// Nothing to do (both fields absent): return the current view unchanged rather
	// than a write. The cache stays valid.
	if in.BaseDomain == nil && in.ControlBaseURL == nil {
		writeJSON(w, http.StatusOK, s.effectiveDomainAdminConfig(r))
		return
	}

	if err := s.deps.Store.SetDomainConfig(r.Context(), in); err != nil {
		writeServiceError(w, err)
		return
	}

	// Invalidate the cached effective value so the next request reads the new DB
	// value (a runtime change applies without a restart).
	s.deps.Domain.InvalidateCache()

	// Best-effort audit (the values are not secret, so they are safe to record).
	if actor, ok := auth.CurrentUser(r.Context()); ok && actor != nil {
		s.auditBestEffort(r.Context(), gen.InsertAuditParams{
			ActorUserID: uuidPtr(actor.UserID),
			Action:      "admin.domain.config",
			Source:      gen.AuditSourceSystem,
			Metadata: auditMeta(map[string]any{
				"base_domain_set":      in.BaseDomain != nil,
				"control_base_url_set": in.ControlBaseURL != nil,
			}),
		})
	}

	writeJSON(w, http.StatusOK, s.effectiveDomainAdminConfig(r))
}

// effectiveDomainAdminConfig resolves the effective domain/URL config for the
// request into the admin view (value + source + locked flag per field).
func (s *server) effectiveDomainAdminConfig(r *http.Request) domainAdminConfig {
	res := s.deps.Domain.Resolve(r.Context(), r)
	return domainAdminConfig{
		BaseDomain:           res.BaseDomain.Value,
		ControlBaseURL:       res.ControlBaseURL.Value,
		BaseDomainSource:     res.BaseDomain.Source,
		ControlBaseURLSource: res.ControlBaseURL.Source,
		BaseDomainLocked:     res.BaseDomain.Locked,
		ControlBaseURLLocked: res.ControlBaseURL.Locked,
	}
}

// validationReason extracts the human-safe reason from a domaincfg validation
// error for the API field detail, falling back to the error string.
func validationReason(err error) string {
	if reason := domaincfg.Reason(err); reason != "" {
		return reason
	}
	// Defensive: a non-domaincfg error still gets a safe generic reason.
	if errors.Is(err, domaincfg.ErrInvalidBaseDomain) || errors.Is(err, domaincfg.ErrInvalidControlBaseURL) {
		return "invalid value"
	}
	return "invalid value"
}
