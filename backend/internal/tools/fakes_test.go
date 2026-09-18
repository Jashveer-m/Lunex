package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/finance"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// The fakes key everything by owner, the same shape the SQL has, and count
// every write. The count is the point: most of the tests below are about what
// did *not* get written.

var testNow = time.Date(2026, 9, 10, 15, 30, 0, 0, time.UTC) // a Thursday

type fakeTasks struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]tasks.Task
	creates []tasks.CreateInput
	updates []uuid.UUID
	filters []tasks.Filter
	err     error
}

func newFakeTasks() *fakeTasks { return &fakeTasks{byUser: map[uuid.UUID][]tasks.Task{}} }

func (f *fakeTasks) seed(owner uuid.UUID, title, status string) tasks.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := tasks.Task{ID: uuid.New(), UserID: owner, Title: title, Status: status, Priority: "medium"}
	f.byUser[owner] = append(f.byUser[owner], t)
	return t
}

func (f *fakeTasks) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates) + len(f.updates)
}

func (f *fakeTasks) List(_ context.Context, userID uuid.UUID, filter tasks.Filter) ([]tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, f.err
	}
	var out []tasks.Task
	for _, t := range f.byUser[userID] {
		desc := ""
		if t.Description != nil {
			desc = *t.Description
		}
		q := strings.ToLower(filter.Query)
		if q != "" && !strings.Contains(strings.ToLower(t.Title), q) && !strings.Contains(strings.ToLower(desc), q) {
			continue
		}
		if filter.Status != "" && t.Status != filter.Status {
			continue
		}
		out = append(out, t)
		if filter.Limit > 0 && len(out) == filter.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeTasks) Get(_ context.Context, userID, id uuid.UUID) (tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.byUser[userID] {
		if t.ID == id {
			return t, nil
		}
	}
	return tasks.Task{}, tasks.ErrNotFound
}

func (f *fakeTasks) Create(_ context.Context, userID uuid.UUID, in tasks.CreateInput) (tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, in)
	t := tasks.Task{ID: uuid.New(), UserID: userID, Title: in.Title, Priority: in.Priority, Status: "pending", Deadline: in.Deadline}
	if t.Priority == "" {
		t.Priority = tasks.DefaultPriority
	}
	f.byUser[userID] = append(f.byUser[userID], t)
	return t, nil
}

func (f *fakeTasks) Update(_ context.Context, userID, id uuid.UUID, in tasks.UpdateInput) (tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, id)
	for i, t := range f.byUser[userID] {
		if t.ID != id {
			continue
		}
		if v, ok := in.Status.Get(); ok {
			t.Status = v
		}
		if v, ok := in.Priority.Get(); ok {
			t.Priority = v
		}
		if v, ok := in.Title.Get(); ok {
			t.Title = v
		}
		f.byUser[userID][i] = t
		return t, nil
	}
	return tasks.Task{}, tasks.ErrNotFound
}

type fakeGoals struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]goals.Goal
	creates []goals.CreateInput
}

func (f *fakeGoals) List(_ context.Context, userID uuid.UUID, filter goals.Filter) ([]goals.Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []goals.Goal
	for _, g := range f.byUser[userID] {
		if filter.Query != "" && !strings.Contains(strings.ToLower(g.Title), strings.ToLower(filter.Query)) {
			continue
		}
		if filter.Status != "" && g.Status != filter.Status {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

func (f *fakeGoals) Create(_ context.Context, userID uuid.UUID, in goals.CreateInput) (goals.Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, in)
	return goals.Goal{ID: uuid.New(), UserID: userID, Title: in.Title, Type: in.Type, Status: "active", Deadline: in.Deadline}, nil
}

type fakeNotes struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]notes.Note
	creates []notes.CreateInput
}

func (f *fakeNotes) List(_ context.Context, userID uuid.UUID, filter notes.Filter) ([]notes.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []notes.Note
	for _, n := range f.byUser[userID] {
		q := strings.ToLower(filter.Query)
		if q != "" && !strings.Contains(strings.ToLower(n.Title+" "+n.Content), q) {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeNotes) Create(_ context.Context, userID uuid.UUID, in notes.CreateInput) (notes.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, in)
	return notes.Note{ID: uuid.New(), UserID: userID, Title: in.Title, Content: in.Content, Tags: in.Tags}, nil
}

// fakeCalendar keeps the overlap rule the SQL has, so a test can seed an event
// that started before the window and see it come back.
type fakeCalendar struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]calendar.Event
	creates []calendar.CreateInput
	filters []calendar.Filter
	err     error
}

func newFakeCalendar() *fakeCalendar {
	return &fakeCalendar{byUser: map[uuid.UUID][]calendar.Event{}}
}

