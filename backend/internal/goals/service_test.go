package goals

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// fakeStore keys everything by (owner, id), the same shape the SQL has, so a
// service that forgot to pass the caller's id shows up here as a miss.
type fakeStore struct {
	mu         sync.Mutex
	byUser     map[uuid.UUID]map[uuid.UUID]Goal
	milestones map[uuid.UUID][]Milestone
	calls      []uuid.UUID
}

func newFakeStore() *fakeStore {
	return &fakeStore{byUser: map[uuid.UUID]map[uuid.UUID]Goal{}, milestones: map[uuid.UUID][]Milestone{}}
}

func (f *fakeStore) Create(_ context.Context, userID uuid.UUID, in CreateInput) (Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	g := Goal{ID: uuid.New(), UserID: userID, Title: in.Title, Type: in.Type, Status: in.Status}
	if f.byUser[userID] == nil {
		f.byUser[userID] = map[uuid.UUID]Goal{}
	}
	f.byUser[userID][g.ID] = g
	return g, nil
}

func (f *fakeStore) ByID(_ context.Context, userID, id uuid.UUID) (Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	g, ok := f.byUser[userID][id]
	if !ok {
		return Goal{}, ErrNotFound
	}
	g.Milestones = f.milestones[id]
	return g, nil
}

func (f *fakeStore) List(_ context.Context, userID uuid.UUID, _ Filter) ([]Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	out := []Goal{}
	for _, g := range f.byUser[userID] {
		out = append(out, g)
	}
	return out, nil
}

func (f *fakeStore) Update(_ context.Context, userID, id uuid.UUID, p Patch) (Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	g, ok := f.byUser[userID][id]
	if !ok {
		return Goal{}, ErrNotFound
	}
	if p.Title != nil {
		g.Title = *p.Title
	}
	f.byUser[userID][id] = g
	return g, nil
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

func (f *fakeStore) CountMilestones(_ context.Context, userID, goalID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if _, ok := f.byUser[userID][goalID]; !ok {
		return 0, ErrNotFound
	}
	return len(f.milestones[goalID]), nil
}

func (f *fakeStore) AddMilestone(_ context.Context, userID, goalID uuid.UUID, in MilestoneInput) (Milestone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if _, ok := f.byUser[userID][goalID]; !ok {
		return Milestone{}, ErrNotFound
	}
	m := Milestone{ID: uuid.New(), GoalID: goalID, Title: in.Title}
	f.milestones[goalID] = append(f.milestones[goalID], m)
	return m, nil
}

func (f *fakeStore) UpdateMilestone(_ context.Context, userID, goalID, milestoneID uuid.UUID, p MilestonePatch) (Milestone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if _, ok := f.byUser[userID][goalID]; !ok {
		return Milestone{}, ErrMilestoneNotFound
	}
	for i, m := range f.milestones[goalID] {
		if m.ID != milestoneID {
			continue
		}
		if p.Completed != nil {
			m.Completed = *p.Completed
		}
		f.milestones[goalID][i] = m
		return m, nil
	}
	return Milestone{}, ErrMilestoneNotFound
}

func newGoal(t *testing.T, svc *Service, owner uuid.UUID) Goal {
	t.Helper()
	g, err := svc.Create(context.Background(), owner, CreateInput{Title: "g", Type: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGoalsAreScopedToTheOwner(t *testing.T) {
	svc := NewService(newFakeStore())
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()
	g := newGoal(t, svc, alice)

	if _, err := svc.Get(ctx, bob, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get as the wrong user = %v, want ErrNotFound", err)
	}
	if _, err := svc.Update(ctx, bob, g.ID, UpdateInput{Title: optional.Of("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update as the wrong user = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(ctx, bob, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete as the wrong user = %v, want ErrNotFound", err)
	}
}

// A milestone is reachable only through a goal, so the goal's owner check is
// what protects it.
func TestMilestonesAreScopedThroughTheirGoal(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()
	g := newGoal(t, svc, alice)

	m, err := svc.AddMilestone(ctx, alice, g.ID, MilestoneInput{Title: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMilestone(ctx, bob, g.ID, MilestoneInput{Title: "hijack"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AddMilestone as the wrong user = %v, want ErrNotFound", err)
	}
	if _, err := svc.UpdateMilestone(ctx, bob, g.ID, m.ID, MilestoneUpdateInput{Completed: optional.Of(true)}); !errors.Is(err, ErrMilestoneNotFound) {
		t.Fatalf("UpdateMilestone as the wrong user = %v, want ErrMilestoneNotFound", err)
	}
	if store.milestones[g.ID][0].Completed {
		t.Fatal("the other user completed the milestone anyway")
	}
}

func TestMilestoneCapIsEnforced(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice := uuid.New()
	g := newGoal(t, svc, alice)

	store.milestones[g.ID] = make([]Milestone, MaxMilestones)
	_, err := svc.AddMilestone(ctx, alice, g.ID, MilestoneInput{Title: "one too many"})
	if err == nil {
		t.Fatalf("a goal accepted more than %d milestones", MaxMilestones)
	}
	if len(store.milestones[g.ID]) != MaxMilestones {
		t.Fatal("the rejected milestone was written anyway")
	}
}

func TestEveryStoreCallCarriesTheAuthenticatedUser(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice := uuid.New()

	g := newGoal(t, svc, alice)
	if _, err := svc.Get(ctx, alice, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.List(ctx, alice, Filter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMilestone(ctx, alice, g.ID, MilestoneInput{Title: "m"}); err != nil {
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
