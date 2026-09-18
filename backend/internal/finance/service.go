package finance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// Store is the slice of the repository the service needs. As everywhere else,
// every method takes the owner id: there is no way to reach an expense without
// saying whose it is, which is what makes the isolation property structural
// rather than a rule each call site has to remember.
type Store interface {
	CreateCategory(ctx context.Context, userID uuid.UUID, name string) (Category, error)
	Categories(ctx context.Context, userID uuid.UUID) ([]Category, error)
	CategoryExists(ctx context.Context, userID, id uuid.UUID) (bool, error)
	// DocumentExists reports whether the caller owns the document a receipt
	// link points at. Owner-scoped, so another user's document is
	// indistinguishable from one that does not exist.
	DocumentExists(ctx context.Context, userID, id uuid.UUID) (bool, error)

	Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Expense, error)
	ByID(ctx context.Context, userID, id uuid.UUID) (Expense, error)
	List(ctx context.Context, userID uuid.UUID, f Filter) ([]Expense, error)
	Summarize(ctx context.Context, userID uuid.UUID, f Filter) (Summary, error)
	Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Expense, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
}

// NodeSyncer mirrors an expense into the personal knowledge graph. Phase 6's
// internal/graph implements it; nil means no graph is wired and this module
// behaves as a plain CRUD resource.
//
// The interface is declared here rather than imported, the same way every other
// resource module declares it: this package depends on "something that records
// an expense in the graph", not on the graph package.
//
// SyncNode returns no error on purpose, and there is deliberately no delete
// counterpart -- the node goes with the row through the trigger migration
// 000009 puts on this table. See notes.NodeSyncer for the argument.
type NodeSyncer interface {
	SyncNode(ctx context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, label string)
}

// graphRefTable is what this module's nodes record in `ref_table`. It matches
// the allow-list in migration 000009 and graph's own map; a mismatch is a write
// error, and the round trip is pinned in internal/db's Phase 9 tests.
const graphRefTable = "expenses"

// Option configures a Service at construction.
type Option func(*Service)

// WithNodeSync wires the knowledge graph into the write path.
func WithNodeSync(g NodeSyncer) Option { return func(s *Service) { s.graph = g } }

// Service holds the finance use cases. It is transport agnostic.
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

// --- categories ---------------------------------------------------------------

// CreateCategory adds one of the user's own categories. The five defaults
// already exist -- migration 000009 seeds them on registration -- so this is for
// the sixth.
func (s *Service) CreateCategory(ctx context.Context, userID uuid.UUID, name string) (Category, error) {
	name, err := ValidateCategoryName(name)
	if err != nil {
		return Category{}, err
	}
	c, err := s.store.CreateCategory(ctx, userID, name)
	if err != nil {
		// A name the user already has is a field error, not a 409: the client
		// asked for a category with that name to exist, and being told which
		// field is wrong is what lets it say so.
		if errors.Is(err, ErrCategoryExists) {
			return Category{}, validate.Errors{{Field: "name", Message: "is already one of your categories"}}
		}
		return Category{}, err
	}
	return c, nil
}

// Categories lists the user's categories.
func (s *Service) Categories(ctx context.Context, userID uuid.UUID) ([]Category, error) {
	return s.store.Categories(ctx, userID)
}

// CategoryByName resolves a name to one of the user's categories, matching
// case-insensitively and ignoring surrounding space.
//
// It exists for create_expense, which is handed a category in the words the
// user said rather than as an id. It never *creates* one: a tool that quietly
// added "Coffe" to the user's categories because a model wrote it that way
// would make the category list a dumping ground for typos, and the analysis
// built on it meaningless. An unmatched name is answered by
// ErrCategoryNotFound, and the caller decides what to tell the user.
func (s *Service) CategoryByName(ctx context.Context, userID uuid.UUID, name string) (Category, error) {
	name = strings.ToLower(strings.Join(strings.Fields(name), " "))
	if name == "" {
		return Category{}, ErrCategoryNotFound
	}
	all, err := s.store.Categories(ctx, userID)
	if err != nil {
		return Category{}, err
	}
	for _, c := range all {
		if strings.ToLower(c.Name) == name {
			return c, nil
		}
	}
	return Category{}, ErrCategoryNotFound
}

