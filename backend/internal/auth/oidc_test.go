package auth

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/oauth2"

	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/config"
)

// fakeExchanger is a programmable codeExchanger.
type fakeExchanger struct {
	token    *oauth2.Token
	err      error
	authURL  string
	gotState string
	gotOpts  int
}

func (f *fakeExchanger) Exchange(_ context.Context, _ string, opts ...oauth2.AuthCodeOption) (*oauth2.Token, error) {
	f.gotOpts = len(opts)
	if f.err != nil {
		return nil, f.err
	}
	return f.token, nil
}

func (f *fakeExchanger) AuthCodeURL(state string, _ ...oauth2.AuthCodeOption) string {
	f.gotState = state
	return f.authURL
}

// fakeVerifier is a programmable tokenVerifier.
type fakeVerifier struct {
	out verifiedToken
	err error
}

func (f *fakeVerifier) Verify(_ context.Context, _ string) (verifiedToken, error) {
	if f.err != nil {
		return verifiedToken{}, f.err
	}
	return f.out, nil
}

// tokenWithID builds an oauth2 token carrying an id_token extra.
func tokenWithID(idToken string) *oauth2.Token {
	return (&oauth2.Token{AccessToken: "at"}).WithExtra(map[string]any{"id_token": idToken})
}

func TestOIDC_Start(t *testing.T) {
	ex := &fakeExchanger{authURL: "https://idp/auth"}
	p := newOIDCProvider(ex, &fakeVerifier{}, config.OIDCConfig{AllowedDomains: []string{"corp.com"}})

	url := p.Start("the-state", "the-nonce", "the-verifier")
	require.Equal(t, "https://idp/auth", url)
	require.Equal(t, "the-state", ex.gotState)
	require.True(t, p.Interactive())
	require.Equal(t, oidcProviderKey, p.Key())
}

func TestOIDC_Exchange(t *testing.T) {
	const nonce = "expected-nonce"

	tests := []struct {
		name     string
		cfg      config.OIDCConfig
		exchange *fakeExchanger
		verify   *fakeVerifier
		wantErr  string
	}{
		{
			name:     "domain allowlist accepts matching hd",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com"}},
			exchange: &fakeExchanger{token: tokenWithID("idtok")},
			verify: &fakeVerifier{out: verifiedToken{
				Nonce:  nonce,
				Claims: idTokenClaims{Subject: "s1", Email: "a@corp.com", EmailVerified: true, HostedDomain: "corp.com"},
			}},
		},
		{
			name:     "domain allowlist accepts via email domain fallback",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com"}},
			exchange: &fakeExchanger{token: tokenWithID("idtok")},
			verify: &fakeVerifier{out: verifiedToken{
				Nonce:  nonce,
				Claims: idTokenClaims{Subject: "s1", Email: "a@corp.com", EmailVerified: true},
			}},
		},
		{
			name:     "email allowlist accepts listed email",
			cfg:      config.OIDCConfig{AllowedEmails: []string{"VIP@other.com"}},
			exchange: &fakeExchanger{token: tokenWithID("idtok")},
			verify: &fakeVerifier{out: verifiedToken{
				Nonce:  nonce,
				Claims: idTokenClaims{Subject: "s2", Email: "vip@other.com", EmailVerified: true},
			}},
		},
		{
			name:     "allowlist rejects non-matching domain",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com"}},
			exchange: &fakeExchanger{token: tokenWithID("idtok")},
			verify: &fakeVerifier{out: verifiedToken{
				Nonce:  nonce,
				Claims: idTokenClaims{Subject: "s3", Email: "intruder@evil.com", EmailVerified: true, HostedDomain: "evil.com"},
			}},
			wantErr: "not allowed",
		},
		{
			name:     "unverified email rejected before allowlist (decision #4)",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com"}},
			exchange: &fakeExchanger{token: tokenWithID("idtok")},
			verify: &fakeVerifier{out: verifiedToken{
				Nonce:  nonce,
				Claims: idTokenClaims{Subject: "s5", Email: "a@corp.com", EmailVerified: false, HostedDomain: "corp.com"},
			}},
			wantErr: "email not verified",
		},
		{
			name:     "multi-domain allowlist accepts second domain",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com", "partner.io"}},
			exchange: &fakeExchanger{token: tokenWithID("idtok")},
			verify: &fakeVerifier{out: verifiedToken{
				Nonce:  nonce,
				Claims: idTokenClaims{Subject: "s6", Email: "p@partner.io", EmailVerified: true},
			}},
		},
		{
			name:     "nonce mismatch rejected",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com"}},
			exchange: &fakeExchanger{token: tokenWithID("idtok")},
			verify: &fakeVerifier{out: verifiedToken{
				Nonce:  "WRONG",
				Claims: idTokenClaims{Subject: "s4", Email: "a@corp.com"},
			}},
			wantErr: "nonce mismatch",
		},
		{
			name:     "missing id_token rejected",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com"}},
			exchange: &fakeExchanger{token: &oauth2.Token{AccessToken: "at"}},
			verify:   &fakeVerifier{},
			wantErr:  "missing id_token",
		},
		{
			name:     "code exchange failure propagates",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com"}},
			exchange: &fakeExchanger{err: errors.New("boom")},
			verify:   &fakeVerifier{},
			wantErr:  "code exchange",
		},
		{
			name:     "verify failure propagates",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com"}},
			exchange: &fakeExchanger{token: tokenWithID("idtok")},
			verify:   &fakeVerifier{err: errors.New("bad sig")},
			wantErr:  "id_token verify",
		},
		{
			name:     "missing sub/email rejected",
			cfg:      config.OIDCConfig{AllowedDomains: []string{"corp.com"}},
			exchange: &fakeExchanger{token: tokenWithID("idtok")},
			verify: &fakeVerifier{out: verifiedToken{
				Nonce:  nonce,
				Claims: idTokenClaims{Subject: "", Email: ""},
			}},
			wantErr: "missing sub/email",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newOIDCProvider(tc.exchange, tc.verify, tc.cfg)
			claims, err := p.Exchange(context.Background(), "code", "verifier", nonce)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, claims.Subject)
			require.NotEmpty(t, claims.Email)
			require.Equal(t, claims.Email, lower(claims.Email)) // emails are lowercased
		})
	}
}

