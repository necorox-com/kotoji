package serve

import (
	"context"
	"io/fs"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

// fixedTime is the deterministic commit time used across serve tests.
var fixedTime = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

// fakeTreeProvider returns a fixed in-memory tree keyed by handle. It models the
// site.Service read side without git/disk (routing-and-serving.md §12 test plan).
type fakeTreeProvider struct {
	// byHandle maps handle -> (fs, siteID). A "" branch means published.
	byHandle map[string]fakeSite
	// redirects maps old handle -> new handle (for the 301 path).
	redirects map[string]string
	commitSHA string
	// branchExists, when set, gates which (handle,branch) pairs resolve; nil = all.
	branchExists map[string]bool
}

type fakeSite struct {
	fsys   fs.FS
	siteID uuid.UUID
}

func (p *fakeTreeProvider) Tree(_ context.Context, t resolve.Target) (TreeHandle, error) {
	if nh, ok := p.redirects[t.Handle]; ok {
		return TreeHandle{}, &RedirectError{NewHandle: nh}
	}
	s, ok := p.byHandle[t.Handle]
	if !ok {
		return TreeHandle{}, ErrSiteNotFound
	}
	if p.branchExists != nil {
		if !p.branchExists[t.Handle+"--"+t.Branch] {
			return TreeHandle{}, ErrBranchNotFound
		}
	}
	sha := p.commitSHA
	if sha == "" {
		sha = "abcdef0123456789abcdef0123456789abcdef01"
	}
	return TreeHandle{
		FS:         s.fsys,
		CommitSHA:  sha,
		CommitTime: fixedTime,
		SiteID:     s.siteID,
		Exists:     true,
	}, nil
}

// staticResolver is a trivial resolve.Resolver that returns a preset Target. It
// lets static-handler tests bypass host parsing (resolver has its own tests).
type staticResolver struct {
	target resolve.Target
	err    error
}

func (s staticResolver) Resolve(*http.Request) (resolve.Target, error) {
	return s.target, s.err
}

// newTestHandler builds a Handler with a fake tree provider and open authz.
func newTestHandler(t *testing.T, target resolve.Target, files fstest.MapFS, authz PreviewAuthz) (*Handler, uuid.UUID) {
	t.Helper()
	sid := uuid.New()
	tp := &fakeTreeProvider{
		byHandle:  map[string]fakeSite{target.Handle: {fsys: files, siteID: sid}},
		commitSHA: "abcdef0123456789abcdef0123456789abcdef01",
	}
	if authz == nil {
		authz = OpenPreviewAuthz{}
	}
	h := NewHandler(Deps{
		Resolver: staticResolver{target: target},
		Trees:    tp,
		Authz:    authz,
		Config: HandlerConfig{
			// Zero-value CacheConfig => ETag ON, base-href injection ON (the defaults).
			Now: func() time.Time { return fixedTime },
		},
	})
	return h, sid
}

// publishedTarget / previewTarget are convenience constructors.
func publishedTarget(handle string) resolve.Target {
	return resolve.Target{Handle: handle, Branch: "published", Source: resolve.SourceHost}
}
func previewTarget(handle, branch string) resolve.Target {
	return resolve.Target{Handle: handle, Branch: branch, IsPreview: true, Source: resolve.SourceHost}
}
func pathTarget(handle string) resolve.Target {
	return resolve.Target{Handle: handle, Branch: "published", Source: resolve.SourcePath, PathPrefix: "/host/" + handle}
}
