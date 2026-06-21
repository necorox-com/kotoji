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

func TestResolve_PathMode_Table(t *testing.T) {
	type want struct {
		handle     string
		branch     string
		isPreview  bool
		source     Source
		pathPrefix string
		errStatus  int
		errCode    string
	}
	cfg := prodCfg()
	cases := []struct {
		name string
		cfg  Config
		host string
		path string
		want want
	}{
		{"path_published_on_control", cfg, "hosting.example.com", "/host/expense-calc/style.css",
			want{handle: "expense-calc", branch: "published", source: SourcePath, pathPrefix: "/host/expense-calc"}},
		{"path_preview_on_control", cfg, "hosting.example.com", "/host/expense-calc--draft/",
			want{handle: "expense-calc", branch: "draft", isPreview: true, source: SourcePath, pathPrefix: "/host/expense-calc--draft"}},
		{"path_preview_asset", cfg, "hosting.example.com", "/host/expense-calc--draft/style.css",
			want{handle: "expense-calc", branch: "draft", isPreview: true, source: SourcePath, pathPrefix: "/host/expense-calc--draft"}},
		{"path_no_label_404", cfg, "hosting.example.com", "/host/",
			want{errStatus: http.StatusNotFound, errCode: CodeNotProjectHost}},
		{"path_host_bare_404", cfg, "hosting.example.com", "/host",
			want{errStatus: http.StatusNotFound, errCode: CodeNotProjectHost}},
		{"path_label_lowercased", cfg, "hosting.example.com", "/host/Expense-Calc/Style.CSS",
			want{handle: "expense-calc", branch: "published", source: SourcePath, pathPrefix: "/host/Expense-Calc"}},
		{"path_percent_encoded_slash_branch_rejected", cfg, "hosting.example.com", "/host/site--feature%2Fx/",
			want{errStatus: http.StatusBadRequest, errCode: CodeBadBranch}},
		{"path_published_via_dashdash_rejected", cfg, "hosting.example.com", "/host/site--published/",
			want{errStatus: http.StatusBadRequest, errCode: CodeBadBranch}},
		// path mode works from a foreign host too (proxy-independent reach).
		{"path_on_foreign_host", cfg, "somecdn.example.org", "/host/expense-calc/",
			want{handle: "expense-calc", branch: "published", source: SourcePath, pathPrefix: "/host/expense-calc"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewResolver(tc.cfg)
			got, err := d.Resolve(newReq(tc.host, tc.path))
			assertResolve(t, got, err, tc.want.handle, tc.want.branch, tc.want.isPreview, false, tc.want.source, tc.want.errStatus, tc.want.errCode)
			if err == nil && got.PathPrefix != tc.want.pathPrefix {
				t.Fatalf("PathPrefix: want %q got %q", tc.want.pathPrefix, got.PathPrefix)
			}
		})
	}
}

func TestResolve_PathFallbackDisabled(t *testing.T) {
	cfg := prodCfg()
	cfg.EnablePathFallback = false
	d := NewResolver(cfg)

	// On the control host, /host/... is no longer a project => control plane.
	got, err := d.Resolve(newReq("hosting.example.com", "/host/expense-calc/"))
	if err != nil {
		t.Fatalf("control host with fallback off should resolve to control, got err %v", err)
	}
	if !got.IsControl {
		t.Fatalf("want IsControl, got %+v", got)
	}

	// On a foreign host, /host/... with fallback off => 421 misdirected (not a project).
	_, err = d.Resolve(newReq("evil.com", "/host/expense-calc/"))
	re, ok := err.(*ResolveError)
	if !ok || re.Status != http.StatusMisdirectedRequest {
		t.Fatalf("want 421 misdirected, got %v", err)
	}
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