// --- expenses -----------------------------------------------------------------

// sync mirrors an expense into the graph, on create and on update alike -- on
// update because the node's label is derived from the expense, and a node still
// carrying the old description is one the chat mention scan matches on wording
// the user has stopped using. It also makes the update path the repair for a
// create whose sync failed.
func (s *Service) sync(ctx context.Context, userID uuid.UUID, e Expense) {
	if s.graph == nil {
		return
	}
	s.graph.SyncNode(ctx, userID, graphRefTable, e.ID, NodeLabel(e))
}

// NodeLabel is what an expense is called in the knowledge graph.
//
// An expense has no title, so the label is built: the description when there is
// one, and otherwise the category and the date. It is never the bare word
// "expense", and that is the whole reason this is a function rather than a
// field. The graph's mention scan matches a node when its label occurs in the
// user's message, so a node labelled "Expense" would fire on every question
// containing the word -- which is the opposite of "the question named something
// the user has".
func NodeLabel(e Expense) string {
	if e.Description != nil && strings.TrimSpace(*e.Description) != "" {
		return strings.TrimSpace(*e.Description)
	}
	kind := "Expense"
	if e.CategoryName != nil && *e.CategoryName != "" {
		kind = *e.CategoryName + " expense"
	}
	return kind + " on " + e.Date.UTC().Format("2006-01-02")
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Expense, error) {
	in, err := ValidateCreate(in)
	if err != nil {
		return Expense{}, err
	}
	if err := s.requireOwnedLinks(ctx, userID, in.CategoryID, in.RelatedDocumentID); err != nil {
		return Expense{}, err
	}
	created, err := s.store.Create(ctx, userID, in)
	if err != nil {
		return Expense{}, linkRace(err)
	}
	s.sync(ctx, userID, created)
	return created, nil
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Expense, error) {
	return s.store.ByID(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Expense, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.List(ctx, userID, f)
}

// Summarize adds up the matching expenses, grouped by currency and category.
//
// It validates the same filter the list does -- so an impossible window is a
// field error here too -- and then ignores its paging: a summary of the first
// page is not a summary.
func (s *Service) Summarize(ctx context.Context, userID uuid.UUID, f Filter) (Summary, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return Summary{}, err
	}
	f.Limit, f.Offset = 0, 0
	return s.store.Summarize(ctx, userID, f)
}

func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in UpdateInput) (Expense, error) {
	p, err := ValidatePatch(in)
	if err != nil {
		return Expense{}, err
	}
	if err := s.requireOwnedLinks(ctx, userID, patchLink(p.CategoryID), patchLink(p.RelatedDocumentID)); err != nil {
		return Expense{}, err
	}
	updated, err := s.store.Update(ctx, userID, id, p)
	if err != nil {
		return Expense{}, linkRace(err)
	}
	s.sync(ctx, userID, updated)
	return updated, nil
}

// Delete makes no graph call: the node goes with the expense in the same
// statement, through the trigger migration 000009 puts on this table.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.Delete(ctx, userID, id)
}

// requireOwnedLinks checks the category and the document an expense points at.
//
// A link to a row the caller does not own is reported as ErrNotFound -- a 404 --
// rather than as a validation error naming the field: the two answers differ,
// and only the first keeps another user's ids unconfirmable. It is the same
// rule calendar.Service applies to related_task_id.
func (s *Service) requireOwnedLinks(ctx context.Context, userID uuid.UUID, categoryID, documentID *uuid.UUID) error {
	if categoryID != nil {
		if err := s.requireOwned(ctx, userID, *categoryID, "category_id", s.store.CategoryExists); err != nil {
			return err
		}
	}
	if documentID != nil {
		if err := s.requireOwned(ctx, userID, *documentID, "related_document_id", s.store.DocumentExists); err != nil {
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

// linkRace turns the foreign-key violation a concurrent delete produces into
// the answer a missing link gets everywhere else.
//
// requireOwnedLinks has already checked that the caller owns the category or
// document, but nothing holds it there: it can be deleted between the check and
// the write. The row it named is gone either way, so "no such expense" is the
// honest answer, and it keeps a race from surfacing as a 500.
func linkRace(err error) error {
	if IsForeignKeyViolation(err) {
		return ErrNotFound
	}
	return err
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
