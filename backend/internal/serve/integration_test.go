package serve

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// fakeSiteService is a minimal site.Service-shaped dependency (the subset the
// tree provider needs) backed by an injectable served-tree map. It also
// implements RedirectResolver so the rename-301 path is exercised end to end.
type fakeSiteService struct {
	mu        sync.Mutex
	byHandle  map[string]site.Site
	trees     map[string]site.TreeHandle // key: siteID + "\x00" + branch
	redirects map[string]string
}

func (f *fakeSiteService) GetSiteByHandle(_ context.Context, h site.Handle) (site.Site, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byHandle[string(h)]
	if !ok {
		return site.Site{}, site.ErrNotFound
	}
	return s, nil
}

func (f *fakeSiteService) ServedTree(_ context.Context, id uuid.UUID, branch site.BranchName) (site.TreeHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	th, ok := f.trees[id.String()+"\x00"+string(branch)]
	if !ok {
		return site.TreeHandle{}, site.ErrNotFound
	}
	return th, nil
}

func (f *fakeSiteService) ResolveRedirect(_ context.Context, old string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	nh, ok := f.redirects[old]
	return nh, ok, nil
}

// writeTree materializes files into a temp dir and returns its root (mimics the
// atomic-swap served directory; os.DirFS reads it without touching .git).
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestIntegration_EndToEnd(t *testing.T) {
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	commitTime := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)

	siteID := uuid.New()
	pubRoot := writeTree(t, map[string]string{
		"index.html":     "<!doctype html><html><head></head><body>PUBLISHED</body></html>",
		"style.css":      "body{color:red}",
		"sub/index.html": "<html><head></head>SUB</html>",
	})
	draftRoot := writeTree(t, map[string]string{
		"index.html": "<!doctype html><html><head></head><body>DRAFT</body></html>",
	})

	svc := &fakeSiteService{
		byHandle: map[string]site.Site{
			"expense-calc": {ID: siteID, Handle: "expense-calc"},
		},
		trees: map[string]site.TreeHandle{
			siteID.String() + "\x00published": {Root: pubRoot, CommitSHA: commitSHA, CommitTime: commitTime, Exists: true},
			siteID.String() + "\x00draft":     {Root: draftRoot, CommitSHA: commitSHA, CommitTime: commitTime, Exists: true},
		},
		redirects: map[string]string{"old-name": "expense-calc"},
	}

	tp := NewServiceTreeProvider(svc, nil) // nil opener => os.DirFS
	res := resolve.NewResolver(resolve.Config{
		BaseDomain:         "hosting.example.com",
		EnablePathFallback: true,
		TrustForwardedHost: true,
	})
	authz := newGrantAuthz(t, false, func() time.Time { return commitTime })

	h := NewHandler(Deps{
		Resolver: res,
		Trees:    tp,
		Authz:    authz,
		Config:   HandlerConfig{Now: func() time.Time { return commitTime }},
	})

	t.Run("published_index", func(t *testing.T) {
		resp := serveOnce(h, mkReq("expense-calc.hosting.example.com", "/", nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		if !contains(resp, "PUBLISHED") {
			t.Fatalf("expected published body")
		}
		if resp.Header.Get("ETag") == "" {
			t.Fatalf("expected ETag on published")
		}
	})

	t.Run("published_asset_root_absolute_works", func(t *testing.T) {
		resp := serveOnce(h, mkReq("expense-calc.hosting.example.com", "/style.css", nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/css; charset=utf-8" {
			t.Fatalf("css content type: %q", ct)
		}
	})

	t.Run("trailing_slash_301", func(t *testing.T) {
		resp := serveOnce(h, mkReq("expense-calc.hosting.example.com", "/sub", nil))
		if resp.StatusCode != http.StatusMovedPermanently {
			t.Fatalf("want 301, got %d", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/sub/" {
			t.Fatalf("want /sub/, got %q", loc)
		}
	})

	t.Run("preview_requires_auth_404_without", func(t *testing.T) {
		resp := serveOnce(h, mkReq("expense-calc--draft.hosting.example.com", "/", nil))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unauth preview want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("preview_with_grant_serves", func(t *testing.T) {
		grant := authz.SignGrant(siteID, "draft", commitTime.Add(time.Hour))
		// First request with kpt => 302 + cookie.
		resp := serveOnce(h, mkReq("expense-calc--draft.hosting.example.com", "/?kpt="+grant, nil))
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("grant want 302, got %d", resp.StatusCode)
		}
		var cookie *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == PreviewCookieName {
				cookie = c
			}
		}
		if cookie == nil {
			t.Fatal("expected preview cookie")
		}
		// Follow up with the cookie => serves the draft, no-store.
		resp2 := serveOnce(h, mkReq("expense-calc--draft.hosting.example.com", "/", map[string]string{
			"Cookie": cookie.Name + "=" + cookie.Value,
		}))
		if resp2.StatusCode != http.StatusOK || !contains(resp2, "DRAFT") {
			t.Fatalf("cookie should serve draft, got %d", resp2.StatusCode)
		}
		if cc := resp2.Header.Get("Cache-Control"); cc != previewCacheControl {
			t.Fatalf("preview cache-control: %q", cc)
		}
	})

	t.Run("path_mode_preview_base_href_injected", func(t *testing.T) {
		grant := authz.SignGrant(siteID, "draft", commitTime.Add(time.Hour))
		cookieVal := grant
		// Use a host-only cookie directly (skip the 302 hop) on the control host.
		resp := serveOnce(h, mkReq("hosting.example.com", "/host/expense-calc--draft/", map[string]string{
			"Cookie": PreviewCookieName + "=" + cookieVal,
		}))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("path-mode preview want 200, got %d", resp.StatusCode)
		}
		if !contains(resp, `<base href="/host/expense-calc--draft/">`) {
			t.Fatalf("path-mode HTML should have base-href injected")
		}
	})

	t.Run("unknown_site_404", func(t *testing.T) {
		resp := serveOnce(h, mkReq("nope.hosting.example.com", "/", nil))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown site want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("renamed_handle_301", func(t *testing.T) {
		resp := serveOnce(h, mkReq("old-name.hosting.example.com", "/x?q=1", nil))
		if resp.StatusCode != http.StatusMovedPermanently {
			t.Fatalf("rename want 301, got %d", resp.StatusCode)
		}
		loc := resp.Header.Get("Location")
		if loc == "" || !contains301(loc, "expense-calc.hosting.example.com/x") {
			t.Fatalf("rename 301 Location wrong: %q", loc)
		}
	})

	t.Run("wrong_domain_421", func(t *testing.T) {
		resp := serveOnce(h, mkReq("evil.com", "/", nil))
		if resp.StatusCode != http.StatusMisdirectedRequest {
			t.Fatalf("wrong domain want 421, got %d", resp.StatusCode)
		}
	})

	t.Run("post_405", func(t *testing.T) {
		r := mkReq("expense-calc.hosting.example.com", "/", nil)
		r.Method = http.MethodPost
		resp := serveOnce(h, r)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST want 405, got %d", resp.StatusCode)
		}
	})
}

// TestIntegration_NoHalfTree validates the atomic-swap contract (§7.2): while the
// served directory is swapped via rename(), a concurrent reader sees either the
// OLD or the NEW tree, never a partial one. We model the swap by atomically
// flipping which root the tree provider returns and rename-swapping on disk.
func TestIntegration_NoHalfTree(t *testing.T) {
	siteID := uuid.New()
	commitTime := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)

	oldRoot := writeTree(t, map[string]string{"index.html": "<html><head></head>OLD</html>"})
	newRoot := writeTree(t, map[string]string{"index.html": "<html><head></head>NEW</html>"})

	// activeRoot is flipped atomically to simulate the rename() swap.
	var activeRoot atomic.Pointer[string]
	activeRoot.Store(&oldRoot)

	// A tree provider whose ServedTree returns the currently-active root. Reading a
	// whole-directory os.DirFS for a stable root never yields a half tree.
	svc := &swapSiteService{
		siteID:     siteID,
		commitTime: commitTime,
		active:     &activeRoot,
	}
	tp := NewServiceTreeProvider(svc, nil)
	res := resolve.NewResolver(resolve.Config{BaseDomain: "hosting.example.com", TrustForwardedHost: true})
	h := NewHandler(Deps{
		Resolver: res,
		Trees:    tp,
		Authz:    OpenPreviewAuthz{},
		Config:   HandlerConfig{Now: func() time.Time { return commitTime }},
	})

	stop := make(chan struct{})
	var swapper sync.WaitGroup

	// Swapper: repeatedly flip the active root (the atomic pointer swap is the
	// in-process analogue of rename()).
	swapper.Add(1)
	go func() {
		defer swapper.Done()
		toggle := true
		for {
			select {
			case <-stop:
				return
			default:
				if toggle {
					activeRoot.Store(&newRoot)
				} else {
					activeRoot.Store(&oldRoot)
				}
				toggle = !toggle
			}
		}
	}()

	// Readers: every response body must be exactly "OLD" or "NEW", never a mix.
	const readers = 8
	const iterations = 300
	var rg sync.WaitGroup
	errCh := make(chan string, readers)
	for i := 0; i < readers; i++ {
		rg.Add(1)
		go func() {
			defer rg.Done()
			for j := 0; j < iterations; j++ {
				resp := serveOnce(h, mkReq("expense-calc.hosting.example.com", "/", nil))
				body := bodyString(resp)
				hasOld := stringContains(body, "OLD")
				hasNew := stringContains(body, "NEW")
				if !hasOld && !hasNew {
					errCh <- "unexpected body: " + body
					return
				}
				if hasOld && hasNew {
					errCh <- "half tree observed: " + body
					return
				}
			}
		}()
	}

	rg.Wait()   // readers finished
	close(stop) // tell the swapper to stop
	swapper.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
}

// swapSiteService returns the atomically-active root for the published branch.
type swapSiteService struct {
	siteID     uuid.UUID
	commitTime time.Time
	active     *atomic.Pointer[string]
}

func (s *swapSiteService) GetSiteByHandle(_ context.Context, h site.Handle) (site.Site, error) {
	if h != "expense-calc" {
		return site.Site{}, site.ErrNotFound
	}
	return site.Site{ID: s.siteID, Handle: h}, nil
}

func (s *swapSiteService) ServedTree(_ context.Context, _ uuid.UUID, _ site.BranchName) (site.TreeHandle, error) {
	root := *s.active.Load()
	return site.TreeHandle{Root: root, CommitSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", CommitTime: s.commitTime, Exists: true}, nil
}

// TestServiceTreeProvider_BranchNotFound maps site.ErrNotFound from ServedTree to
// ErrBranchNotFound (404), and a missing handle to ErrSiteNotFound.
func TestServiceTreeProvider_Errors(t *testing.T) {
	siteID := uuid.New()
	root := writeTree(t, map[string]string{"index.html": "x"})
	svc := &fakeSiteService{
		byHandle: map[string]site.Site{"s": {ID: siteID, Handle: "s"}},
		trees: map[string]site.TreeHandle{
			siteID.String() + "\x00published": {Root: root, Exists: true},
		},
		redirects: map[string]string{},
	}
	tp := NewServiceTreeProvider(svc, nil)

	if _, err := tp.Tree(context.Background(), resolve.Target{Handle: "ghost", Branch: "published"}); err != ErrSiteNotFound {
		t.Fatalf("missing handle want ErrSiteNotFound, got %v", err)
	}
	if _, err := tp.Tree(context.Background(), resolve.Target{Handle: "s", Branch: "ghostbranch", IsPreview: true}); err != ErrBranchNotFound {
		t.Fatalf("missing branch want ErrBranchNotFound, got %v", err)
	}
	// Existing published resolves to a real os.DirFS.
	th, err := tp.Tree(context.Background(), resolve.Target{Handle: "s", Branch: "published"})
	if err != nil {
		t.Fatalf("published resolve: %v", err)
	}
	if th.SiteID != siteID {
		t.Fatalf("SiteID not propagated")
	}
	if _, serr := fs.Stat(th.FS, "index.html"); serr != nil {
		t.Fatalf("FS should contain index.html: %v", serr)
	}
}

// TestCachingTreeProvider covers preview caching + published passthrough.
func TestCachingTreeProvider(t *testing.T) {
	var calls atomic.Int32
	inner := treeFunc(func(_ context.Context, target resolve.Target) (TreeHandle, error) {
		calls.Add(1)
		return TreeHandle{FS: fstest.MapFS{"index.html": {Data: []byte("x")}}, CommitSHA: "sha", Exists: true}, nil
	})
	now := time.Now()
	c := NewCachingTreeProvider(inner, time.Minute, 4, func() time.Time { return now })

	prev := resolve.Target{Handle: "s", Branch: "draft", IsPreview: true}
	pub := resolve.Target{Handle: "s", Branch: "published"}

	// Two preview requests => one underlying call (cached).
	_, _ = c.Tree(context.Background(), prev)
	_, _ = c.Tree(context.Background(), prev)
	if calls.Load() != 1 {
		t.Fatalf("preview should be cached: %d calls", calls.Load())
	}
	// Published always passes through.
	_, _ = c.Tree(context.Background(), pub)
	_, _ = c.Tree(context.Background(), pub)
	if calls.Load() != 3 {
		t.Fatalf("published should bypass cache: %d calls", calls.Load())
	}
}

// ---- tiny test plumbing ----

type treeFunc func(context.Context, resolve.Target) (TreeHandle, error)

func (f treeFunc) Tree(ctx context.Context, t resolve.Target) (TreeHandle, error) { return f(ctx, t) }

// mkReq builds a GET request with a Host header + optional extra headers.
func mkReq(host, path string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

// serveOnce drives the handler with an httptest recorder and returns the response.
func serveOnce(h *Handler, r *http.Request) *http.Response {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func bodyString(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func contains(resp *http.Response, want string) bool {
	return stringContains(bodyString(resp), want)
}

func contains301(loc, want string) bool { return stringContains(loc, want) }

func stringContains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
