package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
// PKCE(S256) + id_token verification + an allowlist gate (hd domain and/or an
// explicit email list). It satisfies AuthProvider.
type OIDCProvider struct {
	oauth    codeExchanger
	verifier tokenVerifier

	// allowedDomain restricts logins to a single Google Workspace `hd` domain
	// ("" disables the domain gate).
	allowedDomain string
	// allowedEmails is an explicit allowlist (lowercased); empty disables it.
	allowedEmails map[string]struct{}
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
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
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
	allowed := make(map[string]struct{}, len(cfg.AllowedEmails))
	for _, e := range cfg.AllowedEmails {
		allowed[strings.ToLower(strings.TrimSpace(e))] = struct{}{}
	}
	return &OIDCProvider{
		oauth:         oauth,
		verifier:      verifier,
		allowedDomain: strings.ToLower(strings.TrimSpace(cfg.AllowedDomain)),
		allowedEmails: allowed,
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
	if p.allowedDomain != "" {
		opts = append(opts, oauth2.SetAuthURLParam("hd", p.allowedDomain))
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
	if err := p.checkAllowlist(claims); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

// checkAllowlist enforces the domain and/or email allowlist. The gates are OR'd:
// a login is permitted if it matches the hd domain OR the explicit email list.
// With neither configured this returns an error (fail-closed) — config.validate
// already requires at least one in oidc mode, so this is defense in depth.
func (p *OIDCProvider) checkAllowlist(c Claims) error {
	if p.allowedDomain == "" && len(p.allowedEmails) == 0 {
		return errors.New("auth: no allowlist configured; refusing login")
	}
	if p.allowedDomain != "" {
		// Prefer the verified `hd` claim; fall back to the email's domain part so
		// a non-Workspace IdP (no hd) can still gate on the address domain.
		if c.HostedDomain == p.allowedDomain || emailDomain(c.Email) == p.allowedDomain {
			return nil
		}
	}
	if len(p.allowedEmails) > 0 {
		if _, ok := p.allowedEmails[c.Email]; ok {
			return nil
		}
	}
	return fmt.Errorf("auth: %s is not on the allowlist", c.Email)
}

// emailDomain returns the lowercased domain part of an email, "" if malformed.
func emailDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}
