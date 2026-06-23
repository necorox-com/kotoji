package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/necorox-com/kotoji/backend/internal/config"
)

func TestDevProvider(t *testing.T) {
	p := NewDevProvider(config.Config{AdminEmail: "Admin@Kotoji.Local"})
	require.Equal(t, devProviderKey, p.Key())
	require.False(t, p.Interactive())
	require.Empty(t, p.Start("s", "n", "v"))

	claims, err := p.Exchange(context.Background(), "ignored", "", "")
	require.NoError(t, err)
	require.Equal(t, devSubject, claims.Subject)
	require.Equal(t, "admin@kotoji.local", claims.Email) // lowercased
	require.True(t, claims.EmailVerified)
}

func TestPasswordProvider(t *testing.T) {
	// nil store => env-only credential (the classic configured-password path).
	p, err := NewPasswordProvider(config.Config{AdminPassword: "correct horse", AdminEmail: "admin@kotoji.local"}, nil)
	require.NoError(t, err)
	require.Equal(t, passwordProviderKey, p.Key())
	require.False(t, p.Interactive())

	// Correct password -> claims.
	claims, err := p.Exchange(context.Background(), "correct horse", "", "")
	require.NoError(t, err)
	require.Equal(t, "admin@kotoji.local", claims.Email)
	require.Equal(t, devSubject, claims.Subject)

	// Wrong password -> ErrBadPassword (constant-time bcrypt compare).
	_, err = p.Exchange(context.Background(), "wrong", "", "")
	require.ErrorIs(t, err, ErrBadPassword)
}

// TestPasswordProvider_EmptyEnvPasswordAllowed: an empty env password is the
// valid first-run state, not an error. With no DB hash either, Exchange refuses
// every password (no credential configured yet).
func TestPasswordProvider_EmptyEnvPasswordAllowed(t *testing.T) {
	p, err := NewPasswordProvider(config.Config{AdminEmail: "admin@kotoji.local"}, nil)
	require.NoError(t, err)
	_, err = p.Exchange(context.Background(), "anything", "", "")
	require.ErrorIs(t, err, ErrBadPassword)
}

// TestPasswordProvider_DBHashTakesPrecedence: when the store has a hash, it is
// verified INSTEAD of the env password (first-run setup credential wins).
func TestPasswordProvider_DBHashTakesPrecedence(t *testing.T) {
	dbHash, err := bcrypt.GenerateFromPassword([]byte("db-password"), bcrypt.MinCost)
	require.NoError(t, err)
	store := &fakeAdminHashStore{hash: string(dbHash), found: true}

	// env password differs from the DB password.
	p, err := NewPasswordProvider(config.Config{AdminPassword: "env-password", AdminEmail: "a@b.c"}, store)
	require.NoError(t, err)

	// DB password is accepted...
	_, err = p.Exchange(context.Background(), "db-password", "", "")
	require.NoError(t, err)
	// ...and the (now superseded) env password is rejected.
	_, err = p.Exchange(context.Background(), "env-password", "", "")
	require.ErrorIs(t, err, ErrBadPassword)
}

// TestPasswordProvider_FallsBackToEnvWhenNoDBHash: with the store present but no
// hash set, the env password is the active credential.
func TestPasswordProvider_FallsBackToEnvWhenNoDBHash(t *testing.T) {
	store := &fakeAdminHashStore{found: false}
	p, err := NewPasswordProvider(config.Config{AdminPassword: "env-password", AdminEmail: "a@b.c"}, store)
	require.NoError(t, err)

	_, err = p.Exchange(context.Background(), "env-password", "", "")
	require.NoError(t, err)
	_, err = p.Exchange(context.Background(), "wrong", "", "")
	require.ErrorIs(t, err, ErrBadPassword)
}

func TestProviderFor(t *testing.T) {
	// none -> dev provider.
	dev, err := ProviderFor(context.Background(), config.Config{AuthMode: config.AuthModeNone, AdminEmail: "a@b.c"}, nil)
	require.NoError(t, err)
	require.Equal(t, devProviderKey, dev.Key())

	// password -> password provider (store may be nil for env-only).
	pw, err := ProviderFor(context.Background(), config.Config{AuthMode: config.AuthModePassword, AdminPassword: "supersecret"}, nil)
	require.NoError(t, err)
	require.Equal(t, passwordProviderKey, pw.Key())

	// unknown -> error.
	_, err = ProviderFor(context.Background(), config.Config{AuthMode: config.AuthMode("bogus")}, nil)
	require.Error(t, err)
}

// TestProvidersFor builds the SET of enabled providers (break-glass). The order
// follows cfg.AuthModes (normalized oidc, password). OIDC discovery is not
// exercised here (no real issuer); password+none are local and sufficient to
// prove the set construction wiring.
func TestProvidersFor(t *testing.T) {
	// password + none would be invalid (none is exclusive), so exercise the two
	// local providers via separate single-element sets, then a password-only set.
	pwOnly, err := ProvidersFor(context.Background(), config.Config{
		AuthMode:      config.AuthModePassword,
		AuthModes:     []config.AuthMode{config.AuthModePassword},
		AdminPassword: "supersecret",
	}, nil)
	require.NoError(t, err)
	require.Len(t, pwOnly, 1)
	require.Equal(t, passwordProviderKey, pwOnly[0].Key())

	noneOnly, err := ProvidersFor(context.Background(), config.Config{
		AuthMode:   config.AuthModeNone,
		AuthModes:  []config.AuthMode{config.AuthModeNone},
		AdminEmail: "a@b.c",
	}, nil)
	require.NoError(t, err)
	require.Len(t, noneOnly, 1)
	require.Equal(t, devProviderKey, noneOnly[0].Key())

	// An unknown mode in the set surfaces an error.
	_, err = ProvidersFor(context.Background(), config.Config{
		AuthModes: []config.AuthMode{config.AuthMode("bogus")},
	}, nil)
	require.Error(t, err)
}
