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
	"github.com/jashveer/lifeos/backend/internal/study"
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

// fakeStudy stands in for the study service. Generation is the interesting
// part: `propose` decides what ProposeFlashcards returns, so a test can say
// "the model wrote these cards" without a model, and `proposals` records that
// generation happened exactly when it should -- during Prepare and not again
// on approval.
type fakeStudy struct {
	mu         sync.Mutex
	plans      map[uuid.UUID][]study.Plan
	docs       map[uuid.UUID][]documents.Document
	cards      [][]study.CreateCardInput
	creates    []study.CreatePlanInput
	filters    []study.Filter
	proposals  []study.GenerateInput
	propose    []study.NewCard
	proposeErr error
	err        error

	// Phase 10b.
	quizzes        map[uuid.UUID][]study.Quiz
	created        []study.CreateQuizInput
	quizProposals  []study.GenerateQuizInput
	proposeQuiz    []study.NewQuestion
	proposeQuizErr error
}

func newFakeStudy() *fakeStudy {
	return &fakeStudy{
		plans: map[uuid.UUID][]study.Plan{},
		docs:  map[uuid.UUID][]documents.Document{},
		propose: []study.NewCard{
			{Front: "How long did the aurora last?", Back: "About forty minutes."},
			{Front: "What does the generator need?", Back: "A new fuel filter."},
		},
		quizzes: map[uuid.UUID][]study.Quiz{},
		proposeQuiz: []study.NewQuestion{
			{
				Question:     "How long did the aurora last?",
				Options:      []string{"About forty minutes", "Two hours"},
				CorrectIndex: 0, Topic: "aurora duration",
			},
			{
				Question:     "What does the generator need?",
				Options:      []string{"An alternator", "A new fuel filter"},
				CorrectIndex: 1, Topic: "generator servicing",
			},
		},
	}
}

func (f *fakeStudy) seedPlan(owner uuid.UUID, title string) study.Plan {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := study.Plan{ID: uuid.New(), UserID: owner, Title: title, Status: study.StatusActive}
	f.plans[owner] = append(f.plans[owner], p)
	return p
}

func (f *fakeStudy) seedDocument(owner uuid.UUID, filename string) documents.Document {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := documents.Document{
		ID: uuid.New(), UserID: owner, Filename: filename,
		Status: documents.StatusReady, ChunkCount: 3,
	}
	f.docs[owner] = append(f.docs[owner], d)
	return d
}

func (f *fakeStudy) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates) + len(f.cards) + len(f.created)
}

func (f *fakeStudy) seedQuiz(owner uuid.UUID, title string, questions int) study.Quiz {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := study.Quiz{ID: uuid.New(), UserID: owner, Title: title, QuestionCount: questions}
	f.quizzes[owner] = append(f.quizzes[owner], q)
	return q
}

