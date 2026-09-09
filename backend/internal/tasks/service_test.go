package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// fakeStore is an in-memory Store. It keys everything by (owner, id) and never
// looks a row up without an owner, which is the same shape the SQL has — so a
// service bug that forgets to pass the caller's id shows up here as a miss.
type fakeStore struct {
	mu     sync.Mutex
	byUser map[uuid.UUID]map[uuid.UUID]Task
	// calls records the owner id the service passed down, so a test can prove
	// the authenticated user — not something from the request body — is what
	// reaches the store.
	calls []uuid.UUID
	edges map[[2]uuid.UUID]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{byUser: map[uuid.UUID]map[uuid.UUID]Task{}, edges: map[[2]uuid.UUID]bool{}}
}

func (f *fakeStore) record(userID uuid.UUID) { f.calls = append(f.calls, userID) }

func (f *fakeStore) Create(_ context.Context, userID uuid.UUID, in CreateInput) (Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(userID)
	t := Task{
		ID: uuid.New(), UserID: userID, Title: in.Title, Priority: in.Priority,
		Status: in.Status, Tags: in.Tags, ParentTaskID: in.ParentTaskID,
	}
	if f.byUser[userID] == nil {
		f.byUser[userID] = map[uuid.UUID]Task{}
	}
	f.byUser[userID][t.ID] = t
	return t, nil
}

func (f *fakeStore) ByID(_ context.Context, userID, id uuid.UUID) (Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(userID)
	t, ok := f.byUser[userID][id]
	if !ok {
		return Task{}, ErrNotFound
	}
	return t, nil
}

func (f *fakeStore) List(_ context.Context, userID uuid.UUID, _ Filter) ([]Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(userID)
	out := []Task{}
	for _, t := range f.byUser[userID] {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeStore) Update(_ context.Context, userID, id uuid.UUID, p Patch) (Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(userID)
	t, ok := f.byUser[userID][id]
	if !ok {
		return Task{}, ErrNotFound
	}
	if p.Title != nil {
		t.Title = *p.Title
	}
	if p.Status != nil {
		t.Status = *p.Status
	}
	if p.ParentTaskID.Set {
		t.ParentTaskID = p.ParentTaskID.Value
	}
	f.byUser[userID][id] = t
	return t, nil
}

func (f *fakeStore) Delete(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(userID)
	if _, ok := f.byUser[userID][id]; !ok {
		return ErrNotFound
	}
	delete(f.byUser[userID], id)
	return nil
}

func (f *fakeStore) Exists(_ context.Context, userID, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(userID)
	_, ok := f.byUser[userID][id]
	return ok, nil
}

func (f *fakeStore) AddDependency(_ context.Context, userID, taskID, dependsOn uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(userID)
	_, a := f.byUser[userID][taskID]
	_, b := f.byUser[userID][dependsOn]
	if !a || !b {
		return ErrNotFound
	}
	f.edges[[2]uuid.UUID{taskID, dependsOn}] = true
	return nil
}

func TestCreateRejectsAnotherUsersParent(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()

	parent, err := svc.Create(ctx, alice, CreateInput{Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}

	// Bob naming Alice's task as a parent must look exactly like naming a task
	// that does not exist. Anything else confirms the id is real.
	_, err = svc.Create(ctx, bob, CreateInput{Title: "child", ParentTaskID: &parent.ID})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateRejectsSelfParent(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice := uuid.New()

	task, err := svc.Create(ctx, alice, CreateInput{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Update(ctx, alice, task.ID, UpdateInput{ParentTaskID: optional.Of(task.ID)})
	var verrs validate.Errors
	if !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if verrs[0].Field != "parent_task_id" {
		t.Fatalf("rejected field = %q, want parent_task_id", verrs[0].Field)
	}
}

func TestAddDependencyRejectsSelfEdge(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice := uuid.New()

	task, err := svc.Create(ctx, alice, CreateInput{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// The table has a CHECK for this; catching it first turns a constraint
	// violation into a 400 that names the field.
	if _, err := svc.AddDependency(ctx, alice, task.ID, task.ID); err == nil {
		t.Fatal("a task was allowed to depend on itself")
	}
	if _, err := svc.AddDependency(ctx, alice, task.ID, uuid.Nil); err == nil {
		t.Fatal("a nil dependency id was accepted")
	}
}

// The authenticated user, and only the authenticated user, reaches the store.
// Nothing in a request body can redirect a read or a write to another owner.
func TestEveryStoreCallCarriesTheAuthenticatedUser(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice := uuid.New()

	task, err := svc.Create(ctx, alice, CreateInput{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, alice, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.List(ctx, alice, Filter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, alice, task.ID, UpdateInput{Title: optional.Of("t2")}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, alice, task.ID); err != nil {
		t.Fatal(err)
	}

	if len(store.calls) == 0 {
		t.Fatal("the store was never called")
	}
	for i, got := range store.calls {
		if got != alice {
			t.Fatalf("store call %d used owner %s, want %s", i, got, alice)
		}
	}
}

func TestGetAndDeleteAreScopedToTheOwner(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()

	task, err := svc.Create(ctx, alice, CreateInput{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, bob, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get as the wrong user = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(ctx, bob, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete as the wrong user = %v, want ErrNotFound", err)
	}
	if _, err := svc.Update(ctx, bob, task.ID, UpdateInput{Title: optional.Of("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update as the wrong user = %v, want ErrNotFound", err)
	}
	// Alice's row is untouched.
	if got, err := svc.Get(ctx, alice, task.ID); err != nil || got.Title != "t" {
		t.Fatalf("Alice's task = %+v, %v", got, err)
	}
}
