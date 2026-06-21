package serve

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

// Preview-authz sentinel errors (routing-and-serving.md §8.1). The static handler
// maps both to the configured PreviewUnauthStatus (404 by default, to avoid
// leaking branch names — fail CLOSED).
var (
	// ErrUnauthorized: no/invalid credential.
	ErrUnauthorized = errors.New("serve: preview unauthorized")
	// ErrForbidden: valid identity but not permitted for THIS site (e.g. a grant
	// minted for site A presented on site B).
	ErrForbidden = errors.New("serve: preview forbidden")
)

// PreviewCookieName is the host-only preview cookie (no Domain attribute =>
// host-only => preserves per-origin isolation, CANONICAL decision #7 / §8.1).
const PreviewCookieName = "kotoji_preview"

// PreviewGrantQueryParam is the one-time signed grant the control plane's
// "Preview" button hands off via a 302 to the preview origin (§8.1 path 2).
const PreviewGrantQueryParam = "kpt"

// PreviewAuthz authorizes a request to view a PREVIEW target. Published targets
// are PUBLIC and are NEVER run through this (enforced by the static handler).
type PreviewAuthz interface {
	// Authorize returns nil if the request may view this preview target. It
	// returns ErrUnauthorized (no/invalid credential) or ErrForbidden (valid
	// credential, wrong site). It may also instruct the handler to SET a host-only
	// cookie and redirect (Action), e.g. after validating a one-time grant param.
	Authorize(ctx context.Context, t resolve.Target, r *http.Request, siteID uuid.UUID) (Action, error)
}

// Action is the optional side effect an authorizer asks the handler to perform.
// The zero value (ActionNone) means "just serve".
type Action struct {
	// SetCookie, when non-nil, is set on the response before redirect/serve. It is
	// a host-only (no Domain), HttpOnly, Secure(per cfg), SameSite=Lax cookie.
	SetCookie *http.Cookie
	// RedirectTo, when non-empty, makes the handler emit a 302 to this URL (used to
	// strip the one-time ?kpt grant param after setting the cookie).
	RedirectTo string
}

// ---- No-auth mode ----

// OpenPreviewAuthz authorizes every preview (dev/no-auth mode, §8.3). Documented
// as dev-only.
type OpenPreviewAuthz struct{}

var _ PreviewAuthz = OpenPreviewAuthz{}

// Authorize always permits the preview.
func (OpenPreviewAuthz) Authorize(context.Context, resolve.Target, *http.Request, uuid.UUID) (Action, error) {
	return Action{}, nil
}

// DenyPreviewAuthz rejects every preview (fail-closed default if no authorizer is
// wired). Returns ErrUnauthorized so the handler 404s.
type DenyPreviewAuthz struct{}

var _ PreviewAuthz = DenyPreviewAuthz{}

// Authorize always denies.
func (DenyPreviewAuthz) Authorize(context.Context, resolve.Target, *http.Request, uuid.UUID) (Action, error) {
	return Action{}, ErrUnauthorized
}

// ---- Signed preview-grant -> host-only cookie (the primary mechanism, §8.1.2) ----

// BearerValidator is an OPTIONAL seam for path-1.5 of §8.1: a scoped per-site
// bearer token (Authorization: Bearer <token>) accepted for programmatic/MCP
// preview. nil disables bearer acceptance.
type BearerValidator interface {
	// ValidateBearer returns nil if the bearer token grants preview access to
	// siteID; ErrForbidden for a valid-but-wrong-site token; ErrUnauthorized
	// otherwise.
	ValidateBearer(ctx context.Context, token string, siteID uuid.UUID) error
}

// GrantAuthzConfig configures the signed-grant preview authorizer.
type GrantAuthzConfig struct {
	// Secret is the HMAC-SHA256 key for grant signatures. REQUIRED (non-empty).
	Secret []byte
	// CookieSecure sets the Secure attribute on the issued cookie (true in prod,
	// false on plain-HTTP dev — routing-and-serving.md §10).
	CookieSecure bool
	// CookieTTL bounds the host-only cookie lifetime. Default 12h if zero.
	CookieTTL time.Duration
	// Now is the injected clock (tests). Defaults to time.Now.
	Now func() time.Time
	// Bearer is the optional scoped-bearer-token validator (§8.1 path 2 bearer arm).
	Bearer BearerValidator
}

const defaultPreviewCookieTTL = 12 * time.Hour

// GrantAuthz implements the signed preview-grant -> host-only cookie flow plus
// optional scoped bearer tokens. It is the PRIMARY preview mechanism
// (routing-and-serving.md §8.1.2): it never relies on a domain-wide cookie, so
// per-origin isolation stays intact (CANONICAL decision #7).
type GrantAuthz struct {
	cfg GrantAuthzConfig
}

var _ PreviewAuthz = (*GrantAuthz)(nil)

