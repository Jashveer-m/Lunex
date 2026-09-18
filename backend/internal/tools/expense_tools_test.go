package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/finance"
)

// testNow is Thursday 10 September 2026, 15:30 UTC (fakes_test.go), so "today"
// is the 10th and "this month" is 1 -- 10 September.

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// --- create_expense ---------------------------------------------------------------

// The call as the model writes it, turned into the proposal the user approves:
// the amount read exactly, the currency read out of how it was written, the
// date resolved by Go rather than by the model.
func TestCreateExpenseCanonicalizesWhatTheModelWrote(t *testing.T) {
	w := newWorld()
	for name, tc := range map[string]struct {
		args     Args
		amount   string
		currency string
		date     string
		summary  string
	}{
		"a bare number": {
			Args{"amount": "500", "description": "coffee"},
			"500.00", "INR", "2026-09-10",
			`Record an expense of INR 500.00 (for coffee) on Thu 10 Sep 2026.`,
		},
		"pence matter": {
			Args{"amount": "12.34", "description": "a pastry"},
			"12.34", "INR", "2026-09-10",
			`Record an expense of INR 12.34 (for a pastry) on Thu 10 Sep 2026.`,
		},
		"a symbol against the number": {
			Args{"amount": "₹1,200", "description": "groceries"},
			"1200.00", "INR", "2026-09-10",
			`Record an expense of INR 1200.00 (for groceries) on Thu 10 Sep 2026.`,
		},
		"a currency in words": {
			Args{"amount": "20 dollars", "description": "a book"},
			"20.00", "USD", "2026-09-10",
			`Record an expense of USD 20.00 (for a book) on Thu 10 Sep 2026.`,
		},
		"a stated currency wins": {
			Args{"amount": "20", "currency": "GBP", "description": "a book"},
			"20.00", "GBP", "2026-09-10",
			`Record an expense of GBP 20.00 (for a book) on Thu 10 Sep 2026.`,
		},
		"a relative day": {
			Args{"amount": "450", "description": "lunch", "date": "yesterday"},
			"450.00", "INR", "2026-09-09",
			`Record an expense of INR 450.00 (for lunch) on Wed 9 Sep 2026.`,
		},
		"a category the user has": {
			Args{"amount": "450", "description": "lunch", "category": "food"},
			"450.00", "INR", "2026-09-10",
			`Record an expense of INR 450.00 under "Food" (for lunch) on Thu 10 Sep 2026.`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			call := prepare(t, w, CreateExpense, tc.args)
			in := input(t, call)
			// The amount is a JSON number, and the number in the document is
			// the exact decimal -- not a float64's idea of it.
			if got := amountText(t, call.Input); got != tc.amount {
				t.Fatalf("amount = %s, want %s", got, tc.amount)
			}
			if in["currency"] != tc.currency {
				t.Fatalf("currency = %v, want %v", in["currency"], tc.currency)
			}
			if in["date"] != tc.date {
				t.Fatalf("date = %v, want %v", in["date"], tc.date)
			}
			if call.Permission != Write {
				t.Fatalf("permission = %v", call.Permission)
			}
			if call.Summary != tc.summary {
				t.Fatalf("summary  = %q\nwant     = %q", call.Summary, tc.summary)
			}
		})
	}
	if n := w.totalWrites(); n != 0 {
		t.Fatalf("preparing proposals wrote %d times", n)
	}
}

