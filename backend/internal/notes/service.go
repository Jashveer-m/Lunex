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

// NodeSyncer mirrors a note into the personal knowledge graph. Phase 6's
// internal/graph implements it; nil means no graph is wired and this module
// behaves exactly as it did before Phase 6.
//
// The interface is declared here rather than imported, the same way
// internal/chat declares the searchers it consumes: this package depends on
// "something that records a note in the graph", not on the graph package.
//
// SyncNode returns no error on purpose. It runs after the note is written,
// so there is nothing useful to do with a failure -- failing the request would
// be a lie about a row that exists. The graph logs its own failures; see
// graph.Service.SyncNode.
//
// There is deliberately no delete counterpart: removing the node when the
// note goes is done by a trigger in migration 000006, so it fires on every
// path a row can leave by, not only the ones this service is on.
type NodeSyncer interface {
	SyncNode(ctx context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, label string)
}

// graphRefTable is what this module's nodes record in `ref_table`. It matches
// the allow-list in migration 000006 and graph's own map; the round trip is
// pinned in internal/db's Phase 6 tests.
const graphRefTable = "notes"

// Option configures a Service at construction. It is variadic rather than a
// parameter because the graph is genuinely optional: every existing caller,
// including this module's own tests, builds a service without one.
type Option func(*Service)

// WithNodeSync wires the knowledge graph into the write path.
func WithNodeSync(g NodeSyncer) Option { return func(s *Service) { s.graph = g } }

// Service holds the note use cases. It is transport agnostic.
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

// sync mirrors a note into the graph, on create and on update alike -- on
// update because the node's label is the note's title, and a node still
// carrying the old title is one the chat mention scan matches on a name the
// user has stopped using. It also makes the update path the repair for a
// create whose sync failed.
func (s *Service) sync(ctx context.Context, userID uuid.UUID, n Note) {
	if s.graph == nil {
		return
	}
	s.graph.SyncNode(ctx, userID, graphRefTable, n.ID, n.Title)
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Note, error) {
	in, err := ValidateCreate(in)
	if err != nil {
		return Note{}, err
	}
	created, err := s.store.Create(ctx, userID, in)
	if err != nil {
		return Note{}, err
	}
	s.sync(ctx, userID, created)
	return created, nil
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
	updated, err := s.store.Update(ctx, userID, id, p)
	if err != nil {
		return Note{}, err
	}
	s.sync(ctx, userID, updated)
	return updated, nil
}

// Delete makes no graph call: the node goes with the note in the same
// statement, through the trigger migration 000006 puts on this table.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.Delete(ctx, userID, id)
}