func (f *fakeStudy) Quizzes(_ context.Context, userID uuid.UUID, filter study.QuizFilter) ([]study.Quiz, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []study.Quiz
	for _, q := range f.quizzes[userID] {
		if filter.Query != "" && !strings.Contains(strings.ToLower(q.Title), strings.ToLower(filter.Query)) {
			continue
		}
		out = append(out, q)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (f *fakeStudy) ProposeQuiz(_ context.Context, _ uuid.UUID, in study.GenerateQuizInput) (study.QuizProposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quizProposals = append(f.quizProposals, in)
	if f.proposeQuizErr != nil {
		return study.QuizProposal{}, f.proposeQuizErr
	}
	questions := f.proposeQuiz
	if in.Count > 0 && len(questions) > in.Count {
		questions = questions[:in.Count]
	}
	title := in.Title
	if title == "" {
		title = study.QuizTitleFor("field-notes.txt", in.Topic)
	}
	return study.QuizProposal{
		DocumentID: in.DocumentID, Filename: "field-notes.txt", StudyPlanID: in.StudyPlanID,
		Topic: in.Topic, Title: title,
		Questions: append([]study.NewQuestion(nil), questions...),
	}, nil
}

func (f *fakeStudy) CreateQuiz(_ context.Context, userID uuid.UUID, in study.CreateQuizInput) (study.Quiz, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, in)
	if f.err != nil {
		return study.Quiz{}, f.err
	}
	q := study.Quiz{
		ID: uuid.New(), UserID: userID, Title: in.Title,
		StudyPlanID: in.StudyPlanID, DocumentID: in.DocumentID,
		QuestionCount: len(in.Questions),
	}
	for _, n := range in.Questions {
		q.Questions = append(q.Questions, study.Question{
			ID: uuid.New(), QuizID: q.ID, Question: n.Question,
			Options: n.Options, CorrectIndex: n.CorrectIndex,
		})
	}
	f.quizzes[userID] = append(f.quizzes[userID], q)
	return q, nil
}

func (f *fakeStudy) Plans(_ context.Context, userID uuid.UUID, filter study.Filter) ([]study.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, f.err
	}
	var out []study.Plan
	for _, p := range f.plans[userID] {
		if filter.Status != "" && p.Status != filter.Status {
			continue
		}
		if filter.Query != "" && !strings.Contains(strings.ToLower(p.Title), strings.ToLower(filter.Query)) {
			continue
		}
		out = append(out, p)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (f *fakeStudy) CreatePlan(_ context.Context, userID uuid.UUID, in study.CreatePlanInput) (study.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, in)
	if f.err != nil {
		return study.Plan{}, f.err
	}
	p := study.Plan{
		ID: uuid.New(), UserID: userID, Title: in.Title, DocumentID: in.DocumentID,
		Status: study.StatusActive,
	}
	f.plans[userID] = append(f.plans[userID], p)
	return p, nil
}

func (f *fakeStudy) ProposeFlashcards(_ context.Context, _ uuid.UUID, in study.GenerateInput) (study.Proposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposals = append(f.proposals, in)
	if f.proposeErr != nil {
		return study.Proposal{}, f.proposeErr
	}
	cards := f.propose
	if in.Count > 0 && len(cards) > in.Count {
		cards = cards[:in.Count]
	}
	return study.Proposal{
		DocumentID: in.DocumentID, StudyPlanID: in.StudyPlanID, Topic: in.Topic,
		Cards: append([]study.NewCard(nil), cards...),
	}, nil
}

func (f *fakeStudy) CreateFlashcards(_ context.Context, userID uuid.UUID, in []study.CreateCardInput) ([]study.Flashcard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards = append(f.cards, in)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]study.Flashcard, 0, len(in))
	for _, c := range in {
		out = append(out, study.Flashcard{
			ID: uuid.New(), UserID: userID, StudyPlanID: c.StudyPlanID,
			DocumentID: c.DocumentID, Front: c.Front, Back: c.Back,
		})
	}
	return out, nil
}

func (f *fakeStudy) ResolveDocument(_ context.Context, userID uuid.UUID, ref string) (documents.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var partial []documents.Document
	for _, d := range f.docs[userID] {
		if d.ID.String() == ref || strings.EqualFold(d.Filename, ref) {
			return d, nil
		}
		if strings.Contains(strings.ToLower(d.Filename), strings.ToLower(ref)) {
			partial = append(partial, d)
		}
	}
	switch len(partial) {
	case 1:
		return partial[0], nil
	case 0:
		return documents.Document{}, study.ErrNotFound
	}
	names := make([]string, 0, len(partial))
	for _, d := range partial {
		names = append(names, d.Filename)
	}
	return documents.Document{}, &study.AmbiguousDocumentError{Ref: ref, Matches: names}
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
	study    *fakeStudy
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
		study:    newFakeStudy(),
		ledger:   newFakeLedger(),
		user:     uuid.New(),
	}
	w.finance.seedCategories(w.user)
	w.study.seedDocument(w.user, "field-notes.txt")
	reg, err := NewRegistry(w.ledger, Standard(Services{
		Tasks: w.tasks, Goals: w.goals, Notes: w.notes, Documents: w.docs,
		Calendar: w.calendar, Finance: w.finance, Study: w.study,
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
	return w.tasks.writes() + g + n + c + f + w.study.writes()
}
