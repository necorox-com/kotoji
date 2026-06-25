package resolve

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newReq builds a request with a given Host and path. It deliberately sets both
// r.Host and the URL so effectiveHost falls back correctly when Host is cleared.
func newReq(host, path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+orPlaceholder(host)+path, nil)
	r.Host = host
	return r
}

func orPlaceholder(h string) string {
	if h == "" {
		return "placeholder.invalid"
	}
	return h
}

// prodCfg / devCfg are the two base-domain parity configs (routing-and-serving.md §10).
func prodCfg() Config {
	return Config{BaseDomain: "hosting.example.com", ControlLabel: "", EnablePathFallback: true, TrustForwardedHost: true}
}
func devCfg() Config {
	return Config{BaseDomain: "localhost", ControlLabel: "kotoji", EnablePathFallback: true, TrustForwardedHost: false}
}

func TestResolve_HostMode_Table(t *testing.T) {
	type want struct {
		handle    string
		branch    string
		isPreview bool
		isControl bool
		source    Source
		errStatus int // 0 => no error
		errCode   string
	}
	cases := []struct {
		name string
		cfg  Config
		host string
		path string
		want want
	}{
		// ---- prod (bare base = control) ----
		{"prod_control_bare", prodCfg(), "hosting.example.com", "/", want{isControl: true, source: SourceHost}},
		{"prod_control_api_path", prodCfg(), "hosting.example.com", "/api/me", want{isControl: true, source: SourceHost}},
		{"prod_published", prodCfg(), "expense-calc.hosting.example.com", "/", want{handle: "expense-calc", branch: "published", source: SourceHost}},
		{"prod_published_asset", prodCfg(), "expense-calc.hosting.example.com", "/style.css", want{handle: "expense-calc", branch: "published", source: SourceHost}},
		{"prod_preview_draft", prodCfg(), "expense-calc--draft.hosting.example.com", "/", want{handle: "expense-calc", branch: "draft", isPreview: true, source: SourceHost}},
		{"prod_preview_feature", prodCfg(), "expense-calc--feature-bob-fix.hosting.example.com", "/", want{handle: "expense-calc", branch: "feature-bob-fix", isPreview: true, source: SourceHost}},
		{"prod_published_via_dashdash_rejected", prodCfg(), "expense-calc--published.hosting.example.com", "/", want{errStatus: http.StatusBadRequest, errCode: CodeBadBranch}},
		{"prod_mixed_case_lowercased", prodCfg(), "Expense-Calc.Hosting.Example.com", "/", want{handle: "expense-calc", branch: "published", source: SourceHost}},
		{"prod_nested_label_404", prodCfg(), "a.b.hosting.example.com", "/", want{errStatus: http.StatusNotFound, errCode: CodeNotProjectHost}},
		{"prod_reserved_word_draft_404", prodCfg(), "draft.hosting.example.com", "/", want{errStatus: http.StatusNotFound, errCode: CodeNotProjectHost}},
		{"prod_reserved_word_api_404", prodCfg(), "api.hosting.example.com", "/", want{errStatus: http.StatusNotFound, errCode: CodeNotProjectHost}},
		{"prod_valid_shape_no_site_resolves", prodCfg(), "nope.hosting.example.com", "/", want{handle: "nope", branch: "published", source: SourceHost}},
		{"prod_wrong_domain_421", prodCfg(), "evil.com", "/", want{errStatus: http.StatusMisdirectedRequest, errCode: CodeMisdirected}},
		{"prod_multi_dashdash_400", prodCfg(), "a--b--c.hosting.example.com", "/", want{errStatus: http.StatusBadRequest, errCode: CodeBadBranch}},
		{"prod_leading_hyphen_handle_bad", prodCfg(), "-bad.hosting.example.com", "/", want{errStatus: http.StatusBadRequest, errCode: CodeBadHandle}},
		{"prod_trailing_hyphen_handle_bad", prodCfg(), "bad-.hosting.example.com", "/", want{errStatus: http.StatusBadRequest, errCode: CodeBadHandle}},
		{"prod_short_handle_resolves", prodCfg(), "x.hosting.example.com", "/", want{handle: "x", branch: "published", source: SourceHost}},
		{"prod_branch_trailing_hyphen_bad", prodCfg(), "site--bad-.hosting.example.com", "/", want{errStatus: http.StatusBadRequest, errCode: CodeBadBranch}},
		// port stripping
		{"prod_published_with_port", prodCfg(), "expense-calc.hosting.example.com:8080", "/", want{handle: "expense-calc", branch: "published", source: SourceHost}},

		// ---- dev (ControlLabel=kotoji) ----
		{"dev_control_kotoji", devCfg(), "kotoji.localhost:8080", "/", want{isControl: true, source: SourceHost}},
		{"dev_bare_base_404_control_required", devCfg(), "localhost:8080", "/", want{errStatus: http.StatusNotFound, errCode: CodeNotProjectHost}},
		{"dev_published", devCfg(), "expense-calc.localhost:8080", "/", want{handle: "expense-calc", branch: "published", source: SourceHost}},
		{"dev_preview", devCfg(), "expense-calc--draft.localhost:8080", "/", want{handle: "expense-calc", branch: "draft", isPreview: true, source: SourceHost}},
		{"dev_nested_404", devCfg(), "a.b.localhost", "/", want{errStatus: http.StatusNotFound, errCode: CodeNotProjectHost}},
		{"dev_reserved_404", devCfg(), "mcp.localhost", "/", want{errStatus: http.StatusNotFound, errCode: CodeNotProjectHost}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewResolver(tc.cfg)
			got, err := d.Resolve(newReq(tc.host, tc.path))
			assertResolve(t, got, err, tc.want.handle, tc.want.branch, tc.want.isPreview, tc.want.isControl, tc.want.source, tc.want.errStatus, tc.want.errCode)
		})
	}
}

