package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/necorox-com/kotoji/backend/internal/config"
)

// oidcProviderKey is the value stored in user_identities.provider for the OIDC
// path. Google is the default IdP but the key is generic so a Keycloak/Authentik
// swap keeps the same identity rows (CANONICAL §4 user_identities).
const oidcProviderKey = "oidc"

// verifiedToken is the decoded, signature-verified id_token reduced to the fields
// this package acts on. It is what the (interface) verifier yields so the
// nonce/allowlist logic can be unit-tested without constructing an *oidc.IDToken
// (whose claims payload is unexported and not test-settable).
type verifiedToken struct {
	Nonce  string
	Claims idTokenClaims
}

// tokenVerifier verifies a raw id_token (signature, iss, aud, exp) and returns
// its reduced claims. The production impl (oidcVerifier) wraps
// *oidc.IDTokenVerifier; tests inject a fake.
type tokenVerifier interface {
	Verify(ctx context.Context, rawIDToken string) (verifiedToken, error)
}

// oidcVerifier adapts the go-oidc *oidc.IDTokenVerifier to tokenVerifier: it
// verifies the token then decodes the reduced claim set.
type oidcVerifier struct {
	v *oidc.IDTokenVerifier
}

func (o *oidcVerifier) Verify(ctx context.Context, rawIDToken string) (verifiedToken, error) {
	idTok, err := o.v.Verify(ctx, rawIDToken)
	if err != nil {
		return verifiedToken{}, err
	}
	var c idTokenClaims
	if err := idTok.Claims(&c); err != nil {
		return verifiedToken{}, fmt.Errorf("auth: oidc claim decode: %w", err)
	}
	return verifiedToken{Nonce: idTok.Nonce, Claims: c}, nil
}

// codeExchanger is the minimal slice of *oauth2.Config used at callback time,
// extracted for the same testability reason.
type codeExchanger interface {
	Exchange(ctx context.Context, code string, opts ...oauth2.AuthCodeOption) (*oauth2.Token, error)
	AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string
}

// idTokenClaims is the subset of standard OIDC claims this package extracts from
// a verified id_token. JSON tags are the OIDC standard claim names.
type idTokenClaims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	HostedDomain  string `json:"hd"`
}

// OIDCProvider is the Google/OIDC AuthProvider: issuer discovery + state/nonce +
// PKCE(S256) + id_token verification + an allowlist gate (hosted-domain list
// and/or an explicit email list). It satisfies AuthProvider.
type OIDCProvider struct {
	oauth    codeExchanger
	verifier tokenVerifier

	// access is the pure, table-testable access-control policy (email allowlist
	// OR domain allowlist, honoring the verified `hd` claim; fail-closed when both
	// are empty). It also resolves the is_admin promotion decision.
	access accessPolicy
	// hdHint is the single domain (if exactly one is configured) used as the
	// Google account-chooser `hd` pre-filter. Pure UX; the hard gate is `access`.
	hdHint string
}

// compile-time guarantee.
var _ AuthProvider = (*OIDCProvider)(nil)

// NewOIDCProvider performs OIDC discovery against cfg.Issuer and wires the real
// oauth2 config + id_token verifier. It is called once at composition; the
// network round-trip (discovery + JWKS) happens here, not per request.
func NewOIDCProvider(ctx context.Context, cfg config.OIDCConfig) (*OIDCProvider, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("auth: oidc requires client id and secret")
	}
	// S2 (SSRF): discovery + JWKS are server-side fetches of an operator-supplied
	// issuer. Use a DEFANGED client (short timeout, capped redirects, and a dialer
	// that refuses connections to non-global IPs) so a malicious/misconfigured issuer
	// cannot pivot to internal endpoints (cloud metadata, localhost services). The
	// admin-write validator (api.validateOIDCIssuer) blocks the obvious cases up
	// front; this dialer is the hard, fail-closed control that also closes the DNS
	// TOCTOU window. The client is bound onto the ctx via oidc.ClientContext.
	dctx := oidc.ClientContext(ctx, ssrfSafeHTTPClient())
	provider, err := oidc.NewProvider(dctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc discovery (%s): %w", cfg.Issuer, err)
	}
	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		// openid is mandatory; email+profile give us the claims we upsert.
		Scopes: []string{oidc.ScopeOpenID, "email", "profile"},
	}
	verifier := &oidcVerifier{v: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})}

	return newOIDCProvider(oauthCfg, verifier, cfg), nil
}

// newOIDCProvider is the DI-friendly constructor: it takes the (interface)
// dependencies directly so tests build an OIDCProvider with fakes.
func newOIDCProvider(oauth codeExchanger, verifier tokenVerifier, cfg config.OIDCConfig) *OIDCProvider {
	access := newAccessPolicy(cfg.AllowedEmails, cfg.AllowedDomains, cfg.AdminEmails)
	// hd pre-filter only when exactly one domain is configured (Google's hd param
	// is a single value; with several we let the account chooser show all).
	var hdHint string
	if len(access.domains) == 1 {
		for d := range access.domains {
			hdHint = d
		}
	}
	return &OIDCProvider{
		oauth:    oauth,
		verifier: verifier,
		access:   access,
		hdHint:   hdHint,
	}
}

// Key returns the provider identifier stored in user_identities.provider.
func (p *OIDCProvider) Key() string { return oidcProviderKey }

// Interactive is always true: the OIDC flow redirects to an external IdP.
func (p *OIDCProvider) Interactive() bool { return true }

