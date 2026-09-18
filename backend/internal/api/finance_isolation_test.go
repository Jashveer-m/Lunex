package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Phase 9 half of the property the whole ownership design exists for, over
// the real router, the real service and real SQL.
//
// These are skipped unless TEST_DATABASE_URL is set; see docs/testing.md.

const (
	expenseDay   = "2026-09-17"
	monthStart   = "2026-09-01"
	monthEnd     = "2026-09-30"
	spendWindow  = "start=" + monthStart + "&end=" + monthEnd
	otherMonth   = "start=2026-10-01&end=2026-10-31"
	lunchAmount  = 450.5
	lunchAsMoney = "450.50"
)

// categories returns the caller's expense categories, by name.
func categories(t *testing.T, srv *httptest.Server, token string) map[string]string {
	t.Helper()
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/expense-categories", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /expense-categories: status %d: %v", resp.StatusCode, body)
	}
	out := map[string]string{}
	list, _ := body["categories"].([]any)
	for _, raw := range list {
		c, _ := raw.(map[string]any)
		out[fmt.Sprint(c["name"])] = fmt.Sprint(c["id"])
	}
	return out
}

func expensesIn(t *testing.T, srv *httptest.Server, token, query string) map[string]any {
	t.Helper()
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/expenses?"+query, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /expenses?%s: status %d: %v", query, resp.StatusCode, body)
	}
	return body
}

func summaryOf(t *testing.T, srv *httptest.Server, token, query string) map[string]any {
	t.Helper()
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/expenses/summary?"+query, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /expenses/summary?%s: status %d: %v", query, resp.StatusCode, body)
	}
	return body
}

// Registration gives every user the default categories, and they are their own.
func TestEveryUserStartsWithTheirOwnCategories(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	aliceCats, bobCats := categories(t, srv, alice), categories(t, srv, bob)
	for _, want := range []string{"Food", "Transport", "Housing", "Utilities", "Other"} {
		if aliceCats[want] == "" {
			t.Fatalf("a new user has no %q category: %v", want, aliceCats)
		}
	}
	if aliceCats["Food"] == bobCats["Food"] {
		t.Fatal("two users share a category row")
	}

	// A category of her own joins them; a name she already has is a field
	// error rather than a second row.
	created := create(t, srv, alice, "/api/v1/expense-categories", map[string]any{"name": "Books"})
	if created == "" {
		t.Fatal("the category was created without an id")
	}
	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/expense-categories", alice,
		map[string]any{"name": "food"})
	if resp.StatusCode != http.StatusBadRequest || body["error"] != "validation_failed" {
		t.Fatalf("a duplicate category = %d %v, want a 400", resp.StatusCode, body)
	}
	if n := len(categories(t, srv, alice)); n != 6 {
		t.Fatalf("Alice has %d categories, want the five defaults plus Books", n)
	}
	// And Bob's list did not grow.
	if n := len(categories(t, srv, bob)); n != 5 {
		t.Fatalf("Bob has %d categories after Alice added one", n)
	}
}

