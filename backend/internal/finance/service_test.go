package finance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// fakeStore keys by (owner, id), the same shape the SQL has -- including the
// two ownership probes, which are the only place this module reads another
// module's rows, and the inclusive date bounds, which are the one thing about
// the filter a test could get wrong without noticing.
type fakeStore struct {
	mu         sync.Mutex
	byUser     map[uuid.UUID]map[uuid.UUID]Expense
	categories map[uuid.UUID][]Category
	documents  map[uuid.UUID]map[uuid.UUID]bool
	calls      []uuid.UUID
	filters    []Filter
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byUser:     map[uuid.UUID]map[uuid.UUID]Expense{},
		categories: map[uuid.UUID][]Category{},
		documents:  map[uuid.UUID]map[uuid.UUID]bool{},
	}
}

// seedDefaults gives a user the categories migration 000009's trigger gives
// them on registration.
func (f *fakeStore) seedDefaults(owner uuid.UUID) map[string]uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]uuid.UUID{}
	for _, name := range DefaultCategories {
		c := Category{ID: uuid.New(), UserID: owner, Name: name}
		f.categories[owner] = append(f.categories[owner], c)
		out[name] = c.ID
	}
	return out
}

func (f *fakeStore) seedDocument(owner uuid.UUID) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	if f.documents[owner] == nil {
		f.documents[owner] = map[uuid.UUID]bool{}
	}
	f.documents[owner][id] = true
	return id
}

func (f *fakeStore) CreateCategory(_ context.Context, userID uuid.UUID, name string) (Category, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	for _, c := range f.categories[userID] {
		if strings.EqualFold(c.Name, name) {
			return Category{}, ErrCategoryExists
		}
	}
	c := Category{ID: uuid.New(), UserID: userID, Name: name}
	f.categories[userID] = append(f.categories[userID], c)
	return c, nil
}

func (f *fakeStore) Categories(_ context.Context, userID uuid.UUID) ([]Category, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	return append([]Category(nil), f.categories[userID]...), nil
}