func TestResolve_LabelTooLong(t *testing.T) {
	// {handle}--{branch} > 63 chars => label_too_long even though each part is valid.
	handle := strings.Repeat("a", 40)
	branch := strings.Repeat("b", 40)
	host := handle + "--" + branch + ".hosting.example.com"
	d := NewResolver(prodCfg())
	_, err := d.Resolve(newReq(host, "/"))
	re, ok := err.(*ResolveError)
	if !ok {
		t.Fatalf("want *ResolveError, got %T (%v)", err, err)
	}
	if re.Status != http.StatusBadRequest || re.Code != CodeLabelTooLong {
		t.Fatalf("want 400/%s, got %d/%s", CodeLabelTooLong, re.Status, re.Code)
	}
}

// TestResolve_SubdomainOnly_NoPathServing is the M1 regression guard: serving is
// subdomain-ONLY. A /host/{handle}/... request to the CONTROL host must NEVER
// resolve to project content (it would otherwise serve untrusted content
// same-origin with the dashboard/API). It must route to the control plane
// instead. The same path on a foreign host is misdirected (421), not a project.
// Subdomain serving is asserted to still work so the fix is surgical.
func TestResolve_SubdomainOnly_NoPathServing(t *testing.T) {
	cfg := prodCfg() // EnablePathFallback is a deprecated no-op; set true here on purpose.

	t.Run("host_path_on_control_routes_to_control_not_project", func(t *testing.T) {
		d := NewResolver(cfg)
		// These are exactly the attack shapes the audit flagged: /host/<handle>/evil.html
		// on the control origin. Each MUST be IsControl (control plane 404s the route),
		// never a project Target carrying a Handle.
		for _, p := range []string{
			"/host/expense-calc/evil.html",
			"/host/expense-calc/",
			"/host/expense-calc--draft/style.css",
			"/host/",
			"/host",
			"/host/Expense-Calc/Style.CSS",
			"/host/site--feature%2Fx/",
		} {
			got, err := d.Resolve(newReq("hosting.example.com", p))
			if err != nil {
				t.Fatalf("control host %q: unexpected error %v (must route to control)", p, err)
			}
			if !got.IsControl {
				t.Fatalf("control host %q: want IsControl (no project serving on control origin), got %+v", p, got)
			}
			if got.Handle != "" || got.PathPrefix != "" || got.Source == SourcePath {
				t.Fatalf("control host %q: leaked project content on control origin: %+v", p, got)
			}
		}
	})

	t.Run("host_path_on_foreign_host_is_misdirected", func(t *testing.T) {
		d := NewResolver(cfg)
		// With path serving removed, a /host/ path on a foreign host has no fallback
		// to honor and is misdirected (not anonymous project content).
		_, err := d.Resolve(newReq("somecdn.example.org", "/host/expense-calc/"))
		re, ok := err.(*ResolveError)
		if !ok || re.Status != http.StatusMisdirectedRequest || re.Code != CodeMisdirected {
			t.Fatalf("foreign host /host/ path: want 421 misdirected, got %v", err)
		}
	})

	t.Run("subdomain_serving_still_works", func(t *testing.T) {
		d := NewResolver(cfg)
		// Published via bare handle host.
		got, err := d.Resolve(newReq("expense-calc.hosting.example.com", "/host/anything/here"))
		if err != nil {
			t.Fatalf("subdomain published: unexpected error %v", err)
		}
		if got.IsControl || got.Handle != "expense-calc" || got.Branch != "published" || got.Source != SourceHost {
			t.Fatalf("subdomain published broken: %+v", got)
		}
		// A /host/-shaped PATH under a project subdomain is just a normal in-site path;
		// the resolver still classifies the host as the project (path is the serve
		// layer's concern), and PathPrefix stays empty (no path-mode stripping).
		if got.PathPrefix != "" || got.Source == SourcePath {
			t.Fatalf("subdomain host must not enter path mode: %+v", got)
		}
		// Preview via {handle}--{branch} host.
		pv, err := d.Resolve(newReq("expense-calc--draft.hosting.example.com", "/"))
		if err != nil {
			t.Fatalf("subdomain preview: unexpected error %v", err)
		}
		if !pv.IsPreview || pv.Branch != "draft" || pv.Source != SourceHost {
			t.Fatalf("subdomain preview broken: %+v", pv)
		}
	})
}

