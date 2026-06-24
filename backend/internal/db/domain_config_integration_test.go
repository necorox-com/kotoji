//go:build integration

// Domain-config store tests against a live Postgres (same harness contract as
// github_config_integration_test.go). They prove SetDomainConfig's partial-update
// semantics: a nil field is untouched, a non-empty pointer overwrites, and an
// empty-string pointer DELETES the key (reverting to the env/derived fallback).
//
//	KOTOJI_DATABASE_URL=postgres://kotoji:kotoji@localhost:5432/kotoji_test?sslmode=disable \
//	    go test -tags=integration ./internal/db/...
package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// newMigratedStoreForDomain reuses the GitHub harness's migrate+open flow but
// cleans the domain keys so each test starts from a known state.
func newMigratedStoreForDomain(t *testing.T) *Store {
	t.Helper()
	s := newMigratedStore(t)
	ctx := context.Background()
	for _, k := range []string{SettingBaseDomain, SettingControlBaseURL} {
		_ = s.DeleteInstanceSetting(ctx, k)
	}
	return s
}

func TestDomainConfigPartialUpdate(t *testing.T) {
	ctx := context.Background()
	s := newMigratedStoreForDomain(t)

	// Absent keys read as unset.
	cfg, err := s.GetDomainConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg.BaseDomainSet)
	require.False(t, cfg.ControlBaseURLSet)

	// Set base only; control stays unset.
	base := "hosting.example.com"
	require.NoError(t, s.SetDomainConfig(ctx, SetDomainConfigInput{BaseDomain: &base}))
	cfg, err = s.GetDomainConfig(ctx)
	require.NoError(t, err)
	require.True(t, cfg.BaseDomainSet)
	require.Equal(t, "hosting.example.com", cfg.BaseDomain)
	require.False(t, cfg.ControlBaseURLSet)

	// A nil base on the next write leaves it untouched; set control.
	ctl := "https://hosting.example.com"
	require.NoError(t, s.SetDomainConfig(ctx, SetDomainConfigInput{ControlBaseURL: &ctl}))
	cfg, err = s.GetDomainConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "hosting.example.com", cfg.BaseDomain) // unchanged
	require.True(t, cfg.ControlBaseURLSet)
	require.Equal(t, "https://hosting.example.com", cfg.ControlBaseURL)
}

func TestDomainConfigEmptyStringDeletes(t *testing.T) {
	ctx := context.Background()
	s := newMigratedStoreForDomain(t)

	base := "hosting.example.com"
	require.NoError(t, s.SetDomainConfig(ctx, SetDomainConfigInput{BaseDomain: &base}))

	// An empty-string pointer deletes the key (reverts to env/derived).
	empty := ""
	require.NoError(t, s.SetDomainConfig(ctx, SetDomainConfigInput{BaseDomain: &empty}))
	cfg, err := s.GetDomainConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg.BaseDomainSet)
	require.Equal(t, "", cfg.BaseDomain)
}