func (f *fakeCalendar) seed(owner uuid.UUID, title string, start time.Time, d time.Duration) calendar.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := calendar.Event{ID: uuid.New(), UserID: owner, Title: title, StartTime: start, EndTime: start.Add(d)}
	f.byUser[owner] = append(f.byUser[owner], e)
	return e
}

func (f *fakeCalendar) List(_ context.Context, userID uuid.UUID, filter calendar.Filter) ([]calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, f.err
	}
	var out []calendar.Event
	for _, e := range f.byUser[userID] {
		if !e.StartTime.Before(filter.End) {
			continue
		}
		if !e.EndTime.After(filter.Start) && e.StartTime.Before(filter.Start) {
			continue
		}
		q := strings.ToLower(filter.Query)
		if q != "" && !strings.Contains(strings.ToLower(e.Title), q) {
			continue
		}
		out = append(out, e)
		if filter.Limit > 0 && len(out) == filter.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeCalendar) Create(_ context.Context, userID uuid.UUID, in calendar.CreateInput) (calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, in)
	e := calendar.Event{
		ID: uuid.New(), UserID: userID, Title: in.Title,
		StartTime: in.StartTime, EndTime: in.EndTime, AllDay: in.AllDay,
	}
	f.byUser[userID] = append(f.byUser[userID], e)
	return e, nil
}

// fakeFinance keeps the rules the SQL has that the tools depend on: the date
// bounds are inclusive, the category filter is by id, and the summary is
// grouped by currency and then by category. Every user starts with the five
// categories migration 000009 seeds, so a test does not have to make them.
type fakeFinance struct {
	mu         sync.Mutex
	byUser     map[uuid.UUID][]finance.Expense
	categories map[uuid.UUID][]finance.Category
	creates    []finance.CreateInput
	filters    []finance.Filter
	err        error
}

func newFakeFinance() *fakeFinance {
	return &fakeFinance{
		byUser:     map[uuid.UUID][]finance.Expense{},
		categories: map[uuid.UUID][]finance.Category{},
	}
}

// seedCategories gives a user the defaults, as registration does.
func (f *fakeFinance) seedCategories(owner uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.categories[owner]) > 0 {
		return
	}
	for _, name := range finance.DefaultCategories {
		f.categories[owner] = append(f.categories[owner], finance.Category{
			ID: uuid.New(), UserID: owner, Name: name,
		})
	}
}

// seed records an expense. category is a name, "" for none.
func (f *fakeFinance) seed(owner uuid.UUID, amount finance.Amount, category, description string, day time.Time) finance.Expense {
	f.seedCategories(owner)
	f.mu.Lock()
	defer f.mu.Unlock()
	e := finance.Expense{
		ID: uuid.New(), UserID: owner, Amount: amount, Currency: finance.DefaultCurrency,
		Date: finance.Day(day),
	}
	if description != "" {
		e.Description = &description
	}
	for _, c := range f.categories[owner] {
		if strings.EqualFold(c.Name, category) {
			id, name := c.ID, c.Name
			e.CategoryID, e.CategoryName = &id, &name
		}
	}
	f.byUser[owner] = append(f.byUser[owner], e)
	return e
}

func (f *fakeFinance) matching(userID uuid.UUID, filter finance.Filter) []finance.Expense {
	var out []finance.Expense
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
		if q := strings.ToLower(filter.Query); q != "" {
			desc := ""
			if e.Description != nil {
				desc = *e.Description
			}
			if !strings.Contains(strings.ToLower(desc), q) {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

func (f *fakeFinance) List(_ context.Context, userID uuid.UUID, filter finance.Filter) ([]finance.Expense, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, f.err
	}
	out := f.matching(userID, filter)
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (f *fakeFinance) Summarize(_ context.Context, userID uuid.UUID, filter finance.Filter) (finance.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return finance.Summary{}, f.err
	}
	out := finance.Summary{Start: filter.Start, End: filter.End}
	byCurrency := map[string]int{}
	for _, e := range f.matching(userID, filter) {
		i, seen := byCurrency[e.Currency]
		if !seen {
			i = len(out.Currencies)
			byCurrency[e.Currency] = i
			out.Currencies = append(out.Currencies, finance.CurrencyTotal{Currency: e.Currency})
		}
		c := &out.Currencies[i]
		c.Total += e.Amount
		c.Count++
		out.Count++

		name := ""
		if e.CategoryName != nil {
			name = *e.CategoryName
		}
		found := false
		for j := range c.Categories {
			if c.Categories[j].Category == name {
				c.Categories[j].Total += e.Amount
				c.Categories[j].Count++
				found = true
				break
			}
		}
		if !found {
			c.Categories = append(c.Categories, finance.CategoryTotal{
				CategoryID: e.CategoryID, Category: name, Total: e.Amount, Count: 1,
			})
		}
	}
	return out, nil
}

func (f *fakeFinance) Categories(_ context.Context, userID uuid.UUID) ([]finance.Category, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]finance.Category(nil), f.categories[userID]...), nil
}

