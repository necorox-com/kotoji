package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/necorox-com/kotoji/backend/internal/auth"
)

// authAdapter widens *auth.Auth to the AuthSurface interface. The only reason it
// exists is that auth.Auth.CSRF() returns the concrete *auth.CSRF, which is not
// assignable to AuthSurface.CSRF()'s csrfGuard return without this shim. It adds
// no behaviour — pure delegation — so the composition root can pass a plain
// *auth.Auth and the router stays mockable.
type authAdapter struct{ a *auth.Auth }

// WrapAuth adapts a concrete *auth.Auth into an AuthSurface for NewRouter.
func WrapAuth(a *auth.Auth) AuthSurface { return authAdapter{a: a} }

func (w authAdapter) Middleware() func(http.Handler) http.Handler { return w.a.Middleware() }
func (w authAdapter) CSRF() csrfGuard                             { return w.a.CSRF() }
func (w authAdapter) RegisterRoutes(r chi.Router)                 { w.a.RegisterRoutes(r) }