func TestCrossUserExpenseIsolation(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	expenseID := create(t, srv, alice, "/api/v1/expenses", map[string]any{
		"amount": lunchAmount, "expense_date": expenseDay,
		"description": "Alice's lunch", "category_id": categories(t, srv, alice)["Food"],
	})

	// Alice's expense exists and Bob does not own it. The answer must be 404 --
	// a 403 would confirm the id is real.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"get expense", http.MethodGet, "/api/v1/expenses/" + expenseID, nil},
		{"patch expense", http.MethodPatch, "/api/v1/expenses/" + expenseID, map[string]any{"amount": 1}},
		{"redate expense", http.MethodPatch, "/api/v1/expenses/" + expenseID, map[string]any{"expense_date": "2026-01-01"}},
		{"delete expense", http.MethodDelete, "/api/v1/expenses/" + expenseID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, srv, tc.method, tc.path, bob, tc.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s as the wrong user = %d, want 404: %v", tc.method, tc.path, resp.StatusCode, body)
			}
			if body["error"] != "not_found" {
				t.Fatalf("error = %v, want not_found", body["error"])
			}
		})
	}

	// Bob's ledger never contains Alice's expense -- not in the list, not in
	// the unbounded list, and not in the total, which is the one that matters
	// most: a number is believed.
	if count, _ := expensesIn(t, srv, bob, spendWindow)["count"].(float64); count != 0 {
		t.Fatalf("Bob's September holds %v expenses, want 0", count)
	}
	if count, _ := expensesIn(t, srv, bob, "")["count"].(float64); count != 0 {
		t.Fatalf("Bob's whole history holds %v expenses, want 0", count)
	}
	bobTotal := summaryOf(t, srv, bob, "")
	if count, _ := bobTotal["count"].(float64); count != 0 {
		t.Fatalf("Bob's spending summary counts %v expenses", count)
	}
	if currencies, _ := bobTotal["currencies"].([]any); len(currencies) != 0 {
		t.Fatalf("Bob's spending summary holds %v", currencies)
	}

	// And none of that touched Alice's expense: every attempt was a no-op, not
	// just an unhelpful status code.
	resp, expense := doJSON(t, srv, http.MethodGet, "/api/v1/expenses/"+expenseID, alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Alice's expense after Bob's attempts = %d %v", resp.StatusCode, expense)
	}
	if expense["description"] != "Alice's lunch" || expense["expense_date"] != expenseDay {
		t.Fatalf("Alice's expense after Bob's attempts = %v", expense)
	}
	if amount, _ := expense["amount"].(float64); amount != lunchAmount {
		t.Fatalf("amount = %v, want %v", expense["amount"], lunchAmount)
	}
}

// Filing an expense under another user's category or receipt would leak the
// existence of a foreign id through an otherwise-successful write.
func TestCrossUserExpenseLinksAreRejected(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	aliceFood := categories(t, srv, alice)["Food"]
	bobExpense := create(t, srv, bob, "/api/v1/expenses", map[string]any{
		"amount": 100, "expense_date": expenseDay, "description": "Bob's coffee",
	})

	for name, body := range map[string]any{
		"another user's category": map[string]any{
			"amount": 100, "expense_date": expenseDay, "category_id": aliceFood,
		},
		"a category that does not exist": map[string]any{
			"amount": 100, "expense_date": expenseDay,
			"category_id": "11111111-1111-4111-8111-111111111111",
		},
		"a document that does not exist": map[string]any{
			"amount": 100, "expense_date": expenseDay,
			"related_document_id": "11111111-1111-4111-8111-111111111111",
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp, out := doJSON(t, srv, http.MethodPost, "/api/v1/expenses", bob, body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("filing under %s = %d, want 404: %v", name, resp.StatusCode, out)
			}
		})
	}
	resp, out := doJSON(t, srv, http.MethodPatch, "/api/v1/expenses/"+bobExpense, bob,
		map[string]any{"category_id": aliceFood})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("patching in another user's category = %d, want 404: %v", resp.StatusCode, out)
	}

	// Nothing was created behind those refusals, and Bob's own expense is
	// untouched.
	if count, _ := expensesIn(t, srv, bob, "")["count"].(float64); count != 1 {
		t.Fatalf("Bob has %v expenses, want only the one he made", count)
	}
	resp, expense := doJSON(t, srv, http.MethodGet, "/api/v1/expenses/"+bobExpense, bob, nil)
	if resp.StatusCode != http.StatusOK || expense["category_id"] != nil {
		t.Fatalf("Bob's expense = %d %v, want no category", resp.StatusCode, expense)
	}
}

