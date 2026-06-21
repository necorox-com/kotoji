package mcpserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validPlaintext builds a well-formed kotoji PAT with a unique, >=12-char suffix.
func validPlaintext(suffix string) string {
	return tokenPrefix + suffix + "0123456789abcdef"
}

func TestVerifier_Valid(t *testing.T) {
	store := newFakeTokenStore()
	siteID, userID, tokenID := uuid.New(), uuid.New(), uuid.New()
	pt := store.seedToken(validPlaintext("aaaa"), siteID, userID, tokenID, tokenOpts{
		scopes:           []string{"read", "write", "publish"},
		creatorCanCreate: true,
		canCreateSites:   true,
	})

	v := NewVerifier(store)
	// Make touch synchronous for deterministic assertion.
	done := make(chan uuid.UUID, 1)
	v.touch = func(id uuid.UUID) { done <- id }

	info, err := v.Verify(context.Background(), pt, nil)
	require.NoError(t, err)
	require.NotNil(t, info)

	c, ok := claimsFromTokenInfo(info)
	require.True(t, ok)
	assert.Equal(t, siteID, c.SiteID)
	assert.Equal(t, userID, c.UserID)
	assert.Equal(t, tokenID, c.TokenID)
	assert.ElementsMatch(t, []string{"read", "write", "publish"}, c.Scopes)
	assert.True(t, c.CanCreateSites, "effective can_create = token AND creator flags")
	assert.Equal(t, userID.String(), info.UserID)
	assert.False(t, info.Expiration.IsZero(), "NULL expiry maps to a far-future sentinel (SDK requires non-zero)")

	select {
	case id := <-done:
		assert.Equal(t, tokenID, id, "last_used_at touch fired with the token id")
	case <-time.After(time.Second):
		t.Fatal("touch was not invoked")
	}
}

func TestVerifier_CanCreate_CappedByCreator(t *testing.T) {
	store := newFakeTokenStore()
	siteID, userID, tokenID := uuid.New(), uuid.New(), uuid.New()
	// Token says it can create, but the creating user cannot → effective false.
	pt := store.seedToken(validPlaintext("bbbb"), siteID, userID, tokenID, tokenOpts{
		scopes:           []string{"read", "write"},
		canCreateSites:   true,
		creatorCanCreate: false,
	})
	v := NewVerifier(store)
	v.touch = func(uuid.UUID) {}

	info, err := v.Verify(context.Background(), pt, nil)
	require.NoError(t, err)
	c, _ := claimsFromTokenInfo(info)
	assert.False(t, c.CanCreateSites, "capability capped by the creating user's flag")
}

func TestVerifier_Malformed(t *testing.T) {
	store := newFakeTokenStore()
	v := NewVerifier(store)
	v.touch = func(uuid.UUID) {}

	for _, tok := range []string{"", "bearer-nonsense", "github_pat_xxx", tokenPrefix[:5]} {
		_, err := v.Verify(context.Background(), tok, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, auth.ErrInvalidToken, "malformed %q unwraps to ErrInvalidToken (→401)", tok)
	}
}

func TestVerifier_UnknownHash(t *testing.T) {
	store := newFakeTokenStore()
	siteID, userID, tokenID := uuid.New(), uuid.New(), uuid.New()
	// Seed a token sharing the prefix but with a different plaintext (hash differs).
	good := validPlaintext("cccc")
	store.seedToken(good, siteID, userID, tokenID, tokenOpts{scopes: []string{"read"}})

	// Same 12-char prefix, different remainder → hash mismatch.
	attacker := good[:prefixLen] + "ZZZZZZZZZZZZZZZZ"
	v := NewVerifier(store)
	v.touch = func(uuid.UUID) {}

	_, err := v.Verify(context.Background(), attacker, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestVerifier_Revoked(t *testing.T) {
	store := newFakeTokenStore()
	siteID, userID, tokenID := uuid.New(), uuid.New(), uuid.New()
	// revoked tokens are not returned by the query → treated as unknown (401).
	pt := store.seedToken(validPlaintext("dddd"), siteID, userID, tokenID, tokenOpts{
		scopes:  []string{"read"},
		revoked: true,
	})
	v := NewVerifier(store)
	v.touch = func(uuid.UUID) {}

	_, err := v.Verify(context.Background(), pt, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestVerifier_Expired(t *testing.T) {
	store := newFakeTokenStore()
	siteID, userID, tokenID := uuid.New(), uuid.New(), uuid.New()
	past := time.Now().Add(-time.Hour)
	pt := store.seedToken(validPlaintext("eeee"), siteID, userID, tokenID, tokenOpts{
		scopes:    []string{"read"},
		expiresAt: &past,
	})
	v := NewVerifier(store)
	v.touch = func(uuid.UUID) {}
	// Fix the clock to "now" so the past expiry is unambiguously stale.
	v.clock = func() time.Time { return time.Now() }

	_, err := v.Verify(context.Background(), pt, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrInvalidToken, "expired token → 401")
}

func TestVerifier_InactiveCreator_Rejected(t *testing.T) {
	store := newFakeTokenStore()
	siteID, userID, tokenID := uuid.New(), uuid.New(), uuid.New()
	// An inactive creating user means the DB query returns no row → 401.
	pt := store.seedToken(validPlaintext("hhhh"), siteID, userID, tokenID, tokenOpts{
		scopes:          []string{"read"},
		creatorInactive: true,
	})
	v := NewVerifier(store)
	v.touch = func(uuid.UUID) {}

	_, err := v.Verify(context.Background(), pt, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestVerifier_DBError_Is500Class(t *testing.T) {
	store := newFakeTokenStore()
	store.prefixErr = errors.New("connection refused")
	v := NewVerifier(store)
	v.touch = func(uuid.UUID) {}

	_, err := v.Verify(context.Background(), validPlaintext("ffff"), nil)
	require.Error(t, err)
	// Infra error must NOT unwrap to ErrInvalidToken (the SDK maps it to 500).
	assert.NotErrorIs(t, err, auth.ErrInvalidToken)
}

func TestVerifier_TouchNeverBlocksOnError(t *testing.T) {
	store := newFakeTokenStore()
	store.touchErr = errors.New("touch boom")
	siteID, userID, tokenID := uuid.New(), uuid.New(), uuid.New()
	pt := store.seedToken(validPlaintext("gggg"), siteID, userID, tokenID, tokenOpts{scopes: []string{"read"}})

	v := NewVerifier(store) // default async touch
	info, err := v.Verify(context.Background(), pt, nil)
	require.NoError(t, err, "a failing last_used_at bump must not fail verification")
	require.NotNil(t, info)
}
