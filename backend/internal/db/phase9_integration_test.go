package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/finance"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/optional"
)

// These cover the Phase 9 SQL: the numeric column that makes money exact, the
// GROUP BY behind the summary, the trigger that seeds a new user's categories,
// the three cascades that differ, and the node the delete trigger removes.
// Cross-user isolation over the whole stack lives in
// internal/api/finance_isolation_test.go.

var (
	sep01 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sep17 = time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	sep30 = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	oct01 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

// categoryID looks one of a user's seeded categories up by name.
func categoryID(t *testing.T, repo *finance.Repository, owner uuid.UUID, name string) uuid.UUID {
	t.Helper()
	all, err := repo.Categories(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("%s has no category called %q; it has %+v", owner, name, all)
	return uuid.Nil
}

func seedExpense(t *testing.T, repo *finance.Repository, owner uuid.UUID, amount, currency string,
	category *uuid.UUID, description string, on time.Time,
) finance.Expense {
	t.Helper()
	a, err := finance.ParseAmount(amount)
	if err != nil {
		t.Fatal(err)
	}
	if currency == "" {
		currency = finance.DefaultCurrency
	}
	e, err := repo.Create(context.Background(), owner, finance.CreateInput{
		Amount: a, Currency: currency, CategoryID: category, Description: description, Date: on,
	})
	if err != nil {
		t.Fatalf("create %s: %v", description, err)
	}
	return e
}

// Registering a user gives them the default categories, through the trigger
// migration 000009 puts on `users` -- not through anything the registration
// code remembers to call.
func TestRegisteringAUserSeedsTheDefaultCategories(t *testing.T) {
	pool := testDB(t)
	repo := finance.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	got, err := repo.Categories(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(finance.DefaultCategories) {
		t.Fatalf("a new user has %d categories, want the %d defaults: %+v",
			len(got), len(finance.DefaultCategories), got)
	}
	have := map[string]bool{}
	for _, c := range got {
		have[c.Name] = true
		if c.UserID != owner {
			t.Fatalf("category %q belongs to %v, not the new user", c.Name, c.UserID)
		}
	}
	for _, want := range finance.DefaultCategories {
		if !have[want] {
			t.Fatalf("a new user has no %q category: %+v", want, got)
		}
	}

	// Two users' "Food" are two rows, and one user's second "Food" is refused
	// by the unique index.
	other := makeUser(t, pool, "bob@example.com")
	if categoryID(t, repo, owner, "Food") == categoryID(t, repo, other, "Food") {
		t.Fatal("two users share a category row")
	}
	// Case-insensitively: "Food" and "food" are one category, because
	// CategoryByName resolves case-insensitively and two rows would make "log
	// it under food" ambiguous and split a breakdown in two.
	// Whitespace is the service's business, not the index's, so this is only
	// the case rule.
	for _, written := range []string{"Food", "food", "FOOD"} {
		if _, err := repo.CreateCategory(context.Background(), owner, written); !errors.Is(err, finance.ErrCategoryExists) {
			t.Fatalf("a duplicate category %q = %v, want ErrCategoryExists", written, err)
		}
	}
}

func TestExpenseRepositoryRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := finance.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	food := categoryID(t, repo, owner, "Food")

	created := seedExpense(t, repo, owner, "450.50", "INR", &food, "lunch with the team", sep17)
	switch {
	case created.Amount != 45050:
		t.Fatalf("amount = %s, want 450.50 stored exactly", created.Amount)
	case created.Currency != "INR":
		t.Fatalf("currency = %q", created.Currency)
	case !created.Date.Equal(sep17):
		t.Fatalf("expense_date = %v, want the day at midnight UTC", created.Date)
	case created.CategoryID == nil || *created.CategoryID != food:
		t.Fatalf("category_id = %v", created.CategoryID)
	// The join is what puts the name on the row, so a client never resolves
	// an id and the assistant never sees one.
	case created.CategoryName == nil || *created.CategoryName != "Food":
		t.Fatalf("category = %v, want the joined name", created.CategoryName)
	}

	// A partial update touches only the columns it names -- and clearing a
	// nullable one is different from leaving it alone.
	transport := categoryID(t, repo, owner, "Transport")
	updated, err := repo.Update(ctx, owner, created.ID, finance.Patch{
		Amount:     ptr(finance.Amount(50000)),
		CategoryID: optional.Of(transport),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	switch {
	case updated.Amount != 50000:
		t.Fatalf("amount = %s, want 500.00", updated.Amount)
	case updated.CategoryName == nil || *updated.CategoryName != "Transport":
		t.Fatalf("category = %v, want Transport", updated.CategoryName)
	case updated.Description == nil || *updated.Description != "lunch with the team":
		t.Fatalf("an unmentioned column was overwritten: %v", updated.Description)
	case !updated.UpdatedAt.After(updated.CreatedAt):
		t.Fatalf("updated_at did not move: %v vs %v", updated.UpdatedAt, updated.CreatedAt)
	}
	cleared, err := repo.Update(ctx, owner, created.ID, finance.Patch{
		Description: optional.Null[string](), CategoryID: optional.Null[uuid.UUID](),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Description != nil || cleared.CategoryID != nil || cleared.CategoryName != nil {
		t.Fatalf("clearing left %+v", cleared)
	}

	if err := repo.Delete(ctx, owner, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ByID(ctx, owner, created.ID); !errors.Is(err, finance.ErrNotFound) {
		t.Fatalf("after delete, ByID = %v, want ErrNotFound", err)
	}
}

// Money is exact in the column as well as in Go. numeric(12,2) holds what was
// written, and a hundred lots of 0.10 sum to 10.00 -- which is the whole reason
// the amount is not a float anywhere in this stack.
func TestAmountsSurviveTheColumnExactly(t *testing.T) {
	pool := testDB(t)
	repo := finance.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	for _, written := range []string{"0.01", "0.10", "12.34", "1234.56", "9999999999.99"} {
		e := seedExpense(t, repo, owner, written, "", nil, written, sep17)
		want, _ := finance.ParseAmount(written)
		if e.Amount != want {
			t.Fatalf("%s came back as %s", written, e.Amount)
		}
		back, err := repo.ByID(context.Background(), owner, e.ID)
		if err != nil || back.Amount != want {
			t.Fatalf("%s read back as %s, %v", written, back.Amount, err)
		}
	}

	pennies := makeUser(t, pool, "penny@example.com")
	for i := 0; i < 100; i++ {
		seedExpense(t, repo, pennies, "0.10", "", nil, "a tenth", sep17)
	}
	summary, err := repo.Summarize(context.Background(), pennies, finance.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Currencies) != 1 || summary.Currencies[0].Total.String() != "10.00" {
		t.Fatalf("a hundred lots of 0.10 = %+v, want 10.00", summary.Currencies)
	}
}

// The date range is inclusive on both ends, and the summary and the list agree
// about which expenses a period holds -- they share one WHERE for exactly that
// reason.
func TestExpenseRangeQueryIncludesBothEnds(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := finance.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	for _, on := range []time.Time{sep01, sep17, sep30, oct01} {
		seedExpense(t, repo, owner, "100.00", "", nil, on.Format(time.DateOnly), on)
	}

	september := finance.Filter{Start: &sep01, End: &sep30, Limit: 100, Sort: finance.DefaultSort}
	listed, err := repo.List(ctx, owner, september)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("September holds %d expenses, want the 1st, the 17th and the 30th", len(listed))
	}
	// Newest first: a ledger reads backwards.
	if !listed[0].Date.Equal(sep30) || !listed[2].Date.Equal(sep01) {
		t.Fatalf("the order is %v, %v, %v; want newest first",
			listed[0].Date, listed[1].Date, listed[2].Date)
	}
	summary, err := repo.Summarize(ctx, owner, september)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Count != len(listed) || summary.Currencies[0].Total.String() != "300.00" {
		t.Fatalf("the summary covers %+v, the list covers %d rows", summary, len(listed))
	}
}

// The aggregate is one GROUP BY: totals per currency, and per category within
// it, with the uncategorized expenses a line of their own rather than dropped.
func TestSummarizeGroupsByCurrencyAndCategory(t *testing.T) {
	pool := testDB(t)
	repo := finance.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	food := categoryID(t, repo, owner, "Food")
	transport := categoryID(t, repo, owner, "Transport")

	seedExpense(t, repo, owner, "450.00", "INR", &food, "lunch", sep17)
	seedExpense(t, repo, owner, "150.50", "INR", &food, "coffee", sep17)
	seedExpense(t, repo, owner, "300.00", "INR", &transport, "taxi", sep17)
	seedExpense(t, repo, owner, "100.00", "INR", nil, "something", sep17)
	seedExpense(t, repo, owner, "20.00", "USD", &food, "an ebook", sep17)

	summary, err := repo.Summarize(context.Background(), owner, finance.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Count != 5 || len(summary.Currencies) != 2 {
		t.Fatalf("summary = %+v, want 5 expenses in 2 currencies", summary)
	}
	// Largest first, and each total is of its own currency only: there is no
	// rate anywhere in this system, so there is no combined figure.
	inr, usd := summary.Currencies[0], summary.Currencies[1]
	if inr.Currency != "INR" || inr.Total.String() != "1000.50" || inr.Count != 4 {
		t.Fatalf("INR = %+v, want 1000.50 across 4", inr)
	}
	if usd.Currency != "USD" || usd.Total.String() != "20.00" {
		t.Fatalf("USD = %+v, want 20.00", usd)
	}

	// The lines add up to the total, which is what makes the breakdown usable
	// as an answer.
	var sum finance.Amount
	uncategorized := 0
	for _, line := range inr.Categories {
		sum += line.Total
		if line.Uncategorized() {
			uncategorized++
		}
	}
	if sum != inr.Total {
		t.Fatalf("the INR lines add up to %s, the total says %s", sum, inr.Total)
	}
	if uncategorized != 1 {
		t.Fatalf("%d uncategorized lines, want exactly one", uncategorized)
	}
	if inr.Categories[0].Category != "Food" || inr.Categories[0].Total.String() != "600.50" {
		t.Fatalf("the biggest INR line is %+v, want Food 600.50", inr.Categories[0])
	}
}

// The CHECK constraints, from the database's side: they hold whatever writes to
// the table, not only when the service remembered to validate.
func TestExpenseConstraints(t *testing.T) {
	pool := testDB(t)
	owner := makeUser(t, pool, "ada@example.com")

	for name, stmt := range map[string]string{
		"an expense of nothing": `INSERT INTO expenses (user_id, amount, expense_date)
			VALUES ($1, 0, '2026-09-17')`,
		"a negative expense": `INSERT INTO expenses (user_id, amount, expense_date)
			VALUES ($1, -40.00, '2026-09-17')`,
		"no date": `INSERT INTO expenses (user_id, amount) VALUES ($1, 100.00)`,
		"a category that is not a category": `INSERT INTO expenses (user_id, amount, expense_date, category_id)
			VALUES ($1, 100.00, '2026-09-17', '11111111-1111-4111-8111-111111111111')`,
	} {
		if _, err := pool.Exec(stmt, owner); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}

	// And the column's scale: more precision than it holds is rounded by
	// Postgres, which is why nothing in Go ever sends it more.
	var stored string
	if err := pool.QueryRow(`INSERT INTO expenses (user_id, amount, expense_date)
		VALUES ($1, 12.345, '2026-09-17') RETURNING amount::text`, owner).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "12.35" && stored != "12.34" {
		t.Fatalf("12.345 stored as %q, want it rounded to two places", stored)
	}
}

// Deleting a user takes their expenses and their categories; deleting a
// category leaves the expenses and un-files them -- the money was still spent.
// The same for a receipt.
func TestFinanceDeletionCascades(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := finance.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	food := categoryID(t, repo, owner, "Food")

	e := seedExpense(t, repo, owner, "450.00", "INR", &food, "lunch", sep17)
	if _, err := pool.Exec(`DELETE FROM expense_categories WHERE id = $1`, food); err != nil {
		t.Fatal(err)
	}
	orphaned, err := repo.ByID(ctx, owner, e.ID)
	if err != nil {
		t.Fatalf("the expense went with its category: %v", err)
	}
	if orphaned.CategoryID != nil || orphaned.CategoryName != nil {
		t.Fatalf("category = %v/%v, want null once the category is gone",
			orphaned.CategoryID, orphaned.CategoryName)
	}

	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"expenses", "expense_categories"} {
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows of %s survived the deleted user", n, table)
		}
	}
}

// An expense node follows its row: it is created with it, renamed with it, and
// removed by the trigger however the row goes.
func TestAnExpenseNodeFollowsItsRow(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	graphSvc := graph.NewService(graph.Deps{Store: graph.NewRepository(pool)})
	repo := finance.NewRepository(pool)
	svc := finance.NewService(repo, finance.WithNodeSync(graphSvc))
	owner := makeUser(t, pool, "ada@example.com")
	food := categoryID(t, repo, owner, "Food")

	amount, _ := finance.ParseAmount("450.00")
	e, err := svc.Create(ctx, owner, finance.CreateInput{
		Amount: amount, CategoryID: &food, Description: "lunch with the team", Date: sep17,
	})
	if err != nil {
		t.Fatal(err)
	}

	node := func() (string, string) {
		t.Helper()
		var nodeType, label string
		err := pool.QueryRow(`SELECT type, label FROM knowledge_nodes
			WHERE user_id = $1 AND ref_table = 'expenses' AND ref_id = $2`, owner, e.ID).Scan(&nodeType, &label)
		if err != nil {
			t.Fatalf("the expense has no node: %v", err)
		}
		return nodeType, label
	}
	nodeType, label := node()
	if nodeType != graph.NodeExpense || label != "lunch with the team" {
		t.Fatalf("node = %q/%q, want an expense node labelled with the description", nodeType, label)
	}

	// Re-describing it renames the node: a node carrying wording the user has
	// stopped using is one the chat mention scan matches on the wrong thing.
	if _, err := svc.Update(ctx, owner, e.ID, finance.UpdateInput{
		Description: optional.Of("lunch with the design team"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, label := node(); label != "lunch with the design team" {
		t.Fatalf("the node label is %q, want the new description", label)
	}

	// Categories are not mirrored: a category is a label on other rows.
	var categoryNodes int
	if err := pool.QueryRow(`SELECT count(*) FROM knowledge_nodes
		WHERE user_id = $1 AND ref_table = 'expense_categories'`, owner).Scan(&categoryNodes); err != nil {
		t.Fatal(err)
	}
	if categoryNodes != 0 {
		t.Fatalf("%d category nodes exist; categories are not mirrored", categoryNodes)
	}

	// And the node goes with the row, through the trigger -- including on a
	// path the service never sees.
	if _, err := pool.Exec(`DELETE FROM expenses WHERE id = $1`, e.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(`SELECT count(*) FROM knowledge_nodes
		WHERE ref_table = 'expenses'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d expense nodes survived the deleted expense", left)
	}
}
