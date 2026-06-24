package domaincfg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/db"
)

// fakeStore is an in-memory domaincfg.Store recording how many times the DB was
// read so the env-set fast path (zero reads) and the cache (one read) are pinned.
type fakeStore struct {
	cfg   db.DomainConfig
	reads int32
	err   error
}

func (f *fakeStore) GetDomainConfig(_ context.Context) (db.DomainConfig, error) {
	atomic.AddInt32(&f.reads, 1)
	if f.err != nil {
		return db.DomainConfig{}, f.err
	}
	return f.cfg, nil
}

func (f *fakeStore) readCount() int { return int(atomic.LoadInt32(&f.reads)) }

// reqFor builds a request with the given Host (and optional X-Forwarded-* headers)
// so the derive path is exercised deterministically.
func reqFor(host, xfHost, xfProto string, tls bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	r.Host = host
	if xfHost != "" {
		r.Header.Set("X-Forwarded-Host", xfHost)
	}
	if xfProto != "" {
		r.Header.Set("X-Forwarded-Proto", xfProto)
	}
	if tls {
		// httptest.NewRequest does not set TLS; the derive path keys off XFP here.
	}
	return r
}

// TestPrecedence covers the WordPress-style env > DB > derive precedence for both
// fields, including the LOCKED guarantee that an env-set value makes the DB value
// ignored AND skips the DB read entirely.
func TestPrecedence(t *testing.T) {
	ctx := context.Background()

	t.Run("both env set: env wins, DB never read, fields locked", func(t *testing.T) {
		store := &fakeStore{cfg: db.DomainConfig{
			BaseDomain: "db.example.com", BaseDomainSet: true,
			ControlBaseURL: "https://db.example.com", ControlBaseURLSet: true,
		}}
		p := New(Config{
			Store:         store,
			EnvBaseDomain: "env.example.com", EnvBaseDomainSet: true,
			EnvControlBaseURL: "https://env.example.com", EnvControlSet: true,
		})
		res := p.Resolve(ctx, reqFor("ignored.host", "", "", false))

		if res.BaseDomain.Value != "env.example.com" || res.BaseDomain.Source != SourceEnv || !res.BaseDomain.Locked {
			t.Fatalf("baseDomain = %+v, want env/locked", res.BaseDomain)
		}
		if res.ControlBaseURL.Value != "https://env.example.com" || res.ControlBaseURL.Source != SourceEnv || !res.ControlBaseURL.Locked {
			t.Fatalf("controlBaseURL = %+v, want env/locked", res.ControlBaseURL)
		}
		if res.ControlHost != "env.example.com" {
			t.Fatalf("controlHost = %q, want env.example.com", res.ControlHost)
		}
		// The CRITICAL invariant for the live deployment: NO DB read on the env path.
		if store.readCount() != 0 {
			t.Fatalf("DB was read %d times on the env-set fast path; want 0", store.readCount())
		}
	})

	t.Run("env empty, DB set: DB wins (editable, not locked)", func(t *testing.T) {
		store := &fakeStore{cfg: db.DomainConfig{
			BaseDomain: "db.example.com", BaseDomainSet: true,
			ControlBaseURL: "https://db.example.com", ControlBaseURLSet: true,
		}}
		p := New(Config{Store: store}) // neither env set
		res := p.Resolve(ctx, reqFor("req.host", "", "", false))

		if res.BaseDomain.Value != "db.example.com" || res.BaseDomain.Source != SourceDB || res.BaseDomain.Locked {
			t.Fatalf("baseDomain = %+v, want db/unlocked", res.BaseDomain)
		}
		if res.ControlBaseURL.Value != "https://db.example.com" || res.ControlBaseURL.Source != SourceDB {
			t.Fatalf("controlBaseURL = %+v, want db", res.ControlBaseURL)
		}
	})

	t.Run("env empty, DB empty: derive from request", func(t *testing.T) {
		store := &fakeStore{} // empty DB
		p := New(Config{Store: store})
		// Simulate the documented edge topology: TLS terminated at the proxy, the
		// original host/scheme carried in X-Forwarded-*.
		res := p.Resolve(ctx, reqFor("internal:8081", "kotoji.example.com", "https", false))

		if res.BaseDomain.Value != "kotoji.example.com" || res.BaseDomain.Source != SourceDerived {
			t.Fatalf("baseDomain = %+v, want derived kotoji.example.com", res.BaseDomain)
		}
		if res.ControlBaseURL.Value != "https://kotoji.example.com" || res.ControlBaseURL.Source != SourceDerived {
			t.Fatalf("controlBaseURL = %+v, want derived https://kotoji.example.com", res.ControlBaseURL)
		}
		if res.ControlHost != "kotoji.example.com" {
			t.Fatalf("controlHost = %q", res.ControlHost)
		}
	})

	t.Run("mixed: base env-locked, control falls through to DB", func(t *testing.T) {
		store := &fakeStore{cfg: db.DomainConfig{
			ControlBaseURL: "https://db.example.com", ControlBaseURLSet: true,
		}}
		p := New(Config{
			Store:         store,
			EnvBaseDomain: "env.example.com", EnvBaseDomainSet: true,
			// control env unset
		})
		res := p.Resolve(ctx, reqFor("req.host", "", "", false))
		if res.BaseDomain.Source != SourceEnv || !res.BaseDomain.Locked {
			t.Fatalf("baseDomain should be env-locked: %+v", res.BaseDomain)
		}
		if res.ControlBaseURL.Source != SourceDB || res.ControlBaseURL.Locked {
			t.Fatalf("controlBaseURL should be db/unlocked: %+v", res.ControlBaseURL)
		}
		// Because one field falls through, the DB IS read (exactly once, cached).
		if store.readCount() != 1 {
			t.Fatalf("DB reads = %d, want 1 (cached)", store.readCount())
		}
	})
}

