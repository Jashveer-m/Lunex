package api_test

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/auth"
	"github.com/jashveer/lifeos/backend/internal/users"
)

// memoryUserStore / memorySessionStore let the router tests run without a
// database. They implement only what auth.Service requires.
type memoryUserStore struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]users.User
	profiles map[uuid.UUID]users.Profile
}

func newMemoryUserStore() *memoryUserStore {
	return &memoryUserStore{byID: map[uuid.UUID]users.User{}, profiles: map[uuid.UUID]users.Profile{}}
}

func (m *memoryUserStore) Create(_ context.Context, email, hash, name, tz string) (users.User, users.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.byID {
		if strings.EqualFold(u.Email, email) {
			return users.User{}, users.Profile{}, users.ErrEmailTaken
		}
	}
	now := time.Now()
	u := users.User{ID: uuid.New(), Email: email, PasswordHash: hash, CreatedAt: now, UpdatedAt: now}
	p := users.Profile{UserID: u.ID, Name: name, Timezone: tz, CreatedAt: now, UpdatedAt: now}
	m.byID[u.ID], m.profiles[u.ID] = u, p
	return u, p, nil
}

func (m *memoryUserStore) ByEmail(_ context.Context, email string) (users.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.byID {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return users.User{}, users.ErrNotFound
}

func (m *memoryUserStore) ByID(_ context.Context, id uuid.UUID) (users.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[id]
	if !ok {
		return users.User{}, users.ErrNotFound
	}
	return u, nil
}

func (m *memoryUserStore) ProfileByUserID(_ context.Context, id uuid.UUID) (users.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.profiles[id]
	if !ok {
		return users.Profile{}, users.ErrNotFound
	}
	return p, nil
}

type memorySessionStore struct {
	mu     sync.Mutex
	byHash map[string]auth.Session
}

func newMemorySessionStore() *memorySessionStore {
	return &memorySessionStore{byHash: map[string]auth.Session{}}
}

func (m *memorySessionStore) Create(_ context.Context, userID uuid.UUID, hash string, expiresAt time.Time, device *string) (auth.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := auth.Session{ID: uuid.New(), UserID: userID, RefreshTokenHash: hash, ExpiresAt: expiresAt, DeviceInfo: device, CreatedAt: time.Now()}
	m.byHash[hash] = s
	return s, nil
}

func (m *memorySessionStore) Rotate(_ context.Context, oldHash, newHash string, expiresAt time.Time) (auth.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byHash[oldHash]
	if !ok || !s.ExpiresAt.After(time.Now()) {
		return auth.Session{}, auth.ErrSessionNotFound
	}
	delete(m.byHash, oldHash)
	s.RefreshTokenHash, s.ExpiresAt = newHash, expiresAt
	m.byHash[newHash] = s
	return s, nil
}

func (m *memorySessionStore) DeleteByTokenHash(_ context.Context, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byHash[hash]; !ok {
		return auth.ErrSessionNotFound
	}
	delete(m.byHash, hash)
	return nil
}
