package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

func do(h *Handler, method, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServe_IndexResolution(t *testing.T) {
	files := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><html><head></head><body>root</body></html>")},
		"sub/index.html":   {Data: []byte("<html><head></head><body>sub</body></html>")},
		"page.html":        {Data: []byte("<html><head></head><body>page</body></html>")},
		"empty/keep.txt":   {Data: []byte("x")},
		"afile":            {Data: []byte("plain")},
		"assets/style.css": {Data: []byte("body{}")},
	}
	h, _ := newTestHandler(t, publishedTarget("expense-calc"), files, nil)

	t.Run("root_serves_index", func(t *testing.T) {
		w := do(h, http.MethodGet, "/")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "root") {
			t.Fatalf("want 200 root, got %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("dir_without_trailing_slash_301", func(t *testing.T) {
		w := do(h, http.MethodGet, "/sub")
		if w.Code != http.StatusMovedPermanently {
			t.Fatalf("want 301, got %d", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/sub/" {
			t.Fatalf("want Location /sub/, got %q", loc)
		}
	})

	t.Run("dir_with_trailing_slash_serves_index", func(t *testing.T) {
		w := do(h, http.MethodGet, "/sub/")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "sub") {
			t.Fatalf("want 200 sub, got %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("dir_without_index_404", func(t *testing.T) {
		w := do(h, http.MethodGet, "/empty/")
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", w.Code)
		}
	})

	t.Run("trailing_slash_on_file_404", func(t *testing.T) {
		w := do(h, http.MethodGet, "/afile/")
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", w.Code)
		}
	})

	t.Run("file_served", func(t *testing.T) {
		w := do(h, http.MethodGet, "/page.html")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "page") {
			t.Fatalf("want 200 page, got %d", w.Code)
		}
	})

	t.Run("missing_no_implicit_html", func(t *testing.T) {
		// /page exists only as page.html; no implicit .html resolution.
		w := do(h, http.MethodGet, "/page")
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404 (no implicit .html), got %d", w.Code)
		}
	})

	t.Run("301_preserves_query", func(t *testing.T) {
		w := do(h, http.MethodGet, "/sub?a=1&b=2")
		if w.Code != http.StatusMovedPermanently {
			t.Fatalf("want 301, got %d", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/sub/?a=1&b=2" {
			t.Fatalf("want query preserved, got %q", loc)
		}
	})
}

func TestServe_MIME_Table(t *testing.T) {
	// One file per allowlisted extension + one unknown extension.
	files := fstest.MapFS{
		"a.html":       {Data: []byte("<html><head></head></html>")},
		"a.css":        {Data: []byte("x")},
		"a.js":         {Data: []byte("x")},
		"a.json":       {Data: []byte("{}")},
		"a.svg":        {Data: []byte("<svg></svg>")},
		"a.png":        {Data: []byte("x")},
		"a.woff2":      {Data: []byte("x")},
		"a.wasm":       {Data: []byte("x")},
		"a.unknownext": {Data: []byte("danger")},
	}
	h, _ := newTestHandler(t, publishedTarget("s"), files, nil)

	cases := []struct {
		path string
		ct   string
	}{
		{"/a.html", "text/html; charset=utf-8"},
		{"/a.css", "text/css; charset=utf-8"},
		{"/a.js", "text/javascript; charset=utf-8"},
		{"/a.json", "application/json; charset=utf-8"},
		{"/a.svg", "image/svg+xml"},
		{"/a.png", "image/png"},
		{"/a.woff2", "font/woff2"},
		{"/a.wasm", "application/wasm"},
	}
	for _, tc := range cases {
		w := do(h, http.MethodGet, tc.path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", tc.path, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != tc.ct {
			t.Fatalf("%s: Content-Type want %q got %q", tc.path, tc.ct, got)
		}
	}

	t.Run("unknown_ext_octet_attachment", func(t *testing.T) {
		w := do(h, http.MethodGet, "/a.unknownext")
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != contentTypeOctetStream {
			t.Fatalf("want octet-stream, got %q", ct)
		}
		if cd := w.Header().Get("Content-Disposition"); cd != "attachment" {
			t.Fatalf("want attachment, got %q", cd)
		}
		if nos := w.Header().Get("X-Content-Type-Options"); nos != "nosniff" {
			t.Fatalf("want nosniff, got %q", nos)
		}
	})

	t.Run("svg_strict_csp", func(t *testing.T) {
		w := do(h, http.MethodGet, "/a.svg")
		if csp := w.Header().Get("Content-Security-Policy"); csp != svgCSP {
			t.Fatalf("svg CSP want %q got %q", svgCSP, csp)
		}
	})
}