// Unlike the calendar, the range is optional: a finance history query with no
// dates is a reasonable question about a table that grows a row per purchase.
// What is refused is a range that runs backwards, or one that is not a date.
func TestExpenseListAcceptsNoRange(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	create(t, srv, alice, "/api/v1/expenses", map[string]any{
		"amount": lunchAmount, "expense_date": expenseDay, "description": "lunch",
	})
	create(t, srv, alice, "/api/v1/expenses", map[string]any{
		"amount": 12000, "expense_date": "2024-02-29", "description": "an old thing",
	})

	all := expensesIn(t, srv, alice, "")
	if count, _ := all["count"].(float64); count != 2 {
		t.Fatalf("an unbounded read = %v expenses, want both", count)
	}
	if all["start"] != nil || all["end"] != nil {
		t.Fatalf("an unbounded read echoed a window back: %v to %v", all["start"], all["end"])
	}

	// A window narrows it, both ends included.
	september := expensesIn(t, srv, alice, spendWindow)
	if count, _ := september["count"].(float64); count != 1 {
		t.Fatalf("September = %v expenses, want the lunch", count)
	}
	if september["start"] != monthStart || september["end"] != monthEnd {
		t.Fatalf("the window was not echoed back: %v to %v", september["start"], september["end"])
	}
	if count, _ := expensesIn(t, srv, alice, otherMonth)["count"].(float64); count != 0 {
		t.Fatalf("October holds %v of September's expenses", count)
	}

	for name, query := range map[string]string{
		"backwards":         "?start=" + monthEnd + "&end=" + monthStart,
		"an unreadable day": "?start=soon",
		"a bad sort":        "?sort=totally_not_a_column",
		"a bad category":    "?category_id=not-an-id",
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/expenses"+query, alice, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("GET /expenses%s = %d, want 400: %v", query, resp.StatusCode, body)
			}
			if body["error"] != "validation_failed" {
				t.Fatalf("error = %v, want validation_failed", body["error"])
			}
		})
	}
}

// The two filters that are not a date: one category, and the expenses filed
// under none -- which is not expressible as a category id and is how "what have
// I not categorised" gets asked.
func TestExpensesCanBeFilteredByCategoryOrTheLackOfOne(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	cats := categories(t, srv, alice)

	create(t, srv, alice, "/api/v1/expenses", map[string]any{
		"amount": 450, "expense_date": expenseDay, "category_id": cats["Food"], "description": "lunch",
	})
	create(t, srv, alice, "/api/v1/expenses", map[string]any{
		"amount": 300, "expense_date": expenseDay, "category_id": cats["Transport"], "description": "taxi",
	})
	create(t, srv, alice, "/api/v1/expenses", map[string]any{
		"amount": 100, "expense_date": expenseDay, "description": "something",
	})

	food := expensesIn(t, srv, alice, "category_id="+cats["Food"])
	if count, _ := food["count"].(float64); count != 1 {
		t.Fatalf("the Food filter returned %v expenses, want 1", count)
	}
	none := expensesIn(t, srv, alice, "category_id=none")
	if count, _ := none["count"].(float64); count != 1 {
		t.Fatalf("category_id=none returned %v expenses, want the uncategorised one", count)
	}
	first, _ := none["expenses"].([]any)
	if e, _ := first[0].(map[string]any); e["description"] != "something" || e["category"] != nil {
		t.Fatalf("category_id=none returned %v", e)
	}

	// The summary takes the same filter, so "how much on food" is one call.
	summary := summaryOf(t, srv, alice, "category_id="+cats["Food"])
	if total, _ := summary["currencies"].([]any); len(total) != 1 {
		t.Fatalf("the filtered summary holds %v", summary["currencies"])
	}
	inr, _ := summary["currencies"].([]any)[0].(map[string]any)
	if got, _ := inr["total"].(float64); got != 450 {
		t.Fatalf("the Food total is %v, want 450.00", inr["total"])
	}

	// And the text filter, which is the only way to reach a description.
	matching := expensesIn(t, srv, alice, "q=taxi")
	if count, _ := matching["count"].(float64); count != 1 {
		t.Fatalf("?q=taxi returned %v expenses, want the taxi", count)
	}
}

