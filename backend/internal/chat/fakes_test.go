package chat

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/memories"
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
	// actions is what AppendTurn recorded, in order, across every user.
	actions []actions.Action
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

func (f *fakeStore) AppendTurn(_ context.Context, userID, convID uuid.UUID, msgs []NewMessage, titleIfDefault string, acts []actions.NewAction) ([]Message, []actions.Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if f.appendErr != nil {
		return nil, nil, f.appendErr
	}
	c, ok := f.byUser[userID][convID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	// The same state-machine check the real Insert makes, before anything is
	// written: an orchestrator that tried to record a write as executed fails
	// here exactly as it would against Postgres.
	for _, n := range acts {
		if err := n.Validate(); err != nil {
			return nil, nil, err
		}
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
	var recorded []actions.Action
	for _, n := range acts {
		f.clock = f.clock.Add(time.Millisecond)
		conv := convID
		a := actions.Action{
			ID: uuid.New(), UserID: userID, ConversationID: &conv,
			ToolName: n.Call.Tool, Input: n.Call.Input, Permission: n.Call.Permission,
			Status: n.Status, CreatedAt: f.clock, UpdatedAt: f.clock,
		}
		if n.Result != nil {
			raw, err := json.Marshal(n.Result)
			if err != nil {
				return nil, nil, err
			}
			a.Result = raw
		}
		f.actions = append(f.actions, a)
		recorded = append(recorded, a)
	}
	return out, recorded, nil
}

// recordedActions is every action the store has been asked to record.
func (f *fakeStore) recordedActions() []actions.Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]actions.Action(nil), f.actions...)
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

// --- Phase 5 fakes ----------------------------------------------------------

type fakeMemories struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]memories.SearchResult
	err     error
	callers []uuid.UUID
	queries []memories.SearchQuery
}

func (f *fakeMemories) Search(_ context.Context, userID uuid.UUID, q memories.SearchQuery) ([]memories.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	// The floor is applied here as well as recorded, so a test can seed a weak
	// match and see it dropped rather than only assert on the query.
	out := []memories.SearchResult{}
	for _, m := range f.byUser[userID] {
		if m.Similarity < q.MinSimilarity {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// fakeExtractor records what the orchestrator handed it after a turn.
type fakeExtractor struct {
	mu    sync.Mutex
	calls []extraction
	err   error
	// stored is what the extractor claims to have written.
	stored []memories.Memory
}

type extraction struct {
	userID    uuid.UUID
	convID    uuid.UUID
	question  string
	answer    string
	ctxWasSet bool
	// What the turn told the extractor not to read as fact.
	unconfirmed []string
	retrieved   []string
	anchor      *graph.Anchor
}

func (f *fakeExtractor) Extract(ctx context.Context, userID, convID uuid.UUID, turn memories.Turn) ([]memories.Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, extraction{
		userID: userID, convID: convID, question: turn.UserMessage, answer: turn.AssistantMessage,
		ctxWasSet: ctx != nil, unconfirmed: turn.Unconfirmed, retrieved: turn.Retrieved,
	})
	if f.err != nil {
		return nil, f.err
	}
	return f.stored, nil
}

func (f *fakeExtractor) seen() []extraction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]extraction(nil), f.calls...)
}

// --- Phase 6 fakes ----------------------------------------------------------

type fakeGraph struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]graph.Neighborhood
	err     error
	callers []uuid.UUID
	// queries records the text the orchestrator handed the lookup, which is
	// what proves the *question* is what gets matched against node labels.
	queries []string
	limits  []int
}

func (f *fakeGraph) Mentioned(_ context.Context, userID uuid.UUID, text string, limit int) ([]graph.Neighborhood, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	f.queries = append(f.queries, text)
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return nil, f.err
	}
	// The mention test is applied here as well as recorded, so a test can seed
	// a node the question does not name and see it left out rather than only
	// assert on the arguments.
	out := []graph.Neighborhood{}
	for _, n := range f.byUser[userID] {
		if graph.Mentions(text, n.Node.Label) {
			out = append(out, n)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// fakeLinker records what the orchestrator handed the relationship extractor
// after a turn.
type fakeLinker struct {
	mu    sync.Mutex
	calls []extraction
	err   error
	// stored is what the extractor claims to have written.
	stored []graph.Edge
}

func (f *fakeLinker) Extract(ctx context.Context, userID, convID uuid.UUID, turn graph.Turn) ([]graph.Edge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, extraction{
		userID: userID, convID: convID, question: turn.UserMessage, answer: turn.AssistantMessage,
		ctxWasSet: ctx != nil, unconfirmed: turn.Unconfirmed, anchor: turn.Anchor,
	})
	if f.err != nil {
		return nil, f.err
	}
	return f.stored, nil
}

func (f *fakeLinker) seen() []extraction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]extraction(nil), f.calls...)
}
