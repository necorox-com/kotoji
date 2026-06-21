package auth

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

func activeUser(t *testing.T, admin bool) gen.User {
	t.Helper()
	return gen.User{
		ID:             uuid.New(),
		Email:          "user@example.com",
		DisplayName:    "User",
		IsAdmin:        admin,
		CanCreateSites: true,
		IsActive:       true,
		CreatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

func TestSessionManager_CreateRotatesAndStores(t *testing.T) {
	store := newFakeStore()
	u := activeUser(t, false)
	store.addUser(u)
	m := NewSessionManager(store, "kotoji_session", time.Hour, false)

	id1, err := m.Create(context.Background(), u.ID, "ua", "1.2.3.4:5555")
	require.NoError(t, err)
	require.NotEmpty(t, id1)

	id2, err := m.Create(context.Background(), u.ID, "ua", "1.2.3.4:5555")
	require.NoError(t, err)

	// Rotation: each Create yields a distinct opaque id (anti session-fixation).
	require.NotEqual(t, id1, id2)
	require.Equal(t, 2, store.sessionCount())
}

func TestSessionManager_Get(t *testing.T) {
	tests := []struct {
		name    string
		prep    func(t *testing.T, m *SessionManager, store *fakeStore) (id string)
		wantErr error
	}{
		{
			name: "valid",
			prep: func(t *testing.T, m *SessionManager, store *fakeStore) string {
				u := activeUser(t, false)
				store.addUser(u)
				id, err := m.Create(context.Background(), u.ID, "ua", "")
				require.NoError(t, err)
				return id
			},
			wantErr: nil,
		},
		{
			name: "expired",
			prep: func(t *testing.T, m *SessionManager, store *fakeStore) string {
				u := activeUser(t, false)
				store.addUser(u)
				id, err := m.Create(context.Background(), u.ID, "ua", "")
				require.NoError(t, err)
				store.expireSession(id)
				return id
			},
			wantErr: ErrSessionNotFound,
		},
		{
			name:    "missing",
			prep:    func(t *testing.T, m *SessionManager, store *fakeStore) string { return "nope" },
			wantErr: ErrSessionNotFound,
		},
		{
			name:    "empty id",
			prep:    func(t *testing.T, m *SessionManager, store *fakeStore) string { return "" },
			wantErr: ErrSessionNotFound,
		},
		{
			name: "inactive user",
			prep: func(t *testing.T, m *SessionManager, store *fakeStore) string {
				u := activeUser(t, false)
				u.IsActive = false
				store.addUser(u)
				id, err := m.Create(context.Background(), u.ID, "ua", "")
				require.NoError(t, err)
				return id
			},
			wantErr: ErrSessionNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			m := NewSessionManager(store, "kotoji_session", time.Hour, false)
			id := tc.prep(t, m, store)

			_, err := m.Get(context.Background(), id)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestSessionManager_Delete(t *testing.T) {
	store := newFakeStore()
	u := activeUser(t, false)
	store.addUser(u)
	m := NewSessionManager(store, "kotoji_session", time.Hour, false)

	id, err := m.Create(context.Background(), u.ID, "ua", "")
	require.NoError(t, err)
	require.Equal(t, 1, store.sessionCount())

	require.NoError(t, m.Delete(context.Background(), id))
	require.Equal(t, 0, store.sessionCount())

	// Idempotent: deleting an empty/missing id is fine.
	require.NoError(t, m.Delete(context.Background(), ""))
	require.NoError(t, m.Delete(context.Background(), "ghost"))
}

func TestSessionManager_CookieHostPrefix(t *testing.T) {
	secure := NewSessionManager(newFakeStore(), "kotoji_session", time.Hour, true)
	require.Equal(t, "__Host-kotoji_session", secure.CookieName())

	insecure := NewSessionManager(newFakeStore(), "kotoji_session", time.Hour, false)
	require.Equal(t, "kotoji_session", insecure.CookieName())

	// SetCookie in secure mode must be host-only: Secure, no Domain, Path=/.
	rec := httptest.NewRecorder()
	secure.SetCookie(rec, "sid-value")
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	c := cookies[0]
	require.Equal(t, "__Host-kotoji_session", c.Name)
	require.True(t, c.Secure)
	require.True(t, c.HttpOnly)
	require.Empty(t, c.Domain) // host-only: no Domain attribute
	require.Equal(t, "/", c.Path)
}

func TestSessionManager_TouchThrottle(t *testing.T) {
	store := newFakeStore()
	u := activeUser(t, false)
	store.addUser(u)
	m := NewSessionManager(store, "kotoji_session", time.Hour, false)

	id, err := m.Create(context.Background(), u.ID, "ua", "")
	require.NoError(t, err)

	// Fresh session: a Get right away must NOT touch (within touchInterval).
	_, err = m.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, 0, store.touched[id])

	// Force last_seen_at stale, then Get should touch exactly once.
	store.mu.Lock()
	s := store.sessions[id]
	s.LastSeenAt = pgtype.Timestamptz{Time: time.Now().Add(-2 * touchInterval), Valid: true}
	store.sessions[id] = s
	store.mu.Unlock()

	_, err = m.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, 1, store.touched[id])
}
