package calendar

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// Store is the slice of the repository the service needs. As everywhere else,
// every method takes the owner id: there is no way to reach an event without
// saying whose it is, which is what makes the isolation property structural
// rather than a rule each call site has to remember.
type Store interface {
	Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Event, error)
	ByID(ctx context.Context, userID, id uuid.UUID) (Event, error)
	List(ctx context.Context, userID uuid.UUID, f Filter) ([]Event, error)
	Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Event, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
	// TaskExists and GoalExists report whether the caller owns the row a
	// related id points at. Both are owner-scoped, so another user's task is
	// indistinguishable from one that does not exist.
	TaskExists(ctx context.Context, userID, id uuid.UUID) (bool, error)
	GoalExists(ctx context.Context, userID, id uuid.UUID) (bool, error)
}

// NodeSyncer mirrors an event into the personal knowledge graph. Phase 6's
// internal/graph implements it; nil means no graph is wired and this module
// behaves as a plain CRUD resource.
//
// The interface is declared here rather than imported, the same way every other
// resource module declares it: this package depends on "something that records
// an event in the graph", not on the graph package.
//
// SyncNode returns no error on purpose, and there is deliberately no delete
// counterpart -- the node goes with the row through the trigger migration
// 000008 puts on this table. See notes.NodeSyncer for the argument.
type NodeSyncer interface {
	SyncNode(ctx context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, label string)
}

// graphRefTable is what this module's nodes record in `ref_table`. It matches
// the allow-list in migration 000008 and graph's own map; a mismatch is a write
// error, and the round trip is pinned in internal/db's Phase 8 tests.
const graphRefTable = "calendar_events"

// Option configures a Service at construction. The graph is genuinely
// optional: this module's own tests build a service without one.
type Option func(*Service)

// WithNodeSync wires the knowledge graph into the write path.
func WithNodeSync(g NodeSyncer) Option { return func(s *Service) { s.graph = g } }

// Service holds the calendar use cases. It is transport agnostic.
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

// sync mirrors an event into the graph, on create and on update alike -- on
// update because the node's label is the event's title, and a node still
// carrying the old title is one the chat mention scan matches on a name the
// user has stopped using. It also makes the update path the repair for a
// create whose sync failed.
func (s *Service) sync(ctx context.Context, userID uuid.UUID, e Event) {
	if s.graph == nil {
		return
	}
	s.graph.SyncNode(ctx, userID, graphRefTable, e.ID, e.Title)
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Event, error) {
	in, err := ValidateCreate(in)
	if err != nil {
		return Event{}, err
	}
	if err := s.requireOwnedLinks(ctx, userID, in.RelatedTaskID, in.RelatedGoalID); err != nil {
		return Event{}, err
	}
	created, err := s.store.Create(ctx, userID, in)
	if err != nil {
		return Event{}, linkRace(err)
	}
	s.sync(ctx, userID, created)
	return created, nil
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Event, error) {
	return s.store.ByID(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Event, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.List(ctx, userID, f)
}

// Update applies a patch.
//
// A patch that moves one end of the event is checked against the stored row,
// not against itself: `{"start_time": "18:00"}` on a 09:00-10:00 meeting
// inverts the interval without ever mentioning the end, and the answer to that
// should be a field error naming end_time rather than a constraint violation
// surfacing as a 500.
func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in UpdateInput) (Event, error) {
	p, err := ValidatePatch(in)
	if err != nil {
		return Event{}, err
	}
	if err := s.requireOwnedLinks(ctx, userID, patchLink(p.RelatedTaskID), patchLink(p.RelatedGoalID)); err != nil {
		return Event{}, err
	}
	if p.MovesTheInterval() {
		// The read is the price of a field-level error. It is one indexed
		// lookup, and it also answers "is this event even yours" before the
		// UPDATE -- which the UPDATE would answer anyway, as ErrNotFound.
		current, err := s.store.ByID(ctx, userID, id)
		if err != nil {
			return Event{}, err
		}
		start, end := current.StartTime, current.EndTime
		if p.StartTime != nil {
			start = *p.StartTime
		}
		if p.EndTime != nil {
			end = *p.EndTime
		}
		if e := interval(start, end); e != nil {
			return Event{}, validate.Errors{*e}
		}
	}
	updated, err := s.store.Update(ctx, userID, id, p)
	if err != nil {
		return Event{}, linkRace(err)
	}
	s.sync(ctx, userID, updated)
	return updated, nil
}

// linkRace turns the foreign-key violation a concurrent delete produces into
// the answer a missing link gets everywhere else.
//
// requireOwnedLinks has already checked that the caller owns the task or goal,
// but nothing holds it there: it can be deleted between the check and the
// write. The row it named is gone either way, so "no such event" is the honest
// answer, and it keeps a race from surfacing as a 500.
func linkRace(err error) error {
	if IsForeignKeyViolation(err) {
		return ErrNotFound
	}
	return err
}

// Delete makes no graph call: the node goes with the event in the same
// statement, through the trigger migration 000008 puts on this table.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.Delete(ctx, userID, id)
}

// requireOwnedLinks checks the task and goal an event points at.
//
// A link to a row the caller does not own is reported as ErrNotFound -- a 404
// -- rather than as a validation error naming the field: the two answers
// differ, and only the first keeps another user's ids unconfirmable. It is the
// same rule tasks.Service applies to parent_task_id.
func (s *Service) requireOwnedLinks(ctx context.Context, userID uuid.UUID, taskID, goalID *uuid.UUID) error {
	if taskID != nil {
		if err := s.requireOwned(ctx, userID, *taskID, "related_task_id", s.store.TaskExists); err != nil {
			return err
		}
	}
	if goalID != nil {
		if err := s.requireOwned(ctx, userID, *goalID, "related_goal_id", s.store.GoalExists); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) requireOwned(ctx context.Context, userID, id uuid.UUID, field string,
	exists func(context.Context, uuid.UUID, uuid.UUID) (bool, error),
) error {
	if id == uuid.Nil {
		return validate.Errors{{Field: field, Message: "must be an id"}}
	}
	ok, err := exists(ctx, userID, id)
	if err != nil {
		return fmt.Errorf("check %s: %w", field, err)
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// patchLink is the id a patch sets a link to, or nil when the patch leaves the
// link alone or clears it. Clearing needs no ownership check: null belongs to
// nobody.
func patchLink(f optional.Field[uuid.UUID]) *uuid.UUID {
	if v, ok := f.Get(); ok {
		return &v
	}
	return nil
}