func (f *fakeStore) CategoryExists(_ context.Context, userID, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	for _, c := range f.categories[userID] {
		if c.ID == id {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) DocumentExists(_ context.Context, userID, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	return f.documents[userID][id], nil
}

func (f *fakeStore) Create(_ context.Context, userID uuid.UUID, in CreateInput) (Expense, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	e := Expense{
		ID: uuid.New(), UserID: userID, Amount: in.Amount, Currency: in.Currency,
		CategoryID: in.CategoryID, Date: in.Date, RelatedDocumentID: in.RelatedDocumentID,
	}
	if in.Description != "" {
		d := in.Description
		e.Description = &d
	}
	e.CategoryName = f.nameOf(userID, in.CategoryID)
	if f.byUser[userID] == nil {
		f.byUser[userID] = map[uuid.UUID]Expense{}
	}
	f.byUser[userID][e.ID] = e
	return e, nil
}

// nameOf is the LEFT JOIN: a category id resolves to its name, and one that
// does not resolve is no category at all.
func (f *fakeStore) nameOf(userID uuid.UUID, id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	for _, c := range f.categories[userID] {
		if c.ID == *id {
			name := c.Name
			return &name
		}
	}
	return nil
}

func (f *fakeStore) ByID(_ context.Context, userID, id uuid.UUID) (Expense, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	e, ok := f.byUser[userID][id]
	if !ok {
		return Expense{}, ErrNotFound
	}
	return e, nil
}

func (f *fakeStore) matching(userID uuid.UUID, filter Filter) []Expense {
	var out []Expense
	for _, e := range f.byUser[userID] {
		switch {
		case filter.Start != nil && e.Date.Before(*filter.Start):
			continue
		case filter.End != nil && e.Date.After(*filter.End):
			continue
		case filter.CategoryID != nil && (e.CategoryID == nil || *e.CategoryID != *filter.CategoryID):
			continue
		case filter.Uncategorized && e.CategoryID != nil:
			continue
		}
		out = append(out, e)
	}
	return out
}

func (f *fakeStore) List(_ context.Context, userID uuid.UUID, filter Filter) ([]Expense, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	f.filters = append(f.filters, filter)
	return f.matching(userID, filter), nil
}

func (f *fakeStore) Summarize(_ context.Context, userID uuid.UUID, filter Filter) (Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	f.filters = append(f.filters, filter)
	out := Summary{Start: filter.Start, End: filter.End}
	for _, e := range f.matching(userID, filter) {
		i := -1
		for j := range out.Currencies {
			if out.Currencies[j].Currency == e.Currency {
				i = j
			}
		}
		if i < 0 {
			i = len(out.Currencies)
			out.Currencies = append(out.Currencies, CurrencyTotal{Currency: e.Currency})
		}
		out.Currencies[i].Total += e.Amount
		out.Currencies[i].Count++
		out.Count++
		name := ""
		if e.CategoryName != nil {
			name = *e.CategoryName
		}
		line := -1
		for j := range out.Currencies[i].Categories {
			if out.Currencies[i].Categories[j].Category == name {
				line = j
			}
		}
		if line < 0 {
			out.Currencies[i].Categories = append(out.Currencies[i].Categories,
				CategoryTotal{CategoryID: e.CategoryID, Category: name})
			line = len(out.Currencies[i].Categories) - 1
		}
		out.Currencies[i].Categories[line].Total += e.Amount
		out.Currencies[i].Categories[line].Count++
	}
	sortSummary(&out)
	return out, nil
}

func (f *fakeStore) Update(_ context.Context, userID, id uuid.UUID, p Patch) (Expense, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	e, ok := f.byUser[userID][id]
	if !ok {
		return Expense{}, ErrNotFound
	}
	if p.Amount != nil {
		e.Amount = *p.Amount
	}
	if p.Currency != nil {
		e.Currency = *p.Currency
	}
	if p.Date != nil {
		e.Date = *p.Date
	}
	if p.CategoryID.Set {
		e.CategoryID = p.CategoryID.Value
		e.CategoryName = f.nameOf(userID, p.CategoryID.Value)
	}
	if p.Description.Set {
		e.Description = p.Description.Value
	}
	if p.RelatedDocumentID.Set {
		e.RelatedDocumentID = p.RelatedDocumentID.Value
	}
	f.byUser[userID][id] = e
	return e, nil
}

func (f *fakeStore) Delete(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if _, ok := f.byUser[userID][id]; !ok {
		return ErrNotFound
	}
	delete(f.byUser[userID], id)
	return nil
}

var _ Store = (*fakeStore)(nil)

// The days these tests work in.
var (
	sept1  = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sept17 = time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	sept30 = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	oct1   = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

func lunch() CreateInput {
	return CreateInput{Amount: 45000, Description: "lunch with the team", Date: sept17}
}

func day(t time.Time) *time.Time { return &t }

// The property the whole ownership design exists for, at the service level.
func TestExpensesAreScopedToTheOwner(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()

	e, err := svc.Create(ctx, alice, lunch())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, bob, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get as the wrong user = %v, want ErrNotFound", err)
	}
	if _, err := svc.Update(ctx, bob, e.ID, UpdateInput{Amount: optional.Of(Amount(1))}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update as the wrong user = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(ctx, bob, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete as the wrong user = %v, want ErrNotFound", err)
	}
	found, err := svc.List(ctx, bob, Filter{})
	if err != nil || len(found) != 0 {
		t.Fatalf("Bob's expenses = %v, %v; want none", found, err)
	}
	// The aggregate is scoped too, which matters more than the list: a total
	// is a number somebody will believe.
	summary, err := svc.Summarize(ctx, bob, Filter{})
	if err != nil || summary.Count != 0 || len(summary.Currencies) != 0 {
		t.Fatalf("Bob's spending summary = %+v, %v; want nothing", summary, err)
	}
	if got, err := svc.Get(ctx, alice, e.ID); err != nil || got.Amount != 45000 {
		t.Fatalf("the owner's expense = %+v, %v", got, err)
	}
	for i, got := range store.calls {
		if got != alice && got != bob {
			t.Fatalf("store call %d used an unexpected owner %s", i, got)
		}
	}
}

// A link to somebody else's category or document is answered as "no such
// expense" -- never as a validation error naming the field, which would
// confirm the id exists, and never by storing the link.
func TestALinkToAnotherUsersRecordIsNotFound(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()
	aliceCats, bobCats := store.seedDefaults(alice), store.seedDefaults(bob)
	bobsDoc := store.seedDocument(bob)

	bobsFood, aliceFood := bobCats["Food"], aliceCats["Food"]
	stranger := uuid.New()
	for name, in := range map[string]CreateInput{
		"another user's category": {Amount: 100, Date: sept17, CategoryID: &bobsFood},
		"another user's document": {Amount: 100, Date: sept17, RelatedDocumentID: &bobsDoc},
		"a category nobody owns":  {Amount: 100, Date: sept17, CategoryID: &stranger},
		"a document nobody owns":  {Amount: 100, Date: sept17, RelatedDocumentID: &stranger},
	} {
		if _, err := svc.Create(ctx, alice, in); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: Create = %v, want ErrNotFound", name, err)
		}
	}
	if n := len(store.byUser[alice]); n != 0 {
		t.Fatalf("%d expenses were created behind a rejected link", n)
	}

	// Her own category links fine, and the patch path checks the same thing.
	own, err := svc.Create(ctx, alice, CreateInput{Amount: 100, Date: sept17, CategoryID: &aliceFood})
	if err != nil {
		t.Fatalf("linking to her own category: %v", err)
	}
	if _, err := svc.Update(ctx, alice, own.ID, UpdateInput{CategoryID: optional.Of(bobsFood)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("patching in another user's category = %v, want ErrNotFound", err)
	}
	// Clearing the link needs no ownership check: null belongs to nobody.
	cleared, err := svc.Update(ctx, alice, own.ID, UpdateInput{CategoryID: optional.Null[uuid.UUID]()})
	if err != nil || cleared.CategoryID != nil {
		t.Fatalf("clearing the category = %+v, %v", cleared, err)
	}
}

// Money is exact. This is the property amount.go exists for, run end to end
// through the service: a hundred expenses of 0.10 add up to 10.00 and not to
// 9.999999999999998.
func TestTotalsAreExact(t *testing.T) {
	svc := NewService(newFakeStore())
	ctx := context.Background()
	alice := uuid.New()

	for i := 0; i < 100; i++ {
		if _, err := svc.Create(ctx, alice, CreateInput{Amount: 10, Date: sept17}); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := svc.Summarize(ctx, alice, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Currencies) != 1 || summary.Currencies[0].Total != 1000 {
		t.Fatalf("a hundred lots of 0.10 = %+v, want 10.00", summary.Currencies)
	}
	if got := summary.Currencies[0].Total.String(); got != "10.00" {
		t.Fatalf("the total renders as %q, want 10.00", got)
	}
}

// Two currencies are two totals, and there is no third number adding them
// together: nothing here knows a rate, so a combined figure would be invented.
func TestCurrenciesAreTotalledSeparately(t *testing.T) {
	svc := NewService(newFakeStore())
	ctx := context.Background()
	alice := uuid.New()

	for _, in := range []CreateInput{
		{Amount: 50000, Currency: "INR", Date: sept17},
		{Amount: 20000, Currency: "INR", Date: sept17},
		{Amount: 2000, Currency: "USD", Date: sept17},
	} {
		if _, err := svc.Create(ctx, alice, in); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := svc.Summarize(ctx, alice, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Currencies) != 2 {
		t.Fatalf("summary has %d currencies, want 2: %+v", len(summary.Currencies), summary.Currencies)
	}
	// Largest first, and each total is of its own currency only.
	if summary.Currencies[0].Currency != "INR" || summary.Currencies[0].Total != 70000 {
		t.Fatalf("first currency = %+v, want INR 700.00", summary.Currencies[0])
	}
	if summary.Currencies[1].Currency != "USD" || summary.Currencies[1].Total != 2000 {
		t.Fatalf("second currency = %+v, want USD 20.00", summary.Currencies[1])
	}
	if summary.Count != 3 {
		t.Fatalf("count = %d, want 3", summary.Count)
	}
}

// The uncategorized expenses are a line of their own rather than dropped: a
// breakdown whose lines do not add up to its total is a breakdown that misleads.
func TestUncategorizedSpendingIsItsOwnLine(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice := uuid.New()
	cats := store.seedDefaults(alice)
	food := cats["Food"]

	if _, err := svc.Create(ctx, alice, CreateInput{Amount: 30000, Date: sept17, CategoryID: &food}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, alice, CreateInput{Amount: 10000, Date: sept17}); err != nil {
		t.Fatal(err)
	}

	summary, err := svc.Summarize(ctx, alice, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	lines := summary.Currencies[0].Categories
	if len(lines) != 2 {
		t.Fatalf("breakdown has %d lines, want 2: %+v", len(lines), lines)
	}
	var sum Amount
	for _, l := range lines {
		sum += l.Total
	}
	if sum != summary.Currencies[0].Total {
		t.Fatalf("the lines add up to %s, the total says %s", sum, summary.Currencies[0].Total)
	}
	if !lines[1].Uncategorized() {
		t.Fatalf("the smaller line is %+v, want the no-category one", lines[1])
	}
}

// The date bounds are inclusive on both ends. September is the 1st to the 30th,
// and an expense on either of those days is in it.
func TestTheDateRangeIncludesBothEnds(t *testing.T) {
	svc := NewService(newFakeStore())
	ctx := context.Background()
	alice := uuid.New()

	for _, d := range []time.Time{sept1, sept17, sept30, oct1} {
		if _, err := svc.Create(ctx, alice, CreateInput{Amount: 100, Date: d}); err != nil {
			t.Fatal(err)
		}
	}
	september, err := svc.List(ctx, alice, Filter{Start: day(sept1), End: day(sept30)})
	if err != nil {
		t.Fatal(err)
	}
	if len(september) != 3 {
		t.Fatalf("September holds %d expenses, want the 1st, the 17th and the 30th", len(september))
	}
	oneDay, err := svc.List(ctx, alice, Filter{Start: day(sept17), End: day(sept17)})
	if err != nil || len(oneDay) != 1 {
		t.Fatalf("one day = %d expenses, %v; want 1", len(oneDay), err)
	}
}

// Unlike the calendar, an unbounded read is offered -- a history question with
// no dates in it is a real question here. What is refused is a range that runs
// backwards.
func TestListAcceptsNoRangeAndRefusesABackwardsOne(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice := uuid.New()

	if _, err := svc.Create(ctx, alice, lunch()); err != nil {
		t.Fatal(err)
	}
	all, err := svc.List(ctx, alice, Filter{})
	if err != nil || len(all) != 1 {
		t.Fatalf("an unbounded list = %d, %v; want the one expense", len(all), err)
	}

	var verrs validate.Errors
	if _, err := svc.List(ctx, alice, Filter{Start: day(sept30), End: day(sept1)}); !errors.As(err, &verrs) {
		t.Fatalf("a backwards range = %v, want a validation error", err)
	}
	if verrs[0].Field != "end" {
		t.Fatalf("the error names %q, want end", verrs[0].Field)
	}
	if _, err := svc.Summarize(ctx, alice, Filter{Start: day(sept30), End: day(sept1)}); !errors.As(err, &verrs) {
		t.Fatalf("the summary took a backwards range: %v", err)
	}
}

// A summary is a summary of everything that matches, not of the first page.
func TestSummarizeIgnoresPaging(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()

	if _, err := svc.Summarize(ctx, uuid.New(), Filter{Limit: 1, Offset: 5}); err != nil {
		t.Fatal(err)
	}
	last := store.filters[len(store.filters)-1]
	if last.Limit != 0 || last.Offset != 0 {
		t.Fatalf("the summary ran with limit %d offset %d, want neither", last.Limit, last.Offset)
	}
}

func TestUpdateAppliesOnlyMentionedFields(t *testing.T) {
	svc := NewService(newFakeStore())
	ctx := context.Background()
	alice := uuid.New()

	e, err := svc.Create(ctx, alice, lunch())
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(ctx, alice, e.ID, UpdateInput{Amount: optional.Of(Amount(50000))})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Amount != 50000 {
		t.Fatalf("amount = %s, want 500.00", updated.Amount)
	}
	if !updated.Date.Equal(sept17) || updated.Description == nil || *updated.Description != "lunch with the team" {
		t.Fatalf("an unmentioned field was overwritten: %+v", updated)
	}
}

// --- categories ---------------------------------------------------------------

// A category name is resolved case-insensitively and with its own spelling
// returned, so everything downstream says "Food" the way the user's list does.
func TestCategoryByNameIsCaseInsensitiveAndNeverCreates(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice := uuid.New()
	store.seedDefaults(alice)

	for _, written := range []string{"food", "FOOD", "  Food  "} {
		c, err := svc.CategoryByName(ctx, alice, written)
		if err != nil || c.Name != "Food" {
			t.Fatalf("%q resolved to %+v, %v; want Food", written, c, err)
		}
	}
	if _, err := svc.CategoryByName(ctx, alice, "Coffe"); !errors.Is(err, ErrCategoryNotFound) {
		t.Fatalf("an unknown category = %v, want ErrCategoryNotFound", err)
	}
	if n := len(store.categories[alice]); n != len(DefaultCategories) {
		t.Fatalf("resolving a name created a category: %d, want %d", n, len(DefaultCategories))
	}
}

// Two users' "Food" are two categories, and one user's second "Food" is a field
// error rather than a duplicate row.
func TestCategoriesAreScopedAndUnique(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()
	store.seedDefaults(alice)
	store.seedDefaults(bob)

	aliceFood, err := svc.CategoryByName(ctx, alice, "Food")
	if err != nil {
		t.Fatal(err)
	}
	bobFood, err := svc.CategoryByName(ctx, bob, "Food")
	if err != nil {
		t.Fatal(err)
	}
	if aliceFood.ID == bobFood.ID {
		t.Fatal("Alice and Bob share a category row")
	}

	var verrs validate.Errors
	if _, err := svc.CreateCategory(ctx, alice, "food"); !errors.As(err, &verrs) {
		t.Fatalf("a duplicate category = %v, want a validation error", err)
	}
	if verrs[0].Field != "name" {
		t.Fatalf("the error names %q, want name", verrs[0].Field)
	}

	added, err := svc.CreateCategory(ctx, alice, "  Books  and   Courses ")
	if err != nil {
		t.Fatal(err)
	}
	if added.Name != "Books and Courses" {
		t.Fatalf("the name was stored as %q, want its whitespace collapsed", added.Name)
	}
}
