// Package finance owns the expense aggregate: what the user spent, on what,
// and when.
//
// It is the Phase 2 resource shape -- transport, service, repository, every
// query scoped to the owner -- with three differences worth naming up front.
//
// The first is money. An amount is an Amount, an int64 of hundredths, and never
// a float; see amount.go for why that is not a stylistic preference.
//
// The second is that nothing converts currencies. An expense is stored in the
// currency it was incurred in and stays there, so there is no such thing as
// "the total" -- only a total per currency, which is the shape Summary has.
// Adding ₹500 to $20 requires a rate, a rate has a date, and a wrong one
// silently produces a plausible number; so this phase does not have one.
//
// The third is that, unlike the calendar, a read here does not require a date
// range. "What have I spent on this?" over the whole history is a question with
// a real answer -- a finance table grows a row per purchase, not per hour of
// the week -- so the range is optional and the paging is what bounds the read.
//
// What this module deliberately is not: it is not a budget. There is no budget
// table, no target, and nothing that reports being over or under one. And
// nothing in it gives financial advice: it adds up the user's own records, and
// the chat system prompt says in so many words that describing them is not
// advising on them. See docs/decisions.md.
package finance

import (
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// ErrNotFound covers "no such expense", "that expense is somebody else's", a
// category that is not the caller's, and a related document that is not
// theirs. One error for all of them is what keeps the API from confirming
// foreign ids.
var ErrNotFound = errors.New("expense not found")

// ErrCategoryNotFound is the same answer for the category endpoints, kept
// separate only so the handler can say "category" rather than "expense" in the
// message. It is a 404 exactly like ErrNotFound.
var ErrCategoryNotFound = errors.New("category not found")

// ErrCategoryExists is a second category with a name the user already has. It
// is the unique index, surfaced as a field error rather than a 500.
var ErrCategoryExists = errors.New("category already exists")

// Category mirrors a row of the expense_categories table.
//
// Categories have no graph node and no updated_at: a category is a label, and
// the only things that happen to one are being created and being deleted with
// its user. Renaming is not offered this phase -- see docs/decisions.md.
type Category struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Name      string
	CreatedAt time.Time
}

// DefaultCategories are what migration 000009's trigger gives every new user.
// They are duplicated here for the tests and for the message create_expense
// shows when a named category does not exist; the migration is the source of
// truth.
var DefaultCategories = []string{"Food", "Transport", "Housing", "Utilities", "Other"}

// Expense mirrors a row of the expenses table.
type Expense struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	Amount   Amount
	Currency string
	// CategoryID is nil for an expense filed under nothing, and *becomes* nil
	// when the category it named is deleted: the money was still spent.
	CategoryID *uuid.UUID
	// CategoryName is not a column -- it is joined on read, so a client and the
	// assistant can show "Food" without a second lookup, and so a deleted
	// category reads back as no category rather than as a dangling id.
	CategoryName *string
	Description  *string
	// Date is the day the money was spent, at midnight UTC. The column is a
	// `date`: there is no time of day here and none should be invented.
	Date              time.Time
	RelatedDocumentID *uuid.UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// CreateInput is a validated new expense.
type CreateInput struct {
	Amount            Amount
	Currency          string
	CategoryID        *uuid.UUID
	Description       string
	Date              time.Time
	RelatedDocumentID *uuid.UUID
}

// UpdateInput is a PATCH body. An unset field is left alone; a field set to
// null clears the column.
type UpdateInput struct {
	Amount            optional.Field[Amount]
	Currency          optional.Field[string]
	CategoryID        optional.Field[uuid.UUID]
	Description       optional.Field[string]
	Date              optional.Field[time.Time]
	RelatedDocumentID optional.Field[uuid.UUID]
}

// Patch is the normalized form of UpdateInput handed to the repository.
type Patch struct {
	Amount            *Amount
	Currency          *string
	CategoryID        optional.Field[uuid.UUID]
	Description       optional.Field[string]
	Date              *time.Time
	RelatedDocumentID optional.Field[uuid.UUID]
}

// Empty reports whether the patch would touch no columns at all.
func (p Patch) Empty() bool {
	return p.Amount == nil && p.Currency == nil && p.Date == nil &&
		!p.CategoryID.Set && !p.Description.Set && !p.RelatedDocumentID.Set
}

