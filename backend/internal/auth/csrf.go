package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// csrfTokenBytes is the entropy of the double-submit CSRF token (256 bits).
const csrfTokenBytes = 32

// csrfHeader is the request header the SPA echoes the cookie value in.
const csrfHeader = "X-CSRF-Token"

// CSRF implements the double-submit-cookie pattern (architecture.md §8.1 #10):
//   - a non-HttpOnly cookie holds a random token the JS client can read,
//   - the client echoes it in the X-CSRF-Token header on unsafe methods,
//   - the middleware requires header == cookie (constant-time) on those methods.
//
// Cookie auth is already SameSite=Lax (blocks cross-site top-level POSTs); this
// is defense-in-depth against same-site/embedded forgery. Bearer-token (MCP/API)
// requests are CSRF-immune and bypass the check.
type CSRF struct {
	cookieName string
	secure     bool
}

// NewCSRF builds a CSRF guard. cookieName is the base name; secure mirrors the
// session cookie's Secure attribute. The CSRF cookie is intentionally readable
// by JS (not HttpOnly) so the SPA can echo it.
func NewCSRF(cookieName string, secure bool) *CSRF {
	return &CSRF{cookieName: cookieName, secure: secure}
}

// CookieName is the on-the-wire CSRF cookie name. It uses the __Host- prefix
// (host-only) in secure mode for the same isolation guarantees as the session
// cookie (decision #7).
func (c *CSRF) CookieName() string {
	if c.secure {
		return "__Host-" + c.cookieName
	}
	return c.cookieName
}

// newCSRFToken returns a fresh URL-safe random token.
func newCSRFToken() (string, error) {
	b := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Issue sets (or refreshes) the CSRF cookie if it is missing, returning the
// token. It is safe to call on any response; login handlers call it so the SPA
// has a token immediately after authentication. Readable by JS (not HttpOnly).
func (c *CSRF) Issue(w http.ResponseWriter, r *http.Request) (string, error) {
	if cur := c.readCookie(r); cur != "" {
		return cur, nil
	}
	tok, err := newCSRFToken()
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     c.CookieName(),
		Value:    tok,
		Path:     "/",
		Secure:   c.secure,
		HttpOnly: false, // MUST be readable by the SPA to echo in the header.
		SameSite: http.SameSiteLaxMode,
	})
	return tok, nil
}

// clearCookie expires the CSRF cookie (logout). Attributes must match Issue.
func (c *CSRF) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.CookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   c.secure,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

// readCookie returns the CSRF cookie value, "" if absent.
func (c *CSRF) readCookie(r *http.Request) string {
	ck, err := r.Cookie(c.CookieName())
	if err != nil {
		return ""
	}
	return ck.Value
}

// isSafeMethod reports whether a method is safe (no state change) and therefore
// exempt from CSRF verification.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// Verify reports whether the request passes the double-submit check. Safe
// methods and bearer-token (Authorization header present) requests always pass.
// For unsafe cookie requests, the header token must be present and equal the
// cookie token (constant-time).
func (c *CSRF) Verify(r *http.Request) bool {
	if isSafeMethod(r.Method) {
		return true
	}
	// Bearer-token requests carry no ambient cookie authority -> not forgeable.
	if r.Header.Get("Authorization") != "" {
		return true
	}
	cookie := c.readCookie(r)
	header := r.Header.Get(csrfHeader)
	if cookie == "" || header == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie), []byte(header)) == 1
}

// Middleware rejects unsafe requests that fail Verify with 403 (code "forbidden")
// before they reach a handler. It is mounted on the mutating API surface.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.Verify(r) {
			writeError(w, r, http.StatusForbidden, codeForbidden, "CSRF token missing or invalid")
			return
		}
		next.ServeHTTP(w, r)
	})
}