func TestResolve_EffectiveHost(t *testing.T) {
	t.Run("xfh_honored_when_trusted", func(t *testing.T) {
		d := NewResolver(prodCfg()) // TrustForwardedHost=true
		r := newReq("hosting.example.com", "/")
		r.Header.Set("X-Forwarded-Host", "expense-calc.hosting.example.com")
		got, err := d.Resolve(r)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got.Handle != "expense-calc" {
			t.Fatalf("XFH not honored: %+v", got)
		}
	})

	t.Run("xfh_ignored_when_untrusted", func(t *testing.T) {
		d := NewResolver(devCfg()) // TrustForwardedHost=false
		r := newReq("kotoji.localhost", "/")
		r.Header.Set("X-Forwarded-Host", "expense-calc.localhost")
		got, err := d.Resolve(r)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !got.IsControl {
			t.Fatalf("XFH should have been ignored, got %+v", got)
		}
	})

	t.Run("xfh_first_token_on_comma_list", func(t *testing.T) {
		d := NewResolver(prodCfg())
		r := newReq("hosting.example.com", "/")
		r.Header.Set("X-Forwarded-Host", "a-tool.hosting.example.com, internal-proxy.local")
		got, err := d.Resolve(r)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got.Handle != "a-tool" {
			t.Fatalf("first token not taken: %+v", got)
		}
	})

	t.Run("port_stripped", func(t *testing.T) {
		d := NewResolver(prodCfg())
		got, err := d.Resolve(newReq("a-tool.hosting.example.com:31337", "/"))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got.Handle != "a-tool" {
			t.Fatalf("port not stripped: %+v", got)
		}
	})

	t.Run("empty_host_falls_back_to_url_host", func(t *testing.T) {
		d := NewResolver(prodCfg())
		r := newReq("a-tool.hosting.example.com", "/")
		r.Host = "" // force the URL.Host fallback
		got, err := d.Resolve(r)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got.Handle != "a-tool" {
			t.Fatalf("URL.Host fallback failed: %+v", got)
		}
	})

	t.Run("truly_empty_host_errors", func(t *testing.T) {
		d := NewResolver(prodCfg())
		r := &http.Request{Method: http.MethodGet, Header: http.Header{}, URL: mustURL("/")}
		_, err := d.Resolve(r)
		re, ok := err.(*ResolveError)
		if !ok || re.Status != http.StatusBadRequest || re.Code != CodeEmptyHost {
			t.Fatalf("want 400 empty_host, got %v", err)
		}
	})
}

func TestResolve_Errors_CarryStatus(t *testing.T) {
	d := NewResolver(prodCfg())
	cases := []struct {
		host, path string
		status     int
		code       string
	}{
		{"evil.com", "/", http.StatusMisdirectedRequest, CodeMisdirected},
		{"a.b.hosting.example.com", "/", http.StatusNotFound, CodeNotProjectHost},
		{"draft.hosting.example.com", "/", http.StatusNotFound, CodeNotProjectHost},
		{"site--published.hosting.example.com", "/", http.StatusBadRequest, CodeBadBranch},
		{"-bad.hosting.example.com", "/", http.StatusBadRequest, CodeBadHandle},
	}
	for _, tc := range cases {
		_, err := d.Resolve(newReq(tc.host, tc.path))
		re, ok := err.(*ResolveError)
		if !ok {
			t.Fatalf("%s: want *ResolveError, got %T", tc.host, err)
		}
		if re.Status != tc.status || re.Code != tc.code {
			t.Fatalf("%s: want %d/%s got %d/%s", tc.host, tc.status, tc.code, re.Status, re.Code)
		}
		// Error() includes the code for log correlation.
		if !strings.Contains(re.Error(), tc.code) {
			t.Fatalf("%s: Error() %q missing code", tc.host, re.Error())
		}
	}
}