func TestServe_PathCleaning(t *testing.T) {
	files := fstest.MapFS{
		"index.html":  {Data: []byte("<html><head></head>ok</html>")},
		"a/b.txt":     {Data: []byte("b")},
		".git/config": {Data: []byte("GIT-METADATA-SECRET")},
	}
	h, _ := newTestHandler(t, publishedTarget("s"), files, nil)

	// Traversal attempts: after path.Clean of a rooted path, ".." can never escape
	// the tree root, and a ".git" segment is rejected outright. The git metadata
	// must NEVER be served regardless of the traversal shape.
	cases := []string{
		"/../etc/passwd",
		"/a/../../b",
		"/a/../../../etc/passwd",
		"/.git/config",
		"/a/../.git/config",
		"/%2e%2e/.git/config",
	}
	for _, p := range cases {
		// Build the URL directly to keep the raw (un-cleaned) form where possible.
		r := httptest.NewRequest(http.MethodGet, "http://s.example.com"+p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), "GIT-METADATA-SECRET") {
			t.Fatalf("%s leaked .git content (code %d): %q", p, w.Code, w.Body.String())
		}
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: want 404 (no escape / .git rejected), got %d", p, w.Code)
		}
	}

	t.Run("nul_byte_404", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://s.example.com/", nil)
		r.URL.Path = "/a\x00b"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("nul byte: want 404, got %d", w.Code)
		}
	})

	t.Run("git_segment_direct_404", func(t *testing.T) {
		w := do(h, http.MethodGet, "/.git/config")
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404 for .git, got %d", w.Code)
		}
	})
}

func TestServe_Methods(t *testing.T) {
	files := fstest.MapFS{"index.html": {Data: []byte("<html><head></head>hi</html>")}}
	h, _ := newTestHandler(t, publishedTarget("s"), files, nil)

	t.Run("get_ok", func(t *testing.T) {
		if w := do(h, http.MethodGet, "/"); w.Code != http.StatusOK {
			t.Fatalf("GET want 200, got %d", w.Code)
		}
	})

	t.Run("head_no_body", func(t *testing.T) {
		w := do(h, http.MethodHead, "/")
		if w.Code != http.StatusOK {
			t.Fatalf("HEAD want 200, got %d", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Fatalf("HEAD should have no body, got %d bytes", w.Body.Len())
		}
		if w.Header().Get("Content-Type") == "" {
			t.Fatalf("HEAD should still set Content-Type")
		}
	})

	t.Run("options_204_allow", func(t *testing.T) {
		w := do(h, http.MethodOptions, "/")
		if w.Code != http.StatusNoContent {
			t.Fatalf("OPTIONS want 204, got %d", w.Code)
		}
		if a := w.Header().Get("Allow"); a != allowMethods {
			t.Fatalf("Allow want %q got %q", allowMethods, a)
		}
	})

	t.Run("post_405_allow", func(t *testing.T) {
		w := do(h, http.MethodPost, "/")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST want 405, got %d", w.Code)
		}
		if a := w.Header().Get("Allow"); a != allowMethods {
			t.Fatalf("Allow want %q got %q", allowMethods, a)
		}
	})
}

