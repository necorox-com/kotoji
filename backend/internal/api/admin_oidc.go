package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// oidcAdminConfig is the admin-screen view of the instance OIDC config. It is
// SECRET-SAFE: the client secret is reduced to a boolean "configured" flag and
// NEVER returned verbatim (LOCKED decision: write-only over the API). The values
// fold env-over-DB (per field) so the admin sees the EFFECTIVE configuration with
// each field's source ("env"|"db"|"derived") and a locked flag (env-set => read-only).
type oidcAdminConfig struct {
	Enabled bool   `json:"enabled"`
	Issuer  string `json:"issuer"`
	// ClientID is the (non-secret) OAuth2 client id; surfaced verbatim so the admin
	// can confirm it. ClientIdSet folds env-or-DB so an env-only deployment shows it
	// configured even though the field is locked.
	ClientID    string `json:"clientId"`
	ClientIDSet bool   `json:"clientIdSet"`
	// ClientSecretSet is the only signal about the secret (never the value itself).
	ClientSecretSet bool `json:"clientSecretSet"`
	// RedirectURL is the configured redirect (may be empty when derived);
	// RedirectURLEffective is what the flow actually uses (configured OR derived from
	// the control base URL + /auth/callback).
	RedirectURL          string   `json:"redirectUrl"`
	RedirectURLEffective string   `json:"redirectUrlEffective"`
	AllowedEmails        []string `json:"allowedEmails"`
	AllowedDomains       []string `json:"allowedDomains"`
	AdminEmails          []string `json:"adminEmails"`

	// Per-field provenance for the GUI (source + locked). The secret has its own
	// triple since its value is never returned.
	EnabledSource        string `json:"enabledSource"`
	EnabledLocked        bool   `json:"enabledLocked"`
	IssuerSource         string `json:"issuerSource"`
	IssuerLocked         bool   `json:"issuerLocked"`
	ClientIDSource       string `json:"clientIdSource"`
	ClientIDLocked       bool   `json:"clientIdLocked"`
	ClientSecretSource   string `json:"clientSecretSource"`
	ClientSecretLocked   bool   `json:"clientSecretLocked"`
	RedirectURLSource    string `json:"redirectUrlSource"`
	RedirectURLLocked    bool   `json:"redirectUrlLocked"`
	AllowedEmailsSource  string `json:"allowedEmailsSource"`
	AllowedEmailsLocked  bool   `json:"allowedEmailsLocked"`
	AllowedDomainsSource string `json:"allowedDomainsSource"`
	AllowedDomainsLocked bool   `json:"allowedDomainsLocked"`
	AdminEmailsSource    string `json:"adminEmailsSource"`
	AdminEmailsLocked    bool   `json:"adminEmailsLocked"`

	// AuthModeLocked reports whether KOTOJI_AUTH_MODE pins the provider set (so the
	// enabled toggle is effectively read-only). Providers is the effective enabled
	// set the change would produce (e.g. ["oidc","password"]).
	AuthModeLocked bool     `json:"authModeLocked"`
	Providers      []string `json:"providers"`
}

// adminGetOIDC GET /api/admin/oidc — return the effective (env-over-DB) OIDC config
// with the client secret reduced to a "configured" boolean.
func (s *server) adminGetOIDC(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.effectiveOIDCAdminConfig(r))
}

