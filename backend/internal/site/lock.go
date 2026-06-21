package site

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// lockRegistry hands out a per-site *sync.RWMutex from a sharded map keyed by the
// site UUID (CANONICAL §1 / site-service.md §8). Read methods take RLock;
// mutations take the full Lock, held across the whole read-tip -> stage -> commit
// sequence so the optimistic-lock comparison and the commit are atomic with
// respect to other writers in this process. Locks are created lazily and never
// deleted in v1 (a bounded number of sites; the cost is negligible).
type lockRegistry struct {
	mu    sync.Mutex
	locks map[uuid.UUID]*sync.RWMutex
}

// newLockRegistry constructs an empty registry.
func newLockRegistry() *lockRegistry {
	return &lockRegistry{locks: make(map[uuid.UUID]*sync.RWMutex)}
}

// get returns the RWMutex for id, creating it on first use. The brief registry
// mutex protects only the map insert, not the held site lock, so distinct sites
// never contend.
func (r *lockRegistry) get(id uuid.UUID) *sync.RWMutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.locks[id]
	if !ok {
		m = &sync.RWMutex{}
		r.locks[id] = m
	}
	return m
}

// fileLock is an OS advisory lock (flock) on a per-site lock file. It is the
// cross-process arm of the lock seam (CANONICAL §1, decision #4: single replica
// in v1, but the flock makes a second process / a stray CLI safe too). The
// in-process RWMutex serializes goroutines; the flock serializes processes that
// share the /data volume.
type fileLock struct {
	f *os.File
}

// lockFilePath is the canonical advisory-lock file inside a site's .git dir
// (CANONICAL §1: /data/sites/{uuid}/.git/kotoji.lock).
func lockFilePath(gitDir string) string {
	return filepath.Join(gitDir, "kotoji.lock")
}

// acquireFileLock opens (creating if needed) the lock file and takes an exclusive
// advisory lock. The caller MUST call release(). It blocks until the lock is
// available; cross-process contention in v1 is rare (single replica) so blocking
// is acceptable and simpler than a busy spin.
func acquireFileLock(gitDir string) (*fileLock, error) {
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		return nil, fmt.Errorf("site: lock mkdir: %w", err)
	}
	f, err := os.OpenFile(lockFilePath(gitDir), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("site: lock open: %w", err)
	}
	// LOCK_EX is the exclusive (write) advisory lock; readers in the same process
	// are already gated by the RWMutex, so the file lock only needs the exclusive
	// mode for cross-process writers.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("site: flock: %w", err)
	}
	return &fileLock{f: f}, nil
}

// release drops the advisory lock and closes the file. Closing the descriptor
// also releases the flock as a safety net if Flock(LOCK_UN) ever fails.
func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
