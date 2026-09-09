package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/users"
)

// fakeUserStore is an in-memory UserStore for service tests.
type fakeUserStore struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]users.User
	profiles map[uuid.UUID]users.Profile
	failWith error // when set, every method returns this
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		byID:     make(map[uuid.UUID]users.User),
		profiles: make(map[uuid.UUID]users.Profile),
	}
}

func (f *fakeUserStore) Create(_ context.Context, email, passwordHash, name, timezone string) (users.User, users.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return users.User{}, users.Profile{}, f.failWith
	}
	for _, u := range f.byID {
		if strings.EqualFold(u.Email, email) {
			return users.User{}, users.Profile{}, users.ErrEmailTaken
		}
	}
	now := time.Now()
	u := users.User{ID: uuid.New(), Email: strings.ToLower(email), PasswordHash: passwordHash, CreatedAt: now, UpdatedAt: now}
	p := users.Profile{UserID: u.ID, Name: name, Timezone: timezone, CreatedAt: now, UpdatedAt: now}
	f.byID[u.ID] = u
	f.profiles[u.ID] = p
	return u, p, nil
}

func (f *fakeUserStore) ByEmail(_ context.Context, email string) (users.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return users.User{}, f.failWith
	}
	for _, u := range f.byID {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return users.User{}, users.ErrNotFound
}

func (f *fakeUserStore) ByID(_ context.Context, id uuid.UUID) (users.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return users.User{}, f.failWith
	}
	u, ok := f.byID[id]
	if !ok {
		return users.User{}, users.ErrNotFound
	}
	return u, nil
}

func (f *fakeUserStore) ProfileByUserID(_ context.Context, id uuid.UUID) (users.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return users.Profile{}, f.failWith
	}
	p, ok := f.profiles[id]
	if !ok {
		return users.Profile{}, users.ErrNotFound
	}
	return p, nil
}

// fakeSessionStore mirrors the Postgres semantics that matter: hash lookups,
// atomic rotation, and expiry.
type fakeSessionStore struct {
	mu       sync.Mutex
	byHash   map[string]Session
	now      func() time.Time
	failWith error
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{byHash: make(map[string]Session), now: time.Now}
}

func (f *fakeSessionStore) Create(_ context.Context, userID uuid.UUID, hash string, expiresAt time.Time, deviceInfo *string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return Session{}, f.failWith
	}
	s := Session{ID: uuid.New(), UserID: userID, RefreshTokenHash: hash, ExpiresAt: expiresAt, DeviceInfo: deviceInfo, CreatedAt: f.now()}
	f.byHash[hash] = s
	return s, nil
}

func (f *fakeSessionStore) Rotate(_ context.Context, oldHash, newHash string, expiresAt time.Time) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return Session{}, f.failWith
	}
	s, ok := f.byHash[oldHash]
	if !ok || !s.ExpiresAt.After(f.now()) {
		return Session{}, ErrSessionNotFound
	}
	delete(f.byHash, oldHash)
	s.RefreshTokenHash = newHash
	s.ExpiresAt = expiresAt
	f.byHash[newHash] = s
	return s, nil
}

func (f *fakeSessionStore) DeleteByTokenHash(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	if _, ok := f.byHash[hash]; !ok {
		return ErrSessionNotFound
	}
	delete(f.byHash, hash)
	return nil
}

func (f *fakeSessionStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byHash)
}

// newTestService builds a Service backed by the fakes, with cheap hash params.
func newTestService(t interface{ Helper() }) (*Service, *fakeUserStore, *fakeSessionStore) {
	t.Helper()
	usersStore := newFakeUserStore()
	sessions := newFakeSessionStore()
	svc := NewService(usersStore, sessions, NewTokenIssuer(testSecret, "lifeos-test", 15*time.Minute), 30*24*time.Hour)
	svc.hashParams = testParams()
	return svc, usersStore, sessions
}
