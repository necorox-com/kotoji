package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/config"
)

// startOIDCCallback drives a full Start -> capture-state-cookie, then issues a
// callback with the given (state, cookie). It returns the recorder so the caller
// asserts the outcome. Reusing the existing OIDC fakeProvider so no real Google is
// needed.
func oidcLoginStart(t *testing.T, a *Auth, fp *fakeProvider) (state string, stateCookie *http.Cookie) {
	t.Helper()
	startRec := httptest.NewRecorder()
	router(a).ServeHTTP(startRec, httptest.NewRequest(http.MethodGet, "/auth/login?next=/dashboard", nil))
	require.Equal(t, http.StatusFound, startRec.Code)
	stateCookie = findCookie(startRec.Result().Cookies(), a.loginStateCookieName())
	require.NotNil(t, stateCookie)
	return fp.gotState, stateCookie
}

func oidcCallback(t *testing.T, a *Auth, state string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=good&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router(a).ServeHTTP(rec, req)
	return rec
}

func newOIDCTestAuth(t *testing.T) (*Auth, *fakeStore, *fakeProvider) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthModeOIDC
	fp := &fakeProvider{
		key: "oidc", interactive: true, startURL: "https://idp/auth",
		exchangeReply: Claims{Subject: "sub-123", Email: "alice@corp.com", Name: "Alice", EmailVerified: true},
	}
	store := newFakeStore()
	a := New(cfg, store, fp)
	a.upserter = &fakeUpserter{store: store}
	return a, store, fp
}

// (a) A normal OIDC callback succeeds exactly once: the single-use guard does not
// interfere with the first, legitimate callback.
func TestOIDCCallback_SingleUse_FirstCallbackSucceeds(t *testing.T) {
	a, store, fp := newOIDCTestAuth(t)
	state, cookie := oidcLoginStart(t, a, fp)

	rec := oidcCallback(t, a, state, cookie)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/dashboard", rec.Header().Get("Location"))
	require.Equal(t, 1, store.sessionCount())
}

// (b) A REPLAYED callback with the SAME state and an otherwise-valid login-state
// cookie is REJECTED. This is the core fix: even though the cookie signature, the
// state match, and the cookie TTL are all still valid, the second consume of the
// same state fails closed (same 403 / "login state mismatch" shape as a real
// mismatch, no info leak).
func TestOIDCCallback_SingleUse_ReplayRejected(t *testing.T) {
	a, store, fp := newOIDCTestAuth(t)
	state, cookie := oidcLoginStart(t, a, fp)

	// First callback succeeds and establishes a session.
	first := oidcCallback(t, a, state, cookie)
	require.Equal(t, http.StatusFound, first.Code)
	require.Equal(t, 1, store.sessionCount())

	// Replay the SAME state + SAME (still-valid) login-state cookie. An attacker who
	// captured the cookie would do exactly this within the TTL.
	replay := oidcCallback(t, a, state, cookie)
	require.Equal(t, http.StatusForbidden, replay.Code)
	// No NEW session was created by the replay (still exactly one from the first use).
	require.Equal(t, 1, store.sessionCount())
}

// (c) Two DIFFERENT concurrent logins (distinct 256-bit states) BOTH succeed: the
// guard is keyed per-state, so independent logins never collide.
func TestOIDCCallback_SingleUse_DistinctStatesBothSucceed(t *testing.T) {
	a, store, fp := newOIDCTestAuth(t)

	state1, cookie1 := oidcLoginStart(t, a, fp)
	state2, cookie2 := oidcLoginStart(t, a, fp)
	require.NotEqual(t, state1, state2, "each login must mint a distinct state")

	rec1 := oidcCallback(t, a, state1, cookie1)
	rec2 := oidcCallback(t, a, state2, cookie2)
	require.Equal(t, http.StatusFound, rec1.Code)
	require.Equal(t, http.StatusFound, rec2.Code)
	require.Equal(t, 2, store.sessionCount())
}

