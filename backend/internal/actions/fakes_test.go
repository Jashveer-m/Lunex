package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// memStore is the actions table in memory. It keeps the repository's contract
// where it matters: owner-scoped everywhere, Approve atomic and single-use,
// Finish only from approved.
type memStore struct {
	mu    sync.Mutex
	rows  map[uuid.UUID]*Action
	clock time.Time
	// finishErr fails Finish, after the tool has run.
	finishErr error
}

func newMemStore() *memStore {
	return &memStore{rows: map[uuid.UUID]*Action{}, clock: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
}

var _ Store = (*memStore)(nil)
var _ tools.Ledger = (*memStore)(nil)

func (m *memStore) record(owner uuid.UUID, conv *uuid.UUID, n NewAction) Action {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := n.Validate(); err != nil {
		panic(err)
	}
	m.clock = m.clock.Add(time.Second)
	a := &Action{
		ID: uuid.New(), UserID: owner, ConversationID: conv, ToolName: n.Call.Tool,
		Input: n.Call.Input, Permission: n.Call.Permission, Status: n.Status,
		CreatedAt: m.clock, UpdatedAt: m.clock,
	}
	if n.Result != nil {
		a.Result, _ = json.Marshal(n.Result)
	}
	m.rows[a.ID] = a
	return *a
}

func (m *memStore) get(id uuid.UUID) Action {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.rows[id]
}

func (m *memStore) owned(userID, id uuid.UUID) (*Action, error) {
	a, ok := m.rows[id]
	if !ok || a.UserID != userID {
		return nil, ErrNotFound
	}
	return a, nil
}

func (m *memStore) ByID(_ context.Context, userID, id uuid.UUID) (Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.owned(userID, id)
	if err != nil {
		return Action{}, err
	}
	return *a, nil
}

func (m *memStore) List(_ context.Context, userID uuid.UUID, f Filter) ([]Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Action{}
	for _, a := range m.rows {
		switch {
		case a.UserID != userID:
		case f.Status != "" && a.Status != f.Status:
		case f.Permission != "" && string(a.Permission) != f.Permission:
		case f.ConversationID != nil && (a.ConversationID == nil || *a.ConversationID != *f.ConversationID):
		default:
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *memStore) Approve(_ context.Context, userID, id uuid.UUID) (tools.Approved, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.owned(userID, id)
	if err != nil {
		return tools.Approved{}, err
	}
	if a.Status != StatusProposed || a.Permission != tools.Write {
		return tools.Approved{}, fmt.Errorf("%w: it is %s", ErrNotPending, a.Status)
	}
	a.Status = StatusApproved
	return tools.Approved{ActionID: a.ID, Tool: a.ToolName, Input: a.Input}, nil
}

func (m *memStore) Reject(_ context.Context, userID, id uuid.UUID) (Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.owned(userID, id)
	if err != nil {
		return Action{}, err
	}
	if a.Status != StatusProposed {
		return Action{}, fmt.Errorf("%w: it is %s", ErrNotPending, a.Status)
	}
	a.Status = StatusRejected
	return *a, nil
}

func (m *memStore) Finish(_ context.Context, userID, id uuid.UUID, o Outcome) (Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finishErr != nil {
		return Action{}, m.finishErr
	}
	a, err := m.owned(userID, id)
	if err != nil {
		return Action{}, err
	}
	if a.Status != StatusApproved {
		return Action{}, fmt.Errorf("%w: it is %s", ErrNotPending, a.Status)
	}
	if o.Failed != "" {
		msg := o.Failed
		a.Status, a.ErrorMessage = StatusFailed, &msg
	} else {
		a.Status = StatusExecuted
		a.Result, _ = json.Marshal(o.Result)
	}
	return *a, nil
}

func (m *memStore) PendingDuplicate(_ context.Context, userID, convID uuid.UUID, call tools.Call) (Action, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.rows {
		if a.UserID == userID && a.ConversationID != nil && *a.ConversationID == convID &&
			a.Status == StatusProposed && a.ToolName == call.Tool && string(a.Input) == string(call.Input) {
			return *a, true, nil
		}
	}
	return Action{}, false, nil
}

// countingTasks is a task service that counts the writes that reach it.
type countingTasks struct {
	mu      sync.Mutex
	creates []tasks.CreateInput
	err     error
	// ctxErr records whether the context a write ran on was already done.
	ctxErr error
}

func (c *countingTasks) List(context.Context, uuid.UUID, tasks.Filter) ([]tasks.Task, error) {
	return nil, nil
}
func (c *countingTasks) Get(context.Context, uuid.UUID, uuid.UUID) (tasks.Task, error) {
	return tasks.Task{}, tasks.ErrNotFound
}
func (c *countingTasks) Update(context.Context, uuid.UUID, uuid.UUID, tasks.UpdateInput) (tasks.Task, error) {
	return tasks.Task{}, errors.New("not in these tests")
}
func (c *countingTasks) Create(ctx context.Context, userID uuid.UUID, in tasks.CreateInput) (tasks.Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creates = append(c.creates, in)
	c.ctxErr = ctx.Err()
	if c.err != nil {
		return tasks.Task{}, c.err
	}
	return tasks.Task{ID: uuid.New(), UserID: userID, Title: in.Title, Status: "pending", Priority: "medium"}, nil
}

func (c *countingTasks) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.creates)
}

// engine is the service over the in-memory store, with the real registry --
// ledger and all -- in front of a counting task service.
type engine struct {
	svc   *Service
	store *memStore
	tasks *countingTasks
	reg   *tools.Registry
	user  uuid.UUID
}

func newEngine() *engine {
	e := &engine{store: newMemStore(), tasks: &countingTasks{}, user: uuid.New()}
	reg, err := tools.NewRegistry(e.store, tools.Standard(tools.Services{Tasks: e.tasks})...)
	if err != nil {
		panic(err)
	}
	e.reg = reg
	e.svc = NewService(e.store, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return e
}

// propose records a create_task proposal the way the chat turn would.
func (e *engine) propose(owner uuid.UUID, title string) Action {
	call, err := e.reg.Prepare(context.Background(), owner, tools.CreateTask, tools.Args{"title": title})
	if err != nil {
		panic(err)
	}
	conv := uuid.New()
	return e.store.record(owner, &conv, Proposal(call))
}
