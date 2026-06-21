package api

import (
	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// Compile-time guarantees the Integration phase can rely on:
//   - the real *db.Store satisfies the narrow MetaStore the API needs, and
//   - the frozen site.Service is the type the Deps.Site field expects, and
//   - a concrete *auth.Auth adapts cleanly into the AuthSurface seam.
//
// If any of these break, the build fails here (in YOUR package) rather than at
// the composition root, keeping the wiring contract explicit and local.
var (
	_ MetaStore   = (*db.Store)(nil)
	_ AuthSurface = authAdapter{a: (*auth.Auth)(nil)}
)

// _ is a typed nil assertion that Deps.Site accepts any site.Service (incl. the
// FakeService used in tests and the gitService used in prod).
var _ = func(svc site.Service) Deps { return Deps{Site: svc} }
