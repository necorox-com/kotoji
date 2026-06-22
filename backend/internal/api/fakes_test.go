package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// ---- fake MetaStore ----

// fakeMetaStore is an in-memory MetaStore for the handler tests: per-site roles,
// members, tokens, users, and a captured audit log. It is concurrency-safe.
type fakeMetaStore struct {
	mu sync.Mutex

	// roles keyed by (siteID|userID) -> role.
	roles map[string]gen.SiteRole
	// members keyed by siteID -> list of member rows.
	members map[uuid.UUID][]gen.ListMembersRow
	// tokens keyed by siteID -> token rows; created tokens land here too.
	tokens map[uuid.UUID][]gen.ListTokensForSiteRow
	// users by id and by email.
	users   map[uuid.UUID]gen.User
	byEmail map[string]gen.User
	// audit captures every InsertAudit call for assertions.
	audit []gen.InsertAuditParams
	// settingsUpdates captures UpdateSiteSettings calls.
	settingsUpdates []gen.UpdateSiteSettingsParams

	// github is the DB-stored GitHub mirror config the admin-github handler reads/
	// writes. setGitHubInputs records every SetGitHubConfig call for assertions.
	github          db.GitHubConfig
	setGitHubInputs []db.SetGitHubConfigInput

	failGetRole bool
}

func newFakeMetaStore() *fakeMetaStore {
	return &fakeMetaStore{
		roles:   map[string]gen.SiteRole{},
		members: map[uuid.UUID][]gen.ListMembersRow{},
		tokens:  map[uuid.UUID][]gen.ListTokensForSiteRow{},
		users:   map[uuid.UUID]gen.User{},
		byEmail: map[string]gen.User{},
	}
}

func roleKey(siteID, userID uuid.UUID) string { return siteID.String() + "|" + userID.String() }

func (f *fakeMetaStore) setRole(siteID, userID uuid.UUID, role gen.SiteRole) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[roleKey(siteID, userID)] = role
	// keep a members row in sync so ListMembers/isSoleOwner reflect the grant.
	u := f.users[userID]
	rows := f.members[siteID]
	found := false
	for i := range rows {
		if rows[i].UserID == userID {
			rows[i].Role = role
			found = true
		}
	}
	if !found {
		rows = append(rows, gen.ListMembersRow{
			SiteID:      siteID,
			UserID:      userID,
			Role:        role,
			Email:       u.Email,
			DisplayName: u.DisplayName,
			CreatedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
	}
	f.members[siteID] = rows
}

func (f *fakeMetaStore) addUser(u gen.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.ID] = u
	f.byEmail[u.Email] = u
}

func (f *fakeMetaStore) GetRole(_ context.Context, arg gen.GetRoleParams) (gen.SiteRole, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGetRole {
		return "", errors.New("forced GetRole failure")
	}
	role, ok := f.roles[roleKey(arg.SiteID, arg.UserID)]
	if !ok {
		return "", pgx.ErrNoRows
	}
	return role, nil
}

func (f *fakeMetaStore) ListMembers(_ context.Context, siteID uuid.UUID) ([]gen.ListMembersRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gen.ListMembersRow, len(f.members[siteID]))
	copy(out, f.members[siteID])
	return out, nil
}

