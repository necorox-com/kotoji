package mcpserver

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryLimiter_BurstThenDeny(t *testing.T) {
	lim := NewMemoryLimiter()
	// Freeze the clock so the bucket never refills during the test.
	now := time.Now()
	lim.clock = func() time.Time { return now }
	tok := uuid.New()

	// publish class: burst 3 → first 3 allowed, 4th denied with a retry hint.
	for i := 0; i < publishBurst; i++ {
		ok, _ := lim.Allow(tok, classPublish)
		require.True(t, ok, "burst slot %d should be allowed", i)
	}
	ok, retry := lim.Allow(tok, classPublish)
	assert.False(t, ok, "over-burst call denied")
	assert.Greater(t, retry, time.Duration(0), "denied call returns a positive retry hint")
}

func TestMemoryLimiter_RefillsOverTime(t *testing.T) {
	lim := NewMemoryLimiter()
	now := time.Now()
	lim.clock = func() time.Time { return now }
	tok := uuid.New()

	// Exhaust the write burst.
	for i := 0; i < writeBurst; i++ {
		ok, _ := lim.Allow(tok, classWrite)
		require.True(t, ok)
	}
	ok, _ := lim.Allow(tok, classWrite)
	require.False(t, ok)

	// Advance well past one token's worth (write = 30/min → 1 every 2s).
	now = now.Add(3 * time.Second)
	ok, _ = lim.Allow(tok, classWrite)
	assert.True(t, ok, "bucket refills as time passes")
}

func TestMemoryLimiter_ClassesAreIndependent(t *testing.T) {
	lim := NewMemoryLimiter()
	now := time.Now()
	lim.clock = func() time.Time { return now }
	tok := uuid.New()

	// Exhaust publish (burst 3); reads must remain available (separate bucket).
	for i := 0; i < publishBurst; i++ {
		ok, _ := lim.Allow(tok, classPublish)
		require.True(t, ok)
	}
	denied, _ := lim.Allow(tok, classPublish)
	require.False(t, denied)

	ok, _ := lim.Allow(tok, classRead)
	assert.True(t, ok, "read bucket is independent of the publish bucket")
}

func TestMemoryLimiter_TokensAreIndependent(t *testing.T) {
	lim := NewMemoryLimiter()
	now := time.Now()
	lim.clock = func() time.Time { return now }
	a, b := uuid.New(), uuid.New()

	for i := 0; i < createBurst; i++ {
		ok, _ := lim.Allow(a, classCreate)
		require.True(t, ok)
	}
	denied, _ := lim.Allow(a, classCreate)
	require.False(t, denied, "token A exhausted")

	ok, _ := lim.Allow(b, classCreate)
	assert.True(t, ok, "token B has its own bucket")
}

func TestLimits_WithDefaults(t *testing.T) {
	l := Limits{}.withDefaults()
	assert.Equal(t, defaultMaxFileBytes, l.MaxFileBytes)
	assert.Equal(t, int64(defaultMaxReadBytes), l.MaxReadBytes)
	assert.Equal(t, defaultMaxDiffBytes, l.MaxDiffBytes)
	assert.Equal(t, defaultMaxListItems, l.MaxListItems)
	assert.Equal(t, defaultMaxLogLimit, l.MaxLogLimit)
	require.NotNil(t, l.Limiter, "withDefaults always provides a limiter")
}
