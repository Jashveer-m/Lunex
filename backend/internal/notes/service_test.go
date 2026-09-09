package notes

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// fakeStore keys by (owner, id), the same shape the SQL has.
type fakeStore struct {
	mu     sync.Mutex
	byUser map[uuid.UUID]map[uuid.UUID]Note
	calls  []uuid.UUID
}

func newFakeStore() *fakeStore {
	return &fakeStore{byUser: map[uuid.UUID]map[uuid.UUID]Note{}}
}

func (f *fakeStore) Create(_ context.Context, userID uuid.UUID, in CreateInput) (Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	n := Note{ID: uuid.New(), UserID: userID, Title: in.Title, Content: in.Content, Tags: in.Tags}
	if f.byUser[userID] == nil {
		f.byUser[userID] = map[uuid.UUID]Note{}
	}
	f.byUser[userID][n.ID] = n
	return n, nil
}

func (f *fakeStore) ByID(_ context.Context, userID, id uuid.UUID) (Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	n, ok := f.byUser[userID][id]
	if !ok {
		return Note{}, ErrNotFound
	}
	return n, nil
}

func (f *fakeStore) List(_ context.Context, userID uuid.UUID, _ Filter) ([]Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	out := []Note{}
	for _, n := range f.byUser[userID] {
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeStore) Update(_ context.Context, userID, id uuid.UUID, p Patch) (Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	n, ok := f.byUser[userID][id]
	if !ok {
		return Note{}, ErrNotFound
	}
	if p.Title != nil {
		n.Title = *p.Title
	}
	if p.Content != nil {
		n.Content = *p.Content
	}
	if p.Tags != nil {
		n.Tags = *p.Tags
	}
	f.byUser[userID][id] = n
	return n, nil
}

func (f *fakeStore) Delete(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if _, ok := f.byUser[userID][id]; !ok {
		return ErrNotFound
	}
	delete(f.byUser[userID], id)
	return nil
}

func TestNotesAreScopedToTheOwner(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()

	n, err := svc.Create(ctx, alice, CreateInput{Title: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, bob, n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get as the wrong user = %v, want ErrNotFound", err)
	}
	if _, err := svc.Update(ctx, bob, n.ID, UpdateInput{Title: optional.Of("hijacked")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update as the wrong user = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(ctx, bob, n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete as the wrong user = %v, want ErrNotFound", err)
	}
	if got, err := svc.Get(ctx, alice, n.ID); err != nil || got.Title != "private" {
		t.Fatalf("the owner's note = %+v, %v", got, err)
	}
	for i, got := range store.calls {
		if got != alice && got != bob {
			t.Fatalf("store call %d used an unexpected owner %s", i, got)
		}
	}
}

func TestUpdateAppliesOnlyMentionedFields(t *testing.T) {
	svc := NewService(newFakeStore())
	ctx := context.Background()
	alice := uuid.New()

	n, err := svc.Create(ctx, alice, CreateInput{Title: "t", Content: "body", Tags: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(ctx, alice, n.ID, UpdateInput{Title: optional.Of("t2")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "t2" {
		t.Fatalf("title = %q, want t2", updated.Title)
	}
	if updated.Content != "body" || len(updated.Tags) != 1 {
		t.Fatalf("an unmentioned field was overwritten: %+v", updated)
	}
}
