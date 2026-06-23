//go:build conformance

// The kin-openapi request/response conformance self-test lives behind the
// `conformance` build tag. Reason: kin-openapi is currently `// indirect` in
// go.mod and its openapi3filter/routers subpackages pull transitive modules that
// need a `require` entry (all hashes are already in the fully-populated go.sum).
// The Integration phase runs `go mod tidy` (its job; this package must not edit
// go.mod), which promotes kin-openapi to a direct require and adds those lines.
// After that, run this suite with:
//
//	go test -tags conformance ./internal/api/...
//
// Gating it keeps the default `go test ./internal/api/...` green on the frozen
// go.mod while still shipping the conformance self-test the contract requires.
package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"

	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

// specPath is the FROZEN contract the wire MUST conform to (CANONICAL §9 #1).
const specPath = "../../../docs/contracts/openapi.yaml"

// loadSpec loads + validates the frozen OpenAPI 3.1 spec. Loading exercises the
// contract itself (a malformed/empty spec fails the whole conformance suite).
func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromFile(specPath)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("spec invalid: %v", err)
	}
	return doc
}

// findRoute locates the operation in the spec for a concrete (method, path) by
// matching each spec path TEMPLATE against the request path, extracting path
// params. This is a self-contained router (no gorillamux dependency) sufficient
// for response/request schema validation.
func findRoute(t *testing.T, doc *openapi3.T, method, concretePath string) (*routers.Route, map[string]string) {
	t.Helper()
	reqSegs := splitPath(concretePath)
	for tmpl, item := range doc.Paths.Map() {
		params, ok := matchTemplate(splitPath(tmpl), reqSegs)
		if !ok {
			continue
		}
		op := item.GetOperation(method)
		if op == nil {
			continue
		}
		return &routers.Route{
			Spec:      doc,
			Path:      tmpl,
			PathItem:  item,
			Method:    method,
			Operation: op,
		}, params
	}
	t.Fatalf("spec has no operation for %s %s", method, concretePath)
	return nil, nil
}

// splitPath splits a URL path (query stripped) into non-empty segments.
func splitPath(p string) []string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// matchTemplate matches a spec path template (segments) against request
// segments, returning the captured {param} values. A "{x}" segment captures any
// single segment; literals must match exactly.
func matchTemplate(tmpl, req []string) (map[string]string, bool) {
	if len(tmpl) != len(req) {
		return nil, false
	}
	params := map[string]string{}
	for i := range tmpl {
		if strings.HasPrefix(tmpl[i], "{") && strings.HasSuffix(tmpl[i], "}") {
			name := strings.TrimSuffix(strings.TrimPrefix(tmpl[i], "{"), "}")
			val, err := url.PathUnescape(req[i])
			if err != nil {
				val = req[i]
			}
			params[name] = val
			continue
		}
		if tmpl[i] != req[i] {
			return nil, false
		}
	}
	return params, true
}

// conformantEnv pairs the live test env with the loaded spec so each call can be
// request- AND response-validated against the contract.
type conformantEnv struct {
	*testEnv
	doc *openapi3.T
}

// checkConformance validates the recorded exchange against the spec: it builds a
// RequestValidationInput from the matched operation, validates the request body
// (when present), then validates the recorded response body+status against the
// operation's response schema. Transport-level security (cookies) is a no-op
// here — the focus is the JSON request/response SHAPES (the drift gate's job).
func (c *conformantEnv) checkConformance(t *testing.T, method, path string, reqBody []byte, reqCType string, rec *httptest.ResponseRecorder) {
	t.Helper()

	route, pathParams := findRoute(t, c.doc, method, path)

	u, err := url.Parse(path)
	if err != nil {
		t.Fatalf("parse path: %v", err)
	}
	httpReq := &http.Request{Method: method, URL: u, Header: http.Header{}}
	if reqCType != "" {
		httpReq.Header.Set("Content-Type", reqCType)
	}

	reqInput := &openapi3filter.RequestValidationInput{
		Request:    httpReq,
		PathParams: pathParams,
		QueryParams: func() url.Values {
			q, _ := url.ParseQuery(u.RawQuery)
			return q
		}(),
		Route: route,
		Options: &openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
	}
	if reqBody != nil {
		httpReq.Body = io.NopCloser(bytes.NewReader(reqBody))
		if err := openapi3filter.ValidateRequest(context.Background(), reqInput); err != nil {
			t.Fatalf("REQUEST not conformant for %s %s: %v", method, path, err)
		}
	}

	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 rec.Code,
		Header:                 rec.Result().Header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if body := rec.Body.Bytes(); len(body) > 0 {
		respInput.SetBodyBytes(body)
	}
	if err := openapi3filter.ValidateResponse(context.Background(), respInput); err != nil {
		t.Fatalf("RESPONSE not conformant for %s %s (status %d): %v\nbody=%s", method, path, rec.Code, err, rec.Body.String())
	}
}

