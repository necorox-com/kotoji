package auth

import (
	"context"
	"encoding/json"
	"net/http"
)

// ctxKey is an unexported context key type to avoid cross-package collisions.
type ctxKey int

const userCtxKey ctxKey = iota

// stable machine error codes (subset of the CANONICAL §3 / openapi enum that the
// auth surface can emit). Kept as constants so handlers never hardcode strings.
const (
	codeUnauthenticated = "unauthenticated"
	codeForbidden       = "forbidden"
	codeInternal        = "internal"
	codeValidation      = "validation"
)

// SessionAuth is the request middleware that loads the session (if any) onto the
// context. It is NON-fatal: a missing/expired/invalid session simply yields no
// user in context (RequireAuth/RequireAdmin enforce presence downstream). This
// lets endpoints like /api/config remain public while sharing one chain.
func (a *Auth) SessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := a.sessions.readCookie(r)
		if id == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, err := a.sessions.Get(r.Context(), id)
		if err != nil {
			// ErrSessionNotFound (expired/inactive/unknown) and store errors both
			// degrade to "anonymous". A store error is logged but never blocks the
			// request here; RequireAuth will reject protected endpoints.
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, &user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Middleware returns the SessionAuth middleware as a plain func so the
// composition root can mount it without importing method values. This is the
// integration seam named in the task.
func (a *Auth) Middleware() func(http.Handler) http.Handler {
	return a.SessionAuth
}

// CurrentUser returns the authenticated user loaded by SessionAuth, or nil if
// the request is anonymous. Handlers use the (user, ok) idiom via the bool.
func CurrentUser(ctx context.Context) (*SessionUser, bool) {
	u, ok := ctx.Value(userCtxKey).(*SessionUser)
	return u, ok
}

// RequireAuth wraps a handler so it returns 401 (code "unauthenticated") when no
// session user is present.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := CurrentUser(r.Context()); !ok {
			writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin wraps a handler so it returns 401 when anonymous and 403 when the
// user is authenticated but not an instance superuser (users.is_admin). is_admin
// is the separate instance axis (CANONICAL §6), distinct from per-site roles.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := CurrentUser(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "authentication required")
			return
		}
		if !u.IsAdmin {
			writeError(w, r, http.StatusForbidden, codeForbidden, "instance admin required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// errorEnvelope is the uniform wire error body (CANONICAL §3 / openapi
// ErrorEnvelope). The auth package emits it for its own failures so the whole
// /api surface stays drift-free regardless of which package handled the request.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// writeError emits a JSON error envelope with the given HTTP status and machine
// code. The message is human-safe (never leaks internals).
func writeError(w http.ResponseWriter, _ *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Encoding a fixed-shape struct cannot realistically fail; the error is
	// ignored because the status line is already committed.
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// writeJSON emits a 200 (or given status) JSON body. Used by the auth handlers.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
