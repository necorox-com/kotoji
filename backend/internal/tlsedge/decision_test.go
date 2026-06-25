package tlsedge

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

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

// --- D2: bounded + cached existence lookup -------------------------------------

// countingExists wraps a SiteExistsFunc result with a call counter so a test can
// assert the cache collapsed repeated probes to a single DB hit.
func countingExists(result bool, calls *int32) SiteExistsFunc {
	return func(_ context.Context, _ string) (bool, error) {
		atomic.AddInt32(calls, 1)
		return result, nil
	}
}

func TestDecider_NegativeResultCachedAcrossProbes(t *testing.T) {
	// D2: an attacker spraying the SAME unknown host repeatedly must NOT hammer the DB
	// once per handshake — the negative result is cached for the TTL window.
	var calls int32
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", countingExists(false, &calls))

	host := "ghost-site.hosting.example.com"
	for i := 0; i < 10; i++ {
		if err := d.Allow(context.Background(), host); err == nil {
			t.Fatalf("probe %d: unknown host must be refused", i)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 DB lookup for 10 identical unknown-host probes (negative cache), got %d", got)
	}
}

func TestDecider_PositiveResultCachedAcrossProbes(t *testing.T) {
	// D2: a real, busy site is also memoized — repeated handshakes for it hit the DB
	// once per TTL window, not once each.
	var calls int32
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", countingExists(true, &calls))

	host := "real-site.hosting.example.com"
	for i := 0; i < 5; i++ {
		if err := d.Allow(context.Background(), host); err != nil {
			t.Fatalf("probe %d: existing site must be allowed, got %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 DB lookup for 5 identical existing-site probes (positive cache), got %d", got)
	}
}

func TestDecider_NegativeCacheExpires(t *testing.T) {
	// D2: the negative cache must EXPIRE so a freshly-created site becomes issuable.
	// Drive the injected clock past existsNegCacheTTL between probes.
	var calls int32
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", countingExists(false, &calls))

	// White-box clock injection (same package): start at a fixed instant.
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	cur := base
	d.now = func() time.Time { return cur }

	host := "new-site.hosting.example.com"
	if err := d.Allow(context.Background(), host); err == nil {
		t.Fatal("first probe must refuse")
	}
	// Within the TTL: served from cache, no new DB hit.
	cur = base.Add(existsNegCacheTTL - time.Second)
	if err := d.Allow(context.Background(), host); err == nil {
		t.Fatal("within-TTL probe must still refuse")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("within-TTL probe should not re-hit DB, calls=%d", got)
	}
	// Past the TTL: the entry expired, so the DB is consulted again.
	cur = base.Add(existsNegCacheTTL + time.Second)
	if err := d.Allow(context.Background(), host); err == nil {
		t.Fatal("post-TTL probe must refuse (still no site)")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("post-TTL probe must re-hit DB, calls=%d (want 2)", got)
	}
}

func TestDecider_LookupBoundedByDeadline(t *testing.T) {
	// D2: the existence lookup runs under its OWN deadline, independent of the
	// (potentially deadline-less) handshake ctx, so a slow DB cannot stretch the
	// handshake. We assert the ctx the lookup receives HAS a deadline.
	var hadDeadline bool
	exists := func(ctx context.Context, _ string) (bool, error) {
		_, hadDeadline = ctx.Deadline()
		return true, nil
	}
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", exists)
	// Pass context.Background() (no deadline) as the handshake ctx; the lookup ctx must
	// still carry its own bound.
	if err := d.Allow(context.Background(), "real-site.hosting.example.com"); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if !hadDeadline {
		t.Fatal("siteExists must be invoked with a bounded (deadline-carrying) context")
	}
}

func TestDecider_LookupErrorNotCached(t *testing.T) {
	// D2 fail-closed + correctness: a transient lookup error must REFUSE and must NOT
	// be cached, so a recovered DB immediately resumes issuing for a real site.
	var calls int32
	dbErr := errors.New("db blip")
	exists := func(_ context.Context, _ string) (bool, error) {
		// First call errors; subsequent calls succeed (DB recovered).
		if atomic.AddInt32(&calls, 1) == 1 {
			return false, dbErr
		}
		return true, nil
	}
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", exists)

	host := "real-site.hosting.example.com"
	if err := d.Allow(context.Background(), host); err == nil {
		t.Fatal("lookup error must refuse (fail-closed)")
	}
	// The error result was NOT cached, so the recovered DB is consulted again -> allow.
	if err := d.Allow(context.Background(), host); err != nil {
		t.Fatalf("after DB recovery the site must be allowed (error not cached), got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 DB lookups (error not cached), got %d", got)
	}
}

func TestDecider_HandshakeCancelSkipsLookup(t *testing.T) {
	// D2: if the handshake ctx is already cancelled (client hung up) we must not even
	// touch the DB — return the cancellation, no lookup.
	var calls int32
	d := newTestDecider(t, "hosting.example.com", "hosting.example.com", countingExists(true, &calls))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Allow(ctx, "real-site.hosting.example.com"); err == nil {
		t.Fatal("cancelled handshake must not be allowed")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("cancelled handshake must skip the DB lookup, calls=%d", got)
	}
}

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
