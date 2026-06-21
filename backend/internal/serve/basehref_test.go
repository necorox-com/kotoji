package serve

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

func TestServe_BaseHrefInjection(t *testing.T) {
	htmlNoBase := []byte("<!doctype html><html><head><title>t</title></head><body>x</body></html>")
	htmlWithBase := []byte(`<!doctype html><html><head><base href="/already/"><title>t</title></head><body>x</body></html>`)
	htmlMetaCharset := []byte(`<!doctype html><html><head><meta charset="utf-8"><title>t</title></head><body>x</body></html>`)

	t.Run("path_mode_injects_after_head", func(t *testing.T) {
		files := fstest.MapFS{"index.html": {Data: htmlNoBase}}
		h, _ := newTestHandler(t, pathTarget("expense-calc"), files, nil)
		w := do(h, http.MethodGet, "/host/expense-calc/")
		body := w.Body.String()
		want := `<base href="/host/expense-calc/">`
		if !strings.Contains(body, want) {
			t.Fatalf("base not injected: %q", body)
		}
		// Injected immediately after <head>, before <title>.
		hi := strings.Index(body, "<head>")
		bi := strings.Index(body, want)
		ti := strings.Index(body, "<title>")
		if !(hi < bi && bi < ti) {
			t.Fatalf("base not placed right after <head>: head=%d base=%d title=%d", hi, bi, ti)
		}
	})

	t.Run("injected_response_is_no_store", func(t *testing.T) {
		files := fstest.MapFS{"index.html": {Data: htmlNoBase}}
		h, _ := newTestHandler(t, pathTarget("expense-calc"), files, nil)
		w := do(h, http.MethodGet, "/host/expense-calc/")
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("injected response Cache-Control want no-store, got %q", cc)
		}
		// No ETag on a request-specific rewrite.
		if w.Header().Get("ETag") != "" {
			t.Fatalf("transformed response must not have ETag")
		}
	})

	t.Run("content_length_recomputed", func(t *testing.T) {
		files := fstest.MapFS{"index.html": {Data: htmlNoBase}}
		h, _ := newTestHandler(t, pathTarget("expense-calc"), files, nil)
		w := do(h, http.MethodGet, "/host/expense-calc/")
		cl := w.Header().Get("Content-Length")
		if cl == "" {
			t.Fatal("missing Content-Length")
		}
		n, _ := strconv.Atoi(cl)
		if n != w.Body.Len() {
			t.Fatalf("Content-Length %d != body len %d", n, w.Body.Len())
		}
		if n <= len(htmlNoBase) {
			t.Fatalf("Content-Length should grow after injection: %d vs %d", n, len(htmlNoBase))
		}
	})

	t.Run("existing_base_not_injected", func(t *testing.T) {
		files := fstest.MapFS{"index.html": {Data: htmlWithBase}}
		h, _ := newTestHandler(t, pathTarget("expense-calc"), files, nil)
		w := do(h, http.MethodGet, "/host/expense-calc/")
		if strings.Count(w.Body.String(), "<base") != 1 {
			t.Fatalf("must not inject a second <base>: %q", w.Body.String())
		}
		if strings.Contains(w.Body.String(), `href="/host/`) {
			t.Fatalf("should keep the existing base, not inject ours")
		}
	})

	t.Run("meta_charset_injects_after_meta", func(t *testing.T) {
		files := fstest.MapFS{"index.html": {Data: htmlMetaCharset}}
		h, _ := newTestHandler(t, pathTarget("expense-calc"), files, nil)
		w := do(h, http.MethodGet, "/host/expense-calc/")
		body := w.Body.String()
		mi := strings.Index(body, "<meta charset")
		bi := strings.Index(body, "<base")
		if !(mi >= 0 && bi >= 0 && mi < bi) {
			t.Fatalf("base must follow <meta charset>: meta=%d base=%d", mi, bi)
		}
	})

	t.Run("host_mode_never_injected", func(t *testing.T) {
		files := fstest.MapFS{"index.html": {Data: htmlNoBase}}
		h, _ := newTestHandler(t, publishedTarget("expense-calc"), files, nil)
		w := do(h, http.MethodGet, "/")
		if strings.Contains(w.Body.String(), "<base") {
			t.Fatalf("Host mode must never inject <base>: %q", w.Body.String())
		}
		// And Host-mode HTML keeps its ETag + no-cache (not no-store).
		if w.Header().Get("ETag") == "" {
			t.Fatalf("Host-mode HTML should keep ETag")
		}
	})

	t.Run("non_html_never_injected", func(t *testing.T) {
		files := fstest.MapFS{"app.js": {Data: []byte("var base='/x';")}}
		h, _ := newTestHandler(t, pathTarget("expense-calc"), files, nil)
		w := do(h, http.MethodGet, "/host/expense-calc/app.js")
		if strings.Contains(w.Body.String(), "<base") {
			t.Fatalf("non-HTML must never be transformed")
		}
	})

	t.Run("head_beyond_scan_limit_served_unmodified", func(t *testing.T) {
		// Pad the document so <head> appears past the scan window.
		pad := strings.Repeat("<!-- filler -->", (baseHrefScanLimit/15)+10)
		big := []byte("<!doctype html>" + pad + "<html><head></head><body>x</body></html>")
		files := fstest.MapFS{"index.html": {Data: big}}
		h, _ := newTestHandler(t, pathTarget("expense-calc"), files, nil)
		w := do(h, http.MethodGet, "/host/expense-calc/")
		if strings.Contains(w.Body.String(), "<base href=\"/host/") {
			t.Fatalf("must not inject when <head> is beyond the scan limit")
		}
	})
}

