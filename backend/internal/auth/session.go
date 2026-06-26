package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// sessionIDBytes is the entropy of the opaque session id (256 bits). Well above
// the 128-bit floor in architecture.md §8.1; base64url-encoded into the cookie.
const sessionIDBytes = 32

// touchInterval throttles last_seen_at writes: the idle clock only advances when
// the session has not been touched within this window, avoiding a DB write on
// every authenticated request (architecture.md §8.1 "sliding refresh").
const touchInterval = 5 * time.Minute

// ErrSessionNotFound is returned by SessionStore.Get when no live (unexpired,
// active-user) session matches the id. The middleware treats it as "no auth".
var ErrSessionNotFound = errors.New("auth: session not found")

// SessionStore is the narrow persistence seam the session layer needs. The real
// *db.Store satisfies it; tests inject a fake. Keeping it minimal (vs the full
// Querier) means tests only implement what auth actually calls.
type SessionStore interface {
	// CreateSession persists a new server-side session row.
	CreateSession(ctx context.Context, arg gen.CreateSessionParams) (gen.Session, error)
	// GetSession returns the joined session+user for a LIVE session (unexpired,
	// active user). pgx.ErrNoRows => no such live session.
	GetSession(ctx context.Context, id string) (gen.GetSessionRow, error)
	// DeleteSession removes one session (logout).
	DeleteSession(ctx context.Context, id string) error
	// TouchSession advances last_seen_at (idle clock); throttled by the caller.
	TouchSession(ctx context.Context, id string) error
	// UpsertUser match-or-creates a user by email at login.
	UpsertUser(ctx context.Context, arg gen.UpsertUserParams) (gen.User, error)
	// UpsertIdentity links (provider, subject) -> user.
	UpsertIdentity(ctx context.Context, arg gen.UpsertIdentityParams) error
}

// compile-time guarantee that the real Store satisfies the narrow seam.
var _ SessionStore = (*db.Store)(nil)

// SessionUser is the identity the middleware loads onto the request context.
// It mirrors the GetSessionRow join (no extra round-trip) plus the session id
// so handlers (logout) can act on the live session.
type SessionUser struct {
	SessionID      string
	UserID         uuid.UUID
	Email          string
	DisplayName    string
	AvatarURL      *string
	IsAdmin        bool
	CanCreateSites bool
}

// SessionManager owns session lifecycle (create/get/delete/touch) and cookie
// emission. It depends only on SessionStore + cookie config, so it is fully
// testable without a database or an HTTP server.
type SessionManager struct {
	store      SessionStore
	cookieName string
	ttl        time.Duration
	// secure controls the cookie Secure attribute. The __Host- prefix REQUIRES
	// Secure in browsers; in dev-over-http tests it may be false, in which case
	// the raw (un-prefixed) cookie name is used so the browser still accepts it.
	secure bool
}

// NewSessionManager builds a SessionManager. ttl is the absolute session
// lifetime; cookieName is the un-prefixed base name (the __Host- prefix is added
// automatically when secure is true).
func NewSessionManager(store SessionStore, cookieName string, ttl time.Duration, secure bool) *SessionManager {
	return &SessionManager{
		store:      store,
		cookieName: cookieName,
		ttl:        ttl,
		secure:     secure,
	}
}

// CookieName is the actual cookie name sent over the wire. Per decision #7 the
// session cookie uses the `__Host-` prefix (host-only, Secure, Path=/, no
// Domain). In insecure dev (no TLS) the prefix is dropped because browsers
// reject __Host- without Secure.
func (m *SessionManager) CookieName() string {
	if m.secure {
		return "__Host-" + m.cookieName
	}
	return m.cookieName
}

