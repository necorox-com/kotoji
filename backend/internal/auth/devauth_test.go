package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

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
	p, err := NewPasswordProvider(config.Config{AdminPassword: "correct horse", AdminEmail: "admin@kotoji.local"})
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

func TestPasswordProvider_RequiresPassword(t *testing.T) {
	_, err := NewPasswordProvider(config.Config{})
	require.Error(t, err)
}

func TestProviderFor(t *testing.T) {
	// none -> dev provider.
	dev, err := ProviderFor(context.Background(), config.Config{AuthMode: config.AuthModeNone, AdminEmail: "a@b.c"})
	require.NoError(t, err)
	require.Equal(t, devProviderKey, dev.Key())

	// password -> password provider.
	pw, err := ProviderFor(context.Background(), config.Config{AuthMode: config.AuthModePassword, AdminPassword: "x"})
	require.NoError(t, err)
	require.Equal(t, passwordProviderKey, pw.Key())

	// unknown -> error.
	_, err = ProviderFor(context.Background(), config.Config{AuthMode: config.AuthMode("bogus")})
	require.Error(t, err)
}
