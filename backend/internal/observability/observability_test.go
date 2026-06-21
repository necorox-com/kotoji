package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	var captured string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)

	assert.NotEmpty(t, captured, "id stored in context")
	assert.Equal(t, captured, rr.Header().Get(requestIDHeader), "id echoed on response")
}

func TestRequestID_HonorsValidInbound(t *testing.T) {
	const in = "abc-123_DEF.456"
	var captured string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestIDHeader, in)
	h.ServeHTTP(rr, req)

	assert.Equal(t, in, captured)
	assert.Equal(t, in, rr.Header().Get(requestIDHeader))
}

func TestRequestID_RejectsUnsafeInbound(t *testing.T) {
	// An inbound id with a control/space char must be replaced, not propagated.
	var captured string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestIDHeader, "bad id with spaces")
	h.ServeHTTP(rr, req)

	assert.NotEqual(t, "bad id with spaces", captured)
	assert.NotEmpty(t, captured)
}

func TestRequestIDFrom_EmptyContext(t *testing.T) {
	assert.Equal(t, "", RequestIDFrom(context.Background()))
}

func TestHealth_OK(t *testing.T) {
	rr := httptest.NewRecorder()
	Health(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestReady_NilCheckerAlwaysReady(t *testing.T) {
	rr := httptest.NewRecorder()
	Ready(nil)(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestReady_FailingChecker503(t *testing.T) {
	checker := ReadinessFunc(func(context.Context) error { return errors.New("db down") })
	rr := httptest.NewRecorder()
	Ready(checker)(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestRecoverer_TurnsPanicInto500(t *testing.T) {
	logger := NewLogger("error", "json")
	h := Recoverer(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rr := httptest.NewRecorder()
	require.NotPanics(t, func() {
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	})
	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestRequestLogger_PassesThrough(t *testing.T) {
	logger := NewLogger("info", "json")
	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusTeapot, rr.Code)
}
