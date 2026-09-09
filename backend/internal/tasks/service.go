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

// Service holds the task use cases. It is transport agnostic.
type Service struct {
	store Store
}

func NewService(store Store) *Service { return &Service{store: store} }

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
	return s.store.Create(ctx, userID, in)
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
	return s.store.Update(ctx, userID, id, p)
}

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