func (f *fakeMetaStore) AddMember(_ context.Context, arg gen.AddMemberParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[roleKey(arg.SiteID, arg.UserID)] = arg.Role
	u := f.users[arg.UserID]
	f.members[arg.SiteID] = append(f.members[arg.SiteID], gen.ListMembersRow{
		SiteID: arg.SiteID, UserID: arg.UserID, Role: arg.Role,
		Email: u.Email, DisplayName: u.DisplayName,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	return nil
}

func (f *fakeMetaStore) UpdateMemberRole(_ context.Context, arg gen.UpdateMemberRoleParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[roleKey(arg.SiteID, arg.UserID)] = arg.Role
	rows := f.members[arg.SiteID]
	for i := range rows {
		if rows[i].UserID == arg.UserID {
			rows[i].Role = arg.Role
		}
	}
	f.members[arg.SiteID] = rows
	return nil
}

func (f *fakeMetaStore) RemoveMember(_ context.Context, arg gen.RemoveMemberParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.roles, roleKey(arg.SiteID, arg.UserID))
	rows := f.members[arg.SiteID]
	out := rows[:0]
	for _, m := range rows {
		if m.UserID != arg.UserID {
			out = append(out, m)
		}
	}
	f.members[arg.SiteID] = out
	return nil
}

func (f *fakeMetaStore) GetMember(_ context.Context, arg gen.GetMemberParams) (gen.SiteMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	role, ok := f.roles[roleKey(arg.SiteID, arg.UserID)]
	if !ok {
		return gen.SiteMember{}, pgx.ErrNoRows
	}
	return gen.SiteMember{SiteID: arg.SiteID, UserID: arg.UserID, Role: role}, nil
}

func (f *fakeMetaStore) CreateToken(_ context.Context, arg gen.CreateTokenParams) (gen.CreateTokenRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := gen.CreateTokenRow{
		ID:             uuid.New(),
		SiteID:         arg.SiteID,
		CreatedBy:      arg.CreatedBy,
		Name:           arg.Name,
		TokenPrefix:    arg.TokenPrefix,
		Scopes:         arg.Scopes,
		CanCreateSites: arg.CanCreateSites,
		CreatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ExpiresAt:      arg.ExpiresAt,
	}
	f.tokens[arg.SiteID] = append(f.tokens[arg.SiteID], gen.ListTokensForSiteRow{
		ID: row.ID, SiteID: row.SiteID, CreatedBy: row.CreatedBy, Name: row.Name,
		TokenPrefix: row.TokenPrefix, Scopes: row.Scopes, CanCreateSites: row.CanCreateSites,
		CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt,
	})
	return row, nil
}

func (f *fakeMetaStore) ListTokensForSite(_ context.Context, siteID uuid.UUID) ([]gen.ListTokensForSiteRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gen.ListTokensForSiteRow, len(f.tokens[siteID]))
	copy(out, f.tokens[siteID])
	return out, nil
}

func (f *fakeMetaStore) RevokeToken(_ context.Context, arg gen.RevokeTokenParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.tokens[arg.SiteID]
	for i := range rows {
		if rows[i].ID == arg.ID {
			rows[i].RevokedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		}
	}
	f.tokens[arg.SiteID] = rows
	return nil
}

func (f *fakeMetaStore) UpdateSiteSettings(_ context.Context, arg gen.UpdateSiteSettingsParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settingsUpdates = append(f.settingsUpdates, arg)
	return nil
}

func (f *fakeMetaStore) GetUserByEmail(_ context.Context, email string) (gen.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byEmail[email]
	if !ok {
		return gen.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeMetaStore) GetUserByID(_ context.Context, id uuid.UUID) (gen.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return gen.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeMetaStore) SetUserAdminFlags(_ context.Context, arg gen.SetUserAdminFlagsParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[arg.ID]
	if !ok {
		return pgx.ErrNoRows
	}
	u.IsAdmin = arg.IsAdmin
	u.CanCreateSites = arg.CanCreateSites
	f.users[arg.ID] = u
	f.byEmail[u.Email] = u
	return nil
}

func (f *fakeMetaStore) InsertAudit(_ context.Context, arg gen.InsertAuditParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audit = append(f.audit, arg)
	return nil
}

// GetGitHubConfig returns the in-memory DB GitHub config (decrypted token).
func (f *fakeMetaStore) GetGitHubConfig(_ context.Context) (db.GitHubConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.github, nil
}

// SetGitHubConfig applies a partial update mirroring the real store's semantics:
// nil fields untouched, empty token keeps the stored one, clearToken removes it.
// It records the raw input so tests can assert the token was/wasn't written.
func (f *fakeMetaStore) SetGitHubConfig(_ context.Context, in db.SetGitHubConfigInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setGitHubInputs = append(f.setGitHubInputs, in)
	if in.Enabled != nil {
		f.github.Enabled = *in.Enabled
		f.github.EnabledSet = true
	}
	if in.Org != nil {
		f.github.Org = *in.Org
	}
	if in.WebhookSecret != nil {
		f.github.WebhookSecret = *in.WebhookSecret
	}
	switch {
	case in.ClearToken:
		f.github.Token, f.github.TokenSet = "", false
	case in.Token != nil && *in.Token != "":
		f.github.Token, f.github.TokenSet = *in.Token, true
	}
	return nil
}

func (f *fakeMetaStore) ListAuditForSite(_ context.Context, arg gen.ListAuditForSiteParams) ([]gen.AuditLog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []gen.AuditLog{}
	for i, a := range f.audit {
		if a.SiteID != nil && arg.SiteID != nil && *a.SiteID == *arg.SiteID {
			out = append(out, gen.AuditLog{
				ID:          int64(i + 1),
				ActorUserID: a.ActorUserID,
				SiteID:      a.SiteID,
				TokenID:     a.TokenID,
				Action:      a.Action,
				Source:      a.Source,
				CommitSha:   a.CommitSha,
				Metadata:    a.Metadata,
				CreatedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
			})
		}
	}
	return out, nil
}

func (f *fakeMetaStore) auditActions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.audit))
	for i, a := range f.audit {
		out[i] = a.Action
	}
	return out
}