// TestSpecLoadsAndIsValid asserts the frozen contract loads + validates and has
// exactly the documented number of paths.
func TestSpecLoadsAndIsValid(t *testing.T) {
	doc := loadSpec(t)
	// 26 = 23 baseline + POST /api/sites/{handle}/mirror + POST /auth/setup
	// (first-run admin-password setup) + /api/admin/github (GET+PUT, one path).
	if got := doc.Paths.Len(); got != 26 {
		t.Fatalf("spec paths = %d, want 26", got)
	}
}

// TestResponseConformance drives representative success responses through the
// spec validator, asserting every JSON body matches the frozen schema. This is
// the request/response conformance self-test required by the task.
func TestResponseConformance(t *testing.T) {
	base := newTestEnv(t)
	c := &conformantEnv{testEnv: base, doc: loadSpec(t)}

	owner := c.newUser()
	c.createSite("conform-site", owner)
	fc := c.readDraftFile(owner, "conform-site", "index.html")

	// Publish runs against its own site so the shared "conform-site" draft tip is
	// not invalidated by the writeFile fixture (which advances the tip).
	c.createSite("conform-publish", owner)
	pubFC := c.readDraftFile(owner, "conform-publish", "index.html")

	type fixture struct {
		name     string
		method   string
		path     string
		body     any
		wantCode int
	}
	fixtures := []fixture{
		{"listSites", http.MethodGet, "/api/sites", nil, http.StatusOK},
		{"getSite", http.MethodGet, "/api/sites/conform-site", nil, http.StatusOK},
		{"createSite", http.MethodPost, "/api/sites", openapi.CreateSiteRequest{Handle: "conform-new"}, http.StatusCreated},
		{"listBranches", http.MethodGet, "/api/sites/conform-site/branches", nil, http.StatusOK},
		{"listFiles", http.MethodGet, "/api/sites/conform-site/branches/draft/files", nil, http.StatusOK},
		{"readFile", http.MethodGet, "/api/sites/conform-site/branches/draft/file?path=index.html", nil, http.StatusOK},
		{"listMembers", http.MethodGet, "/api/sites/conform-site/members", nil, http.StatusOK},
		{"listTokens", http.MethodGet, "/api/tokens", nil, http.StatusOK},
		{"getLog", http.MethodGet, "/api/sites/conform-site/branches/draft/log", nil, http.StatusOK},
		// mirrorSite returns 200 with a MirrorResult even when not linked (ok=false);
		// this validates the new response schema against the contract.
		{"mirrorSite", http.MethodPost, "/api/sites/conform-site/mirror", nil, http.StatusOK},
		{
			"writeFile", http.MethodPut, "/api/sites/conform-site/branches/draft/file",
			openapi.WriteFileRequest{Path: "index.html", Content: "<h1>c</h1>", BaseSha: fc.Sha}, http.StatusOK,
		},
		{
			"createToken", http.MethodPost, "/api/tokens",
			openapi.CreateTokenRequest{Name: "ci", Scopes: []openapi.TokenScope{openapi.Read}}, http.StatusCreated,
		},
		{
			"publish", http.MethodPost, "/api/sites/conform-publish/publish",
			openapi.PublishRequest{BaseSha: pubFC.Sha}, http.StatusOK,
		},
	}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			rb := c.request(f.method, f.path).as(owner)
			var reqBytes []byte
			var ctype string
			if f.body != nil {
				rb = rb.json(f.body)
				reqBytes = mustJSON(t, f.body)
				ctype = "application/json"
			}
			rec := rb.do()
			if rec.Code != f.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, f.wantCode, rec.Body.String())
			}
			c.checkConformance(t, f.method, f.path, reqBytes, ctype, rec)
		})
	}
}

// TestErrorConformance asserts error envelopes also conform to the spec.
func TestErrorConformance(t *testing.T) {
	base := newTestEnv(t)
	c := &conformantEnv{testEnv: base, doc: loadSpec(t)}

	owner := c.newUser()
	c.createSite("err-conform-site", owner)

	t.Run("conflict envelope on stale write", func(t *testing.T) {
		body := openapi.WriteFileRequest{Path: "index.html", Content: "x", BaseSha: "0000000000000000000000000000000000000000"}
		rec := c.request(http.MethodPut, "/api/sites/err-conform-site/branches/draft/file").as(owner).json(body).do()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		c.checkConformance(t, http.MethodPut, "/api/sites/err-conform-site/branches/draft/file", mustJSON(t, body), "application/json", rec)
	})

	t.Run("not found envelope", func(t *testing.T) {
		rec := c.request(http.MethodGet, "/api/sites/missing-xyz").as(owner).do()
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		c.checkConformance(t, http.MethodGet, "/api/sites/missing-xyz", nil, "", rec)
	})

	t.Run("validation envelope on bad create", func(t *testing.T) {
		body := openapi.CreateSiteRequest{Handle: "admin"} // reserved
		rec := c.request(http.MethodPost, "/api/sites").as(owner).json(body).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		c.checkConformance(t, http.MethodPost, "/api/sites", mustJSON(t, body), "application/json", rec)
	})
}