// The summary is the endpoint the assistant answers "how much did I spend"
// from, and it is exact: the money is a numeric column and an int64 of
// hundredths, never a float.
func TestSpendingSummaryIsExactAndGroupedByCurrency(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	cats := categories(t, srv, alice)

	for _, e := range []map[string]any{
		{"amount": lunchAmount, "expense_date": expenseDay, "category_id": cats["Food"], "description": "lunch"},
		{"amount": 150.5, "expense_date": expenseDay, "category_id": cats["Food"], "description": "coffee"},
		{"amount": 300, "expense_date": expenseDay, "category_id": cats["Transport"], "description": "taxi"},
		{"amount": 100, "expense_date": expenseDay, "description": "something"},
		{"amount": 20, "currency": "USD", "expense_date": expenseDay, "category_id": cats["Food"], "description": "an ebook"},
		{"amount": 999, "expense_date": "2026-08-30", "category_id": cats["Food"], "description": "last month"},
	} {
		create(t, srv, alice, "/api/v1/expenses", e)
	}

	summary := summaryOf(t, srv, alice, spendWindow)
	if count, _ := summary["count"].(float64); count != 5 {
		t.Fatalf("the summary counts %v expenses, want September's five", count)
	}
	currencies, _ := summary["currencies"].([]any)
	if len(currencies) != 2 {
		t.Fatalf("the summary holds %d currencies, want INR and USD: %v", len(currencies), currencies)
	}

	// Largest first, each total of its own currency only, and no grand total:
	// nothing here knows a rate.
	inr, _ := currencies[0].(map[string]any)
	if inr["currency"] != "INR" {
		t.Fatalf("the first currency is %v, want INR", inr["currency"])
	}
	// 450.50 + 150.50 + 300 + 100 -- and that it is 1001.00 rather than
	// 1000.9999999 is the point of the whole money type.
	if total, _ := inr["total"].(float64); total != 1001 {
		t.Fatalf("the INR total is %v, want 1001.00 exactly", inr["total"])
	}
	if _, hasGrandTotal := summary["total"]; hasGrandTotal {
		t.Fatal("the summary has a total across currencies; nothing here converts between them")
	}

	// The category lines add up to the total, with the uncategorized expense a
	// line of its own -- a breakdown whose lines do not add up misleads.
	lines, _ := inr["categories"].([]any)
	var sum, uncategorized float64
	for _, raw := range lines {
		line, _ := raw.(map[string]any)
		amount, _ := line["total"].(float64)
		sum += amount
		if line["category"] == nil {
			uncategorized = amount
		}
	}
	if sum != 1001 {
		t.Fatalf("the category lines add up to %v, the total says 1001.00", sum)
	}
	if uncategorized != 100 {
		t.Fatalf("the no-category line is %v, want 100", uncategorized)
	}
	biggest, _ := lines[0].(map[string]any)
	if biggest["category"] != "Food" {
		t.Fatalf("the biggest line is %v, want Food", biggest["category"])
	}
	if total, _ := biggest["total"].(float64); total != 601 {
		t.Fatalf("Food = %v, want 601.00", biggest["total"])
	}
}

// An amount is stored to the penny, whether it arrived as a JSON number or as a
// string, and a fraction the column cannot hold is refused rather than rounded.
func TestExpenseAmountsAreExactOverTheWire(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	for _, sent := range []any{lunchAmount, lunchAsMoney} {
		id := create(t, srv, alice, "/api/v1/expenses", map[string]any{
			"amount": sent, "expense_date": expenseDay, "description": fmt.Sprint(sent),
		})
		resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/expenses/"+id, alice, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET expense: %d %v", resp.StatusCode, body)
		}
		if amount, _ := body["amount"].(float64); amount != lunchAmount {
			t.Fatalf("%v came back as %v", sent, body["amount"])
		}
	}

	for name, body := range map[string]any{
		"nothing at all":   map[string]any{"amount": 0, "expense_date": expenseDay},
		"a negative sum":   map[string]any{"amount": -40, "expense_date": expenseDay},
		"three decimals":   map[string]any{"amount": 12.345, "expense_date": expenseDay},
		"a bad currency":   map[string]any{"amount": 100, "currency": "rupees", "expense_date": expenseDay},
		"no date":          map[string]any{"amount": 100},
		"an unknown field": map[string]any{"amount": 100, "expense_date": expenseDay, "vat": 18},
	} {
		t.Run(name, func(t *testing.T) {
			resp, out := doJSON(t, srv, http.MethodPost, "/api/v1/expenses", alice, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("POST %s = %d, want 400: %v", name, resp.StatusCode, out)
			}
		})
	}
	if count, _ := expensesIn(t, srv, alice, "")["count"].(float64); count != 2 {
		t.Fatalf("a rejected write landed: %v expenses, want the two good ones", count)
	}
}

