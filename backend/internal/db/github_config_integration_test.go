//go:build integration

// GitHub-config store tests against a live Postgres (same harness contract as
// migrations_integration_test.go). They prove the token is ENCRYPTED at rest
// (the raw setting value never equals the plaintext, and a different key cannot
// decrypt it) and that SetGitHubConfig's partial-update / empty-keeps-existing /
// clear semantics behave as documented.
//
//	KOTOJI_DATABASE_URL=postgres://kotoji:kotoji@localhost:5432/kotoji_test?sslmode=disable \
//	    go test -tags=integration ./internal/db/...
package db

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/migrate"
	"github.com/necorox-com/kotoji/backend/internal/secretbox"
)

// newMigratedStore opens a Store against the test DB, migrates fully up, and
// installs a deterministic secret box. It skips when no DB URL is configured.
func newMigratedStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("KOTOJI_DATABASE_URL")
	if dsn == "" {
		t.Skip("KOTOJI_DATABASE_URL not set; skipping live GitHub-config test")
	}
	ctx := context.Background()
	require.NoError(t, migrate.Run(ctx, dsn, slog.New(slog.NewTextHandler(io.Discard, nil))))

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	s := NewWithPool(pool)
	box, err := secretbox.New(secretbox.ResolveKey("", "test-seed", "", "", ""))
	require.NoError(t, err)
	s.SetSecretBox(box)

	// Clean any prior GitHub settings so the test starts from a known state.
	for _, k := range []string{SettingGitHubMirrorEnabled, SettingGitHubOrg, SettingGitHubWebhookSecret, SettingGitHubToken} {
		_ = s.DeleteInstanceSetting(ctx, k)
	}
	return s
}

func TestGitHubConfigEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	s := newMigratedStore(t)

	plaintext := "ghp_supersecrettoken1234567890"
	enabled := true
	org := "necorox-com"
	require.NoError(t, s.SetGitHubConfig(ctx, SetGitHubConfigInput{
		Enabled: &enabled,
		Org:     &org,
		Token:   &plaintext,
	}))

	// The raw stored value must be ciphertext (never the plaintext).
	raw, err := s.GetInstanceSetting(ctx, SettingGitHubToken)
	require.NoError(t, err)
	require.NotEqual(t, plaintext, raw, "token must be encrypted at rest")
	require.NotContains(t, raw, plaintext)

	// GetGitHubConfig decrypts it back.
	cfg, err := s.GetGitHubConfig(ctx)
	require.NoError(t, err)
	require.True(t, cfg.Enabled)
	require.True(t, cfg.EnabledSet)
	require.Equal(t, org, cfg.Org)
	require.True(t, cfg.TokenSet)
	require.Equal(t, plaintext, cfg.Token)

	// A Store with a DIFFERENT key cannot decrypt -> TokenSet=false (no crash).
	other := NewWithPool(s.pool)
	box2, _ := secretbox.New(secretbox.ResolveKey("", "OTHER-seed", "", "", ""))
	other.SetSecretBox(box2)
	cfg2, err := other.GetGitHubConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg2.TokenSet, "wrong key must yield TokenSet=false")
	require.Empty(t, cfg2.Token)
}

func TestGitHubConfigEmptyTokenKeepsExisting(t *testing.T) {
	ctx := context.Background()
	s := newMigratedStore(t)

	first := "ghp_first"
	require.NoError(t, s.SetGitHubConfig(ctx, SetGitHubConfigInput{Token: &first}))

	// An empty Token update must NOT clobber the stored token.
	empty := ""
	org := "acme"
	require.NoError(t, s.SetGitHubConfig(ctx, SetGitHubConfigInput{Token: &empty, Org: &org}))
	cfg, err := s.GetGitHubConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, first, cfg.Token, "empty token must keep the existing one")
	require.Equal(t, org, cfg.Org)

	// A nil Token update likewise leaves it untouched.
	enabled := false
	require.NoError(t, s.SetGitHubConfig(ctx, SetGitHubConfigInput{Enabled: &enabled}))
	cfg, err = s.GetGitHubConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, first, cfg.Token)
	require.False(t, cfg.Enabled)

	// A non-empty token overwrites.
	second := "ghp_second"
	require.NoError(t, s.SetGitHubConfig(ctx, SetGitHubConfigInput{Token: &second}))
	cfg, err = s.GetGitHubConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, second, cfg.Token)

	// ClearToken removes it -> TokenSet=false.
	require.NoError(t, s.SetGitHubConfig(ctx, SetGitHubConfigInput{ClearToken: true}))
	cfg, err = s.GetGitHubConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg.TokenSet)
	require.Empty(t, cfg.Token)
}

func TestGitHubConfigAbsentKeysAreUnset(t *testing.T) {
	ctx := context.Background()
	s := newMigratedStore(t)

	cfg, err := s.GetGitHubConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg.EnabledSet, "no key => EnabledSet false (fall back to env)")
	require.False(t, cfg.Enabled)
	require.False(t, cfg.TokenSet)
	require.Empty(t, cfg.Org)
	require.Empty(t, cfg.WebhookSecret)
}

func TestSetGitHubConfigRefusesTokenWithoutBox(t *testing.T) {
	ctx := context.Background()
	s := newMigratedStore(t)
	s.box = nil // simulate a misconfigured instance (no secret key)

	tok := "ghp_x"
	err := s.SetGitHubConfig(ctx, SetGitHubConfigInput{Token: &tok})
	require.Error(t, err, "must refuse to store a token with no encryption configured")
}
