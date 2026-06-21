package serve

import (
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// Compile-time assertion: the frozen site.Service interface satisfies the narrow
// siteResolver seam the production tree provider depends on, so the Integration
// phase can wire NewServiceTreeProvider(svc, nil) directly with a site.Service.
var _ siteResolver = (site.Service)(nil)

// A concrete *site.FakeService also satisfies it (used in downstream tests).
var _ siteResolver = (*site.FakeService)(nil)