// TestCacheAndInvalidate pins that the DB is read at most once until the cache is
// invalidated (the admin PUT seam), then re-read.
func TestCacheAndInvalidate(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{cfg: db.DomainConfig{BaseDomain: "one.example.com", BaseDomainSet: true}}
	p := New(Config{Store: store}) // env empty => DB consulted

	for i := 0; i < 5; i++ {
		_ = p.Resolve(ctx, reqFor("h", "", "", false))
	}
	if store.readCount() != 1 {
		t.Fatalf("DB reads = %d after 5 resolves, want 1 (cached)", store.readCount())
	}

	// Admin changes the DB value and invalidates the cache.
	store.cfg.BaseDomain = "two.example.com"
	p.InvalidateCache()

	res := p.Resolve(ctx, reqFor("h", "", "", false))
	if res.BaseDomain.Value != "two.example.com" {
		t.Fatalf("post-invalidate baseDomain = %q, want two.example.com", res.BaseDomain.Value)
	}
	if store.readCount() != 2 {
		t.Fatalf("DB reads = %d after invalidate, want 2", store.readCount())
	}
}

// TestReadErrorFallsBackToDerive pins that a DB read failure degrades to the
// derived value rather than crashing (fail safe on a transient blip).
func TestReadErrorFallsBackToDerive(t *testing.T) {
	store := &fakeStore{err: errors.New("db down")}
	p := New(Config{Store: store})
	res := p.Resolve(context.Background(), reqFor("internal:8081", "fresh.example.com", "https", false))
	if res.BaseDomain.Source != SourceDerived || res.BaseDomain.Value != "fresh.example.com" {
		t.Fatalf("on DB error want derived; got %+v", res.BaseDomain)
	}
}

// TestLockedHelpers pins the EnvLocked helpers used by the admin PUT to reject a
// write to an env-pinned field.
func TestLockedHelpers(t *testing.T) {
	cases := []struct {
		name             string
		baseSet, ctlSet  bool
		baseVal, ctlVal  string
		wantBase, wantCt bool
	}{
		{"both set", true, true, "b", "https://c", true, true},
		{"set but empty => not locked", true, true, "", "", false, false},
		{"none set", false, false, "", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(Config{
				EnvBaseDomain: c.baseVal, EnvBaseDomainSet: c.baseSet,
				EnvControlBaseURL: c.ctlVal, EnvControlSet: c.ctlSet,
			})
			if p.EnvBaseDomainLocked() != c.wantBase {
				t.Fatalf("EnvBaseDomainLocked = %v, want %v", p.EnvBaseDomainLocked(), c.wantBase)
			}
			if p.EnvControlBaseURLLocked() != c.wantCt {
				t.Fatalf("EnvControlBaseURLLocked = %v, want %v", p.EnvControlBaseURLLocked(), c.wantCt)
			}
		})
	}
}

// TestNilRequestDerive pins that Resolve tolerates a nil request (a startup probe
// / admin read without an incoming Host): derived values are empty.
func TestNilRequestDerive(t *testing.T) {
	p := New(Config{Store: &fakeStore{}})
	res := p.Resolve(context.Background(), nil)
	if res.BaseDomain.Value != "" || res.ControlBaseURL.Value != "" {
		t.Fatalf("nil request should derive empty, got %+v / %+v", res.BaseDomain, res.ControlBaseURL)
	}
	if res.BaseDomain.Source != SourceDerived {
		t.Fatalf("nil request source = %q, want derived", res.BaseDomain.Source)
	}
}
