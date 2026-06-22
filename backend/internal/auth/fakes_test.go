package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// fakeStore is an in-memory implementation of StoreDeps (sessions + WithTx +
// user/identity upsert) used by every auth test. It is concurrency-safe so the
// session/middleware tests can exercise it without data races.
type fakeStore struct {
	mu sync.Mutex

	sessions map[string]gen.Session
	users    map[uuid.UUID]gen.User // by id
	byEmail  map[string]uuid.UUID   // email -> user id
	idents   map[string]uuid.UUID   // provider|subject -> user id
	touched  map[string]int         // session id -> touch count (assertions)

	// adminHash is the DB-stored admin password hash (first-run setup). adminHashSet
	// distinguishes "" (unset) from a real (empty-ish) value.
	adminHash    string
	adminHashSet bool

	// failNextCreate / failNextGet force a store error for error-path tests.
	failCreate bool
	failGet    bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions: map[string]gen.Session{},
		users:    map[uuid.UUID]gen.User{},
		byEmail:  map[string]uuid.UUID{},
		idents:   map[string]uuid.UUID{},
		touched:  map[string]int{},
	}
}

func (f *fakeStore) CreateSession(_ context.Context, arg gen.CreateSessionParams) (gen.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate {
		return gen.Session{}, errors.New("forced create failure")
	}
	s := gen.Session{
		ID:         arg.ID,
		UserID:     arg.UserID,
		CreatedAt:  pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ExpiresAt:  arg.ExpiresAt,
		LastSeenAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UserAgent:  arg.UserAgent,
		IpAddr:     arg.IpAddr,
	}
	f.sessions[arg.ID] = s
	return s, nil
}

func (f *fakeStore) GetSession(_ context.Context, id string) (gen.GetSessionRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet {
		return gen.GetSessionRow{}, errors.New("forced get failure")
	}
	s, ok := f.sessions[id]
	if !ok {
		return gen.GetSessionRow{}, pgx.ErrNoRows
	}
	// Mirror the SQL WHERE expires_at > now(): expired sessions are "not found".
	if s.ExpiresAt.Valid && !s.ExpiresAt.Time.After(time.Now()) {
		return gen.GetSessionRow{}, pgx.ErrNoRows
	}
	u, ok := f.users[s.UserID]
	if !ok || !u.IsActive {
		return gen.GetSessionRow{}, pgx.ErrNoRows
	}
	return gen.GetSessionRow{
		ID:             s.ID,
		UserID:         s.UserID,
		CreatedAt:      s.CreatedAt,
		ExpiresAt:      s.ExpiresAt,
		LastSeenAt:     s.LastSeenAt,
		Email:          u.Email,
		DisplayName:    u.DisplayName,
		AvatarUrl:      u.AvatarUrl,
		IsAdmin:        u.IsAdmin,
		CanCreateSites: u.CanCreateSites,
	}, nil
}

func (f *fakeStore) DeleteSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
	return nil
}

func (f *fakeStore) TouchSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched[id]++
	if s, ok := f.sessions[id]; ok {
		s.LastSeenAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		f.sessions[id] = s
	}
	return nil
}

func (f *fakeStore) UpsertUser(_ context.Context, arg gen.UpsertUserParams) (gen.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.byEmail[arg.Email]; ok {
		u := f.users[id]
		u.DisplayName = arg.DisplayName
		if arg.AvatarUrl != nil {
			u.AvatarUrl = arg.AvatarUrl
		}
		f.users[id] = u
		return u, nil
	}
	u := gen.User{
		ID:          uuid.New(),
		Email:       arg.Email,
		DisplayName: arg.DisplayName,
		AvatarUrl:   arg.AvatarUrl,
		IsActive:    true,
		CreatedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.users[u.ID] = u
	f.byEmail[u.Email] = u.ID
	return u, nil
}

func (f *fakeStore) UpsertIdentity(_ context.Context, arg gen.UpsertIdentityParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idents[arg.Provider+"|"+arg.Subject] = arg.UserID
	return nil
}

// GetAdminPasswordHash mirrors *db.Store: a missing hash is (.., false, nil).
func (f *fakeStore) GetAdminPasswordHash(_ context.Context) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.adminHashSet {
		return "", false, nil
	}
	return f.adminHash, true, nil
}

// SetAdminPasswordHash records the first-run admin hash (idempotent overwrite).
func (f *fakeStore) SetAdminPasswordHash(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adminHash, f.adminHashSet = hash, true
	return nil
}

// WithTx runs fn against the SAME fakeStore via a thin gen.Queries shim. The
// fake does not implement real transactions (single in-memory map under a mutex
// is atomic enough for these tests); fn either fully succeeds or returns an err.
func (f *fakeStore) WithTx(ctx context.Context, fn func(q *gen.Queries) error) error {
	// We cannot build a *gen.Queries over the fake, so the storeUpserter path is
	// tested via a dedicated fakeUpserter instead. WithTx here exists only to
	// satisfy StoreDeps; callers in tests use fakeUpserter, never this method.
	return errors.New("fakeStore.WithTx not used in tests; inject fakeUpserter")
}

// --- helpers to seed the fake ---

func (f *fakeStore) addUser(u gen.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.ID] = u
	f.byEmail[u.Email] = u.ID
}

func (f *fakeStore) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

func (f *fakeStore) expireSession(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[id]; ok {
		s.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}
		f.sessions[id] = s
	}
}

// fakeUpserter is an in-memory UserUpserter so completeLogin can be tested
// without a real DB transaction. It records every linked identity.
type fakeUpserter struct {
	store     *fakeStore
	calls     int
	lastInput UpsertLoginInput
}

func (f *fakeUpserter) UpsertLogin(ctx context.Context, in UpsertLoginInput) (gen.User, error) {
	f.calls++
	f.lastInput = in
	u, err := f.store.UpsertUser(ctx, gen.UpsertUserParams{
		Email:       in.Email,
		DisplayName: in.DisplayName,
		AvatarUrl:   in.AvatarURL,
	})
	if err != nil {
		return gen.User{}, err
	}
	if err := f.store.UpsertIdentity(ctx, gen.UpsertIdentityParams{
		UserID:   u.ID,
		Provider: in.Provider,
		Subject:  in.Subject,
	}); err != nil {
		return gen.User{}, err
	}
	return u, nil
}

// fakeAdminHashStore is a one-method AdminHashStore for the PasswordProvider unit
// tests (DB-hash precedence / env fallback) without the full fakeStore.
type fakeAdminHashStore struct {
	hash  string
	found bool
	err   error
}

func (f *fakeAdminHashStore) GetAdminPasswordHash(_ context.Context) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	return f.hash, f.found, nil
}

// fakeProvider is a mock AuthProvider with programmable Start/Exchange behavior.
type fakeProvider struct {
	key           string
	interactive   bool
	startURL      string
	exchangeReply Claims
	exchangeErr   error

	// captured handshake args for assertions.
	gotState    string
	gotNonce    string
	gotVerifier string
	gotCode     string
}

func (p *fakeProvider) Key() string       { return p.key }
func (p *fakeProvider) Interactive() bool { return p.interactive }

func (p *fakeProvider) Start(state, nonce, verifier string) string {
	p.gotState, p.gotNonce, p.gotVerifier = state, nonce, verifier
	return p.startURL
}

func (p *fakeProvider) Exchange(_ context.Context, code, verifier, nonce string) (Claims, error) {
	p.gotCode, p.gotVerifier, p.gotNonce = code, verifier, nonce
	if p.exchangeErr != nil {
		return Claims{}, p.exchangeErr
	}
	return p.exchangeReply, nil
}