func TestServe_SecurityHeaders(t *testing.T) {
	files := fstest.MapFS{
		"index.html":     {Data: []byte("<html><head></head>ok</html>")},
		"sub/index.html": {Data: []byte("<html><head></head>sub</html>")},
	}
	h, _ := newTestHandler(t, publishedTarget("s"), files, nil)

	assertHeaders := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		hd := w.Header()
		csp := hd.Get("Content-Security-Policy")
		for _, want := range []string{"frame-ancestors 'none'", "object-src 'none'", "base-uri 'self'", "connect-src *"} {
			if !strings.Contains(csp, want) {
				t.Fatalf("CSP missing %q: %q", want, csp)
			}
		}
		if hd.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("missing nosniff")
		}
		if hd.Get("Referrer-Policy") != "strict-origin-when-cross-origin" {
			t.Fatalf("missing referrer policy")
		}
		if !strings.Contains(hd.Get("Permissions-Policy"), "geolocation=()") {
			t.Fatalf("missing permissions policy")
		}
		if hd.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("missing XFO")
		}
		if hd.Get("Cross-Origin-Opener-Policy") != "same-origin" {
			t.Fatalf("missing COOP")
		}
		if hd.Get("Cross-Origin-Resource-Policy") != "same-origin" {
			t.Fatalf("missing CORP")
		}
		if hd.Get("Server") != "kotoji" {
			t.Fatalf("Server banner want kotoji got %q", hd.Get("Server"))
		}
		if len(w.Result().Cookies()) != 0 {
			t.Fatalf("data-plane response must not Set-Cookie")
		}
		if hd.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("data-plane must not set CORS headers")
		}
	}

	t.Run("on_200", func(t *testing.T) { assertHeaders(t, do(h, http.MethodGet, "/")) })
	t.Run("on_301", func(t *testing.T) { assertHeaders(t, do(h, http.MethodGet, "/sub")) })
	t.Run("on_404", func(t *testing.T) { assertHeaders(t, do(h, http.MethodGet, "/nope")) })

	t.Run("sandbox_off_by_default", func(t *testing.T) {
		w := do(h, http.MethodGet, "/")
		if strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Fatalf("sandbox should be OFF by default")
		}
	})

	t.Run("sandbox_on_when_configured", func(t *testing.T) {
		sec := DefaultSecurityHeaderConfig()
		sec.TopLevelSandbox = true
		sec.CSP = "" // force rebuild
		sec = sec.normalize()
		h2 := NewHandler(Deps{
			Resolver: staticResolver{target: publishedTarget("s")},
			Trees:    &fakeTreeProvider{byHandle: map[string]fakeSite{"s": {fsys: files}}},
			Authz:    OpenPreviewAuthz{},
			Config:   HandlerConfig{Security: sec, Now: func() time.Time { return fixedTime }},
		})
		w := do(h2, http.MethodGet, "/")
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox allow-scripts") {
			t.Fatalf("sandbox should be present when configured: %q", w.Header().Get("Content-Security-Policy"))
		}
	})
}

func TestServe_Caching(t *testing.T) {
	files := fstest.MapFS{
		"index.html": {Data: []byte("<html><head></head>ok</html>")},
		"style.css":  {Data: []byte("body{}")},
	}

	t.Run("html_no_cache_with_etag", func(t *testing.T) {
		h, _ := newTestHandler(t, publishedTarget("s"), files, nil)
		w := do(h, http.MethodGet, "/")
		if cc := w.Header().Get("Cache-Control"); cc != htmlCacheControl {
			t.Fatalf("html Cache-Control want %q got %q", htmlCacheControl, cc)
		}
		if w.Header().Get("ETag") == "" {
			t.Fatalf("html should have ETag")
		}
		if lm := w.Header().Get("Last-Modified"); lm != fixedTime.UTC().Format(http.TimeFormat) {
			t.Fatalf("Last-Modified want commit time, got %q", lm)
		}
	})

	t.Run("asset_max_age", func(t *testing.T) {
		h, _ := newTestHandler(t, publishedTarget("s"), files, nil)
		w := do(h, http.MethodGet, "/style.css")
		if cc := w.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=") {
			t.Fatalf("asset Cache-Control want public max-age, got %q", cc)
		}
	})

	t.Run("if_none_match_304", func(t *testing.T) {
		h, _ := newTestHandler(t, publishedTarget("s"), files, nil)
		w := do(h, http.MethodGet, "/style.css")
		etag := w.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("expected etag")
		}
		r := httptest.NewRequest(http.MethodGet, "http://s/style.css", nil)
		r.Header.Set("If-None-Match", etag)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, r)
		if w2.Code != http.StatusNotModified {
			t.Fatalf("want 304, got %d", w2.Code)
		}
	})

	t.Run("preview_no_store_no_etag", func(t *testing.T) {
		h, _ := newTestHandler(t, previewTarget("s", "draft"), files, OpenPreviewAuthz{})
		w := do(h, http.MethodGet, "/style.css")
		if cc := w.Header().Get("Cache-Control"); cc != previewCacheControl {
			t.Fatalf("preview Cache-Control want %q got %q", previewCacheControl, cc)
		}
		if w.Header().Get("ETag") != "" {
			t.Fatalf("preview must not set ETag")
		}
	})
}

