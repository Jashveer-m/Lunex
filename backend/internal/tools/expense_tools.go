package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/finance"
)

// expenseRecord is an expense as a tool reports it: what was spent, on what,
// and when.
type expenseRecord struct {
	ID          string         `json:"id"`
	Amount      finance.Amount `json:"amount"`
	Currency    string         `json:"currency"`
	Category    *string        `json:"category"`
	Description *string        `json:"description"`
	Date        string         `json:"date"`
}

func toExpenseRecord(e finance.Expense) expenseRecord {
	return expenseRecord{
		ID: e.ID.String(), Amount: e.Amount, Currency: e.Currency,
		Category: e.CategoryName, Description: e.Description,
		Date: e.Date.UTC().Format(time.DateOnly),
	}
}

var expenseRecordSchema = object(map[string]Schema{
	"id":          uuidField("the expense's id"),
	"amount":      number("how much was spent"),
	"currency":    str("the currency it was spent in, as a 3-letter code"),
	"category":    str("the category it is filed under, or null"),
	"description": str("what it was for, or null"),
	"date":        str("the day it was spent, as YYYY-MM-DD"),
}, "id", "amount", "currency", "category", "description", "date")

// The window a spending analysis uses when the user named no dates.
//
// Unlike search_expenses, which looks at the whole history when no dates are
// given, an analysis defaults to the current calendar month: "how much am I
// spending" is a question about now, and a total over three years presented as
// the answer to it would be wrong in a way the user could not see. It is not
// the invented filter the hardening pass forbids, for the same two reasons the
// calendar's default window is not: it is the same window every time rather
// than a guess at what the user meant, and the summary the user and the model
// both see names the dates it used.
const spendingDefaultIsThisMonth = true

// TopSpendingCategories is how many category lines one analysis puts in front
// of the model. The totals themselves are complete -- the currency total is the
// sum of everything, not of these lines -- and what is cut is only the tail of
// the breakdown, which is said in words when it happens.
const TopSpendingCategories = 8

// Argument aliases for the grounded arguments. They are declared on the Param
// rather than only read in prepare, because the router drops an ungrounded
// filter by *key*: a "month" the tool reads and the declaration does not
// mention is a window that escapes the grounding check. See Param.Aliases and
// Router.dropUngroundedFilters.
var (
	// Every key spendingWindow actually reads has to be in here, `start_date`
	// and `from_date` included: the router drops an ungrounded argument by
	// *key*, so a window written under a key the tool reads and the
	// declaration does not name would escape the check entirely -- which is
	// the whole failure these lists exist to prevent.
	spendStartAliases = []string{"from", "since", "when", "range", "period", "month", "date", "day",
		"after", "start_date", "from_date"}
	spendEndAliases = []string{"to", "until", "till", "through", "before", "end_date"}
	categoryAliases = []string{"category_name", "in", "kind", "type", "spent_on", "on"}
)

// --- search_expenses ------------------------------------------------------------

// searchExpensesInput is the resolved query, canonical. The dates are
// YYYY-MM-DD or empty, and empty means "no bound" rather than "today".
type searchExpensesInput struct {
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
	Category string `json:"category,omitempty"`
	Query    string `json:"query,omitempty"`
}