// adminPutOIDC PUT /api/admin/oidc — persist a partial update. Body fields are all
// OPTIONAL (partial update): a nil field is left untouched. The client secret is
// WRITE-ONLY — an empty/absent secret keeps the stored one; clearClientSecret
// removes it. Validation (decision #5, fail-closed):
//   - a field whose env var is SET is REJECTED with 409 (not silently no-op'd),
//   - ENABLING OIDC requires a client id + secret present (in the request OR already
//     effective) AND at least one access gate (allowed emails OR domains) — else 422,
//   - the redirect URL, when supplied, must be an absolute http(s) URL — else 422.
//
// On success the caches are invalidated (DB read + built provider) so the change
// applies without a restart, and the same secret-safe view is returned.
func (s *server) adminPutOIDC(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled           *bool     `json:"enabled"`
		Issuer            *string   `json:"issuer"`
		ClientID          *string   `json:"clientId"`
		ClientSecret      *string   `json:"clientSecret"`
		ClearClientSecret bool      `json:"clearClientSecret"`
		RedirectURL       *string   `json:"redirectUrl"`
		AllowedEmails     *[]string `json:"allowedEmails"`
		AllowedDomains    *[]string `json:"allowedDomains"`
		AdminEmails       *[]string `json:"adminEmails"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// Resolve the CURRENT effective config so the fail-closed check can consider
	// "already configured" credentials/gates that the request does not re-supply.
	cur := s.deps.OIDC.Resolve(r.Context(), r)

	var in db.SetOIDCConfigInput

	// --- env-locked rejections (409) for every targeted field ---
	if body.Enabled != nil {
		// The enabled toggle is locked when KOTOJI_AUTH_MODE pins the provider set.
		if s.deps.OIDC.AuthModeEnvLocked() {
			writeOIDCLocked(w, "enabled", "KOTOJI_AUTH_MODE")
			return
		}
		in.Enabled = body.Enabled
	}
	if body.Issuer != nil {
		if cur.Issuer.Locked {
			writeOIDCLocked(w, "issuer", "KOTOJI_OIDC_ISSUER")
			return
		}
		in.Issuer = body.Issuer
	}
	if body.ClientID != nil {
		if cur.ClientID.Locked {
			writeOIDCLocked(w, "clientId", "KOTOJI_OIDC_CLIENT_ID")
			return
		}
		in.ClientID = body.ClientID
	}
	if body.ClientSecret != nil || body.ClearClientSecret {
		if cur.ClientSecretLck {
			writeOIDCLocked(w, "clientSecret", "KOTOJI_OIDC_CLIENT_SECRET")
			return
		}
		in.ClientSecret = body.ClientSecret
		in.ClearClientSecret = body.ClearClientSecret
	}
	if body.RedirectURL != nil {
		if cur.RedirectURL.Locked {
			writeOIDCLocked(w, "redirectUrl", "KOTOJI_OIDC_REDIRECT_URL")
			return
		}
		// A non-empty redirect must be an absolute http(s) URL; empty reverts to derived.
		if *body.RedirectURL != "" {
			if err := validateRedirectURL(*body.RedirectURL); err != "" {
				writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid redirect URL", validationDetails{Field: "redirectUrl", Reason: err})
				return
			}
		}
		in.RedirectURL = body.RedirectURL
	}
	if body.AllowedEmails != nil {
		if cur.AllowedEmails.Locked {
			writeOIDCLocked(w, "allowedEmails", "KOTOJI_OIDC_ALLOWED_EMAILS")
			return
		}
		csv := joinCSV(*body.AllowedEmails)
		in.AllowedEmails = &csv
	}
	if body.AllowedDomains != nil {
		if cur.AllowedDomains.Locked {
			writeOIDCLocked(w, "allowedDomains", "KOTOJI_OIDC_ALLOWED_DOMAINS")
			return
		}
		csv := joinCSV(*body.AllowedDomains)
		in.AllowedDomains = &csv
	}
	if body.AdminEmails != nil {
		if cur.AdminEmails.Locked {
			writeOIDCLocked(w, "adminEmails", "KOTOJI_OIDC_ADMIN_EMAILS")
			return
		}
		csv := joinCSV(*body.AdminEmails)
		in.AdminEmails = &csv
	}

	// --- fail-closed save validation (decision #5) ---
	// Determine the would-be-enabled state (request override, else current effective).
	wouldEnable := cur.Enabled.Value
	if in.Enabled != nil {
		wouldEnable = *in.Enabled
	}
	if wouldEnable {
		// Credentials: a client id + secret must be present AFTER the write — either
		// supplied now or already effective (and not being cleared).
		clientIDPresent := cur.ClientID.Value != ""
		if body.ClientID != nil {
			clientIDPresent = *body.ClientID != ""
		}
		secretPresent := cur.ClientSecretSet
		if body.ClearClientSecret {
			secretPresent = false
		}
		if body.ClientSecret != nil && *body.ClientSecret != "" {
			secretPresent = true
		}
		if !clientIDPresent || !secretPresent {
			writeError(w, http.StatusUnprocessableEntity, codeValidation,
				"enabling OIDC requires a client id and client secret",
				validationDetails{Field: "clientSecret", Reason: "client id + secret must be configured to enable OIDC"})
			return
		}
		// Access gate (FAIL-CLOSED): at least one of allowed emails / domains must be
		// present AFTER the write, else any verified account could sign in.
		emailsPresent := len(cur.AllowedEmails.Value) > 0
		if body.AllowedEmails != nil {
			emailsPresent = len(joinCSV(*body.AllowedEmails)) > 0
		}
		domainsPresent := len(cur.AllowedDomains.Value) > 0
		if body.AllowedDomains != nil {
			domainsPresent = len(joinCSV(*body.AllowedDomains)) > 0
		}
		if !emailsPresent && !domainsPresent {
			writeError(w, http.StatusUnprocessableEntity, codeValidation,
				"enabling OIDC requires an allowed-emails or allowed-domains gate (fail-closed: empty allowlists deny all sign-ins)",
				validationDetails{Field: "allowedDomains", Reason: "set allowedEmails and/or allowedDomains before enabling OIDC"})
			return
		}
	}

	// Nothing to write (no fields supplied): return the current view unchanged.
	if !oidcInputHasWrite(in) {
		writeJSON(w, http.StatusOK, s.effectiveOIDCAdminConfig(r))
		return
	}

	if err := s.deps.Store.SetOIDCConfig(r.Context(), in); err != nil {
		writeServiceError(w, err)
		return
	}

	// Invalidate BOTH caches so the next request reads the new DB value AND rebuilds
	// the OIDC provider (re-runs discovery) from the changed config.
	s.deps.OIDC.InvalidateCache()
	s.deps.OIDC.InvalidateProvider()

	// Best-effort audit (no secret values — only "what changed").
	if actor, ok := auth.CurrentUser(r.Context()); ok && actor != nil {
		s.auditBestEffort(r.Context(), gen.InsertAuditParams{
			ActorUserID: uuidPtr(actor.UserID),
			Action:      "admin.oidc.config",
			Source:      gen.AuditSourceSystem,
			Metadata: auditMeta(map[string]any{
				"enabled_set":         body.Enabled != nil,
				"issuer_set":          body.Issuer != nil,
				"client_id_set":       body.ClientID != nil,
				"client_secret_set":   body.ClientSecret != nil && *body.ClientSecret != "",
				"client_secret_clear": body.ClearClientSecret,
				"redirect_set":        body.RedirectURL != nil,
				"allowed_emails_set":  body.AllowedEmails != nil,
				"allowed_domains_set": body.AllowedDomains != nil,
				"admin_emails_set":    body.AdminEmails != nil,
			}),
		})
	}

	writeJSON(w, http.StatusOK, s.effectiveOIDCAdminConfig(r))
}

// effectiveOIDCAdminConfig resolves the effective OIDC config for the request into
// the secret-safe admin view (value + source + locked per field; secret as a boolean).
func (s *server) effectiveOIDCAdminConfig(r *http.Request) oidcAdminConfig {
	res := s.deps.OIDC.Resolve(r.Context(), r)
	// The "configured redirect" is empty when the effective value was derived; the
	// effective redirect is always the value the flow uses.
	configuredRedirect := ""
	if res.RedirectURL.Source != "derived" {
		configuredRedirect = res.RedirectURL.Value
	}
	return oidcAdminConfig{
		Enabled:              res.Enabled.Value,
		Issuer:               res.Issuer.Value,
		ClientID:             res.ClientID.Value,
		ClientIDSet:          res.ClientID.Value != "",
		ClientSecretSet:      res.ClientSecretSet,
		RedirectURL:          configuredRedirect,
		RedirectURLEffective: res.RedirectURL.Value,
		AllowedEmails:        nonNilStrings(res.AllowedEmails.Value),
		AllowedDomains:       nonNilStrings(res.AllowedDomains.Value),
		AdminEmails:          nonNilStrings(res.AdminEmails.Value),

		EnabledSource:        res.Enabled.Source,
		EnabledLocked:        res.Enabled.Locked,
		IssuerSource:         res.Issuer.Source,
		IssuerLocked:         res.Issuer.Locked,
		ClientIDSource:       res.ClientID.Source,
		ClientIDLocked:       res.ClientID.Locked,
		ClientSecretSource:   res.ClientSecretSrc,
		ClientSecretLocked:   res.ClientSecretLck,
		RedirectURLSource:    res.RedirectURL.Source,
		RedirectURLLocked:    res.RedirectURL.Locked,
		AllowedEmailsSource:  res.AllowedEmails.Source,
		AllowedEmailsLocked:  res.AllowedEmails.Locked,
		AllowedDomainsSource: res.AllowedDomains.Source,
		AllowedDomainsLocked: res.AllowedDomains.Locked,
		AdminEmailsSource:    res.AdminEmails.Source,
		AdminEmailsLocked:    res.AdminEmails.Locked,

		AuthModeLocked: s.deps.OIDC.AuthModeEnvLocked(),
		Providers:      s.deps.OIDC.Providers(r.Context(), r),
	}
}

// writeOIDCLocked emits the 409 envelope for an env-locked OIDC field edit (mirrors
// the domain handler's locked rejection so the wire shape is consistent).
func writeOIDCLocked(w http.ResponseWriter, field, envVar string) {
	writeError(w, http.StatusConflict, codeConflict,
		field+" is locked by the environment ("+envVar+" is set)",
		validationDetails{Field: field, Reason: "locked by environment"})
}

// oidcInputHasWrite reports whether the partial input would write anything.
func oidcInputHasWrite(in db.SetOIDCConfigInput) bool {
	return in.Enabled != nil || in.Issuer != nil || in.ClientID != nil ||
		in.ClientSecret != nil || in.ClearClientSecret || in.RedirectURL != nil ||
		in.AllowedEmails != nil || in.AllowedDomains != nil || in.AdminEmails != nil
}

// validateRedirectURL returns "" when s is an absolute http(s) URL with a host, else
// a human-safe reason for the 422 field detail.
func validateRedirectURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "not a valid URL"
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "must use the http or https scheme"
	}
	if u.Host == "" || u.Hostname() == "" {
		return "must include a host"
	}
	return ""
}

// joinCSV normalizes a list (trim, drop empties) and joins it for storage. The store
// layer keeps the raw CSV; the effective layer re-splits + lowercases it.
func joinCSV(in []string) string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, ",")
}

// nonNilStrings returns a non-nil slice (so the JSON renders [] not null), preserving
// the contract that the list fields are always arrays.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
