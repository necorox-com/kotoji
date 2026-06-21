package serve

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// Tree-provider sentinel errors. The static handler maps these to 404 (branded
// pages) and 301 (rename redirect) per routing-and-serving.md §7.3.
var (
	// ErrSiteNotFound: the handle has no live site.
	ErrSiteNotFound = errors.New("serve: site not found")
	// ErrBranchNotFound: the site exists but the requested branch/preview does not.
	ErrBranchNotFound = errors.New("serve: branch not found")
)

// RedirectError signals that the resolved handle is a FORMER handle that maps to
// a current one; the static handler emits a 301 to the canonical handle,
// preserving branch+path+query (routing-and-serving.md §9, CANONICAL §5.5).
type RedirectError struct {
	// NewHandle is the current canonical handle the old one redirects to.
	NewHandle string
}

func (e *RedirectError) Error() string { return "serve: handle redirect -> " + e.NewHandle }

// TreeHandle is the materialized, ready-to-serve tree for a resolved Target,
// without invoking git on the request path (routing-and-serving.md §7.1). The FS
// is rooted at the site's web root and EXCLUDES .git by construction.
type TreeHandle struct {
	FS         fs.FS     // rooted at web root; immutable for CommitSHA
	CommitSHA  string    // 40-hex; "" only allowed in dev/empty-site
	CommitTime time.Time // commit timestamp -> Last-Modified
	SiteID     uuid.UUID // resolved site UUID (for preview authz scoping)
	Exists     bool
}

// TreeProvider gives the data plane an immutable file tree for a resolved Target.
// Implementations MUST NOT run git on the request path; the production impl reads
// a materialized worktree (site.Service.ServedTree -> os.DirFS).
type TreeProvider interface {
	// Tree resolves a Target to its served file tree. Returns ErrSiteNotFound /
	// ErrBranchNotFound for 404 mapping, or *RedirectError for a 301.
	Tree(ctx context.Context, t resolve.Target) (TreeHandle, error)
}

// ---- Narrow DI seams onto site.Service ----

// siteResolver is the subset of site.Service the tree provider needs to map a
// handle+branch to a served tree. site.Service satisfies it; tests use a fake.
type siteResolver interface {
	// GetSiteByHandle resolves a CURRENT handle to its site (404s old handles).
	GetSiteByHandle(ctx context.Context, h site.Handle) (site.Site, error)
	// ServedTree returns the materialized tree for (siteID, branch).
	ServedTree(ctx context.Context, id uuid.UUID, branch site.BranchName) (site.TreeHandle, error)
}

// RedirectResolver is an OPTIONAL seam: if the injected site service also resolves
// former handles to current ones, the tree provider emits *RedirectError so the
// static handler can 301. site.Service does NOT expose this on its frozen
// interface, so it is detected via a runtime type assertion and may be nil.
type RedirectResolver interface {
	// ResolveRedirect returns (newHandle, true) if oldHandle is a registered
	// former handle, else ("", false). Errors are infrastructure failures.
	ResolveRedirect(ctx context.Context, oldHandle string) (string, bool, error)
}

// fsOpener turns a materialized worktree root path into an fs.FS. Injected so
// tests can substitute an in-memory FS for the on-disk os.DirFS. The production
// default is osDirFS.
type fsOpener func(root string) fs.FS

// osDirFS is the production opener: an OS-backed, read-only directory FS rooted at
// the materialized served worktree (which excludes .git by construction, §7.2).
func osDirFS(root string) fs.FS { return os.DirFS(root) }

// ServiceTreeProvider is the production TreeProvider backed by site.Service.
type ServiceTreeProvider struct {
	svc      siteResolver
	redirect RedirectResolver // optional; nil disables redirect handling
	open     fsOpener
}

var _ TreeProvider = (*ServiceTreeProvider)(nil)

// NewServiceTreeProvider wires a site.Service-shaped dependency to the data plane.
// If svc also implements RedirectResolver, former-handle 301s are enabled
// automatically. open may be nil (defaults to os.DirFS).
func NewServiceTreeProvider(svc siteResolver, open fsOpener) *ServiceTreeProvider {
	if open == nil {
		open = osDirFS
	}
	p := &ServiceTreeProvider{svc: svc, open: open}
	if rr, ok := svc.(RedirectResolver); ok {
		p.redirect = rr
	}
	return p
}

