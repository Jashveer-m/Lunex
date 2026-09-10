package tasks

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

// Store is the slice of the repository the service needs. Note that every
// method takes the owner id: there is no way to reach a task without saying
// whose it is, which is what makes the isolation property structural rather
// than a rule each call site has to remember.
type Store interface {
	Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Task, error)
	ByID(ctx context.Context, userID, id uuid.UUID) (Task, error)
	List(ctx context.Context, userID uuid.UUID, f Filter) ([]Task, error)
	Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Task, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
	Exists(ctx context.Context, userID, id uuid.UUID) (bool, error)
	AddDependency(ctx context.Context, userID, taskID, dependsOn uuid.UUID) error
}

// NodeSyncer mirrors a task into the personal knowledge graph. Phase 6's
// internal/graph implements it; nil means no graph is wired and this module
// behaves exactly as it did in Phase 2.
//
// The interface is declared here rather than imported, the same way
// internal/chat declares the searchers it consumes: this package depends on
// "something that records a task in the graph", not on the graph package.
//
// SyncNode returns no error on purpose. It runs after the task is written, so
// there is nothing useful to do with a failure -- failing the request would be
// a lie about a row that exists, and undoing the write would throw away what
// the user asked for to protect an index derived from it. The graph logs its
// own failures; see graph.Service.SyncNode.
//
// There is deliberately no delete counterpart. Removing the node when the task
// goes is done by a trigger in migration 000006, not from here: a task can be
// deleted by a route this service never sees -- a parent task cascading to its
// subtasks, a user being deleted -- and a rule that only fires on the paths
// somebody remembered to wire is a rule that leaves orphans. See
// docs/decisions.md.
type NodeSyncer interface {
	SyncNode(ctx context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, label string)
}

// graphRefTable is what a task's node records in `ref_table`. It matches the
// allow-list in migration 000006 and graph's own map; a mismatch is a write
// error, and the round trip is pinned in internal/db's Phase 6 tests.
const graphRefTable = "tasks"

// Option configures a Service at construction. It is variadic rather than a
// parameter because the graph is genuinely optional: every existing caller,
// including the Phase 2 tests, builds a service without one.
type Option func(*Service)

// WithNodeSync wires the knowledge graph into the write path.
func WithNodeSync(g NodeSyncer) Option { return func(s *Service) { s.graph = g } }

// Service holds the task use cases. It is transport agnostic.
type Service struct {
	store Store
	graph NodeSyncer
}

func NewService(store Store, opts ...Option) *Service {
	s := &Service{store: store}
	for _, o := range opts {
		o(s)
	}
	return s
}

// sync mirrors a task into the graph, on create and on update alike.
//
// On update too, because the node's label is the task's title: a node still
// carrying the old title is one the chat mention scan matches on a name the
// user has stopped using, and shows to the model as the current one. It also
// makes the update path the repair for a create whose sync failed.
func (s *Service) sync(ctx context.Context, userID uuid.UUID, t Task) {
	if s.graph == nil {
		return
	}
	s.graph.SyncNode(ctx, userID, graphRefTable, t.ID, t.Title)
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Task, error) {
	in, err := ValidateCreate(in)
	if err != nil {
		return Task{}, err
	}
	if in.ParentTaskID != nil {
		if err := s.requireOwnedTask(ctx, userID, *in.ParentTaskID, "parent_task_id"); err != nil {
			return Task{}, err
		}
	}
	created, err := s.store.Create(ctx, userID, in)
	if err != nil {
		return Task{}, err
	}
	s.sync(ctx, userID, created)
	return created, nil
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Task, error) {
	return s.store.ByID(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Task, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.List(ctx, userID, f)
}

func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in UpdateInput) (Task, error) {
	p, err := ValidatePatch(in)
	if err != nil {
		return Task{}, err
	}
	if parent, ok := p.ParentTaskID.Get(); ok {
		// A task that is its own parent is a cycle of length one, and the
		// tasks table has no CHECK to stop it.
		if parent == id {
			return Task{}, validate.Errors{{Field: "parent_task_id", Message: "must not be the task itself"}}
		}
		if err := s.requireOwnedTask(ctx, userID, parent, "parent_task_id"); err != nil {
			return Task{}, err
		}
	}
	updated, err := s.store.Update(ctx, userID, id, p)
	if err != nil {
		return Task{}, err
	}
	s.sync(ctx, userID, updated)
	return updated, nil
}

// Delete makes no graph call: the node goes with the task in the same
// statement, through the trigger migration 000006 puts on this table. That
// covers the subtask cascade too, which this method never sees.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.Delete(ctx, userID, id)
}

// AddDependency records that a task waits on another, then returns the task
// with its refreshed dependency list.
func (s *Service) AddDependency(ctx context.Context, userID, taskID, dependsOn uuid.UUID) (Task, error) {
	if dependsOn == uuid.Nil {
		return Task{}, validate.Errors{{Field: "depends_on_task_id", Message: "is required"}}
	}
	if taskID == dependsOn {
		// The table's CHECK would reject this too; catching it here turns a
		// constraint violation into a field-level 400.
		return Task{}, validate.Errors{{Field: "depends_on_task_id", Message: "must not be the task itself"}}
	}
	if err := s.store.AddDependency(ctx, userID, taskID, dependsOn); err != nil {
		return Task{}, err
	}
	return s.store.ByID(ctx, userID, taskID)
}

// requireOwnedTask reports a task the caller does not own as ErrNotFound, not
// as a validation error naming the field: the two answers differ, and only the
// first keeps another user's ids unconfirmable.
func (s *Service) requireOwnedTask(ctx context.Context, userID, id uuid.UUID, field string) error {
	if id == uuid.Nil {
		return validate.Errors{{Field: field, Message: "must be a task id"}}
	}
	ok, err := s.store.Exists(ctx, userID, id)
	if err != nil {
		return fmt.Errorf("check %s: %w", field, err)
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}
