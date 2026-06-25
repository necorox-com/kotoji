package tlsedge

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

// newTestEngine builds an Engine with the minimal valid Config for header/timeout
// assertions. It injects a self-signed issuer so New does not wire the ACME path
// (irrelevant here) and a never-allow Decider (the gate is exercised in decision_test).
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	res := resolve.NewResolver(resolve.Config{BaseDomain: "hosting.example.com"})
	dec, err := NewDecider(func() string { return "hosting.example.com" }, res, existsForHandles())
	if err != nil {
		t.Fatalf("NewDecider: %v", err)
	}
	// Leaving Issuers nil makes New build the default ACME issuer; that constructor is
	// pure (no network, no socket bind) so it is safe in a unit test — we never call Run.
	eng, err := New(Config{
		Handler:    http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		Decider:    dec,
		StorageDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

// HS1: the :443 (auto-TLS) handler must emit Strict-Transport-Security, since in
// auto mode kotoji terminates TLS itself and thus owns HSTS.
func TestEngine_TLSHandlerEmitsHSTS(t *testing.T) {
	eng := newTestEngine(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://expense.hosting.example.com/", nil)
	eng.tlsSrv.Handler.ServeHTTP(rec, req)

	got := rec.Header().Get("Strict-Transport-Security")
	if got != hstsValue {
		t.Fatalf("Strict-Transport-Security = %q, want %q", got, hstsValue)
	}
}

// HS1: the :80 challenge/redirect handler must NOT emit HSTS over plain HTTP (HSTS
// on a plain-HTTP response is meaningless and browsers ignore it; correctness check).
func TestEngine_HTTPRedirectHandlerNoHSTS(t *testing.T) {
	eng := newTestEngine(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://expense.hosting.example.com/", nil)
	eng.httpSrv.Handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("plain-HTTP :80 handler must not set HSTS, got %q", got)
	}
}

// HS1 unit: hstsHandler injects the header before delegating, so it survives even
// when the wrapped handler writes its own headers.
func TestHSTSHandler_SetsHeaderBeforeNext(t *testing.T) {
	var sawDuringNext string
	wrapped := hstsHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sawDuringNext = w.Header().Get("Strict-Transport-Security")
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if sawDuringNext != hstsValue {
		t.Fatalf("HSTS must be set BEFORE next runs; next saw %q, want %q", sawDuringNext, hstsValue)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != hstsValue {
		t.Fatalf("final HSTS = %q, want %q", got, hstsValue)
	}
}

// D1: both engine servers must bound every connection phase, not just header read.
func TestEngine_ServersHaveAllTimeouts(t *testing.T) {
	eng := newTestEngine(t)

	for name, srv := range map[string]*http.Server{":443": eng.tlsSrv, ":80": eng.httpSrv} {
		if srv.ReadHeaderTimeout != readHeaderTimeout {
			t.Errorf("%s ReadHeaderTimeout = %v, want %v", name, srv.ReadHeaderTimeout, readHeaderTimeout)
		}
		if srv.ReadTimeout != readTimeout {
			t.Errorf("%s ReadTimeout = %v, want %v", name, srv.ReadTimeout, readTimeout)
		}
		if srv.WriteTimeout != writeTimeout {
			t.Errorf("%s WriteTimeout = %v, want %v", name, srv.WriteTimeout, writeTimeout)
		}
		if srv.IdleTimeout != idleTimeout {
			t.Errorf("%s IdleTimeout = %v, want %v", name, srv.IdleTimeout, idleTimeout)
		}
		// No phase may be left unbounded (zero == no limit == the D1 vulnerability).
		if srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 || srv.ReadHeaderTimeout <= 0 {
			t.Errorf("%s has an unbounded connection phase", name)
		}
	}
}
