//go:build conformance

package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"

	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

// TestVisibilityEnumConformance proves the renamed enum VALUE is wired into the
// frozen OpenAPI contract: a CreateSiteRequest with visibility "members"
// validates, while the retired "internal" is rejected by the spec (the request
// validator is what turns an invalid visibility into a 422).
func TestVisibilityEnumConformance(t *testing.T) {
	doc := loadSpec(t)

	check := func(t *testing.T, body []byte) error {
		t.Helper()
		route, pathParams := findRoute(t, doc, http.MethodPost, "/api/sites")
		u, _ := url.Parse("/api/sites")
		httpReq := &http.Request{Method: http.MethodPost, URL: u, Header: http.Header{}}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Body = io.NopCloser(bytes.NewReader(body))
		reqInput := &openapi3filter.RequestValidationInput{
			Request:    httpReq,
			PathParams: pathParams,
			Route:      route,
			Options: &openapi3filter.Options{
				AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			},
		}
		return openapi3filter.ValidateRequest(context.Background(), reqInput)
	}

	t.Run("members is accepted by the spec", func(t *testing.T) {
		vis := openapi.Members
		body := mustJSON(t, openapi.CreateSiteRequest{Handle: "smoke-members", Visibility: &vis})
		if err := check(t, body); err != nil {
			t.Fatalf("visibility 'members' must be spec-valid, got: %v", err)
		}
	})

	t.Run("internal is rejected by the spec", func(t *testing.T) {
		// The retired value is sent raw (the Go enum no longer has a constant for
		// it) — the spec's enum constraint must reject it.
		body := []byte(`{"handle":"smoke-internal","visibility":"internal"}`)
		if err := check(t, body); err == nil {
			t.Fatal("visibility 'internal' must be rejected by the spec enum, but validation passed")
		}
	})
}