// Start builds the authorization-endpoint URL. The nonce is bound into the
// request via a query param (verified against the id_token nonce at callback);
// PKCE S256 derives the code_challenge from the caller-held verifier.
func (p *OIDCProvider) Start(state, nonce, pkceVerifier string) string {
	opts := []oauth2.AuthCodeOption{
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(pkceVerifier),
		oauth2.AccessTypeOnline,
	}
	// hd hint: pre-filter the account chooser to the Workspace domain (the hard
	// gate is still the post-exchange allowlist check).
	if p.hdHint != "" {
		opts = append(opts, oauth2.SetAuthURLParam("hd", p.hdHint))
	}
	return p.oauth.AuthCodeURL(state, opts...)
}

// Exchange trades the code for tokens, verifies the id_token (iss/aud/exp/sig via
// the verifier, nonce here), enforces the allowlist, and returns Claims.
func (p *OIDCProvider) Exchange(ctx context.Context, code, pkceVerifier, expectedNonce string) (Claims, error) {
	// PKCE: the verifier is echoed so the token endpoint can re-derive and match
	// the code_challenge sent in Start (proves the same client).
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Claims{}, fmt.Errorf("auth: oidc code exchange: %w", err)
	}

	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Claims{}, errors.New("auth: oidc response missing id_token")
	}

	vt, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return Claims{}, fmt.Errorf("auth: oidc id_token verify: %w", err)
	}

	// Replay/CSRF defense: the id_token nonce MUST equal the one minted in Start.
	if vt.Nonce != expectedNonce {
		return Claims{}, errors.New("auth: oidc nonce mismatch")
	}

	c := vt.Claims
	if c.Subject == "" || c.Email == "" {
		return Claims{}, errors.New("auth: oidc id_token missing sub/email")
	}

	claims := Claims{
		Subject:       c.Subject,
		Email:         strings.ToLower(c.Email),
		EmailVerified: c.EmailVerified,
		Name:          c.Name,
		HostedDomain:  strings.ToLower(c.HostedDomain),
	}
	// Decision #4: the email MUST be verified by the IdP before the allowlist /
	// domain gate is even consulted (an unverified address is attacker-spoofable).
	if !claims.EmailVerified {
		return Claims{}, ErrEmailNotVerified
	}
	dec := p.access.decide(claims.Email, claims.HostedDomain)
	if !dec.Allow {
		return Claims{}, fmt.Errorf("%w: %s", ErrNotAllowed, claims.Email)
	}
	// Admin promotion is carried on the claims so completeLogin can apply it via
	// the existing PromoteUserAdmin path (decision #3).
	claims.IsAdmin = dec.Admin
	return claims, nil
}

// --- S2: SSRF-safe discovery HTTP client ---

const (
	// oidcDiscoveryTimeout bounds the entire discovery + JWKS fetch. A reachable
	// public IdP answers in well under this; a hung internal endpoint (a slow-loris
	// SSRF target) is cut off rather than tying up the boot/login path.
	oidcDiscoveryTimeout = 10 * time.Second
	// oidcMaxRedirects caps redirect following so an issuer cannot bounce the client
	// through a redirect chain to smuggle a request to an internal host.
	oidcMaxRedirects = 3
)

// ssrfSafeHTTPClient builds the hardened http.Client used for OIDC discovery and
// JWKS retrieval. It (1) bounds the total time, (2) caps redirects, and (3) uses a
// control function that REFUSES to connect to any non-global IP — the fail-closed
// SSRF control that also closes the DNS-rebinding/TOCTOU window the admin-side
// hostname check cannot (the address is re-validated at actual dial time).
func ssrfSafeHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		// DialContext re-resolves the host and refuses any non-global IP AFTER DNS
		// resolution, dialing the exact vetted IP — so it blocks even a rebinding
		// issuer whose name resolved public at validation time (TOCTOU-safe).
		DialContext:           ssrfGuardedDialContext(dialer),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   oidcDiscoveryTimeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= oidcMaxRedirects {
				return fmt.Errorf("auth: oidc discovery: too many redirects (>%d)", oidcMaxRedirects)
			}
			return nil
		},
	}
}

// ssrfGuardedDialContext wraps a dialer so that every dial first re-resolves the
// host and refuses the connection unless EVERY candidate address is a global
// unicast IP. Resolving + dialing the chosen IP ourselves (rather than handing the
// hostname to the OS dialer) means the IP we vet is exactly the IP we connect to —
// no rebinding gap.
func ssrfGuardedDialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("auth: oidc discovery: bad dial address %q: %w", addr, err)
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("auth: oidc discovery: resolve %q: %w", host, err)
		}
		var lastErr error
		for _, ip := range ips {
			// FAIL-CLOSED: refuse any non-global address (loopback/link-local/private/
			// metadata). This is the hard SSRF gate the admin validator backstops.
			if !isGlobalIP(ip) {
				lastErr = fmt.Errorf("auth: oidc discovery: refusing to connect to non-public address %s", ip)
				continue
			}
			conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("auth: oidc discovery: no usable address for %q", host)
		}
		return nil, lastErr
	}
}

// isGlobalIP reports whether ip is a routable public address (not loopback,
// link-local, private, unspecified, or multicast). It mirrors api.isGlobalUnicast
// (kept local so the auth package does not import api); a small unit test pins the
// two in agreement is unnecessary because the semantics are the stdlib's.
func isGlobalIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if ip.IsPrivate() {
		return false
	}
	return true
}
