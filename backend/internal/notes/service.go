package notes

import (
	"context"

	"github.com/google/uuid"
)

// Store is the slice of the repository the service needs. Every method takes
// the owner id, so a note cannot be reached without saying whose it is.
type Store interface {
	Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Note, error)
	ByID(ctx context.Context, userID, id uuid.UUID) (Note, error)
	List(ctx context.Context, userID uuid.UUID, f Filter) ([]Note, error)
	Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Note, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
}

// Service holds the note use cases. It is transport agnostic.
type Service struct {
	store Store
}

func NewService(store Store) *Service { return &Service{store: store} }

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Note, error) {
	in, err := ValidateCreate(in)
	if err != nil {
		return Note{}, err
	}
	return s.store.Create(ctx, userID, in)
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Note, error) {
	return s.store.ByID(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Note, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.List(ctx, userID, f)
}

func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in UpdateInput) (Note, error) {
	p, err := ValidatePatch(in)
	if err != nil {
		return Note{}, err
	}
	return s.store.Update(ctx, userID, id, p)
}

func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.Delete(ctx, userID, id)
}
