package auth

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/necorox-com/kotoji/backend/internal/config"
)

// devProviderKey / passwordProviderKey are the user_identities.provider values
// for the non-OIDC modes. The subject is the admin email (stable) so the same
// identity row is reused across logins.
const (
	devProviderKey      = "dev"
	passwordProviderKey = "password"
)

// devSubject is the synthetic, stable OIDC-`sub` analogue for the dev/password
// single admin. It is deterministic so repeated logins resolve the same user.
const devSubject = "kotoji-local-admin"

// DevProvider is the no-auth provider for local development (AUTH_MODE=none).
// It authenticates instantly as a fixed local admin with NO credential check, so
// it MUST never be enabled in production (config.validate forbids it). It is
// non-interactive: the login handler logs the user straight in.
type DevProvider struct {
	email string
	name  string
}

// compile-time guarantee.
var _ AuthProvider = (*DevProvider)(nil)

// NewDevProvider builds the no-auth provider from the admin identity in config.
func NewDevProvider(cfg config.Config) *DevProvider {
	return &DevProvider{
		email: defaultIfEmpty(strings.ToLower(cfg.AdminEmail), "admin@kotoji.local"),
		name:  "Local Admin",
	}
}

// Key returns the dev provider identifier.
func (p *DevProvider) Key() string { return devProviderKey }

// Interactive is false: no external redirect; the handler logs in immediately.
func (p *DevProvider) Interactive() bool { return false }

// Start is a no-op for the non-interactive dev provider (returns "").
func (p *DevProvider) Start(_, _, _ string) string { return "" }

// Exchange returns the fixed local-admin claims unconditionally. The code and
// nonce are ignored — there is no real handshake in no-auth mode.
func (p *DevProvider) Exchange(_ context.Context, _, _, _ string) (Claims, error) {
	return Claims{
		Subject:       devSubject,
		Email:         p.email,
		EmailVerified: true,
		Name:          p.name,
	}, nil
}

// PasswordProvider authenticates a single admin against a bcrypt-checked password
// (AUTH_MODE=password). It is non-interactive: the login handler renders/accepts
// a password form and calls Exchange with the submitted password as the "code".
type PasswordProvider struct {
	email      string
	name       string
	passwdHash []byte // bcrypt hash of the configured admin password
}

// compile-time guarantee.
var _ AuthProvider = (*PasswordProvider)(nil)

// NewPasswordProvider builds the admin-password provider. The plaintext password
// from config is hashed ONCE at construction with bcrypt; only the hash is kept,
// and the per-attempt comparison is constant-time via bcrypt.CompareHashAndPassword.
func NewPasswordProvider(cfg config.Config) (*PasswordProvider, error) {
	if cfg.AdminPassword == "" {
		return nil, errors.New("auth: password mode requires an admin password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return &PasswordProvider{
		email:      defaultIfEmpty(strings.ToLower(cfg.AdminEmail), "admin@kotoji.local"),
		name:       "Admin",
		passwdHash: hash,
	}, nil
}

// Key returns the password provider identifier.
func (p *PasswordProvider) Key() string { return passwordProviderKey }

// Interactive is false: the password form is local, not an external IdP redirect.
func (p *PasswordProvider) Interactive() bool { return false }

// Start is a no-op for the password provider (returns "").
func (p *PasswordProvider) Start(_, _, _ string) string { return "" }

// Exchange treats the `code` argument as the submitted plaintext password and
// compares it (constant-time) against the stored bcrypt hash. A mismatch returns
// ErrBadPassword so the handler can map it to 401 without leaking which part of
// the credential failed.
func (p *PasswordProvider) Exchange(_ context.Context, password, _, _ string) (Claims, error) {
	if err := bcrypt.CompareHashAndPassword(p.passwdHash, []byte(password)); err != nil {
		return Claims{}, ErrBadPassword
	}
	return Claims{
		Subject:       devSubject,
		Email:         p.email,
		EmailVerified: true,
		Name:          p.name,
	}, nil
}

// ErrBadPassword is the opaque credential-failure sentinel for password mode.
var ErrBadPassword = errors.New("auth: invalid password")

// defaultIfEmpty returns def when s is empty, else s.
func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