// (d) The in-memory store expires entries after the TTL and cleans them up. We
// inject a fake clock so the TTL/sweep windows are deterministic.
func TestMemSingleUseStore_ExpiresAndCleansUp(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	s := newMemSingleUseStore(clock)

	const ttl = 10 * time.Minute

	// First use of a key -> firstUse=true; an immediate replay -> false.
	firstUse, err := s.Consume(context.Background(), "k", ttl)
	require.NoError(t, err)
	require.True(t, firstUse)

	replay, err := s.Consume(context.Background(), "k", ttl)
	require.NoError(t, err)
	require.False(t, replay, "replay within the TTL must be rejected")
	require.Equal(t, 1, s.size())

	// Advance past the TTL: the entry is now expired. Consuming the same key again
	// treats it as first use (and re-arms it).
	now = now.Add(ttl + time.Second)
	firstUseAgain, err := s.Consume(context.Background(), "k", ttl)
	require.NoError(t, err)
	require.True(t, firstUseAgain, "an expired entry is forgotten -> first use again")

	// Cleanup: advance past the sweep interval and touch a DIFFERENT key. The periodic
	// sweep should drop the now-expired re-armed "k" entry, leaving only the new key.
	now = now.Add(ttl + singleUseSweepInterval + time.Second)
	_, err = s.Consume(context.Background(), "other", ttl)
	require.NoError(t, err)
	require.Equal(t, 1, s.size(), "expired entries must be swept, leaving only the live one")
}

// TestMemSingleUseStore_Bounded asserts the hard cap cannot be exceeded: once the
// map is full of LIVE entries, a further distinct key is rejected (firstUse=false)
// rather than growing the map unboundedly.
func TestMemSingleUseStore_Bounded(t *testing.T) {
	now := time.Unix(0, 0)
	s := newMemSingleUseStore(func() time.Time { return now })
	s.maxEntries = 3 // shrink the cap for a fast test

	const ttl = 10 * time.Minute
	for i, k := range []string{"a", "b", "c"} {
		firstUse, err := s.Consume(context.Background(), k, ttl)
		require.NoError(t, err)
		require.True(t, firstUse, "entry %d should be first use", i)
	}
	require.Equal(t, 3, s.size())

	// The map is full of LIVE entries; a new key cannot be recorded.
	firstUse, err := s.Consume(context.Background(), "d", ttl)
	require.NoError(t, err)
	require.False(t, firstUse, "a new key past the cap (all live) must fail closed")
	require.Equal(t, 3, s.size())
}

// TestOIDCCallback_SingleUse_ConcurrentDoubleSubmitSameState drives the FULL
// callback path concurrently with the SAME state + SAME login-state cookie (an
// attacker double-submitting a captured handshake). Exactly ONE callback may win
// (302 + a session); every other concurrent submit must be rejected (403, no extra
// session). This proves the Consume check-and-set is atomic under the mutex with NO
// TOCTOU window between the state compare and the single-use record. Run under -race
// to also assert no data race across the shared Auth/store/provider.
func TestOIDCCallback_SingleUse_ConcurrentDoubleSubmitSameState(t *testing.T) {
	a, store, fp := newOIDCTestAuth(t)
	state, cookie := oidcLoginStart(t, a, fp)

	const submits = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	rejects := 0
	wg.Add(submits)
	// A shared start barrier so all goroutines race the SAME state into the callback
	// at (as near as possible to) the same instant — maximizing the TOCTOU window we
	// are trying to close.
	start := make(chan struct{})
	for i := 0; i < submits; i++ {
		go func() {
			defer wg.Done()
			<-start
			// Each goroutine gets its OWN cookie copy + request; they all carry the
			// identical state value and the identical signed cookie payload.
			c := *cookie
			rec := oidcCallback(t, a, state, &c)
			mu.Lock()
			switch rec.Code {
			case http.StatusFound:
				successes++
			case http.StatusForbidden:
				rejects++
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, 1, successes, "exactly one concurrent double-submit of the same state may win")
	require.Equal(t, submits-1, rejects, "every other concurrent submit must be rejected (403)")
	require.Equal(t, 1, store.sessionCount(), "only the single winning callback creates a session")
}

// TestMemSingleUseStore_Concurrent exercises Consume under concurrency for ONE key:
// exactly one goroutine must observe firstUse=true (the race detector also guards
// the map). This backs the "atomically records key" contract.
func TestMemSingleUseStore_Concurrent(t *testing.T) {
	s := newMemSingleUseStore(nil)
	const goroutines = 64
	const ttl = time.Minute

	var wg sync.WaitGroup
	var mu sync.Mutex
	firstUses := 0
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ok, err := s.Consume(context.Background(), "race-key", ttl)
			require.NoError(t, err)
			if ok {
				mu.Lock()
				firstUses++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, firstUses, "exactly one concurrent Consume may win firstUse")
}
