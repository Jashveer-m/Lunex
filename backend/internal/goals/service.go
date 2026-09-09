package goals

import (
	"context"
	"strconv"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

// Store is the slice of the repository the service needs. Every method takes
// the owner id, so there is no way to reach a goal without saying whose it is.
type Store interface {
	Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Goal, error)
	ByID(ctx context.Context, userID, id uuid.UUID) (Goal, error)
	List(ctx context.Context, userID uuid.UUID, f Filter) ([]Goal, error)
	Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Goal, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
	CountMilestones(ctx context.Context, userID, goalID uuid.UUID) (int, error)
	AddMilestone(ctx context.Context, userID, goalID uuid.UUID, in MilestoneInput) (Milestone, error)
	UpdateMilestone(ctx context.Context, userID, goalID, milestoneID uuid.UUID, p MilestonePatch) (Milestone, error)
}

// Service holds the goal use cases. It is transport agnostic.
type Service struct {
	store Store
}

func NewService(store Store) *Service { return &Service{store: store} }

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Goal, error) {
	in, err := ValidateCreate(in)
	if err != nil {
		return Goal{}, err
	}
	return s.store.Create(ctx, userID, in)
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Goal, error) {
	return s.store.ByID(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Goal, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.List(ctx, userID, f)
}

func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in UpdateInput) (Goal, error) {
	p, err := ValidatePatch(in)
	if err != nil {
		return Goal{}, err
	}
	return s.store.Update(ctx, userID, id, p)
}

func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.Delete(ctx, userID, id)
}

// AddMilestone appends a milestone to one of the caller's goals. The count
// check runs first and doubles as the ownership check: a goal that is not the
// caller's returns ErrNotFound before anything is written.
func (s *Service) AddMilestone(ctx context.Context, userID, goalID uuid.UUID, in MilestoneInput) (Milestone, error) {
	in, err := ValidateMilestone(in)
	if err != nil {
		return Milestone{}, err
	}
	n, err := s.store.CountMilestones(ctx, userID, goalID)
	if err != nil {
		return Milestone{}, err
	}
	if n >= MaxMilestones {
		return Milestone{}, validate.Errors{{
			Field:   "milestones",
			Message: "a goal may have at most " + strconv.Itoa(MaxMilestones) + " milestones",
		}}
	}
	return s.store.AddMilestone(ctx, userID, goalID, in)
}

func (s *Service) UpdateMilestone(ctx context.Context, userID, goalID, milestoneID uuid.UUID, in MilestoneUpdateInput) (Milestone, error) {
	p, err := ValidateMilestonePatch(in)
	if err != nil {
		return Milestone{}, err
	}
	return s.store.UpdateMilestone(ctx, userID, goalID, milestoneID, p)
}