// ---- fake SessionStore for the real *auth.Auth middleware ----

// fakeSessionStore is a minimal auth.StoreDeps so the tests use the REAL auth
// SessionAuth middleware (the only thing that can set auth.CurrentUser). Cookie
// value -> session row; the joined user comes from the seeded user map.
type fakeSessionStore struct {
	mu       sync.Mutex
	sessions map[string]gen.Session
	users    map[uuid.UUID]gen.User
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: map[string]gen.Session{}, users: map[uuid.UUID]gen.User{}}
}

// seed registers a live session id -> user so a request bearing that cookie is
// authenticated as user.
func (f *fakeSessionStore) seed(sessionID string, u gen.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.ID] = u
	f.sessions[sessionID] = gen.Session{
		ID:         sessionID,
		UserID:     u.ID,
		ExpiresAt:  pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		LastSeenAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

func (f *fakeSessionStore) CreateSession(_ context.Context, arg gen.CreateSessionParams) (gen.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := gen.Session{ID: arg.ID, UserID: arg.UserID, ExpiresAt: arg.ExpiresAt}
	f.sessions[arg.ID] = s
	return s, nil
}

func (f *fakeSessionStore) GetSession(_ context.Context, id string) (gen.GetSessionRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return gen.GetSessionRow{}, pgx.ErrNoRows
	}
	u := f.users[s.UserID]
	return gen.GetSessionRow{
		ID: s.ID, UserID: s.UserID, ExpiresAt: s.ExpiresAt, LastSeenAt: s.LastSeenAt,
		Email: u.Email, DisplayName: u.DisplayName, AvatarUrl: u.AvatarUrl,
		IsAdmin: u.IsAdmin, CanCreateSites: u.CanCreateSites,
	}, nil
}

func (f *fakeSessionStore) DeleteSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
	return nil
}
func (f *fakeSessionStore) TouchSession(context.Context, string) error { return nil }
func (f *fakeSessionStore) UpsertUser(context.Context, gen.UpsertUserParams) (gen.User, error) {
	return gen.User{}, nil
}
func (f *fakeSessionStore) UpsertIdentity(context.Context, gen.UpsertIdentityParams) error {
	return nil
}
func (f *fakeSessionStore) WithTx(_ context.Context, fn func(q *gen.Queries) error) error {
	return errors.New("not used in api tests")
}
func (f *fakeSessionStore) GetAdminPasswordHash(context.Context) (string, bool, error) {
	return "", false, nil
}
func (f *fakeSessionStore) SetAdminPasswordHash(context.Context, string) error { return nil }
func (f *fakeSessionStore) PromoteUserAdmin(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[id]; ok {
		u.IsAdmin = true
		f.users[id] = u
	}
	return nil
}
func (f *fakeSessionStore) GetGitHubConfig(context.Context) (db.GitHubConfig, error) {
	return db.GitHubConfig{}, nil
}

// compile-time: fakeSessionStore satisfies auth.StoreDeps.
var _ auth.StoreDeps = (*fakeSessionStore)(nil)