// Strict about content: each of these is a proposal the user must never be
// shown, because it would fail on approval or means nothing.
func TestCreateExpenseDeclinesWhatItCannotPropose(t *testing.T) {
	w := newWorld()
	for name, tc := range map[string]struct {
		args Args
		want string
	}{
		"no amount":         {Args{"description": "coffee"}, "needs an amount"},
		"not a number":      {Args{"amount": "some money"}, "could not read"},
		"nothing at all":    {Args{"amount": "0"}, "greater than zero"},
		"a negative amount": {Args{"amount": "-40"}, "greater than zero"},
		// It parses; it is validation that says it is too big, and its message
		// is the useful one.
		"more than fits":     {Args{"amount": "99999999999"}, "must be at most"},
		"a decimal comma":    {Args{"amount": "12,34"}, "could not read"},
		"three decimals":     {Args{"amount": "12.345"}, "could not read"},
		"an unreadable date": {Args{"amount": "500", "date": "sometime last spring-ish"}, "could not read"},
		"a bad currency":     {Args{"amount": "500", "currency": "rupeez"}, "currency"},
	} {
		t.Run(name, func(t *testing.T) {
			if reason := declined(t, w, CreateExpense, tc.args); !strings.Contains(reason, tc.want) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.want)
			}
		})
	}
	if n := w.totalWrites(); n != 0 {
		t.Fatalf("a declined proposal wrote %d times", n)
	}
}

// A category the user does not have is refused rather than invented, and the
// refusal names the categories that do exist so the model can ask a precise
// question.
//
// This is the opposite of what the two read tools do with the same argument,
// and deliberately: a search filed under a name that matches nothing can be
// re-run, and money filed under the wrong label is wrong in every total from
// then on.
func TestCreateExpenseNeverInventsACategory(t *testing.T) {
	w := newWorld()
	reason := declined(t, w, CreateExpense, Args{"amount": "250", "category": "Coffe", "description": "flat white"})
	for _, want := range []string{"no category called", `"Coffe"`, "Food", "Transport", "record it without a category"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want it to mention %q", reason, want)
		}
	}
	w.finance.mu.Lock()
	defer w.finance.mu.Unlock()
	if n := len(w.finance.categories[w.user]); n != len(finance.DefaultCategories) {
		t.Fatalf("a refused proposal left %d categories, want the %d seeded ones",
			n, len(finance.DefaultCategories))
	}
}

// Approving is what writes, and it writes what the proposal showed -- including
// the category, which the stored input carries by name and the run resolves
// back to the id.
func TestAnApprovedExpenseIsCreatedAsProposed(t *testing.T) {
	w := newWorld()
	call := prepare(t, w, CreateExpense, Args{
		"amount": "₹450.50", "category": "food", "description": "lunch", "date": "yesterday",
	})
	if n := w.totalWrites(); n != 0 {
		t.Fatalf("preparing wrote %d times", n)
	}
	// The stored input is the readable one: a name, not a uuid, because it is
	// what the user is shown before approving.
	in := input(t, call)
	if in["category"] != "Food" {
		t.Fatalf("the stored category = %v, want the name Food", in["category"])
	}

	id := w.ledger.add(w.user, call, "proposed")
	exec, err := w.reg.RunApproved(context.Background(), w.user, id)
	if err != nil || exec.Err != nil {
		t.Fatal(err, exec.Err)
	}
	if len(w.finance.creates) != 1 {
		t.Fatalf("creates = %+v", w.finance.creates)
	}
	created := w.finance.creates[0]
	switch {
	case created.Amount != 45050:
		t.Fatalf("created %s, want 450.50", created.Amount)
	case created.Currency != "INR":
		t.Fatalf("currency = %q", created.Currency)
	case !created.Date.Equal(day(2026, time.September, 9)):
		t.Fatalf("date = %v, want yesterday", created.Date)
	case created.CategoryID == nil:
		t.Fatal("the approved write lost the category")
	}
	if len(exec.Result.Expenses) != 1 || exec.Result.Count() != 1 {
		t.Fatalf("result = %+v", exec.Result)
	}
}

// --- search_expenses --------------------------------------------------------------