func TestServe_NotFoundPages(t *testing.T) {
	t.Run("builtin_404", func(t *testing.T) {
		files := fstest.MapFS{"index.html": {Data: []byte("<html><head></head>ok</html>")}}
		h, _ := newTestHandler(t, publishedTarget("s"), files, nil)
		w := do(h, http.MethodGet, "/missing")
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "kotoji") {
			t.Fatalf("builtin 404 should be branded: %q", w.Body.String())
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("404 must carry security headers")
		}
	})

	t.Run("per_site_404_override", func(t *testing.T) {
		files := fstest.MapFS{
			"index.html": {Data: []byte("<html><head></head>ok</html>")},
			"404.html":   {Data: []byte("<html><head></head>CUSTOM NOT FOUND</html>")},
		}
		h, _ := newTestHandler(t, publishedTarget("s"), files, nil)
		w := do(h, http.MethodGet, "/missing")
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404 status, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "CUSTOM NOT FOUND") {
			t.Fatalf("per-site 404 not served: %q", w.Body.String())
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("per-site 404 must carry security headers")
		}
	})
}

func TestServe_TreeProviderErrors(t *testing.T) {
	t.Run("site_not_found_404", func(t *testing.T) {
		tp := &fakeTreeProvider{byHandle: map[string]fakeSite{}}
		h := NewHandler(Deps{
			Resolver: staticResolver{target: publishedTarget("ghost")},
			Trees:    tp,
			Authz:    OpenPreviewAuthz{},
			Config:   HandlerConfig{Now: func() time.Time { return fixedTime }},
		})
		w := do(h, http.MethodGet, "/")
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", w.Code)
		}
	})

	t.Run("branch_not_found_404", func(t *testing.T) {
		tp := &fakeTreeProvider{
			byHandle:     map[string]fakeSite{"s": {fsys: fstest.MapFS{}}},
			branchExists: map[string]bool{"s--published": true},
		}
		h := NewHandler(Deps{
			Resolver: staticResolver{target: previewTarget("s", "ghostbranch")},
			Trees:    tp,
			Authz:    OpenPreviewAuthz{},
			Config:   HandlerConfig{Now: func() time.Time { return fixedTime }},
		})
		w := do(h, http.MethodGet, "/")
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", w.Code)
		}
	})

	t.Run("renamed_handle_301_host_mode", func(t *testing.T) {
		tp := &fakeTreeProvider{redirects: map[string]string{"old-name": "expense-calc"}}
		h := NewHandler(Deps{
			Resolver: staticResolver{target: publishedTarget("old-name")},
			Trees:    tp,
			Authz:    OpenPreviewAuthz{},
			Config:   HandlerConfig{Now: func() time.Time { return fixedTime }},
		})
		r := httptest.NewRequest(http.MethodGet, "http://old-name.hosting.example.com/x?q=1", nil)
		r.Host = "old-name.hosting.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMovedPermanently {
			t.Fatalf("want 301, got %d", w.Code)
		}
		loc := w.Header().Get("Location")
		if !strings.Contains(loc, "expense-calc.hosting.example.com/x") || !strings.HasSuffix(loc, "?q=1") {
			t.Fatalf("301 Location want new handle + path + query, got %q", loc)
		}
	})

	t.Run("renamed_handle_301_path_mode", func(t *testing.T) {
		tp := &fakeTreeProvider{redirects: map[string]string{"old-name": "new-name"}}
		target := resolve.Target{Handle: "old-name", Branch: "published", Source: resolve.SourcePath, PathPrefix: "/host/old-name"}
		h := NewHandler(Deps{
			Resolver: staticResolver{target: target},
			Trees:    tp,
			Authz:    OpenPreviewAuthz{},
			Config:   HandlerConfig{Now: func() time.Time { return fixedTime }},
		})
		r := httptest.NewRequest(http.MethodGet, "http://hosting.example.com/host/old-name/x", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMovedPermanently {
			t.Fatalf("want 301, got %d", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/host/new-name/x" {
			t.Fatalf("path-mode 301 want /host/new-name/x, got %q", loc)
		}
	})
}
