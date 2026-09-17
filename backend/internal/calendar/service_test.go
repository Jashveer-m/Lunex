package calendar

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// fakeStore keys by (owner, id), the same shape the SQL has -- including the
// two ownership probes, which are the only place this module reads another
// module's rows.
type fakeStore struct {
	mu     sync.Mutex
	byUser map[uuid.UUID]map[uuid.UUID]Event
	tasks  map[uuid.UUID]map[uuid.UUID]bool
	goals  map[uuid.UUID]map[uuid.UUID]bool
	calls  []uuid.UUID
	// filters records the window every List was given.
	filters []Filter
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byUser: map[uuid.UUID]map[uuid.UUID]Event{},
		tasks:  map[uuid.UUID]map[uuid.UUID]bool{},
		goals:  map[uuid.UUID]map[uuid.UUID]bool{},
	}
}

// seedTask and seedGoal record a row of somebody's tasks or goals, so a link
// to it resolves.
func (f *fakeStore) seedTask(owner uuid.UUID) uuid.UUID { return seedLink(f.tasks, owner) }
func (f *fakeStore) seedGoal(owner uuid.UUID) uuid.UUID { return seedLink(f.goals, owner) }

func seedLink(m map[uuid.UUID]map[uuid.UUID]bool, owner uuid.UUID) uuid.UUID {
	id := uuid.New()
	if m[owner] == nil {
		m[owner] = map[uuid.UUID]bool{}
	}
	m[owner][id] = true
	return id
}

func (f *fakeStore) Create(_ context.Context, userID uuid.UUID, in CreateInput) (Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	e := Event{
		ID: uuid.New(), UserID: userID, Title: in.Title,
		StartTime: in.StartTime, EndTime: in.EndTime, AllDay: in.AllDay,
		RelatedTaskID: in.RelatedTaskID, RelatedGoalID: in.RelatedGoalID,
	}
	if f.byUser[userID] == nil {
		f.byUser[userID] = map[uuid.UUID]Event{}
	}
	f.byUser[userID][e.ID] = e
	return e, nil
}

func (f *fakeStore) ByID(_ context.Context, userID, id uuid.UUID) (Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	e, ok := f.byUser[userID][id]
	if !ok {
		return Event{}, ErrNotFound
	}
	return e, nil
}

func (f *fakeStore) List(_ context.Context, userID uuid.UUID, filter Filter) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	f.filters = append(f.filters, filter)
	out := []Event{}
	for _, e := range f.byUser[userID] {
		if e.StartTime.Before(filter.End) && e.EndTime.After(filter.Start) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) Update(_ context.Context, userID, id uuid.UUID, p Patch) (Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	e, ok := f.byUser[userID][id]
	if !ok {
		return Event{}, ErrNotFound
	}
	if p.Title != nil {
		e.Title = *p.Title
	}
	if p.StartTime != nil {
		e.StartTime = *p.StartTime
	}
	if p.EndTime != nil {
		e.EndTime = *p.EndTime
	}
	if p.AllDay != nil {
		e.AllDay = *p.AllDay
	}
	if p.RelatedTaskID.Set {
		e.RelatedTaskID = p.RelatedTaskID.Value
	}
	f.byUser[userID][id] = e
	return e, nil
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

func (f *fakeStore) TaskExists(_ context.Context, userID, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	return f.tasks[userID][id], nil
}

func (f *fakeStore) GoalExists(_ context.Context, userID, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	return f.goals[userID][id], nil
}

var _ Store = (*fakeStore)(nil)

// The window every test that does not care about the window uses.
var (
	day   = time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC) // a Thursday
	week  = Filter{Start: day, End: day.AddDate(0, 0, 7)}
	nine  = day.Add(9 * time.Hour)
	tenAM = day.Add(10 * time.Hour)
)

func meeting() CreateInput {
	return CreateInput{Title: "Standup", StartTime: nine, EndTime: tenAM}
}