// TestResolve_BaseDomain_Parity asserts the identical handle/branch outcome for
// the same logical request under both base domains (routing-and-serving.md §10).
func TestResolve_BaseDomain_Parity(t *testing.T) {
	prod := NewResolver(prodCfg())
	dev := NewResolver(devCfg())

	gp, err := prod.Resolve(newReq("shop--draft.hosting.example.com", "/x"))
	if err != nil {
		t.Fatalf("prod err: %v", err)
	}
	gd, err := dev.Resolve(newReq("shop--draft.localhost:8080", "/x"))
	if err != nil {
		t.Fatalf("dev err: %v", err)
	}
	if gp.Handle != gd.Handle || gp.Branch != gd.Branch || gp.IsPreview != gd.IsPreview {
		t.Fatalf("parity broken: prod=%+v dev=%+v", gp, gd)
	}
}

// TestReservedHandlesInSyncWithGrammar guards the local reserved list against
// drift from the canonical 12-word set.
func TestReservedHandlesInSync(t *testing.T) {
	canonical := []string{
		"draft", "preview", "published", "www", "api", "internal",
		"host", "admin", "app", "static", "assets", "mcp",
	}
	if len(reservedHandles) != len(canonical) {
		t.Fatalf("reserved count drift: got %d want %d", len(reservedHandles), len(canonical))
	}
	for _, w := range canonical {
		if _, ok := reservedHandles[w]; !ok {
			t.Fatalf("reserved word %q missing", w)
		}
	}
}

// ---- shared assertions / helpers ----

func assertResolve(t *testing.T, got Target, err error, handle, branch string, isPreview, isControl bool, source Source, errStatus int, errCode string) {
	t.Helper()
	if errStatus != 0 {
		re, ok := err.(*ResolveError)
		if !ok {
			t.Fatalf("want *ResolveError %d/%s, got %T (%v) target=%+v", errStatus, errCode, err, err, got)
		}
		if re.Status != errStatus {
			t.Fatalf("status: want %d got %d (%s)", errStatus, re.Status, re.Code)
		}
		if errCode != "" && re.Code != errCode {
			t.Fatalf("code: want %s got %s", errCode, re.Code)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.IsControl != isControl {
		t.Fatalf("IsControl: want %v got %v (%+v)", isControl, got.IsControl, got)
	}
	if isControl {
		return // control targets carry no handle/branch
	}
	if got.Handle != handle {
		t.Fatalf("Handle: want %q got %q", handle, got.Handle)
	}
	if got.Branch != branch {
		t.Fatalf("Branch: want %q got %q", branch, got.Branch)
	}
	if got.IsPreview != isPreview {
		t.Fatalf("IsPreview: want %v got %v", isPreview, got.IsPreview)
	}
	if got.Source != source {
		t.Fatalf("Source: want %v got %v", source, got.Source)
	}
}

func mustURL(p string) *url.URL {
	u, err := url.Parse(p)
	if err != nil {
		panic(err)
	}
	return u
}

// TestResolve_BaseDomainFunc pins the dynamic base-domain seam: when BaseDomainFunc
// is set it overrides the static field per request (the env-EMPTY path), and an
// empty return falls back to the static BaseDomain. This is the data-plane half of
// the WordPress-style runtime domain config.
func TestResolve_BaseDomainFunc(t *testing.T) {
	t.Run("dynamic base overrides static for classification", func(t *testing.T) {
		// Static base is "old.example.com"; the func returns the runtime DB value.
		cfg := Config{
			BaseDomain:         "old.example.com",
			EnablePathFallback: true,
			TrustForwardedHost: false,
			BaseDomainFunc:     func(*http.Request) string { return "new.example.com" },
		}
		r := NewResolver(cfg)

		// A project host under the DYNAMIC base resolves as a project.
		got, err := r.Resolve(newReq("blog.new.example.com", "/"))
		if err != nil {
			t.Fatalf("dynamic project host: unexpected error %v", err)
		}
		if got.IsControl || got.Handle != "blog" {
			t.Fatalf("want project handle=blog, got %+v", got)
		}

		// The DYNAMIC bare base is the control host.
		ctrl, err := r.Resolve(newReq("new.example.com", "/"))
		if err != nil {
			t.Fatalf("dynamic control host: unexpected error %v", err)
		}
		if !ctrl.IsControl {
			t.Fatalf("dynamic bare base should be control, got %+v", ctrl)
		}

		// A host under the STALE static base is now foreign (misdirected).
		if _, err := r.Resolve(newReq("blog.old.example.com", "/")); err == nil {
			t.Fatalf("stale static-base host should no longer resolve as a project")
		}
	})

	t.Run("empty func return falls back to static base", func(t *testing.T) {
		cfg := Config{
			BaseDomain:         "fallback.example.com",
			EnablePathFallback: true,
			BaseDomainFunc:     func(*http.Request) string { return "" }, // unconfigured
		}
		r := NewResolver(cfg)
		got, err := r.Resolve(newReq("blog.fallback.example.com", "/"))
		if err != nil {
			t.Fatalf("fallback project host: unexpected error %v", err)
		}
		if got.Handle != "blog" {
			t.Fatalf("want handle=blog via static fallback, got %+v", got)
		}
	})
}