func searchExpensesTool(s Services) Tool {
	return define(Tool{
		Name: SearchExpenses,
		Description: "Look at the expenses the user has recorded: what they spent, on what, and when. " +
			"Use it to list individual purchases. For a total, use analyze_spending instead.",
		Permission: Read,
		Params: []Param{
			{Name: "start", Type: "string", Filter: true, Aliases: spendStartAliases,
				Description: `the earliest day to look at, in the user's own words ("this month", "since june") or as YYYY-MM-DD`},
			{Name: "end", Type: "string", Filter: true, Aliases: spendEndAliases,
				Description: "the latest day to look at, if the user gave a range"},
			{Name: "category", Type: "string", Filter: true, Aliases: categoryAliases,
				Description: "the category the user named, if they named one"},
			{Name: "query", Type: "string", Description: "a word or phrase from what the expense was for, if the user gave one"},
		},
		Output: object(map[string]Schema{
			"start":    str("the first day looked at, or null for the whole history"),
			"end":      str("the last day looked at, or null"),
			"count":    integer("how many expenses are listed"),
			"more":     boolean("whether more expenses matched than are listed"),
			"expenses": listOf(expenseRecordSchema),
		}, "start", "end", "count", "more", "expenses"),
	},
		func(ctx context.Context, userID uuid.UUID, a Args) (searchExpensesInput, error) {
			in := searchExpensesInput{Query: a.String("query", "q", "search", "text", "keyword", "description", "for")}
			start, end, err := spendingWindow(SearchExpenses, a, s.Now(), false)
			if err != nil {
				return in, err
			}
			in.Start, in.End = dayString(start), dayString(end)

			// A category the user's list does not have is not a category: it
			// becomes part of the text search instead, so "what did I spend on
			// coffee" answers from the descriptions rather than coming back
			// empty because there is no Coffee category. create_expense does
			// the opposite and asks -- filing money under the wrong label is
			// not recoverable by reading it again.
			raw := a.String(append([]string{"category"}, categoryAliases...)...)
			if raw != "" {
				c, resolved, err := resolveCategory(ctx, s, userID, raw)
				if err != nil {
					return in, err
				}
				if resolved {
					in.Category = c.Name
				} else if in.Query == "" {
					in.Query = raw
				}
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in searchExpensesInput) (Result, error) {
			filter, err := storedFilter(ctx, s, userID, in.Start, in.End, in.Category)
			if err != nil {
				return Result{}, fmt.Errorf("search expenses: %w", err)
			}
			found, more, err := search(in.Query, func(term string, limit int) ([]finance.Expense, error) {
				f := filter
				f.Query, f.Limit, f.Sort = term, limit, finance.DefaultSort
				return s.Finance.List(ctx, userID, f)
			}, func(e finance.Expense) uuid.UUID { return e.ID })
			if err != nil {
				return Result{}, fmt.Errorf("search expenses: %w", err)
			}
			records := make([]expenseRecord, 0, len(found))
			for _, e := range found {
				records = append(records, toExpenseRecord(e))
			}
			return Result{
				Output: map[string]any{
					"start": nullableDay(in.Start), "end": nullableDay(in.End),
					"count": len(records), "more": more, "expenses": records,
				},
				Expenses: found, More: more,
			}, nil
		},
		func(in searchExpensesInput) string {
			out := "Look at the recorded expenses"
			if in.Category != "" {
				out += " filed under " + quoted(in.Category)
			}
			if in.Query != "" {
				out += " for " + quoted(in.Query)
			}
			return out + " " + spanPhrase(in.Start, in.End) + "."
		},
	)
}

// --- analyze_spending -----------------------------------------------------------

type analyzeSpendingInput struct {
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
	Category string `json:"category,omitempty"`
	Query    string `json:"query,omitempty"`
}

func analyzeSpendingTool(s Services) Tool {
	return define(Tool{
		Name: AnalyzeSpending,
		Description: "Add up what the user spent over a period, broken down by category. " +
			"Use it for how much they spent in total, on a category, or in a month. " +
			"It reports what their own records add up to; it is not financial advice.",
		Permission: Read,
		Params: []Param{
			{Name: "start", Type: "string", Filter: true, Aliases: spendStartAliases,
				Description: `the first day to add up, in the user's own words ("this month", "last year") or as YYYY-MM-DD`},
			{Name: "end", Type: "string", Filter: true, Aliases: spendEndAliases,
				Description: "the last day to add up, if the user gave a range"},
			{Name: "category", Type: "string", Filter: true, Aliases: categoryAliases,
				Description: "the category the user named, if they named one"},
			{Name: "query", Type: "string", Description: "a word or phrase from what the money was spent on, if the user named something that is not a category"},
		},
		Output: object(map[string]Schema{
			"start": str("the first day added up"),
			"end":   str("the last day added up"),
			"count": integer("how many expenses went into the totals"),
			"currencies": listOf(object(map[string]Schema{
				"currency": str("the currency these totals are in"),
				"total":    number("everything spent in this currency over the period"),
				"count":    integer("how many expenses"),
				"categories": listOf(object(map[string]Schema{
					"category": str("the category, or null for the expenses filed under none"),
					"total":    number("spent on this category"),
					"count":    integer("how many expenses"),
				}, "category", "total", "count")),
			}, "currency", "total", "count", "categories")),
		}, "start", "end", "count", "currencies"),
	},
		func(ctx context.Context, userID uuid.UUID, a Args) (analyzeSpendingInput, error) {
			in := analyzeSpendingInput{Query: a.String("query", "q", "search", "text", "keyword", "description", "for")}
			start, end, err := spendingWindow(AnalyzeSpending, a, s.Now(), spendingDefaultIsThisMonth)
			if err != nil {
				return in, err
			}
			in.Start, in.End = dayString(start), dayString(end)

			raw := a.String(append([]string{"category"}, categoryAliases...)...)
			if raw != "" {
				c, resolved, err := resolveCategory(ctx, s, userID, raw)
				if err != nil {
					return in, err
				}
				if resolved {
					in.Category = c.Name
				} else if in.Query == "" {
					in.Query = raw
				}
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in analyzeSpendingInput) (Result, error) {
			filter, err := storedFilter(ctx, s, userID, in.Start, in.End, in.Category)
			if err != nil {
				return Result{}, fmt.Errorf("analyze spending: %w", err)
			}
			filter.Query = in.Query
			summary, err := s.Finance.Summarize(ctx, userID, filter)
			if err != nil {
				return Result{}, fmt.Errorf("analyze spending: %w", err)
			}
			return Result{
				Output:  summaryOutput(summary, in),
				Reports: []Report{{Title: spendingTitle(in), Text: spendingReport(summary, in)}},
			}, nil
		},
		func(in analyzeSpendingInput) string {
			out := "Add up what was spent"
			if in.Category != "" {
				out += " on " + quoted(in.Category)
			}
			if in.Query != "" {
				out += " on " + quoted(in.Query)
			}
			return out + " " + spanPhrase(in.Start, in.End) + "."
		},
	)
}

func summaryOutput(s finance.Summary, in analyzeSpendingInput) map[string]any {
	currencies := make([]map[string]any, 0, len(s.Currencies))
	for _, c := range s.Currencies {
		lines := make([]map[string]any, 0, len(c.Categories))
		for _, line := range c.Categories {
			var name *string
			if !line.Uncategorized() {
				n := line.Category
				name = &n
			}
			lines = append(lines, map[string]any{
				"category": name, "total": line.Total, "count": line.Count,
			})
		}
		currencies = append(currencies, map[string]any{
			"currency": c.Currency, "total": c.Total, "count": c.Count, "categories": lines,
		})
	}
	return map[string]any{
		"start": nullableDay(in.Start), "end": nullableDay(in.End),
		"count": s.Count, "currencies": currencies,
	}
}

// spendingTitle is what the report is called in the context block.
func spendingTitle(in analyzeSpendingInput) string {
	what := "Spending"
	if in.Category != "" {
		what = "Spending on " + in.Category
	} else if in.Query != "" {
		what = "Spending on " + in.Query
	}
	return what + " " + spanPhrase(in.Start, in.End)
}

// spendingReport writes the totals out for the model, in whole sentences with
// the currency on every figure.
//
// Every number the model is shown is written here, and none of it is left for
// the model to work out: a 3B model shown a list of expenses will add them up
// and get it wrong, and a total that is wrong about the user's own money is
// exactly the failure the finance rule in the system prompt is about. The
// wording also says what the figures do *not* cover -- only what the user
// recorded -- because the answer built from it should say so too.
func spendingReport(s finance.Summary, in analyzeSpendingInput) string {
	span := spanPhrase(in.Start, in.End)
	filtered := ""
	switch {
	case in.Category != "":
		filtered = " filed under " + in.Category
	case in.Query != "":
		filtered = " matching " + quoted(in.Query)
	}
	if s.Count == 0 {
		return "No expenses" + filtered + " are recorded " + span + "."
	}

	var b strings.Builder
	for i, c := range s.Currencies {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Total recorded%s %s: %s %s across %s.",
			filtered, span, c.Currency, c.Total.String(), plural(c.Count, "expense"))
		if in.Category == "" {
			for j, line := range c.Categories {
				if j == TopSpendingCategories {
					fmt.Fprintf(&b, "\n- and %d more categories, included in the total above",
						len(c.Categories)-TopSpendingCategories)
					break
				}
				name := line.Category
				if line.Uncategorized() {
					name = "no category"
				}
				fmt.Fprintf(&b, "\n- %s: %s %s (%s)", name, c.Currency, line.Total.String(),
					plural(line.Count, "expense"))
			}
		}
	}
	if len(s.Currencies) > 1 {
		b.WriteString("\nThe currencies are listed separately and are not added together; " +
			"nothing here converts between them.")
	}
	b.WriteString("\nThese are the expenses the user recorded themselves over that period, and nothing else.")
	return b.String()
}

// --- create_expense -------------------------------------------------------------

// createExpenseInput is the canonical proposal. The category is stored by
// *name* and not by id, for two reasons: the stored input is what the user is
// shown before approving, and "Food" is readable where a uuid is not; and the
// declared params are the whole of what a canonical input may carry, which a
// resolved id would break. It is re-resolved when the approval runs -- the name
// is already known to match one of the user's categories, and nothing this
// phase offers renames or deletes one.
type createExpenseInput struct {
	Amount      finance.Amount `json:"amount"`
	Currency    string         `json:"currency"`
	Category    string         `json:"category,omitempty"`
	Description string         `json:"description,omitempty"`
	Date        string         `json:"date"`
}

func createExpenseTool(s Services) Tool {
	return define(Tool{
		Name: CreateExpense,
		Description: "Propose recording an expense the user says they made. " +
			"It is only added to their records after the user approves it.",
		Permission: Write,
		Params: []Param{
			{Name: "amount", Type: "string", Required: true,
				Description: `how much was spent, as the user said it ("500", "12.40", "₹1,200")`},
			// Grounded: the one argument of this write the user cannot check by
			// reading the proposal, because "USD 1450.50" reads like something
			// they said. See Param.Grounded for the measured case. The
			// synonyms are how a currency is actually named -- "$", "rupees"
			// -- so saying it in any of those ways keeps it.
			{Name: "currency", Type: "string", Grounded: true, Aliases: []string{"curr", "unit"},
				Synonyms:    currencyWords,
				Description: "the currency, as a 3-letter code, only if the user said which currency"},
			{Name: "category", Type: "string", Description: "which of the user's categories it belongs to, if they said"},
			{Name: "description", Type: "string", Description: "what the money was spent on"},
			{Name: "date", Type: "string",
				Description: `when it was spent, in the user's own words ("today", "yesterday") or as YYYY-MM-DD; leave out if they did not say`},
		},
		Output: object(map[string]Schema{"expense": expenseRecordSchema}, "expense"),
	},
		func(ctx context.Context, userID uuid.UUID, a Args) (createExpenseInput, error) {
			amount, currency, err := spentAmount(a)
			if err != nil {
				return createExpenseInput{}, err
			}
			in := createExpenseInput{
				Amount:      amount,
				Currency:    currency,
				Description: a.String("description", "details", "note", "notes", "what", "item", "merchant", "for"),
			}

			// No date is today, not an error and not a guess: "I spent 500 on
			// lunch" said today is about today, and the summary names the date
			// so a user who meant yesterday can see that it does not say so.
			raw := a.String("date", "expense_date", "when", "on", "day")
			day := Day(s.Now())
			if raw != "" {
				m, err := ParseMoment(raw, s.Now())
				if err != nil {
					return in, invalid(CreateExpense,
						"could not read %s as a date: ask the user which day they mean", quoted(raw))
				}
				// Backwards: a bare "5 September" is the one that has already
				// happened. See InThePast.
				day = InThePast(raw, m.Day(), s.Now())
			}
			// Whatever is left, an expense is something that has happened.
			// "I spent 500 next Friday" is not a sentence with a meaning, and
			// recording it would put money in a period no total covers.
			if day.After(Day(s.Now())) {
				return in, invalid(CreateExpense,
					"an expense cannot be dated in the future (%s): ask the user which day they spent it",
					day.Format("Mon 2 Jan 2006"))
			}
			in.Date = day.Format(time.DateOnly)

			// A named category has to be one the user already has. Unlike the
			// two reads, an unmatched name is refused rather than folded into
			// the text: this call files money under a label, every later total
			// is grouped by that label, and a tool that quietly invented
			// "Coffe" because a model spelled it that way would make the
			// breakdown meaningless. The message names the categories that do
			// exist, so the model can ask a precise question.
			if named := a.String("category", "category_name", "kind", "type"); named != "" {
				c, resolved, err := resolveCategory(ctx, s, userID, named)
				if err != nil {
					return in, err
				}
				if !resolved {
					known, err := categoryNames(ctx, s, userID)
					if err != nil {
						return in, err
					}
					return in, invalid(CreateExpense,
						"the user has no category called %s (they have: %s): ask which one it belongs to, or record it without a category",
						quoted(named), strings.Join(known, ", "))
				}
				in.Category = c.Name
			}

			// The service's own validation, run now so the user is never shown
			// a proposal that would fail on approval.
			v, err := finance.ValidateCreate(in.toService(nil))
			if err != nil {
				return in, fieldProblems(CreateExpense, err)
			}
			in.Currency, in.Description = v.Currency, v.Description
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in createExpenseInput) (Result, error) {
			var categoryID *uuid.UUID
			if in.Category != "" {
				c, err := s.Finance.CategoryByName(ctx, userID, in.Category)
				if err != nil {
					// The category existed when this was proposed. If it does
					// not now, the approved write fails and is recorded as
					// having failed, rather than quietly filing the money
					// under nothing the user did not choose.
					return Result{}, fmt.Errorf("record expense under %q: %w", in.Category, err)
				}
				categoryID = &c.ID
			}
			e, err := s.Finance.Create(ctx, userID, in.toService(categoryID))
			if err != nil {
				return Result{}, err
			}
			return Result{
				Output:   map[string]any{"expense": toExpenseRecord(e)},
				Expenses: []finance.Expense{e},
			}, nil
		},
		func(in createExpenseInput) string {
			out := "Record an expense of " + in.Currency + " " + in.Amount.String()
			if in.Category != "" {
				out += " under " + quoted(in.Category)
			}
			out += details("for", in.Description)
			if day, err := time.Parse(time.DateOnly, in.Date); err == nil {
				out += " on " + formatDay(day)
			}
			return out + "."
		},
	)
}

func (in createExpenseInput) toService(categoryID *uuid.UUID) finance.CreateInput {
	out := finance.CreateInput{
		Amount: in.Amount, Currency: in.Currency, Description: in.Description,
		CategoryID: categoryID,
	}
	if day, err := time.Parse(time.DateOnly, in.Date); err == nil {
		out.Date = day.UTC()
	}
	return out
}

// --- shared helpers -------------------------------------------------------------

// spentAmount reads how much was spent, and the currency if the user said it in
// the same breath ("₹500", "20 dollars").
//
// An amount that cannot be read is an ArgumentError rather than a default:
// there is no sensible default for how much money somebody spent, and a
// proposal showing the wrong number is one a user might approve without
// noticing.
func spentAmount(a Args) (finance.Amount, string, error) {
	raw := a.String("amount", "cost", "price", "value", "sum", "spent", "total")
	if raw == "" {
		return 0, "", invalid(CreateExpense, "an expense needs an amount: ask the user how much they spent")
	}
	amount, spelled, err := parseMoney(raw)
	if err != nil {
		return 0, "", invalid(CreateExpense,
			"could not read %s as an amount of money: ask the user how much they spent", quoted(raw))
	}
	// An explicit currency argument wins over one read out of the amount: the
	// model wrote it as its own answer rather than as part of a number. What it
	// wrote is not checked here -- finance.ValidateCreate wants three letters,
	// and its field error is the better message.
	if currency := a.String("currency", "curr", "unit"); currency != "" {
		if code, ok := currencyWords[strings.ToLower(strings.TrimSpace(currency))]; ok {
			return amount, code, nil
		}
		return amount, currency, nil
	}
	return amount, spelled, nil
}

// currencyWords maps the symbols and names people write onto the codes the
// column stores. It is short on purpose: what is not here is passed through as
// written and checked by finance.ValidateCreate, which wants three letters.
//
// It is also the `currency` parameter's Synonyms, which is what lets "I spent
// 20 dollars" keep a currency the message never spells as a code -- see
// tools.ValueGrounded. A currency this map does not have is still accepted when
// the user writes the code itself, which grounds on its own.
var currencyWords = map[string]string{
	"₹": "INR", "rs": "INR", "rs.": "INR", "inr": "INR", "rupee": "INR", "rupees": "INR",
	"$": "USD", "usd": "USD", "dollar": "USD", "dollars": "USD",
	"£": "GBP", "gbp": "GBP", "pound": "GBP", "pounds": "GBP",
	"€": "EUR", "eur": "EUR", "euro": "EUR", "euros": "EUR",
	"¥": "JPY", "jpy": "JPY", "yen": "JPY",
}

// parseMoney reads "500", "₹500", "500 rupees", "20.50 USD".
//
// It returns the amount and the currency it found, which is "" when the value
// was a bare number -- and "" means "the user did not say", which
// finance.ValidateCreate turns into the column default rather than a guess.
func parseMoney(raw string) (finance.Amount, string, error) {
	currency := ""
	fields := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(raw)), func(r rune) bool {
		return r == ' ' || r == '\t'
	})
	var digits []string
	for _, f := range fields {
		// A symbol is written against the number ("₹500"), so it is stripped
		// from either end before the word lookup.
		for symbol, code := range currencyWords {
			if len([]rune(symbol)) == 1 && strings.Contains(f, symbol) {
				currency = code
				f = strings.ReplaceAll(f, symbol, "")
			}
		}
		if code, ok := currencyWords[strings.Trim(f, ".,")]; ok {
			currency = code
			continue
		}
		if f != "" {
			digits = append(digits, f)
		}
	}
	amount, err := finance.ParseAmount(strings.Join(digits, ""))
	if err != nil {
		return 0, "", err
	}
	return amount, currency, nil
}