// Filter is the query behind GET /expenses and, with the paging ignored, behind
// GET /expenses/summary.
//
// Start and End are optional and *inclusive*: `?start=2026-09-01&end=2026-09-30`
// is September, all of it. That is the opposite of calendar.Filter, which is
// half-open, and the difference is deliberate: these are dates rather than
// timestamps, and there is no moment between the last instant of the 30th and
// the first of the 1st for an exclusive bound to be more correct about. An
// exclusive end would mean a month query had to name the 1st of the next month,
// which is not what anybody types.
type Filter struct {
	// Start and End are nil when the caller named no bound. Unlike the
	// calendar, an unbounded read is offered: see the package comment.
	Start *time.Time
	End   *time.Time
	// CategoryID narrows to one category. Uncategorized narrows to the
	// expenses filed under none -- which is not expressible as a CategoryID,
	// and is how "what have I not categorised" gets asked.
	CategoryID    *uuid.UUID
	Uncategorized bool
	// Query keeps the expenses whose description contains it, case-insensitively
	// and literally -- `%` and `_` are characters, not wildcards. Same contract
	// as tasks.Filter.Query.
	Query  string
	Sort   string
	Limit  int
	Offset int
}

// Sorts maps the public `sort` values onto SQL.
var Sorts = map[string]string{
	"expense_date":  "expense_date ASC",
	"-expense_date": "expense_date DESC",
	"amount":        "amount ASC",
	"-amount":       "amount DESC",
	"created_at":    "created_at ASC",
	"-created_at":   "created_at DESC",
	"updated_at":    "updated_at ASC",
	"-updated_at":   "updated_at DESC",
}

// DefaultSort is most recent first. A calendar reads forwards because it is
// about what has not happened yet; a ledger reads backwards because it is about
// what has.
const DefaultSort = "-expense_date"

// Paging bounds.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Field limits particular to this module. The shared ones live in
// internal/validate.
const (
	// MaxCategoryNameLen matches validate.MaxCategoryLen, which is the limit on
	// the free-text `category` column tasks already have.
	MaxCategoryNameLen = 100
	// MaxDescriptionLen is what an expense's note can be. It is shorter than
	// validate.MaxDescriptionLen on purpose: this is "lunch with the team",
	// not a document.
	MaxDescriptionLen = 1_000
	// CurrencyLen is the length of an ISO 4217 code. The code itself is not
	// checked against a list, because nothing here does anything with it but
	// group by it and print it; what is checked is that it is three letters, so
	// the grouping cannot be defeated by "rs" and "Rs " being different keys.
	CurrencyLen = 3
)

// DefaultCurrency matches the column default in migration 000009.
const DefaultCurrency = "INR"

// Summary is the aggregate behind GET /expenses/summary and the
// analyze_spending tool.
//
// It is grouped by currency first and by category within it, and it has no
// grand total, because there is not one: adding two currencies needs a rate
// this phase does not have. A single-currency user -- which is most of them --
// sees one CurrencyTotal and reads it as the total.
type Summary struct {
	// Start and End echo the window the totals cover, nil for an unbounded end.
	Start *time.Time
	End   *time.Time
	// Currencies is ordered by total, largest first.
	Currencies []CurrencyTotal
	// Count is how many expenses went into the whole summary.
	Count int
}

// CurrencyTotal is everything spent in one currency over the window.
type CurrencyTotal struct {
	Currency   string
	Total      Amount
	Count      int
	Categories []CategoryTotal
}

// CategoryTotal is one category's share of a currency's spending.
type CategoryTotal struct {
	// CategoryID and Category are nil and "" for the expenses filed under no
	// category. They are reported as a line of their own rather than dropped:
	// a summary whose lines do not add up to its total is a summary that
	// misleads.
	CategoryID *uuid.UUID
	Category   string
	Total      Amount
	Count      int
}

// Uncategorized reports whether this line is the no-category one.
func (c CategoryTotal) Uncategorized() bool { return c.CategoryID == nil }

// sortSummary puts the biggest numbers first: the currency most was spent in,
// and within it the category most was spent on.
//
// Ordering in Go rather than in SQL because the ordering is over the *summed*
// values, and the sums for one currency are assembled from several group rows.
// Ties break on the name so the order is stable -- a summary that reorders
// itself between two identical requests is one a client cannot diff.
func sortSummary(s *Summary) {
	sort.SliceStable(s.Currencies, func(i, j int) bool {
		a, b := s.Currencies[i], s.Currencies[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Currency < b.Currency
	})
	for i := range s.Currencies {
		lines := s.Currencies[i].Categories
		sort.SliceStable(lines, func(i, j int) bool {
			if lines[i].Total != lines[j].Total {
				return lines[i].Total > lines[j].Total
			}
			return lines[i].Category < lines[j].Category
		})
	}
}
