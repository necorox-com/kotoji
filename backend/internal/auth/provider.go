// Package auth implements the kotoji control-plane authentication surface:
// the AuthProvider abstraction (OIDC / dev / admin-password), server-side
// sessions backed by the metadata Store, the double-submit CSRF guard, the
// session-loading middleware, and the HTTP handlers that wire them together
// (/auth/login, /auth/callback, /auth/logout, /api/me, /api/config).
//
// Design notes (see docs/architecture.md §3g + CANONICAL §6):
//   - AuthProvider is a DI seam: oidc.go (Google/OIDC), devauth.go (no-auth +
//     admin-password). The mode is chosen by config.AuthMode at composition.
//   - Sessions are opaque, server-side, host-only `__Host-` cookies; the id is
//     rotated on every successful login (anti session-fixation).
//   - users.is_admin is a SEPARATE axis from per-site roles; this package only
//     loads identity into the request context — per-site authz lives above.
package auth

import "context"

// Claims is the identity an AuthProvider resolves after a successful exchange.
// Fields mirror the OIDC standard claims this package needs; everything else is
// intentionally dropped (least data). HostedDomain is the Google Workspace `hd`
// claim used for the domain allowlist gate.
type Claims struct {
	// Subject is the stable, opaque, provider-scoped OIDC `sub`. It is the join
	// key for user_identities(provider, subject) — NEVER match on email.
	Subject string
	// Email is the verified email used to upsert the users row.
	Email string
	// EmailVerified reports whether the IdP asserts the email is verified.
	EmailVerified bool
	// Name is the human display name (best-effort; may be empty).
	Name string
	// HostedDomain is the Google `hd` claim (Workspace domain), "" if absent.
	HostedDomain string
	// IsAdmin is the provider-resolved instance-admin decision. The OIDC provider
	// sets it from the KOTOJI_OIDC_ADMIN_EMAILS allowlist (decision #3); the
	// password/dev providers leave it false (their admin status is governed by the
	// single-admin promotion path in completeLogin, not this flag).
	IsAdmin bool
}

// AuthProvider abstracts the login handshake so the OIDC, dev, and
// admin-password implementations are swappable behind config.AuthMode.
//
// Start returns the IdP authorization URL the browser is redirected to. The
// state and nonce are caller-generated, single-use, and bound to a short-lived
// cookie so the callback can detect CSRF/replay. PKCE (S256) is layered inside
// the OIDC impl using a verifier the caller persists alongside state.
//
// Exchange completes the handshake: it trades the authorization code for tokens,
// verifies the id_token (iss, aud, exp, nonce, signature), and returns the
// resolved Claims. A non-nil error means the login MUST be rejected.
type AuthProvider interface {
	// Key is the stable provider identifier stored in user_identities.provider
	// (e.g. "google", "oidc", "dev"). It must be stable across restarts.
	Key() string

	// Start builds the authorization-endpoint URL for the given single-use
	// state, nonce, and PKCE verifier. Providers that do not perform an external
	// redirect (dev/password) may ignore the arguments; callers branch on
	// Interactive() to decide whether to redirect.
	Start(state, nonce, pkceVerifier string) (authURL string)

	// Interactive reports whether Start yields a real external redirect URL.
	// false => the provider authenticates locally (dev no-auth / admin-password)
	// and the login handler must not 302 to an IdP.
	Interactive() bool

	// Exchange verifies the callback (code + nonce + verifier) and returns the
	// authenticated Claims. expectedNonce is the nonce minted in Start; the impl
	// MUST verify the id_token's nonce equals it.
	Exchange(ctx context.Context, code, pkceVerifier, expectedNonce string) (Claims, error)
}
