package tlsedge

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

// newTestDecider wires a Decider over a real resolve.DefaultResolver (the SAME
// classifier the data plane uses) so the gate's host classification is exercised
// end to end, with the control host + site-existence supplied by closures.
func newTestDecider(t *testing.T, baseDomain, controlHost string, exists SiteExistsFunc) *Decider {
	t.Helper()
	res := resolve.NewResolver(resolve.Config{
		BaseDomain:         baseDomain,
		ControlLabel:       "", // bare base domain is the control host
		EnablePathFallback: true,
		// The gate builds a header-less synthetic request, so trusting forwarded
		// host is irrelevant here; mirror the data plane's default anyway.
		TrustForwardedHost: true,
	})
	d, err := NewDecider(func() string { return controlHost }, res, exists)
	if err != nil {
		t.Fatalf("NewDecider: %v", err)
	}
	return d
}

// existsForHandles returns a SiteExistsFunc that reports true for the given set.
func existsForHandles(handles ...string) SiteExistsFunc {
	set := make(map[string]struct{}, len(handles))
	for _, h := range handles {
		set[h] = struct{}{}
	}
	return func(_ context.Context, handle string) (bool, error) {
		_, ok := set[handle]
		return ok, nil
	}
}

func TestDecider_AllowsControlHost(t *testing.T) {
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", existsForHandles())
	// The effective control host is always servable by this instance.
	if err := d.Allow(context.Background(), "hosting.example.com"); err != nil {
		t.Fatalf("control host should be allowed, got: %v", err)
	}
}

func TestDecider_AllowsControlHostCaseAndPortInsensitive(t *testing.T) {
	// controlHost getter returns a host:port and mixed case; the gate normalizes.
	d := newTestDecider(t, "hosting.example.com", "Hosting.Example.com:443", existsForHandles())
	if err := d.Allow(context.Background(), "HOSTING.EXAMPLE.COM"); err != nil {
		t.Fatalf("control host (normalized) should be allowed, got: %v", err)
	}
}

func TestDecider_AllowsExistingSite(t *testing.T) {
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", existsForHandles("expense-calc"))
	// expense-calc.hosting.example.com classifies to handle "expense-calc", which exists.
	if err := d.Allow(context.Background(), "expense-calc.hosting.example.com"); err != nil {
		t.Fatalf("existing site host should be allowed, got: %v", err)
	}
}

func TestDecider_AllowsExistingPreview(t *testing.T) {
	// A preview host {handle}--{branch}.base classifies to handle "blog" (preview);
	// the existence check keys on the bare handle, which exists.
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", existsForHandles("blog"))
	if err := d.Allow(context.Background(), "blog--feature-x.hosting.example.com"); err != nil {
		t.Fatalf("existing preview host should be allowed, got: %v", err)
	}
}

func TestDecider_RefusesUnknownSite(t *testing.T) {
	// Host classifies to a project handle, but no such site exists => refuse.
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", existsForHandles("real-site"))
	err := d.Allow(context.Background(), "does-not-exist.hosting.example.com")
	if err == nil {
		t.Fatal("unknown site host must be refused")
	}
	if !errors.Is(err, errRefused) {
		t.Fatalf("expected errRefused, got: %v", err)
	}
}

func TestDecider_RefusesForeignHost(t *testing.T) {
	// A host not under the base domain is foreign => the resolver rejects it, and we
	// must NOT attempt issuance (no ACME rate-limit burn for arbitrary names).
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", existsForHandles("anything"))
	for _, host := range []string{"evil.com", "attacker.example.net", "hosting.example.com.evil.com"} {
		if err := d.Allow(context.Background(), host); err == nil {
			t.Fatalf("foreign host %q must be refused", host)
		}
	}
}

func TestDecider_RefusesReservedLabel(t *testing.T) {
	// Reserved labels (api, admin, www, ...) are not project hosts; the resolver
	// classifies them to "not a project host" and the gate refuses.
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", existsForHandles())
	for _, host := range []string{"api.hosting.example.com", "admin.hosting.example.com", "www.hosting.example.com"} {
		if err := d.Allow(context.Background(), host); err == nil {
			t.Fatalf("reserved-label host %q must be refused", host)
		}
	}
}

func TestDecider_RefusesEmptyHost(t *testing.T) {
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", existsForHandles())
	if err := d.Allow(context.Background(), ""); err == nil {
		t.Fatal("empty host must be refused")
	}
	if err := d.Allow(context.Background(), "   "); err == nil {
		t.Fatal("whitespace host must be refused")
	}
}

func TestDecider_RefusesOnStoreError(t *testing.T) {
	// FAIL-CLOSED: a backing-store error must REFUSE, never allow, so a transient DB
	// blip cannot become an unbounded issuance vector.
	dbErr := errors.New("db unavailable")
	exists := func(_ context.Context, _ string) (bool, error) { return false, dbErr }
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", exists)
	err := d.Allow(context.Background(), "some-site.hosting.example.com")
	if err == nil {
		t.Fatal("store error must cause refusal (fail-closed)")
	}
	if !errors.Is(err, errRefused) {
		t.Fatalf("expected errRefused wrapping the store error, got: %v", err)
	}
}

func TestDecider_AllowsControlHostViaResolverWhenGetterEmpty(t *testing.T) {
	// When the control-host getter is empty (fresh, unconfigured instance) but the
	// base domain itself is the control host, the resolver still classifies the bare
	// base host as control => allowed.
	d := newTestDecider(t, "hosting.example.com", "", existsForHandles())
	if err := d.Allow(context.Background(), "hosting.example.com"); err != nil {
		t.Fatalf("bare base host should be allowed via resolver control classification, got: %v", err)
	}
}

func TestNewDecider_RejectsNilSeams(t *testing.T) {
	res := resolve.NewResolver(resolve.Config{BaseDomain: "x.example.com"})
	ok := func(context.Context, string) (bool, error) { return true, nil }
	ch := func() string { return "x.example.com" }

	if _, err := NewDecider(nil, res, ok); err == nil {
		t.Fatal("nil ControlHostFunc must error")
	}
	if _, err := NewDecider(ch, nil, ok); err == nil {
		t.Fatal("nil Resolver must error")
	}
	if _, err := NewDecider(ch, res, nil); err == nil {
		t.Fatal("nil SiteExistsFunc must error")
	}
}

// stubResolver lets a test drive the gate's behavior when the resolver returns an
// arbitrary Target (e.g. a control target) without going through host grammar.
type stubResolver struct {
	target resolve.Target
	err    error
}

func (s stubResolver) Resolve(*http.Request) (resolve.Target, error) { return s.target, s.err }

func TestDecider_AllowsWhenResolverReportsControlTarget(t *testing.T) {
	// If the resolver classifies the host as IsControl (and the getter did not
	// match), the gate still allows it — it is our control plane.
	d, err := NewDecider(
		func() string { return "" },
		stubResolver{target: resolve.Target{IsControl: true}},
		existsForHandles(),
	)
	if err != nil {
		t.Fatalf("NewDecider: %v", err)
	}
	if err := d.Allow(context.Background(), "control.example.com"); err != nil {
		t.Fatalf("control target should be allowed, got: %v", err)
	}
}
