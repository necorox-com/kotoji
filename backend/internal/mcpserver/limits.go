package mcpserver

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// toolClass buckets tools for rate limiting (mcp.md §10.3). Reads, writes,
// publish and create_site each get their own token-bucket so an AI write loop
// can't starve reads and a runaway create can't spawn projects.
type toolClass int

const (
	classRead toolClass = iota
	classWrite
	classPublish
	classCreate
)

// Size caps (mcp.md §10.2). Defaults mirror the Zip-upload guards so MCP is not a
// bypass around the upload path's size protections. Overridable via Limits.
const (
	defaultMaxFileBytes = 5 << 20 // 5 MiB: single write_file content
	defaultMaxReadBytes = 1 << 20 // 1 MiB: inline read before truncation
	defaultMaxDiffBytes = 1 << 20 // 1 MiB: total get_diff patch bytes
	defaultMaxListItems = 5000    // list_files max entries
	defaultMaxLogLimit  = 100     // get_log.limit hard cap (CANONICAL LogOptions caps 100)
)

// Rate defaults (mcp.md §10.3): per-token-id token buckets.
const (
	readPerMin    = 120
	readBurst     = 30
	writePerMin   = 30
	writeBurst    = 10
	publishPerMin = 6
	publishBurst  = 3
	createPerMin  = 3
	createBurst   = 3
)

// Limits bundles the configurable size caps and the rate Limiter. New() takes one
// so prod can pull values from env/config and tests inject deterministic values.
// Zero-value sub-fields are filled by withDefaults().
type Limits struct {
	MaxFileBytes int   // write_file content cap (bytes)
	MaxReadBytes int64 // read_file inline cap before truncation (bytes)
	MaxDiffBytes int   // get_diff total patch budget (bytes)
	MaxListItems int   // list_files max entries
	MaxLogLimit  int   // get_log.limit hard cap

	// Limiter is the per-token rate gate. nil => an in-memory token-bucket limiter.
	Limiter Limiter
}

// DefaultLimits returns the mcp.md §10 defaults with an in-memory limiter.
func DefaultLimits() Limits {
	return Limits{}.withDefaults()
}

// withDefaults fills any zero field with its spec default and ensures a Limiter
// exists. It returns a copy (value receiver) so the original is never mutated.
func (l Limits) withDefaults() Limits {
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = defaultMaxFileBytes
	}
	if l.MaxReadBytes <= 0 {
		l.MaxReadBytes = defaultMaxReadBytes
	}
	if l.MaxDiffBytes <= 0 {
		l.MaxDiffBytes = defaultMaxDiffBytes
	}
	if l.MaxListItems <= 0 {
		l.MaxListItems = defaultMaxListItems
	}
	if l.MaxLogLimit <= 0 {
		l.MaxLogLimit = defaultMaxLogLimit
	}
	if l.Limiter == nil {
		l.Limiter = NewMemoryLimiter()
	}
	return l
}

// Limiter gates how often a token may call tools of a given class. Allow reports
// whether the call may proceed and, if not, a hint for how long to back off. It
// is an interface so tests inject a deterministic fake and prod can later swap an
// in-memory bucket for a Postgres/Redis-backed one (mcp.md §10.3).
type Limiter interface {
	// Allow reports whether tokenID may perform a class action now. When denied,
	// retryAfter is a positive backoff hint surfaced to the client.
	Allow(tokenID uuid.UUID, class toolClass) (ok bool, retryAfter time.Duration)
}

// memoryLimiter is the default in-process Limiter: one x/time/rate.Limiter per
// (tokenID, class). Buckets are created lazily and retained for the process
// lifetime (the token set is small; eviction is an Open Question, not needed v1).
type memoryLimiter struct {
	mu       sync.Mutex
	buckets  map[bucketKey]*rate.Limiter
	clock    func() time.Time
	settings map[toolClass]rateSetting
}

type bucketKey struct {
	token uuid.UUID
	class toolClass
}

type rateSetting struct {
	perMin int
	burst  int
}

// NewMemoryLimiter builds the default in-memory per-token token-bucket limiter
// with the mcp.md §10.3 rates.
func NewMemoryLimiter() *memoryLimiter {
	return &memoryLimiter{
		buckets: make(map[bucketKey]*rate.Limiter),
		clock:   time.Now,
		settings: map[toolClass]rateSetting{
			classRead:    {perMin: readPerMin, burst: readBurst},
			classWrite:   {perMin: writePerMin, burst: writeBurst},
			classPublish: {perMin: publishPerMin, burst: publishBurst},
			classCreate:  {perMin: createPerMin, burst: createBurst},
		},
	}
}

// Allow consults (creating lazily) the per-(token,class) bucket. On denial it
// estimates retryAfter from the bucket's reservation delay.
func (m *memoryLimiter) Allow(tokenID uuid.UUID, class toolClass) (bool, time.Duration) {
	m.mu.Lock()
	key := bucketKey{token: tokenID, class: class}
	lim, ok := m.buckets[key]
	if !ok {
		set := m.settings[class]
		// Convert per-minute to a per-second rate; burst is the bucket depth.
		lim = rate.NewLimiter(rate.Limit(float64(set.perMin)/60.0), set.burst)
		m.buckets[key] = lim
	}
	m.mu.Unlock()

	now := m.clock()
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		// Should not happen with burst >= 1; treat as denied with a default backoff.
		return false, time.Second
	}
	delay := r.DelayFrom(now)
	if delay > 0 {
		// Not allowed now: cancel the reservation so we don't consume future
		// capacity, and report the wait as the retry hint.
		r.Cancel()
		return false, delay
	}
	return true, 0
}
