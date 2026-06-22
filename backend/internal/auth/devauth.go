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

// AdminHashStore is the narrow persistence seam the PasswordProvider needs to
// resolve the first-run, DB-stored admin password. *db.Store satisfies it via
// GetAdminPasswordHash. Keeping it minimal (one method) means tests inject a tiny
// fake and the provider never depends on the full Querier.
type AdminHashStore interface {
	// GetAdminPasswordHash returns the DB-stored bcrypt hash and whether it is set.
	// A missing credential is (.., false, nil) — never an error — so the provider
	// can fall back to the env password cleanly.
	GetAdminPasswordHash(ctx context.Context) (hash string, found bool, err error)
}

// PasswordProvider authenticates a single admin against a bcrypt-checked password
// (AUTH_MODE=password). It is non-interactive: the login handler renders/accepts
// a password form and calls Exchange with the submitted password as the "code".
//
// Credential precedence (first-run setup): the DB-stored hash wins when present,
// otherwise the env-derived hash. This lets an instance booted WITHOUT an env
// password be configured at runtime via /auth/setup, after which the DB hash is
// authoritative — the env password is only the fallback bootstrap credential.
type PasswordProvider struct {
	email string
	name  string
	// envHash is the bcrypt hash of the env KOTOJI_AUTH_ADMIN_PASSWORD, or nil
	// when no env password is configured (first-run / DB-only mode).
	envHash []byte
	// store resolves the DB-stored admin hash; nil in unit tests that only
	// exercise the env credential (Exchange falls back to envHash then).
	store AdminHashStore
}

// compile-time guarantee.
var _ AuthProvider = (*PasswordProvider)(nil)

// NewPasswordProvider builds the admin-password provider. The env plaintext
// password (when set) is hashed ONCE at construction with bcrypt; only the hash
// is kept. store supplies the runtime DB-stored hash that takes precedence over
// the env hash (first-run setup). store may be nil (no DB-backed credential).
//
// An empty env password is NOT an error here: it is the valid first-run state
// where the admin password will be set via /auth/setup. At least one of the env
// password or a DB hash must exist for a login to succeed (enforced at Exchange).
func NewPasswordProvider(cfg config.Config, store AdminHashStore) (*PasswordProvider, error) {
	var envHash []byte
	if cfg.AdminPassword != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(cfg.AdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		envHash = h
	}
	return &PasswordProvider{
		email:   defaultIfEmpty(strings.ToLower(cfg.AdminEmail), "admin@kotoji.local"),
		name:    "Admin",
		envHash: envHash,
		store:   store,
	}, nil
}

// Key returns the password provider identifier.
func (p *PasswordProvider) Key() string { return passwordProviderKey }

// Interactive is false: the password form is local, not an external IdP redirect.
func (p *PasswordProvider) Interactive() bool { return false }

// Start is a no-op for the password provider (returns "").
func (p *PasswordProvider) Start(_, _, _ string) string { return "" }

// Exchange treats the `code` argument as the submitted plaintext password and
// compares it (constant-time) against the active bcrypt hash. The DB-stored hash
// is preferred (first-run setup) and the env hash is the fallback. A mismatch —
// or no configured credential at all — returns ErrBadPassword so the handler can
// map it to 401 without leaking which part of the credential failed.
func (p *PasswordProvider) Exchange(ctx context.Context, password, _, _ string) (Claims, error) {
	hash := p.activeHash(ctx)
	if hash == nil {
		// No credential configured (env empty AND no DB hash). This is the
		// first-run state; a normal login cannot succeed until /auth/setup runs.
		return Claims{}, ErrBadPassword
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
		return Claims{}, ErrBadPassword
	}
	return Claims{
		Subject:       devSubject,
		Email:         p.email,
		EmailVerified: true,
		Name:          p.name,
	}, nil
}

// activeHash resolves the bcrypt hash to compare against: the DB-stored hash when
// present (first-run setup credential), else the env-derived hash. A store read
// failure degrades to the env hash so a transient DB blip cannot lock out an
// instance that still has its env password.
func (p *PasswordProvider) activeHash(ctx context.Context) []byte {
	if p.store != nil {
		if h, found, err := p.store.GetAdminPasswordHash(ctx); err == nil && found {
			return []byte(h)
		}
	}
	return p.envHash
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
