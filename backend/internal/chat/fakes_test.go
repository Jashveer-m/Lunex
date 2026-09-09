package chat

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// The fakes key everything by (owner, id), the same shape the SQL has, so an
// orchestrator that forgot to pass the caller's id shows up here as a miss
// rather than as data belonging to nobody.

type fakeStore struct {
	mu       sync.Mutex
	byUser   map[uuid.UUID]map[uuid.UUID]Conversation
	messages map[uuid.UUID][]Message
	// callers records the user id every method was given.
	callers []uuid.UUID
	// appendErr fails the persistence step, after generation succeeded.
	appendErr error
	clock     time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byUser:   map[uuid.UUID]map[uuid.UUID]Conversation{},
		messages: map[uuid.UUID][]Message{},
		clock:    time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
	}
}

// seed creates a conversation directly, the way a previous request would have.
func (f *fakeStore) seed(userID uuid.UUID) Conversation {
	c, _ := f.CreateConversation(context.Background(), userID, DefaultTitle)
	return c
}

func (f *fakeStore) CreateConversation(_ context.Context, userID uuid.UUID, title string) (Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if f.byUser[userID] == nil {
		f.byUser[userID] = map[uuid.UUID]Conversation{}
	}
	c := Conversation{ID: uuid.New(), UserID: userID, Title: title, CreatedAt: f.clock, UpdatedAt: f.clock}
	f.byUser[userID][c.ID] = c
	return c, nil
}

func (f *fakeStore) ConversationByID(_ context.Context, userID, id uuid.UUID) (Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	c, ok := f.byUser[userID][id]
	if !ok {
		return Conversation{}, ErrNotFound
	}
	c.MessageCount = len(f.messages[id])
	return c, nil
}

func (f *fakeStore) ListConversations(_ context.Context, userID uuid.UUID, _ Filter) ([]Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	out := []Conversation{}
	for _, c := range f.byUser[userID] {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeStore) DeleteConversation(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if _, ok := f.byUser[userID][id]; !ok {
		return ErrNotFound
	}
	delete(f.byUser[userID], id)
	delete(f.messages, id)
	return nil
}

func (f *fakeStore) Messages(_ context.Context, userID, convID uuid.UUID, limit int) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if _, ok := f.byUser[userID][convID]; !ok {
		return nil, nil
	}
	all := f.messages[convID]
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return append([]Message(nil), all...), nil
}

func (f *fakeStore) AppendTurn(_ context.Context, userID, convID uuid.UUID, msgs []NewMessage, titleIfDefault string) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	c, ok := f.byUser[userID][convID]
	if !ok {
		return nil, ErrNotFound
	}
	if c.Title == DefaultTitle && titleIfDefault != "" {
		c.Title = titleIfDefault
	}
	c.UpdatedAt = f.clock
	f.byUser[userID][convID] = c

	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		f.clock = f.clock.Add(time.Millisecond)
		written := Message{
			ID: uuid.New(), ConversationID: convID, Role: m.Role,
			Content: m.Content, Sources: m.Sources, CreatedAt: f.clock,
		}
		f.messages[convID] = append(f.messages[convID], written)
		out = append(out, written)
	}
	return out, nil
}

func (f *fakeStore) stored(convID uuid.UUID) []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.messages[convID]...)
}

func (f *fakeStore) callersSeen() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.callers...)
}

// --- retrieval fakes --------------------------------------------------------

type fakeDocs struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]documents.SearchResult
	err     error
	callers []uuid.UUID
	queries []documents.SearchQuery
}

func (f *fakeDocs) Search(_ context.Context, userID uuid.UUID, q documents.SearchQuery) ([]documents.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[userID], nil
}

type fakeTasks struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]tasks.Task
	err     error
	callers []uuid.UUID
	filters []tasks.Filter
}

func (f *fakeTasks) List(_ context.Context, userID uuid.UUID, filter tasks.Filter) ([]tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, f.err
	}
	out := []tasks.Task{}
	for _, t := range f.byUser[userID] {
		if filter.Status == "" || t.Status == filter.Status {
			out = append(out, t)
		}
	}
	return out, nil
}

type fakeGoals struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]goals.Goal
	err     error
	callers []uuid.UUID
	filters []goals.Filter
}

func (f *fakeGoals) List(_ context.Context, userID uuid.UUID, filter goals.Filter) ([]goals.Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, f.err
	}
	out := []goals.Goal{}
	for _, g := range f.byUser[userID] {
		if filter.Status == "" || g.Status == filter.Status {
			out = append(out, g)
		}
	}
	return out, nil
}

type fakeNotes struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]notes.Note
	err     error
	callers []uuid.UUID
	filters []notes.Filter
}

func (f *fakeNotes) List(_ context.Context, userID uuid.UUID, filter notes.Filter) ([]notes.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[userID], nil
}