// NewGrantAuthz constructs a GrantAuthz. It returns an error if Secret is empty
// (fail-closed: a misconfigured signer must not silently allow previews).
func NewGrantAuthz(cfg GrantAuthzConfig) (*GrantAuthz, error) {
	if len(cfg.Secret) == 0 {
		return nil, errors.New("serve: preview grant secret is required")
	}
	if cfg.CookieTTL <= 0 {
		cfg.CookieTTL = defaultPreviewCookieTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &GrantAuthz{cfg: cfg}, nil
}

// Grant is the decoded grant payload: which site+branch the grant authorizes and
// when it expires. Signed as "<uuid>:<branch>:<expUnix>:<sigB64url>".
type Grant struct {
	SiteID uuid.UUID
	Branch string
	Exp    time.Time
}

// SignGrant produces a one-time grant string the control plane appends as ?kpt=.
// It is exported so the control plane (preview-grant endpoint) shares ONE codec
// with the data plane — no format drift.
func (a *GrantAuthz) SignGrant(siteID uuid.UUID, branch string, exp time.Time) string {
	payload := siteID.String() + ":" + branch + ":" + strconv.FormatInt(exp.Unix(), 10)
	sig := a.mac(payload)
	return payload + ":" + base64.RawURLEncoding.EncodeToString(sig)
}

// mac computes the HMAC-SHA256 over a payload.
func (a *GrantAuthz) mac(payload string) []byte {
	m := hmac.New(sha256.New, a.cfg.Secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// verifyGrant parses + verifies a signed grant token (constant-time). It returns
// ErrUnauthorized on any structural/signature/expiry failure (fail-closed).
func (a *GrantAuthz) verifyGrant(token string) (Grant, error) {
	// payload has exactly 3 colon-separated fields; the signature is the 4th.
	// Split from the RIGHT once to isolate the signature, then split the payload.
	lastColon := strings.LastIndexByte(token, ':')
	if lastColon < 0 {
		return Grant{}, ErrUnauthorized
	}
	payload, sigB64 := token[:lastColon], token[lastColon+1:]
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return Grant{}, ErrUnauthorized
	}
	expected := a.mac(payload)
	if subtle.ConstantTimeCompare(sig, expected) != 1 {
		return Grant{}, ErrUnauthorized
	}
	parts := strings.Split(payload, ":")
	if len(parts) != 3 {
		return Grant{}, ErrUnauthorized
	}
	id, err := uuid.Parse(parts[0])
	if err != nil {
		return Grant{}, ErrUnauthorized
	}
	expUnix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Grant{}, ErrUnauthorized
	}
	exp := time.Unix(expUnix, 0)
	if !a.cfg.Now().Before(exp) {
		return Grant{}, ErrUnauthorized // expired
	}
	return Grant{SiteID: id, Branch: parts[1], Exp: exp}, nil
}

// Authorize implements PreviewAuthz. Order of credential acceptance:
//  1. One-time ?kpt grant param: verify -> set host-only cookie -> 302 strip kpt.
//  2. Existing kotoji_preview cookie: verify, must match this site+branch.
//  3. Authorization: Bearer <token> (if a BearerValidator is wired).
//
// Any failure returns ErrUnauthorized/ErrForbidden so the handler 404s (no leak).
func (a *GrantAuthz) Authorize(ctx context.Context, t resolve.Target, r *http.Request, siteID uuid.UUID) (Action, error) {
	// 1. One-time grant param.
	if raw := r.URL.Query().Get(PreviewGrantQueryParam); raw != "" {
		g, err := a.verifyGrant(raw)
		if err != nil {
			return Action{}, err
		}
		if g.SiteID != siteID || !branchMatches(g.Branch, t.Branch) {
			return Action{}, ErrForbidden
		}
		// Re-sign a cookie value scoped to this site+branch with the cookie TTL.
		cookieVal := a.SignGrant(siteID, t.Branch, a.cfg.Now().Add(a.cfg.CookieTTL))
		return Action{
			SetCookie: a.newCookie(cookieVal),
			// Use the path-relative request URI so the redirect stays same-origin
			// (correct under both Host and path mode).
			RedirectTo: stripGrantParam(r.URL.RequestURI()),
		}, nil
	}

	// 2. Existing host-only cookie.
	if ck, err := r.Cookie(PreviewCookieName); err == nil && ck.Value != "" {
		g, verr := a.verifyGrant(ck.Value)
		if verr == nil {
			if g.SiteID != siteID || !branchMatches(g.Branch, t.Branch) {
				return Action{}, ErrForbidden // a cookie for site A is rejected on site B
			}
			return Action{}, nil
		}
		// fall through to bearer; a stale cookie is not fatal on its own.
	}

	// 3. Scoped bearer token.
	if a.cfg.Bearer != nil {
		if tok := bearerToken(r); tok != "" {
			if err := a.cfg.Bearer.ValidateBearer(ctx, tok, siteID); err != nil {
				return Action{}, err
			}
			return Action{}, nil
		}
	}

	return Action{}, ErrUnauthorized
}

// newCookie builds the host-only (no Domain) preview cookie (§8.1.2 / decision #7).
func (a *GrantAuthz) newCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     PreviewCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(a.cfg.CookieTTL.Seconds()),
		// Deliberately NO Domain attribute => host-only => per-origin isolation.
	}
}

// ---- helpers ----

// branchMatches compares a grant's branch against the resolved target branch.
func branchMatches(grantBranch, targetBranch string) bool {
	return grantBranch == targetBranch
}

// bearerToken extracts a Bearer token from the Authorization header (case-insensitive scheme).
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// stripGrantParam removes the one-time kpt param from a URL so it is not retained
// in history/referrer after the cookie is set.
func stripGrantParam(rawURL string) string {
	i := strings.IndexByte(rawURL, '?')
	if i < 0 {
		return rawURL
	}
	base := rawURL[:i]
	q := rawURL[i+1:]
	var kept []string
	for _, pair := range strings.Split(q, "&") {
		if pair == "" {
			continue
		}
		key := pair
		if eq := strings.IndexByte(pair, '='); eq >= 0 {
			key = pair[:eq]
		}
		if key == PreviewGrantQueryParam {
			continue
		}
		kept = append(kept, pair)
	}
	if len(kept) == 0 {
		return base
	}
	return base + "?" + strings.Join(kept, "&")
}