func TestOIDC_AllowlistNoneConfigured(t *testing.T) {
	// Defense in depth: with neither gate AND a verified email, the access policy
	// fails closed (decision #2). config.validate rejects this at boot, so this is
	// the belt-and-suspenders check at the provider seam.
	p := newOIDCProvider(
		&fakeExchanger{token: tokenWithID("idtok")},
		&fakeVerifier{out: verifiedToken{Nonce: "n", Claims: idTokenClaims{Subject: "s", Email: "a@b.com", EmailVerified: true}}},
		config.OIDCConfig{},
	)
	_, err := p.Exchange(context.Background(), "code", "verifier", "n")
	require.ErrorIs(t, err, ErrNotAllowed)
}

// TestOIDC_AdminEmailPromotion: a verified email in AdminEmails yields
// claims.IsAdmin=true; a non-listed allowed email yields false.
func TestOIDC_AdminEmailPromotion(t *testing.T) {
	cfg := config.OIDCConfig{
		AllowedDomains: []string{"corp.com"},
		AdminEmails:    []string{"boss@corp.com"},
	}
	mk := func(email string) *OIDCProvider {
		return newOIDCProvider(
			&fakeExchanger{token: tokenWithID("idtok")},
			&fakeVerifier{out: verifiedToken{Nonce: "n", Claims: idTokenClaims{Subject: "s", Email: email, EmailVerified: true, HostedDomain: "corp.com"}}},
			cfg,
		)
	}
	admin, err := mk("boss@corp.com").Exchange(context.Background(), "c", "v", "n")
	require.NoError(t, err)
	require.True(t, admin.IsAdmin)

	normal, err := mk("worker@corp.com").Exchange(context.Background(), "c", "v", "n")
	require.NoError(t, err)
	require.False(t, normal.IsAdmin)
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + ('a' - 'A')
		}
	}
	return string(out)
}
