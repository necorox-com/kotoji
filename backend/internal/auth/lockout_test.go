package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/config"
)

// --- R2 unit tests for the in-memory lockout tracker ---

func TestLoginLockout_LocksAfterThreshold(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := newLoginLockout(clock)
	const key = "1.2.3.4"

	// The first (threshold-1) failures must NOT lock the key.
	for i := 0; i < lockoutThreshold-1; i++ {
		require.Zero(t, l.recordFailure(key), "failure %d should not lock yet", i+1)
		locked, _ := l.locked(key)
		require.False(t, locked, "key must not be locked before the threshold")
	}

	// The threshold-th failure arms the first lockout window.
	delay := l.recordFailure(key)
	require.Equal(t, lockoutBaseDelay, delay, "first lockout uses the base delay")
	locked, retry := l.locked(key)
	require.True(t, locked, "key must be locked at the threshold")
	require.Equal(t, lockoutBaseDelay, retry)

	// The window elapses -> unlocked again.
	now = now.Add(lockoutBaseDelay)
	locked, _ = l.locked(key)
	require.False(t, locked, "key must unlock after the window elapses")
}

func TestLoginLockout_ExponentialBackoffCapped(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLoginLockout(func() time.Time { return now })
	const key = "9.9.9.9"

	// Drive well past the threshold and confirm the backoff doubles then caps.
	var last time.Duration
	for i := 0; i < lockoutThreshold+40; i++ {
		last = l.recordFailure(key)
	}
	require.Equal(t, lockoutMaxDelay, last, "backoff must saturate at the max delay")
}

func TestLoginLockout_SuccessResets(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLoginLockout(func() time.Time { return now })
	const key = "5.5.5.5"

	for i := 0; i < lockoutThreshold; i++ {
		l.recordFailure(key)
	}
	locked, _ := l.locked(key)
	require.True(t, locked)

	// A correct credential clears the state entirely.
	l.recordSuccess(key)
	locked, _ = l.locked(key)
	require.False(t, locked, "success must reset the lockout")
	// And the counter is back to zero: the next single failure does not re-lock.
	require.Zero(t, l.recordFailure(key))
}

func TestLoginLockout_EmptyKeyNeverLocks(t *testing.T) {
	l := newLoginLockout(nil)
	// An empty source key is the no-signal case: never locked, never recorded.
	require.Zero(t, l.recordFailure(""))
	locked, _ := l.locked("")
	require.False(t, locked)
}

// --- R2 integration: the password POST gates on the lockout ---

// passwordOnlyAuth wires an Auth with ONLY the password provider over a fake store,
// using a fixed env password. It mirrors composition for AUTH_MODE="password".
func passwordOnlyAuth(t *testing.T, envPassword string) (*Auth, *fakeStore) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthModePassword
	cfg.AuthModes = []config.AuthMode{config.AuthModePassword}
	cfg.AdminEmail = "admin@kotoji.local"
	cfg.AdminPassword = envPassword

	store := newFakeStore()
	pw, err := NewPasswordProvider(cfg, store)
	require.NoError(t, err)
	a := NewWithProviders(cfg, store, pw)
	a.upserter = &fakeUpserter{store: store}
	return a, store
}

// TestLoginPassword_LocksOutAfterRepeatedFailures is the headline R2 behavior:
// after lockoutThreshold wrong-password POSTs from the same source, the next POST
// is rejected with 429 BEFORE the bcrypt compare (and a correct password is also
// blocked while locked).
func TestLoginPassword_LocksOutAfterRepeatedFailures(t *testing.T) {
	const good = "break-glass-pw-1234"
	a, _ := passwordOnlyAuth(t, good)

	// Inject a controllable clock so the lockout window is deterministic.
	now := time.Unix(1_000, 0)
	a.lockout.setClock(func() time.Time { return now })

	h := router(a)

	// threshold wrong attempts: each returns 401 (not yet locked on the response).
	for i := 0; i < lockoutThreshold; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, postForm("/auth/login", url.Values{"password": {"wrong"}}))
		require.Equal(t, http.StatusUnauthorized, rec.Code, "attempt %d should be 401", i+1)
	}

	// The NEXT attempt is locked out -> 429 with a Retry-After header, even with the
	// CORRECT password (the gate runs before the credential is checked).
	locked := httptest.NewRecorder()
	h.ServeHTTP(locked, postForm("/auth/login", url.Values{"password": {good}}))
	require.Equal(t, http.StatusTooManyRequests, locked.Code, "must be locked out after the threshold")
	require.NotEmpty(t, locked.Header().Get("Retry-After"), "429 must carry Retry-After")

	// After the lockout window elapses, the correct password succeeds (302 redirect).
	now = now.Add(lockoutMaxDelay)
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, postForm("/auth/login", url.Values{"password": {good}}))
	require.Equal(t, http.StatusFound, ok.Code, "correct password should succeed once unlocked")
}

// TestLoginPassword_SuccessKeepsCounterClear: a correct password resets the
// counter so a legitimate admin who mistyped a few times is not progressively
// penalized after a successful login.
func TestLoginPassword_SuccessKeepsCounterClear(t *testing.T) {
	const good = "break-glass-pw-1234"
	a, _ := passwordOnlyAuth(t, good)
	now := time.Unix(2_000, 0)
	a.lockout.setClock(func() time.Time { return now })
	h := router(a)

	// A couple of failures (below threshold), then a success.
	for i := 0; i < lockoutThreshold-1; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, postForm("/auth/login", url.Values{"password": {"wrong"}}))
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	good1 := httptest.NewRecorder()
	h.ServeHTTP(good1, postForm("/auth/login", url.Values{"password": {good}}))
	require.Equal(t, http.StatusFound, good1.Code)

	// The counter is now clear: a fresh single failure must still be a 401, NOT a
	// 429 (it would be a 429 only if the pre-success failures still counted).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postForm("/auth/login", url.Values{"password": {"wrong"}}))
	require.Equal(t, http.StatusUnauthorized, rec.Code, "counter must have reset on success")
}