// Deleting a user takes their expenses; deleting a category leaves them and
// clears the link -- the money was still spent.
func TestExpenseDeletionCascades(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	books := create(t, srv, alice, "/api/v1/expense-categories", map[string]any{"name": "Books"})
	expenseID := create(t, srv, alice, "/api/v1/expenses", map[string]any{
		"amount": 899, "expense_date": expenseDay, "description": "a paperback", "category_id": books,
	})

	pool := mustPool(t)
	if _, err := pool.Exec(`DELETE FROM expense_categories WHERE id = $1`, books); err != nil {
		t.Fatal(err)
	}
	resp, expense := doJSON(t, srv, http.MethodGet, "/api/v1/expenses/"+expenseID, alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the expense went with its category: %d %v", resp.StatusCode, expense)
	}
	if expense["category_id"] != nil || expense["category"] != nil {
		t.Fatalf("category = %v/%v, want null once the category is gone",
			expense["category_id"], expense["category"])
	}

	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(`SELECT count(*) FROM expenses`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d expenses survived the deleted user", n)
	}
}

// An expense gets a graph node like every other mirrored row, and the node
// follows what it is for and goes with the expense. Categories get none.
func TestAnExpenseGetsAGraphNode(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	expenseID := create(t, srv, alice, "/api/v1/expenses", map[string]any{
		"amount": lunchAmount, "expense_date": expenseDay, "description": "lunch with the team",
	})

	node := func() map[string]any {
		t.Helper()
		resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/knowledge-graph?type=expense", alice, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /knowledge-graph?type=expense: %d %v", resp.StatusCode, body)
		}
		nodes, _ := body["nodes"].([]any)
		if len(nodes) != 1 {
			t.Fatalf("the expense has %d nodes, want 1: %v", len(nodes), body)
		}
		n, _ := nodes[0].(map[string]any)
		return n
	}

	n := node()
	if n["ref_table"] != "expenses" || n["ref_id"] != expenseID || n["label"] != "lunch with the team" {
		t.Fatalf("node = %v", n)
	}

	// Re-describing the expense renames the node.
	resp, _ := doJSON(t, srv, http.MethodPatch, "/api/v1/expenses/"+expenseID, alice,
		map[string]any{"description": "lunch with the design team"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-describe = %d", resp.StatusCode)
	}
	if label := node()["label"]; label != "lunch with the design team" {
		t.Fatalf("the node label is %v, want the new description", label)
	}

	// A mirrored node cannot be deleted on its own -- the way to remove it is
	// to delete the expense, which takes it through the trigger.
	nodeID, _ := node()["id"].(string)
	resp, body := doJSON(t, srv, http.MethodDelete, "/api/v1/knowledge-graph/nodes/"+nodeID, alice, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting a mirrored node = %d, want 409: %v", resp.StatusCode, body)
	}
	resp, _ = doJSON(t, srv, http.MethodDelete, "/api/v1/expenses/"+expenseID, alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete expense = %d", resp.StatusCode)
	}
	resp, g := doJSON(t, srv, http.MethodGet, "/api/v1/knowledge-graph?type=expense", alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if nodes, _ := g["nodes"].([]any); len(nodes) != 0 {
		t.Fatalf("%d expense nodes survived the deleted expense", len(nodes))
	}
}