// Tree implements TreeProvider. It resolves the handle to a site (consulting the
// redirect table on miss), then materializes the served tree for the branch.
func (p *ServiceTreeProvider) Tree(ctx context.Context, t resolve.Target) (TreeHandle, error) {
	branch := site.BranchName(t.Branch)
	if branch == "" {
		branch = site.BranchPublished
	}

	s, err := p.svc.GetSiteByHandle(ctx, site.Handle(t.Handle))
	if err != nil {
		if errors.Is(err, site.ErrNotFound) {
			// Try a former-handle redirect before declaring 404.
			if p.redirect != nil {
				if nh, ok, rerr := p.redirect.ResolveRedirect(ctx, t.Handle); rerr == nil && ok {
					return TreeHandle{}, &RedirectError{NewHandle: nh}
				}
			}
			return TreeHandle{}, ErrSiteNotFound
		}
		return TreeHandle{}, err
	}

	th, err := p.svc.ServedTree(ctx, s.ID, branch)
	if err != nil {
		if errors.Is(err, site.ErrNotFound) {
			return TreeHandle{}, ErrBranchNotFound
		}
		return TreeHandle{}, err
	}
	if !th.Exists {
		return TreeHandle{}, ErrBranchNotFound
	}

	return TreeHandle{
		FS:         p.open(th.Root),
		CommitSHA:  th.CommitSHA,
		CommitTime: th.CommitTime,
		SiteID:     s.ID,
		Exists:     true,
	}, nil
}

// ---- Optional preview cache (LRU/TTL) ----

// CachingTreeProvider wraps a TreeProvider and caches PREVIEW trees by
// (siteID, branch, commitSHA) with a TTL + max-entry LRU bound, so a hot preview
// does not re-materialize on every request (routing-and-serving.md §7.2 / Q2).
// Published trees are pass-through: they are already an atomic-swap dir on disk,
// cheap to re-resolve, and must reflect a publish immediately.
type CachingTreeProvider struct {
	inner TreeProvider
	ttl   time.Duration
	max   int
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]*cacheEntry // key: siteID + "\x00" + branch
	// lruOrder is append-only insertion order used for eviction; compacted lazily.
	lruOrder []string
}

type cacheEntry struct {
	th       TreeHandle
	expires  time.Time
	lastUsed time.Time
}

// NewCachingTreeProvider wraps inner with a preview cache. ttl<=0 or max<=0
// disables caching (pure pass-through). now may be nil (time.Now).
func NewCachingTreeProvider(inner TreeProvider, ttl time.Duration, max int, now func() time.Time) *CachingTreeProvider {
	if now == nil {
		now = time.Now
	}
	return &CachingTreeProvider{
		inner:   inner,
		ttl:     ttl,
		max:     max,
		now:     now,
		entries: make(map[string]*cacheEntry),
	}
}

var _ TreeProvider = (*CachingTreeProvider)(nil)

// Tree serves previews from the cache when fresh and the underlying commit is
// unchanged; published targets bypass the cache entirely.
func (c *CachingTreeProvider) Tree(ctx context.Context, t resolve.Target) (TreeHandle, error) {
	if !t.IsPreview || c.ttl <= 0 || c.max <= 0 {
		return c.inner.Tree(ctx, t)
	}
	key := t.Handle + "\x00" + t.Branch

	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Before(e.expires) {
		e.lastUsed = c.now()
		th := e.th
		c.mu.Unlock()
		return th, nil
	}
	c.mu.Unlock()

	// Miss / stale: materialize outside the lock, then store. Redirect/NotFound
	// errors are not cached (cheap, and must re-check on the next request).
	th, err := c.inner.Tree(ctx, t)
	if err != nil {
		return TreeHandle{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictIfNeededLocked()
	now := c.now()
	c.entries[key] = &cacheEntry{th: th, expires: now.Add(c.ttl), lastUsed: now}
	c.lruOrder = append(c.lruOrder, key)
	return th, nil
}

// evictIfNeededLocked drops the least-recently-used entry when at capacity. Caller
// holds c.mu.
func (c *CachingTreeProvider) evictIfNeededLocked() {
	if len(c.entries) < c.max {
		return
	}
	// Find the LRU live entry. lruOrder may hold stale keys (re-inserted or
	// removed); skip those.
	var victimKey string
	var victimTime time.Time
	for k, e := range c.entries {
		if victimKey == "" || e.lastUsed.Before(victimTime) {
			victimKey, victimTime = k, e.lastUsed
		}
	}
	if victimKey != "" {
		delete(c.entries, victimKey)
	}
	// Compact lruOrder occasionally to bound memory.
	if len(c.lruOrder) > 4*c.max {
		c.lruOrder = c.lruOrder[:0]
	}
}