// resolveCategory matches what the model wrote against the user's categories.
// It returns the category's own spelling, so everything downstream -- the
// filter, the summary, the proposal -- says "Food" the way the user's list
// does rather than the way the model typed it.
func resolveCategory(ctx context.Context, s Services, userID uuid.UUID, name string) (finance.Category, bool, error) {
	c, err := s.Finance.CategoryByName(ctx, userID, name)
	switch {
	case errors.Is(err, finance.ErrCategoryNotFound):
		return finance.Category{}, false, nil
	case err != nil:
		return finance.Category{}, false, fmt.Errorf("resolve category %q: %w", name, err)
	}
	return c, true, nil
}

func categoryNames(ctx context.Context, s Services, userID uuid.UUID) ([]string, error) {
	all, err := s.Finance.Categories(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	out := make([]string, 0, len(all))
	for _, c := range all {
		out = append(out, c.Name)
	}
	if len(out) == 0 {
		out = append(out, "none")
	}
	return out, nil
}

// storedFilter rebuilds the finance filter from a canonical stored input. The
// category is re-resolved by name at run time rather than stored as an id,
// because a read's input is also what the action log shows the user, and a
// name is readable where a uuid is not.
func storedFilter(ctx context.Context, s Services, userID uuid.UUID, start, end, category string) (finance.Filter, error) {
	var f finance.Filter
	if day, err := time.Parse(time.DateOnly, start); err == nil {
		d := day.UTC()
		f.Start = &d
	}
	if day, err := time.Parse(time.DateOnly, end); err == nil {
		d := day.UTC()
		f.End = &d
	}
	if category == "" {
		return f, nil
	}
	c, err := s.Finance.CategoryByName(ctx, userID, category)
	if errors.Is(err, finance.ErrCategoryNotFound) {
		// The category was there when the call was prepared and is not now --
		// a stored read replayed after the user deleted it. Dropping the
		// filter would silently answer about everything, so the read answers
		// about nothing instead, which is the truth: there are no expenses in
		// a category that does not exist. An id nothing owns is how that is
		// said in a filter.
		gone := uuid.New()
		f.CategoryID = &gone
		return f, nil
	}
	if err != nil {
		return f, err
	}
	f.CategoryID = &c.ID
	return f, nil
}

// spendingWindow resolves the period a finance read runs over.
//
// Both ends may be absent -- the router drops one the user's message does not
// give -- and each absence has an answer that does not involve guessing. No
// dates at all is either the whole history (a search) or the current month (an
// analysis); a start alone is that day, or the stretch a phrase like "last
// month" names; an end alone is everything up to it.
//
// The returned times are nil for "no bound", and the far end is *inclusive*:
// "September" ends on the 30th, which is what finance.Filter wants.
func spendingWindow(tool string, a Args, now time.Time, defaultToThisMonth bool) (start, end *time.Time, err error) {
	rawStart := a.String(append([]string{"start"}, spendStartAliases...)...)
	rawEnd := a.String(append([]string{"end"}, spendEndAliases...)...)

	switch {
	case rawStart == "" && rawEnd == "":
		if !defaultToThisMonth {
			return nil, nil, nil
		}
		today := Day(now)
		first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		return &first, &today, nil
	case rawEnd == "":
		// One phrase, which may name a stretch ("last month") or a day.
		from, to, err := ParseWindow(rawStart, now)
		if err != nil {
			return nil, nil, unreadableSpendDate(tool, rawStart)
		}
		// ParseWindow is half-open; this module's filter is inclusive.
		last := InThePast(rawStart, Day(to.Add(-time.Nanosecond)), now)
		first := InThePast(rawStart, Day(from), now)
		return &first, &last, nil
	case rawStart == "":
		// An end with no start is left where the phrase puts it. "Everything up
		// to 30 September", read backwards, would end last September and hide
		// this year's spending; read forwards it simply includes everything,
		// which is the truthful answer to an open-ended question.
		m, err := ParseMoment(rawEnd, now)
		if err != nil {
			return nil, nil, unreadableSpendDate(tool, rawEnd)
		}
		last := m.Day()
		return nil, &last, nil
	}

	from, err := ParseMoment(rawStart, now)
	if err != nil {
		return nil, nil, unreadableSpendDate(tool, rawStart)
	}
	to, err := ParseMoment(rawEnd, now)
	if err != nil {
		return nil, nil, unreadableSpendDate(tool, rawEnd)
	}
	// The start is read backwards, for the reason InThePast gives. The end is
	// then anchored to it rather than read backwards on its own: in "1
	// September to 30 September" the 30th has not happened yet, and pulling it
	// back a year on its own would invert the range the user plainly meant.
	first := InThePast(rawStart, from.Day(), now)
	last := anchoredAfter(rawEnd, to.Day(), first)
	if last.Before(first) {
		return nil, nil, invalid(tool, "the period ends before it starts: ask the user which dates they mean")
	}
	return &first, &last, nil
}

// anchoredAfter puts a year-less end date in the earliest year that does not
// put it before the start. A date the phrase gave a year for is left alone --
// that year is what the user said.
func anchoredAfter(raw string, day, start time.Time) time.Time {
	if !NamesABareCalendarDate(raw) {
		return day
	}
	for day.Before(start) {
		day = day.AddDate(1, 0, 0)
	}
	for !day.AddDate(-1, 0, 0).Before(start) {
		day = day.AddDate(-1, 0, 0)
	}
	return day
}

func unreadableSpendDate(tool, raw string) error {
	return invalid(tool, "could not read %s as a date: ask the user for a date such as 2026-09-30", quoted(raw))
}

// Day is a timestamp reduced to the date it falls on, at midnight UTC. It is
// finance.Day, re-exported here so the tools do not have to import the module
// for one line.
func Day(t time.Time) time.Time { return finance.Day(t) }

func dayString(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}

// nullableDay renders a stored bound for the tool's JSON output: the date, or
// JSON null for "no bound", which is a real answer here rather than a missing
// one.
func nullableDay(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// spanPhrase renders the period a finance read covers, in the inclusive terms a
// person reads it in. An unbounded read says so rather than naming a date it
// did not use.
func spanPhrase(start, end string) string {
	from, noStart := time.Parse(time.DateOnly, start)
	to, noEnd := time.Parse(time.DateOnly, end)
	switch {
	case noStart != nil && noEnd != nil:
		return "over the whole recorded history"
	case noStart != nil:
		return "up to " + formatDay(to)
	case noEnd != nil:
		return "since " + formatDay(from)
	case from.Equal(to):
		return "on " + formatDay(from)
	}
	return "from " + formatDay(from) + " to " + formatDay(to)
}

// plural renders a count with its noun: "1 expense", "4 expenses".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