// The property the whole ownership design exists for, at the service level.
func TestEventsAreScopedToTheOwner(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()

	e, err := svc.Create(ctx, alice, meeting())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, bob, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get as the wrong user = %v, want ErrNotFound", err)
	}
	if _, err := svc.Update(ctx, bob, e.ID, UpdateInput{Title: optional.Of("hijacked")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update as the wrong user = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(ctx, bob, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete as the wrong user = %v, want ErrNotFound", err)
	}
	found, err := svc.List(ctx, bob, week)
	if err != nil || len(found) != 0 {
		t.Fatalf("Bob's week = %v, %v; want no events", found, err)
	}
	if got, err := svc.Get(ctx, alice, e.ID); err != nil || got.Title != "Standup" {
		t.Fatalf("the owner's event = %+v, %v", got, err)
	}
	for i, got := range store.calls {
		if got != alice && got != bob {
			t.Fatalf("store call %d used an unexpected owner %s", i, got)
		}
	}
}

// A link to somebody else's task or goal is answered as "no such event" --
// never as a validation error naming the field, which would confirm the id
// exists, and never by storing the link.
func TestALinkToAnotherUsersRecordIsNotFound(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()
	bobsTask, bobsGoal := store.seedTask(bob), store.seedGoal(bob)
	aliceTask := store.seedTask(alice)

	for name, in := range map[string]CreateInput{
		"another user's task": {Title: "x", StartTime: nine, EndTime: tenAM, RelatedTaskID: &bobsTask},
		"another user's goal": {Title: "x", StartTime: nine, EndTime: tenAM, RelatedGoalID: &bobsGoal},
		"a task nobody owns":  {Title: "x", StartTime: nine, EndTime: tenAM, RelatedTaskID: idOf(uuid.New())},
	} {
		if _, err := svc.Create(ctx, alice, in); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: Create = %v, want ErrNotFound", name, err)
		}
	}
	if n := len(store.byUser[alice]); n != 0 {
		t.Fatalf("%d events were created behind a rejected link", n)
	}

	// Her own task links fine, and the patch path checks the same thing.
	own, err := svc.Create(ctx, alice, CreateInput{
		Title: "Deep work", StartTime: nine, EndTime: tenAM, RelatedTaskID: &aliceTask,
	})
	if err != nil {
		t.Fatalf("linking to her own task: %v", err)
	}
	if _, err := svc.Update(ctx, alice, own.ID, UpdateInput{RelatedTaskID: optional.Of(bobsTask)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("patching in another user's task = %v, want ErrNotFound", err)
	}
	// Clearing the link needs no ownership check: null belongs to nobody.
	cleared, err := svc.Update(ctx, alice, own.ID, UpdateInput{RelatedTaskID: optional.Null[uuid.UUID]()})
	if err != nil || cleared.RelatedTaskID != nil {
		t.Fatalf("clearing the link = %+v, %v", cleared, err)
	}
}

// Moving one end of an event is checked against the stored row: a patch that
// mentions only the start can still invert the interval.
func TestMovingOneEndIsCheckedAgainstTheStoredEvent(t *testing.T) {
	svc := NewService(newFakeStore())
	ctx := context.Background()
	alice := uuid.New()

	e, err := svc.Create(ctx, alice, meeting())
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Update(ctx, alice, e.ID, UpdateInput{StartTime: optional.Of(day.Add(18 * time.Hour))})
	var verrs validate.Errors
	if !errors.As(err, &verrs) || verrs[0].Field != "end_time" {
		t.Fatalf("moving the start past the end = %v, want a field error on end_time", err)
	}
	unchanged, _ := svc.Get(ctx, alice, e.ID)
	if !unchanged.StartTime.Equal(nine) {
		t.Fatalf("the rejected patch moved the event to %v", unchanged.StartTime)
	}

	// Moving both ends together is fine, and so is moving one end the right
	// way.
	moved, err := svc.Update(ctx, alice, e.ID, UpdateInput{
		StartTime: optional.Of(day.Add(18 * time.Hour)), EndTime: optional.Of(day.Add(19 * time.Hour)),
	})
	if err != nil || !moved.EndTime.Equal(day.Add(19*time.Hour)) {
		t.Fatalf("moving both ends = %+v, %v", moved, err)
	}
	if _, err := svc.Update(ctx, alice, e.ID, UpdateInput{EndTime: optional.Of(day.Add(20 * time.Hour))}); err != nil {
		t.Fatalf("extending the event: %v", err)
	}
}

func TestUpdateAppliesOnlyMentionedFields(t *testing.T) {
	svc := NewService(newFakeStore())
	ctx := context.Background()
	alice := uuid.New()

	e, err := svc.Create(ctx, alice, meeting())
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(ctx, alice, e.ID, UpdateInput{Title: optional.Of("Retro")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Retro" {
		t.Fatalf("title = %q, want Retro", updated.Title)
	}
	if !updated.StartTime.Equal(nine) || !updated.EndTime.Equal(tenAM) {
		t.Fatalf("an unmentioned field was overwritten: %+v", updated)
	}
}

// The list is a range query and nothing else: a caller that names no window
// gets a validation error rather than the whole table.
func TestListRequiresAWindow(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()

	for name, f := range map[string]Filter{
		"no window at all": {},
		"no end":           {Start: day},
		"no start":         {End: day},
		"backwards":        {Start: day.AddDate(0, 0, 7), End: day},
		"empty":            {Start: day, End: day},
		"too wide":         {Start: day, End: day.Add(MaxWindow + time.Hour)},
	} {
		var verrs validate.Errors
		if _, err := svc.List(ctx, uuid.New(), f); !errors.As(err, &verrs) {
			t.Fatalf("%s: List = %v, want a validation error", name, err)
		}
	}
	if len(store.filters) != 0 {
		t.Fatalf("a rejected window reached the store: %+v", store.filters)
	}
}

func idOf(id uuid.UUID) *uuid.UUID { return &id }