// TestMaybeInjectBaseHref_Unit exercises the pure transform directly for edge cases.
func TestMaybeInjectBaseHref_Unit(t *testing.T) {
	pathT := resolve.Target{Handle: "h", Branch: "published", Source: resolve.SourcePath, PathPrefix: "/host/h"}
	hostT := resolve.Target{Handle: "h", Branch: "published", Source: resolve.SourceHost}

	t.Run("host_mode_noop", func(t *testing.T) {
		in := []byte("<head></head>")
		out, mod := maybeInjectBaseHref(in, hostT, ServeOptions{InjectBaseHref: true})
		if mod || string(out) != string(in) {
			t.Fatalf("host mode must be a noop")
		}
	})

	t.Run("disabled_noop", func(t *testing.T) {
		in := []byte("<head></head>")
		_, mod := maybeInjectBaseHref(in, pathT, ServeOptions{InjectBaseHref: false})
		if mod {
			t.Fatalf("disabled must be a noop")
		}
	})

	t.Run("no_head_noop", func(t *testing.T) {
		in := []byte("<html><body>no head here</body></html>")
		_, mod := maybeInjectBaseHref(in, pathT, ServeOptions{InjectBaseHref: true})
		if mod {
			t.Fatalf("missing <head> must be a noop")
		}
	})

	t.Run("case_insensitive_head", func(t *testing.T) {
		in := []byte("<HTML><HEAD></HEAD></HTML>")
		out, mod := maybeInjectBaseHref(in, pathT, ServeOptions{InjectBaseHref: true})
		if !mod || !strings.Contains(string(out), `<base href="/host/h/">`) {
			t.Fatalf("uppercase HEAD should still inject: %q", out)
		}
	})

	t.Run("basefoo_not_treated_as_base", func(t *testing.T) {
		// "<basefoo" is not a <base> tag; injection should still happen.
		in := []byte("<head><basefoobar></head>")
		out, mod := maybeInjectBaseHref(in, pathT, ServeOptions{InjectBaseHref: true})
		if !mod || strings.Count(string(out), `href="/host/h/"`) != 1 {
			t.Fatalf("<basefoo> must not block injection: %q", out)
		}
	})
}
