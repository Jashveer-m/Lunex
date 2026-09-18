package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/finance"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// toolFinance is one finance service playing the part it plays in production
// for the tools. Its Create counts a write the chat turn must never make: the
// registry here is built with no ledger, so there is no path from a turn to it,
// and this counts anyway.
type toolFinance struct {
	mu         sync.Mutex
	byUser     map[uuid.UUID][]finance.Expense
	categories map[uuid.UUID][]finance.Category
	creates    int
}

func newToolFinance() *toolFinance {
	return &toolFinance{
		byUser:     map[uuid.UUID][]finance.Expense{},
		categories: map[uuid.UUID][]finance.Category{},
	}
}

func (f *toolFinance) seedCategories(owner uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, name := range finance.DefaultCategories {
		f.categories[owner] = append(f.categories[owner],
			finance.Category{ID: uuid.New(), UserID: owner, Name: name})
	}
}

// spend records an expense the user already has, on the given day.
func (f *toolFinance) spend(owner uuid.UUID, amount finance.Amount, category, what string, day time.Time) finance.Expense {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := finance.Expense{
		ID: uuid.New(), UserID: owner, Amount: amount, Currency: finance.DefaultCurrency,
		Date: finance.Day(day),
	}
	if what != "" {
		e.Description = &what
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

func (f *toolFinance) matching(userID uuid.UUID, filter finance.Filter) []finance.Expense {
	var out []finance.Expense
	for _, e := range f.byUser[userID] {
		switch {
		case filter.Start != nil && e.Date.Before(*filter.Start):
			continue
		case filter.End != nil && e.Date.After(*filter.End):
			continue
		case filter.CategoryID != nil && (e.CategoryID == nil || *e.CategoryID != *filter.CategoryID):
			continue
		}
		out = append(out, e)
	}
	return out
}

func (f *toolFinance) List(_ context.Context, userID uuid.UUID, filter finance.Filter) ([]finance.Expense, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.matching(userID, filter)
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (f *toolFinance) Summarize(_ context.Context, userID uuid.UUID, filter finance.Filter) (finance.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := finance.Summary{Start: filter.Start, End: filter.End}
	for _, e := range f.matching(userID, filter) {
		if len(out.Currencies) == 0 {
			out.Currencies = append(out.Currencies, finance.CurrencyTotal{Currency: e.Currency})
		}
		c := &out.Currencies[0]
		c.Total += e.Amount
		c.Count++
		out.Count++
		name := ""
		if e.CategoryName != nil {
			name = *e.CategoryName
		}
		found := false
		for i := range c.Categories {
			if c.Categories[i].Category == name {
				c.Categories[i].Total += e.Amount
				c.Categories[i].Count++
				found = true
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

func (f *toolFinance) Categories(_ context.Context, userID uuid.UUID) ([]finance.Category, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]finance.Category(nil), f.categories[userID]...), nil
}

func (f *toolFinance) CategoryByName(_ context.Context, userID uuid.UUID, name string) (finance.Category, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.categories[userID] {
		if strings.EqualFold(c.Name, strings.TrimSpace(name)) {
			return c, nil
		}
	}
	return finance.Category{}, finance.ErrCategoryNotFound
}

func (f *toolFinance) Create(context.Context, uuid.UUID, finance.CreateInput) (finance.Expense, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	return finance.Expense{}, errors.New("the chat turn must never record an expense")
}

func (f *toolFinance) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

// The harness clock is Thursday 10 September 2026, 12:00 UTC.
var (
	sept2 = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	sept4 = time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
)

const askSpending = `{"tool": "analyze_spending", "arguments": {"start": "this month"}}`

// --- the financial-advice boundary ------------------------------------------------

// The rule the master spec requires, checked where it has to hold: in the
// prompt the answering model is given.
//
// It is two rules and both are asserted. The first is the grounding rule
// applied to arithmetic -- the totals are already computed and must not be
// re-totalled or converted. The second is the boundary: the assistant describes
// what the user's own records show and does not advise on money, and does not
// claim to be a professional.
func TestTheFinanceRuleIsInThePromptWhenAFinanceToolRuns(t *testing.T) {
	h := newToolHarness(t, routingReply(askSpending, func(string) string {
		return "Your records show ₹750.00 across 2 expenses this month [S1]."
	}))
	h.finance.spend(h.user, 45000, "Food", "lunch", sept2)
	h.finance.spend(h.user, 30000, "Transport", "taxi", sept4)

	if _, _, err := h.send(t, "How much have I spent this month?"); err != nil {
		t.Fatal(err)
	}

	prompt := h.answerPrompt()
	for _, want := range []string{
		// the framing: informational, from the user's own records
		"added up from the expenses the user recorded themselves",
		"Describe what the user's own records show",
		// the boundary, in the words the model has to not write
		"You are not a financial adviser",
		"do not tell them what to do with their money",
		"no advice to invest, save, borrow, buy, sell",
		"cannot give financial advice",
		// the arithmetic rule
		"never add, re-total or convert anything yourself",
		"Totals in different currencies are separate",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the finance rule is missing %q from the prompt:\n%s", want, prompt)
		}
	}
}

// The rule is in every prompt, not only the ones a finance tool ran for.
//
// "Should I put my savings in an index fund?" retrieves nothing at all -- there
// is no source to attach a rule to -- and it is exactly the question where a
// model with no rule in front of it answers confidently.
func TestTheFinanceRuleIsInThePromptWithNothingRetrieved(t *testing.T) {
	h := newHarness(t, &ai.Mock{})
	if _, _, err := h.send(t, "Should I put my savings into an index fund?"); err != nil {
		t.Fatal(err)
	}
	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{"You are not a financial adviser", "cannot give financial advice"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the advice boundary is missing %q from a turn with no finance tool:\n%s", want, prompt)
		}
	}
}

// --- the totals reaching the model -------------------------------------------------

// A spending analysis arrives as one source carrying the figures already worked
// out, and is recorded as a source the answer cited -- so the citation trail
// says what the number came from.
func TestSpendingTotalsReachTheModelAlreadyComputed(t *testing.T) {
	h := newToolHarness(t, routingReply(askSpending, func(string) string {
		return "Your records show INR 750.00 across 2 expenses [S1]."
	}))
	h.finance.spend(h.user, 45000, "Food", "lunch", sept2)
	h.finance.spend(h.user, 30000, "Transport", "taxi", sept4)

	turn, _, err := h.send(t, "How much have I spent this month?")
	if err != nil {
		t.Fatal(err)
	}

	prompt := h.answerPrompt()
	for _, want := range []string{
		// The user said "this month", so the period is the whole of September
		// -- not the month so far, which is what no dates at all would mean.
		`spending: "Spending from Tue 1 Sep 2026 to Wed 30 Sep 2026"`,
		"Total recorded from Tue 1 Sep 2026 to Wed 30 Sep 2026: INR 750.00 across 2 expenses.",
		"- Food: INR 450.00 (1 expense)",
		"- Transport: INR 300.00 (1 expense)",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt is missing %q:\n%s", want, prompt)
		}
	}

	if len(turn.Assistant.Sources) != 1 {
		t.Fatalf("recorded %d sources, want the one total: %+v", len(turn.Assistant.Sources), turn.Assistant.Sources)
	}
	got := turn.Assistant.Sources[0]
	switch {
	case got.Type != SourceSpending:
		t.Fatalf("type = %q, want %q", got.Type, SourceSpending)
	case got.Tool != tools.AnalyzeSpending:
		t.Fatalf("tool = %q, want %q", got.Tool, tools.AnalyzeSpending)
	case got.ID != uuid.Nil:
		t.Fatalf("id = %v, want the zero uuid: a total is not a row to open", got.ID)
	case !got.Cited:
		t.Fatal("the answer cited [S1] and the source is not marked as cited")
	}

	// A read changed nothing, and nothing recorded an expense.
	if n := h.finance.writes(); n != 0 {
		t.Fatalf("a spending question made %d writes", n)
	}
}

// One expense reaches the model with its amount and currency together and its
// date spelled out, so an answer never has to work either out.
func TestAnExpenseReachesTheModelWrittenOut(t *testing.T) {
	const findLunch = `{"tool": "search_expenses", "arguments": {"query": "lunch"}}`
	h := newToolHarness(t, routingReply(findLunch, func(string) string {
		return "You recorded lunch on 2 September [S1]."
	}))
	h.finance.spend(h.user, 45000, "Food", "lunch with the team", sept2)

	turn, _, err := h.send(t, "What did I spend on lunch?")
	if err != nil {
		t.Fatal(err)
	}
	prompt := h.answerPrompt()
	for _, want := range []string{
		`expense: "lunch with the team"`, "INR 450.00", "on Wed 2 Sep 2026", "category Food",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt is missing %q:\n%s", want, prompt)
		}
	}
	if len(turn.Assistant.Sources) != 1 || turn.Assistant.Sources[0].Type != SourceExpense {
		t.Fatalf("recorded %+v, want one expense source", turn.Assistant.Sources)
	}
}

// --- the write rule, from the chat side --------------------------------------------

// Asking the assistant to record an expense proposes it and records nothing.
// It is the Phase 7 rule, checked for the one write this phase adds, because
// "logged it" about money that is not logged is the version of that lie the
// user is least able to catch.
func TestRecordingAnExpenseIsProposedNeverWritten(t *testing.T) {
	const proposeLunch = `{"tool": "create_expense", "arguments": {"amount": "450", "description": "lunch", "category": "Food"}}`
	h := newToolHarness(t, routingReply(proposeLunch, func(string) string {
		return "I have prepared that for you to approve."
	}))

	turn, _, err := h.send(t, "I spent 450 on lunch today, log it under food")
	if err != nil {
		t.Fatal(err)
	}
	if n := h.finance.writes(); n != 0 {
		t.Fatalf("the turn recorded %d expenses; it must only propose", n)
	}
	if len(turn.Actions) != 1 {
		t.Fatalf("the turn produced %d actions, want one proposal", len(turn.Actions))
	}
	a := turn.Actions[0]
	switch {
	case a.ToolName != tools.CreateExpense:
		t.Fatalf("tool = %q", a.ToolName)
	case a.Permission != tools.Write:
		t.Fatalf("permission = %q", a.Permission)
	case a.Status != "proposed":
		t.Fatalf("status = %q, want proposed", a.Status)
	}
	if want := `Record an expense of INR 450.00 under "Food" (for lunch) on Thu 10 Sep 2026.`; a.Summary != want {
		t.Fatalf("summary = %q\nwant      %q", a.Summary, want)
	}
	// The answering model is told to say it is waiting, and never that it is
	// done.
	if !strings.Contains(h.answerPrompt(), "Never say that it is done") {
		t.Fatal("the proposal was not announced with the rule that it has not happened")
	}
}