func TestSearchExpensesResolvesThePeriod(t *testing.T) {
	w := newWorld()
	for name, tc := range map[string]struct {
		args    Args
		start   string
		end     string
		summary string
	}{
		// No dates at all is the whole history, not a window nobody asked for.
		// The module offers an unbounded read, and the summary says so.
		"nothing given": {
			Args{}, "", "",
			"Look at the recorded expenses over the whole recorded history.",
		},
		"a single day": {
			Args{"start": "yesterday"}, "2026-09-09", "2026-09-09",
			"Look at the recorded expenses on Wed 9 Sep 2026.",
		},
		"a named stretch": {
			Args{"start": "last month"}, "2026-08-01", "2026-08-31",
			"Look at the recorded expenses from Sat 1 Aug 2026 to Mon 31 Aug 2026.",
		},
		// The far end is inclusive of its day: September is the 1st to the
		// 30th, and an expense on the 30th is in it.
		"a range": {
			Args{"start": "2026-09-01", "end": "2026-09-30"}, "2026-09-01", "2026-09-30",
			"Look at the recorded expenses from Tue 1 Sep 2026 to Wed 30 Sep 2026.",
		},
		"an end alone": {
			Args{"until": "2026-09-05"}, "", "2026-09-05",
			"Look at the recorded expenses up to Sat 5 Sep 2026.",
		},
		"a category the user has": {
			Args{"category": "transport"}, "", "",
			`Look at the recorded expenses filed under "Transport" over the whole recorded history.`,
		},
		// A word that is not one of the user's categories is searched for in
		// the descriptions instead of filtering to nothing.
		"a word that is not a category": {
			Args{"category": "coffee"}, "", "",
			`Look at the recorded expenses for "coffee" over the whole recorded history.`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			call := prepare(t, w, SearchExpenses, tc.args)
			in := input(t, call)
			if start, _ := in["start"].(string); start != tc.start {
				t.Fatalf("start = %v, want %q", in["start"], tc.start)
			}
			if end, _ := in["end"].(string); end != tc.end {
				t.Fatalf("end = %v, want %q", in["end"], tc.end)
			}
			if call.Permission != Read {
				t.Fatalf("permission = %v", call.Permission)
			}
			if call.Summary != tc.summary {
				t.Fatalf("summary  = %q\nwant     = %q", call.Summary, tc.summary)
			}
		})
	}

	if reason := declined(t, w, SearchExpenses, Args{"start": "the 32nd of Octember"}); !strings.Contains(reason, "could not read") {
		t.Fatalf("reason = %q", reason)
	}
	if reason := declined(t, w, SearchExpenses, Args{"start": "2026-09-18", "end": "2026-09-14"}); !strings.Contains(reason, "ends before it starts") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestSearchExpensesReturnsOnlyTheOwnersExpenses(t *testing.T) {
	w := newWorld()
	lunch := w.finance.seed(w.user, 45000, "Food", "lunch with the team", day(2026, time.September, 9))
	w.finance.seed(w.user, 200000, "Housing", "rent", day(2026, time.August, 1))
	w.finance.seed(uuid.New(), 99900, "Food", "somebody else's lunch", day(2026, time.September, 9))

	res, err := w.reg.RunRead(context.Background(), w.user,
		prepare(t, w, SearchExpenses, Args{"start": "2026-09-01", "end": "2026-09-30"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Expenses) != 1 || res.Expenses[0].ID != lunch.ID {
		t.Fatalf("found %+v, want only the owner's September expense", res.Expenses)
	}
	// The recorded output names the period as well as what was in it, so the
	// action log says which days "nothing" was about.
	out, _ := json.Marshal(res.Output)
	for _, want := range []string{`"count":1`, `"start":"2026-09-01"`, `"end":"2026-09-30"`, `"amount":450.00`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output = %s, want it to contain %s", out, want)
		}
	}
}

// An unbounded read is unbounded: no dates means the whole history, and the
// output says the period was open rather than naming days it did not use.
func TestSearchExpensesWithNoDatesLooksAtEverything(t *testing.T) {
	w := newWorld()
	w.finance.seed(w.user, 200000, "Housing", "rent", day(2024, time.January, 1))

	res, err := w.reg.RunRead(context.Background(), w.user, prepare(t, w, SearchExpenses, Args{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Expenses) != 1 {
		t.Fatalf("an unbounded search found %d, want the old expense", len(res.Expenses))
	}
	out, _ := json.Marshal(res.Output)
	if !strings.Contains(string(out), `"start":null`) || !strings.Contains(string(out), `"end":null`) {
		t.Fatalf("output = %s, want both bounds null", out)
	}
}

// --- analyze_spending -------------------------------------------------------------

// With no dates, an analysis is about the current month -- and says so. "How
// much am I spending" is a question about now, and a total over three years
// presented as its answer would be wrong in a way the user could not see.
func TestAnalyzeSpendingDefaultsToThisMonthAndSaysSo(t *testing.T) {
	w := newWorld()
	call := prepare(t, w, AnalyzeSpending, Args{})
	in := input(t, call)
	if in["start"] != "2026-09-01" || in["end"] != "2026-09-10" {
		t.Fatalf("period = %v to %v, want the month so far", in["start"], in["end"])
	}
	if want := "Add up what was spent from Tue 1 Sep 2026 to Thu 10 Sep 2026."; call.Summary != want {
		t.Fatalf("summary  = %q\nwant     = %q", call.Summary, want)
	}
}

// The arithmetic happens in the store and arrives written out, because this is
// the number the user will compare against their bank.
func TestAnalyzeSpendingTotalsByCategory(t *testing.T) {
	w := newWorld()
	w.finance.seed(w.user, 45000, "Food", "lunch", day(2026, time.September, 2))
	w.finance.seed(w.user, 15050, "Food", "coffee", day(2026, time.September, 3))
	w.finance.seed(w.user, 30000, "Transport", "taxi", day(2026, time.September, 4))
	w.finance.seed(w.user, 10000, "", "something", day(2026, time.September, 5))
	w.finance.seed(w.user, 999999, "Food", "last month's dinner", day(2026, time.August, 30))
	w.finance.seed(uuid.New(), 500000, "Food", "somebody else's feast", day(2026, time.September, 2))

	res, err := w.reg.RunRead(context.Background(), w.user, prepare(t, w, AnalyzeSpending, Args{}))
	if err != nil {
		t.Fatal(err)
	}
	// 450.00 + 150.50 + 300.00 + 100.00, and nothing from August or from
	// anybody else.
	out, _ := json.Marshal(res.Output)
	for _, want := range []string{`"total":1000.50`, `"count":4`, `"currency":"INR"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output = %s, want it to contain %s", out, want)
		}
	}

	// The report is one source, already worded, with every figure written out
	// and none left for the model to compute.
	if len(res.Reports) != 1 || res.Count() != 1 {
		t.Fatalf("result = %+v, want one report", res)
	}
	text := res.Reports[0].Text
	for _, want := range []string{
		"INR 1000.50", "4 expenses", "Food: INR 600.50 (2 expenses)",
		"Transport: INR 300.00 (1 expense)", "no category: INR 100.00",
		"the expenses the user recorded themselves",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report = %q\nwant it to contain %q", text, want)
		}
	}
	if len(res.Expenses) != 0 {
		t.Fatal("an analysis returned raw rows as well as the totals")
	}
}

// Two currencies are two totals and the report says they are not added
// together: nothing here knows a rate, so a combined figure would be invented.
func TestAnalyzeSpendingKeepsCurrenciesApart(t *testing.T) {
	w := newWorld()
	w.finance.seed(w.user, 50000, "Food", "lunch", day(2026, time.September, 2))
	usd := w.finance.seed(w.user, 2000, "Food", "an ebook", day(2026, time.September, 3))
	w.finance.mu.Lock()
	for i := range w.finance.byUser[w.user] {
		if w.finance.byUser[w.user][i].ID == usd.ID {
			w.finance.byUser[w.user][i].Currency = "USD"
		}
	}
	w.finance.mu.Unlock()

	res, err := w.reg.RunRead(context.Background(), w.user, prepare(t, w, AnalyzeSpending, Args{}))
	if err != nil {
		t.Fatal(err)
	}
	text := res.Reports[0].Text
	for _, want := range []string{"INR 500.00", "USD 20.00", "not added together"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report = %q\nwant it to contain %q", text, want)
		}
	}
	if strings.Contains(text, "520.00") {
		t.Fatalf("the report added two currencies together: %q", text)
	}
}

// Nothing recorded is said as nothing recorded, over the dates it looked at --
// never as a total of zero and never as a claim about what the user spends.
func TestAnalyzeSpendingSaysWhenThereIsNothing(t *testing.T) {
	w := newWorld()
	res, err := w.reg.RunRead(context.Background(), w.user,
		prepare(t, w, AnalyzeSpending, Args{"start": "2026-09-01", "end": "2026-09-30"}))
	if err != nil {
		t.Fatal(err)
	}
	text := res.Reports[0].Text
	if !strings.Contains(text, "No expenses") || !strings.Contains(text, "Tue 1 Sep 2026") {
		t.Fatalf("report = %q, want it to say nothing is recorded over those days", text)
	}
}

func TestAnalyzeSpendingNeverCrossesUsers(t *testing.T) {
	w := newWorld()
	w.finance.seed(uuid.New(), 500000, "Food", "their groceries", day(2026, time.September, 2))
	res, err := w.reg.RunRead(context.Background(), w.user, prepare(t, w, AnalyzeSpending, Args{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Reports[0].Text, "No expenses") {
		t.Fatalf("analyze_spending saw another user's money: %q", res.Reports[0].Text)
	}
}

// amountText reads the amount back out of a stored input as it was written,
// rather than through a float64 -- which is the whole point of writing it as an
// exact decimal in the first place.
func amountText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return string(fields["amount"])
}

// A bare "5 September", in a module that reads backwards.
//
// calendarDate resolves a year-less date to the *next* one, because it was
// written for deadlines. An expense is behind you: "5 September" said on the
// 10th of September 2026 is this year's, and resolving it forward would record
// a purchase dated 2027 that no total for any period the user asks about would
// ever include.
func TestFinanceDatesResolveBackwards(t *testing.T) {
	w := newWorld()

	// testNow is Thu 10 September 2026, so the 30th of September has not
	// happened yet and the 5th has.
	for name, tc := range map[string]struct {
		args Args
		want string
	}{
		"a day this month that has passed": {
			Args{"amount": "500", "date": "5 September"}, "2026-09-05"},
		"a day last year": {
			Args{"amount": "500", "date": "30 December"}, "2025-12-30"},
		// An explicit year is what the user said, and is honoured.
		"an explicit past year": {
			Args{"amount": "500", "date": "2024-02-29"}, "2024-02-29"},
		"yesterday": {
			Args{"amount": "500", "date": "yesterday"}, "2026-09-09"},
	} {
		t.Run(name, func(t *testing.T) {
			in := input(t, prepare(t, w, CreateExpense, tc.args))
			if in["date"] != tc.want {
				t.Fatalf("date = %v, want %v", in["date"], tc.want)
			}
		})
	}

	// And what is still in the future is declined rather than recorded: money
	// cannot have been spent on a day that has not happened, and an expense
	// dated forwards falls outside every period anybody asks about.
	for _, raw := range []string{"tomorrow", "next friday", "2027-01-05"} {
		reason := declined(t, w, CreateExpense, Args{"amount": "500", "date": raw})
		if !strings.Contains(reason, "cannot be dated in the future") {
			t.Fatalf("date %q = %q, want it refused as a future date", raw, reason)
		}
	}

	// The same backwards reading for a period that is read, not written.
	call := prepare(t, w, SearchExpenses, Args{"start": "1 September", "end": "30 September"})
	in := input(t, call)
	if in["start"] != "2026-09-01" || in["end"] != "2026-09-30" {
		t.Fatalf("period = %v to %v, want this September", in["start"], in["end"])
	}
}
