package site

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// memStore is an in-memory site.Store for the real gitService contract tests:
// it lets us exercise the production git logic with NO Postgres (the PG-backed
// adapter is integration-tagged). It enforces the same handle uniqueness +
// redirect + soft-delete semantics the SQL queries do.
type memStore struct {
	mu        sync.Mutex
	sites     map[uuid.UUID]*SiteRecord
	redirects map[string]uuid.UUID // old handle -> site id
	clock     func() time.Time
}

func newMemStore() *memStore {
	return &memStore{
		sites:     make(map[uuid.UUID]*SiteRecord),
		redirects: make(map[string]uuid.UUID),
		clock:     time.Now,
	}
}

var _ Store = (*memStore)(nil)

func (m *memStore) CreateSiteWithOwner(ctx context.Context, in StoreCreateSite) (SiteRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Final-guard uniqueness across live handles + redirects + reserved.
	if m.takenLocked(in.Handle) {
		return SiteRecord{}, errUnique
	}
	now := m.clock().UTC()
	rec := SiteRecord{
		ID:            uuid.New(),
		Handle:        in.Handle,
		OwnerID:       in.OwnerID,
		Visibility:    in.Visibility,
		DefaultBranch: string(BranchDraft),
		PublishMode:   in.PublishMode,
		GitHubRepo:    in.GitHubRepo,
		WebRoot:       in.WebRoot,
		Description:   in.Description,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	cp := rec
	m.sites[rec.ID] = &cp
	return rec, nil
}

func (m *memStore) GetSiteByID(ctx context.Context, id uuid.UUID) (SiteRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.sites[id]
	if !ok {
		return SiteRecord{}, false, nil
	}
	return *rec, true, nil
}

func (m *memStore) GetSiteByHandle(ctx context.Context, handle string) (SiteRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lower := strings.ToLower(handle)
	for _, rec := range m.sites {
		if rec.DeletedAt == nil && rec.Handle == lower {
			return *rec, true, nil
		}
	}
	return SiteRecord{}, false, nil
}

func (m *memStore) GetSiteByRedirect(ctx context.Context, oldHandle string) (SiteRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.redirects[strings.ToLower(oldHandle)]
	if !ok {
		return SiteRecord{}, false, nil
	}
	rec, ok := m.sites[id]
	if !ok || rec.DeletedAt != nil {
		return SiteRecord{}, false, nil
	}
	return *rec, true, nil
}

func (m *memStore) ListSitesForUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]SiteRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SiteRecord
	for _, rec := range m.sites {
		if rec.DeletedAt != nil {
			continue
		}
		if userID == uuid.Nil || rec.OwnerID == userID {
			out = append(out, *rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if offset > len(out) {
		offset = len(out)
	}
	out = out[offset:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) HandleIsTaken(ctx context.Context, handle string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.takenLocked(handle), nil
}

// takenLocked checks live handles, redirects, and reserved words. Caller holds mu.
func (m *memStore) takenLocked(handle string) bool {
	lower := strings.ToLower(handle)
	if IsReservedHandle(Handle(lower)) {
		return true
	}
	if _, ok := m.redirects[lower]; ok {
		return true
	}
	for _, rec := range m.sites {
		if rec.DeletedAt == nil && rec.Handle == lower {
			return true
		}
	}
	return false
}

func (m *memStore) RenameHandleWithRedirect(ctx context.Context, id uuid.UUID, oldHandle, newHandle string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.sites[id]
	if !ok {
		return errNoRows
	}
	// rename-back: clear a redirect of THIS site pointing at newHandle.
	if owner, ok := m.redirects[strings.ToLower(newHandle)]; ok && owner == id {
		delete(m.redirects, strings.ToLower(newHandle))
	}
	m.redirects[strings.ToLower(oldHandle)] = id
	rec.Handle = strings.ToLower(newHandle)
	rec.UpdatedAt = m.clock().UTC()
	return nil
}

func (m *memStore) SoftDeleteSite(ctx context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.sites[id]
	if !ok {
		return errNoRows
	}
	now := m.clock().UTC()
	rec.DeletedAt = &now
	rec.UpdatedAt = now
	return nil
}

func (m *memStore) SetPublished(ctx context.Context, id uuid.UUID, sha *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.sites[id]
	if !ok {
		return errNoRows
	}
	rec.PublishedSHA = sha
	now := m.clock().UTC()
	rec.PublishedAt = &now
	rec.UpdatedAt = now
	return nil
}

func (m *memStore) SetSiteRemote(ctx context.Context, id uuid.UUID, repo *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.sites[id]
	if !ok {
		return errNoRows
	}
	rec.GitHubRepo = repo
	rec.UpdatedAt = m.clock().UTC()
	return nil
}

// errUnique mimics a Postgres 23505 unique violation so the mapping in
// CreateSite (isUniqueViolation) routes it to ErrHandleTaken.
var errUnique = &fakeErr{"duplicate key value violates unique constraint (SQLSTATE 23505)"}

// errNoRows mimics a missing row.
var errNoRows = &fakeErr{"no rows in result set"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }
