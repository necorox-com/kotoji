//go:build integration

// OIDC-config store tests against a live Postgres (same harness contract as
// github_config_integration_test.go). They prove the client secret is ENCRYPTED at
// rest (the raw setting value never equals the plaintext, and a different key cannot
// decrypt it) and that SetOIDCConfig's partial-update / empty-keeps-existing / clear
// semantics behave as documented.
//
//	KOTOJI_DATABASE_URL=postgres://kotoji:kotoji@localhost:5432/kotoji_test?sslmode=disable \
//	    go test -tags=integration ./internal/db/...
package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/secretbox"
)

// newMigratedOIDCStore opens a migrated Store with a deterministic box and clears
// any prior OIDC settings so each test starts clean. It reuses newMigratedStore from
// github_config_integration_test.go (same package) for the pool + migration + box,
// then wipes the OIDC keys.
func newMigratedOIDCStore(t *testing.T) *Store {
	t.Helper()
	s := newMigratedStore(t)
	ctx := context.Background()
	for _, k := range []string{
		SettingOIDCEnabled, SettingOIDCIssuer, SettingOIDCClientID, SettingOIDCClientSecret,
		SettingOIDCRedirectURL, SettingOIDCAllowedEmails, SettingOIDCAllowedDomains, SettingOIDCAdminEmails,
	} {
		_ = s.DeleteInstanceSetting(ctx, k)
	}
	return s
}

func TestOIDCConfigSecretEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	s := newMigratedOIDCStore(t)

	secret := "GOCSPX-super-secret-client-secret"
	enabled := true
	issuer := "https://accounts.google.com"
	clientID := "123.apps.googleusercontent.com"
	domains := "corp.com,partner.io"
	require.NoError(t, s.SetOIDCConfig(ctx, SetOIDCConfigInput{
		Enabled:        &enabled,
		Issuer:         &issuer,
		ClientID:       &clientID,
		ClientSecret:   &secret,
		AllowedDomains: &domains,
	}))

	// The raw stored secret must be ciphertext (never the plaintext).
	raw, err := s.GetInstanceSetting(ctx, SettingOIDCClientSecret)
	require.NoError(t, err)
	require.NotEqual(t, secret, raw, "client secret must be encrypted at rest")
	require.NotContains(t, raw, secret)

	// GetOIDCConfig decrypts it back + folds the plain fields.
	cfg, err := s.GetOIDCConfig(ctx)
	require.NoError(t, err)
	require.True(t, cfg.Enabled)
	require.True(t, cfg.EnabledSet)
	require.Equal(t, issuer, cfg.Issuer)
	require.True(t, cfg.IssuerSet)
	require.Equal(t, clientID, cfg.ClientID)
	require.True(t, cfg.ClientIDSet)
	require.True(t, cfg.ClientSecretSet)
	require.Equal(t, secret, cfg.ClientSecret)
	require.Equal(t, domains, cfg.AllowedDomains)

	// A Store with a DIFFERENT key cannot decrypt -> ClientSecretSet=false (no crash).
	other := NewWithPool(s.pool)
	box2, _ := secretbox.New(secretbox.ResolveKey("", "OTHER-seed", "", "", ""))
	other.SetSecretBox(box2)
	cfg2, err := other.GetOIDCConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg2.ClientSecretSet, "wrong key must yield ClientSecretSet=false")
	require.Empty(t, cfg2.ClientSecret)
}

func TestOIDCConfigEmptySecretKeepsExisting(t *testing.T) {
	ctx := context.Background()
	s := newMigratedOIDCStore(t)

	first := "secret-first"
	require.NoError(t, s.SetOIDCConfig(ctx, SetOIDCConfigInput{ClientSecret: &first}))

	// An empty ClientSecret update must NOT clobber the stored secret.
	empty := ""
	clientID := "new-client"
	require.NoError(t, s.SetOIDCConfig(ctx, SetOIDCConfigInput{ClientSecret: &empty, ClientID: &clientID}))
	cfg, err := s.GetOIDCConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, first, cfg.ClientSecret, "empty secret must keep the existing one")
	require.Equal(t, clientID, cfg.ClientID)

	// A nil ClientSecret update likewise leaves it untouched.
	enabled := false
	require.NoError(t, s.SetOIDCConfig(ctx, SetOIDCConfigInput{Enabled: &enabled}))
	cfg, err = s.GetOIDCConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, first, cfg.ClientSecret)
	require.False(t, cfg.Enabled)

	// A non-empty secret overwrites.
	second := "secret-second"
	require.NoError(t, s.SetOIDCConfig(ctx, SetOIDCConfigInput{ClientSecret: &second}))
	cfg, err = s.GetOIDCConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, second, cfg.ClientSecret)

	// ClearClientSecret removes it -> ClientSecretSet=false.
	require.NoError(t, s.SetOIDCConfig(ctx, SetOIDCConfigInput{ClearClientSecret: true}))
	cfg, err = s.GetOIDCConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg.ClientSecretSet)
	require.Empty(t, cfg.ClientSecret)
}

func TestOIDCConfigEmptyPlainFieldReverts(t *testing.T) {
	ctx := context.Background()
	s := newMigratedOIDCStore(t)

	// Set then clear a plain field (empty pointer DELETES the key -> *Set=false).
	issuer := "https://idp.example"
	require.NoError(t, s.SetOIDCConfig(ctx, SetOIDCConfigInput{Issuer: &issuer}))
	cfg, err := s.GetOIDCConfig(ctx)
	require.NoError(t, err)
	require.True(t, cfg.IssuerSet)

	empty := ""
	require.NoError(t, s.SetOIDCConfig(ctx, SetOIDCConfigInput{Issuer: &empty}))
	cfg, err = s.GetOIDCConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg.IssuerSet, "empty pointer should delete the key")
	require.Empty(t, cfg.Issuer)
}

func TestOIDCConfigAbsentKeysAreUnset(t *testing.T) {
	ctx := context.Background()
	s := newMigratedOIDCStore(t)

	cfg, err := s.GetOIDCConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg.EnabledSet, "no key => EnabledSet false (fall back to env)")
	require.False(t, cfg.ClientIDSet)
	require.False(t, cfg.ClientSecretSet)
	require.False(t, cfg.AllowedDomainsSet)
}

func TestSetOIDCConfigRefusesSecretWithoutBox(t *testing.T) {
	ctx := context.Background()
	s := newMigratedOIDCStore(t)
	s.box = nil // simulate a misconfigured instance (no secret key)

	secret := "x"
	err := s.SetOIDCConfig(ctx, SetOIDCConfigInput{ClientSecret: &secret})
	require.Error(t, err, "must refuse to store a secret with no encryption configured")
}