// newSessionID returns a URL-safe, 256-bit opaque session id. crypto/rand is the
// only acceptable source for a security token; an RNG failure is fatal to login.
func newSessionID() (string, error) {
	b := make([]byte, sessionIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: session id entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Create persists a fresh session for userID and returns its opaque id. The id
// is freshly generated every call, which is what makes "rotate on login" work:
// the login handler always calls Create (never reuses a pre-auth id), so an
// attacker-fixed pre-login cookie can never be inherited (architecture.md §8.1).
func (m *SessionManager) Create(ctx context.Context, userID uuid.UUID, userAgent, remoteAddr string) (string, error) {
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	if _, err := m.store.CreateSession(ctx, gen.CreateSessionParams{
		ID:        id,
		UserID:    userID,
		ExpiresAt: tsFromTime(time.Now().Add(m.ttl)),
		UserAgent: userAgent,
		IpAddr:    parseIP(remoteAddr),
	}); err != nil {
		return "", fmt.Errorf("auth: create session: %w", err)
	}
	return id, nil
}

// Get loads a live session by id, returning the joined user. Expired sessions
// and inactive users are filtered by the query (WHERE expires_at > now() AND
// is_active). A pgx no-rows result maps to ErrSessionNotFound. On success it
// best-effort touches last_seen_at when the idle clock is stale.
func (m *SessionManager) Get(ctx context.Context, id string) (SessionUser, error) {
	if id == "" {
		return SessionUser{}, ErrSessionNotFound
	}
	row, err := m.store.GetSession(ctx, id)
	if err != nil {
		if db.IsNotFound(err) {
			return SessionUser{}, ErrSessionNotFound
		}
		return SessionUser{}, fmt.Errorf("auth: get session: %w", err)
	}

	// Sliding idle refresh, throttled: only write when last_seen_at is older than
	// touchInterval. A touch failure is non-fatal (the session is still valid).
	if isStale(row.LastSeenAt, touchInterval) {
		_ = m.store.TouchSession(ctx, id)
	}

	return SessionUser{
		SessionID:      row.ID,
		UserID:         row.UserID,
		Email:          row.Email,
		DisplayName:    row.DisplayName,
		AvatarURL:      row.AvatarUrl,
		IsAdmin:        row.IsAdmin,
		CanCreateSites: row.CanCreateSites,
	}, nil
}

// Delete destroys a session (logout). A missing row is not an error.
func (m *SessionManager) Delete(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if err := m.store.DeleteSession(ctx, id); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// SetCookie writes the host-only session cookie. Per decision #7: Secure,
// HttpOnly, SameSite=Lax, Path=/, NO Domain (host-only) — the `__Host-` prefix
// the CookieName encodes enforces exactly that. MaxAge matches the session TTL.
func (m *SessionManager) SetCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.CookieName(),
		Value:    id,
		Path:     "/", // __Host- requires Path=/ and no Domain.
		MaxAge:   int(m.ttl.Seconds()),
		Secure:   m.secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie expires the session cookie (logout). The attributes MUST match the
// set attributes (name, path) or the browser will not overwrite it.
func (m *SessionManager) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.CookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   m.secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// readCookie extracts the session id from the request cookie, "" if absent.
//
// SINGLE-DOMAIN ISOLATION (architecture.md §8.1, SECURITY.md "Single-domain
// isolation model"): hosted sites share the control plane's registrable domain,
// so a hosted subdomain can set a bare-name cookie (e.g. `kotoji_session`) on the
// shared parent ("cookie tossing"). The control plane MUST NOT accept it. We read
// ONLY m.CookieName() — which is the `__Host-`-prefixed name in production
// (secure=true). A `__Host-` cookie is, by browser rule, un-tossable: it cannot
// carry a Domain attribute and is keyed host-only, so a sibling subdomain can
// never write it. There is DELIBERATELY no fallback to the bare name in prod; a
// tossed bare cookie is invisible here and cannot shadow or fixate the session.
// Only in dev (secure=false, http) does CookieName() return the bare name,
// because browsers reject `__Host-` without Secure.
func (m *SessionManager) readCookie(r *http.Request) string {
	c, err := r.Cookie(m.CookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

// --- small pgtype / netip adapters (kept local to the session layer) ---

// tsFromTime wraps a time.Time as a valid pgtype.Timestamptz.
func tsFromTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// isStale reports whether ts is valid and older than d ago. An invalid ts (never
// set) is treated as stale so the first read schedules a touch.
func isStale(ts pgtype.Timestamptz, d time.Duration) bool {
	if !ts.Valid {
		return true
	}
	return time.Since(ts.Time) > d
}

// parseIP turns an http.Request RemoteAddr ("ip:port" or bare ip) into a
// *netip.Addr for the inet column, or nil when it cannot be parsed (the column
// is nullable). Best-effort: a missing IP must never block login.
func parseIP(remoteAddr string) *netip.Addr {
	if remoteAddr == "" {
		return nil
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return &addr
}