func (f *fakeFinance) CategoryByName(_ context.Context, userID uuid.UUID, name string) (finance.Category, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.categories[userID] {
		if strings.EqualFold(c.Name, strings.TrimSpace(name)) {
			return c, nil
		}
	}
	return finance.Category{}, finance.ErrCategoryNotFound
}

func (f *fakeFinance) Create(_ context.Context, userID uuid.UUID, in finance.CreateInput) (finance.Expense, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, in)
	e := finance.Expense{
		ID: uuid.New(), UserID: userID, Amount: in.Amount, Currency: in.Currency,
		CategoryID: in.CategoryID, Date: in.Date,
	}
	if in.Description != "" {
		d := in.Description
		e.Description = &d
	}
	for _, c := range f.categories[userID] {
		if in.CategoryID != nil && c.ID == *in.CategoryID {
			name := c.Name
			e.CategoryName = &name
		}
	}
	f.byUser[userID] = append(f.byUser[userID], e)
	return e, nil
}

type fakeDocs struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]documents.SearchResult
	queries []documents.SearchQuery
	err     error
}

func (f *fakeDocs) Search(_ context.Context, userID uuid.UUID, q documents.SearchQuery) ([]documents.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[userID], nil
}

// fakeLedger is the actions table's approval gate, in memory. It keeps the
// real one's contract: owner-scoped, atomic, single-use.
type fakeLedger struct {
	mu      sync.Mutex
	rows    map[uuid.UUID]*ledgerRow
	approve int
}

type ledgerRow struct {
	owner  uuid.UUID
	tool   string
	input  json.RawMessage
	status string
}

var (
	errLedgerNotFound   = errors.New("action not found")
	errLedgerNotPending = errors.New("action is not awaiting approval")
)

func newFakeLedger() *fakeLedger { return &fakeLedger{rows: map[uuid.UUID]*ledgerRow{}} }

func (l *fakeLedger) add(owner uuid.UUID, call Call, status string) uuid.UUID {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := uuid.New()
	l.rows[id] = &ledgerRow{owner: owner, tool: call.Tool, input: call.Input, status: status}
	return id
}

func (l *fakeLedger) Approve(_ context.Context, userID, id uuid.UUID) (Approved, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.approve++
	r, ok := l.rows[id]
	if !ok || r.owner != userID {
		return Approved{}, errLedgerNotFound
	}
	if r.status != "proposed" {
		return Approved{}, errLedgerNotPending
	}
	r.status = "approved"
	return Approved{ActionID: id, Tool: r.tool, Input: r.input}, nil
}

// world is one registry over fresh fakes.
type world struct {
	reg      *Registry
	tasks    *fakeTasks
	goals    *fakeGoals
	notes    *fakeNotes
	docs     *fakeDocs
	calendar *fakeCalendar
	finance  *fakeFinance
	ledger   *fakeLedger
	user     uuid.UUID
}

func newWorld() *world {
	w := &world{
		tasks:    newFakeTasks(),
		goals:    &fakeGoals{byUser: map[uuid.UUID][]goals.Goal{}},
		notes:    &fakeNotes{byUser: map[uuid.UUID][]notes.Note{}},
		docs:     &fakeDocs{byUser: map[uuid.UUID][]documents.SearchResult{}},
		calendar: newFakeCalendar(),
		finance:  newFakeFinance(),
		ledger:   newFakeLedger(),
		user:     uuid.New(),
	}
	w.finance.seedCategories(w.user)
	reg, err := NewRegistry(w.ledger, Standard(Services{
		Tasks: w.tasks, Goals: w.goals, Notes: w.notes, Documents: w.docs,
		Calendar: w.calendar, Finance: w.finance,
		DocumentMinSimilarity: 0.5, Now: func() time.Time { return testNow },
	})...)
	if err != nil {
		panic(err)
	}
	w.reg = reg
	return w
}

// totalWrites counts every create and update any tool has made.
func (w *world) totalWrites() int {
	w.goals.mu.Lock()
	g := len(w.goals.creates)
	w.goals.mu.Unlock()
	w.notes.mu.Lock()
	n := len(w.notes.creates)
	w.notes.mu.Unlock()
	w.calendar.mu.Lock()
	c := len(w.calendar.creates)
	w.calendar.mu.Unlock()
	w.finance.mu.Lock()
	f := len(w.finance.creates)
	w.finance.mu.Unlock()
	return w.tasks.writes() + g + n + c + f
}
