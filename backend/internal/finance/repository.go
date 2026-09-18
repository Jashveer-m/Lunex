package finance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jashveer/lifeos/backend/internal/db"
)

// Repository is the Postgres store for expenses and their categories.
//
// Every statement carries `user_id = $n`. Ownership is not a check the service
// performs after loading a row -- it is part of the WHERE clause, so an expense
// belonging to another user is never fetched in the first place and an UPDATE
// or DELETE aimed at one matches zero rows.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// --- categories ---------------------------------------------------------------

// CreateCategory adds one category. The unique index is the authority on
// duplicates, not a SELECT before the INSERT: two concurrent requests for
// "Food" both pass a check and only one passes the index.
func (r *Repository) CreateCategory(ctx context.Context, userID uuid.UUID, name string) (Category, error) {
	var c Category
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO expense_categories (user_id, name)
		VALUES ($1, $2)
		RETURNING id, user_id, name, created_at`, userID, name,
	).Scan(&c.ID, &c.UserID, &c.Name, &c.CreatedAt)
	if isUniqueViolation(err) {
		return Category{}, ErrCategoryExists
	}
	if err != nil {
		return Category{}, fmt.Errorf("insert expense category: %w", err)
	}
	return c, nil
}

// Categories returns the owner's categories, by name.
func (r *Repository) Categories(ctx context.Context, userID uuid.UUID) ([]Category, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, user_id, name, created_at FROM expense_categories
		WHERE user_id = $1 ORDER BY lower(name) ASC, id ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("select expense categories: %w", err)
	}
	defer rows.Close()

	out := []Category{}
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.UserID, &c.Name, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan expense category: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expense categories: %w", err)
	}
	return out, nil
}

// CategoryExists backs the ownership check on category_id. Owner-scoped, so
// another user's category is indistinguishable from one that does not exist.
func (r *Repository) CategoryExists(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	return r.ownsRow(ctx, "expense_categories", userID, id)
}

// DocumentExists backs the ownership check on related_document_id.
//
// It reads another module's table, which is the same considered exception
// calendar.Repository.TaskExists makes and for the same reason: the coupling
// already exists in the schema -- expenses has a foreign key to documents --
// and the foreign key alone is not enough, since it would happily accept
// *another user's* document id, which is precisely the leak the whole ownership
// design exists to prevent. See docs/decisions.md.
func (r *Repository) DocumentExists(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	return r.ownsRow(ctx, "documents", userID, id)
}

// ownsRow is the shared probe. `table` is never caller-supplied: the two
// methods above are the only callers and each passes a literal.
func (r *Repository) ownsRow(ctx context.Context, table string, userID, id uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx,
		`SELECT true FROM `+table+` WHERE id = $1 AND user_id = $2`, id, userID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check %s exists: %w", table, err)
	}
	return ok, nil
}

// --- expenses -----------------------------------------------------------------

// expenseColumns is the RETURNING/SELECT list, shared so the scan order cannot
// drift.
//
// `amount::text` rather than `amount`: the driver would hand a numeric back as
// a float64 or a []byte depending on how it is asked, and the first of those is
// the rounding this module exists to avoid. The text is exactly the digits
// Postgres holds, and ParseAmount turns them into the same int64 that was
// written. See amount.go.
const expenseColumns = `e.id, e.user_id, e.amount::text, e.currency, e.category_id, c.name,
	e.description, e.expense_date, e.related_document_id, e.created_at, e.updated_at`

// expenseFrom is the join that puts the category's name on every row, so a
// client never has to resolve an id and a deleted category reads back as no
// category rather than as a dangling one.
//
// The join carries `c.user_id = e.user_id` as well as the id. The write path
// already refuses a category the caller does not own, so it cannot match
// otherwise; carrying it means the isolation holds on this statement rather
// than because of a check somewhere else, which is the rule every other query
// here follows.
const expenseFrom = ` FROM expenses e
	LEFT JOIN expense_categories c ON c.id = e.category_id AND c.user_id = e.user_id`

func scanExpense(row interface{ Scan(...any) error }) (Expense, error) {
	var e Expense
	var amount string
	err := row.Scan(&e.ID, &e.UserID, &amount, &e.Currency, &e.CategoryID, &e.CategoryName,
		&e.Description, &e.Date, &e.RelatedDocumentID, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return Expense{}, err
	}
	if e.Amount, err = ParseAmount(amount); err != nil {
		return Expense{}, fmt.Errorf("read stored amount %q: %w", amount, err)
	}
	e.Date = Day(e.Date)
	return e, nil
}

func (r *Repository) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Expense, error) {
	var id uuid.UUID
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO expenses (user_id, amount, currency, category_id, description,
			expense_date, related_document_id)
		VALUES ($1, $2::numeric, $3, $4, $5, $6, $7)
		RETURNING id`,
		userID, in.Amount.String(), in.Currency, in.CategoryID, nullString(in.Description),
		in.Date, in.RelatedDocumentID).Scan(&id)
	if err != nil {
		return Expense{}, fmt.Errorf("insert expense: %w", err)
	}
	// Read back through the join rather than RETURNING, so the created row and
	// a listed one are built by the same query and carry the category's name
	// the same way.
	return r.ByID(ctx, userID, id)
}

func (r *Repository) ByID(ctx context.Context, userID, id uuid.UUID) (Expense, error) {
	e, err := scanExpense(r.db.QueryRowContext(ctx,
		`SELECT `+expenseColumns+expenseFrom+` WHERE e.id = $1 AND e.user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Expense{}, ErrNotFound
	}
	if err != nil {
		return Expense{}, fmt.Errorf("select expense: %w", err)
	}
	return e, nil
}

// List returns the owner's expenses matching the filter.
//
// The date bounds are inclusive on both ends -- see Filter -- and both are
// optional: the leading `user_id` of the composite index answers the unbounded
// read, and the date narrows it when there is one.
func (r *Repository) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Expense, error) {
	where, args := f.conditions(userID)
	order, ok := Sorts[f.Sort]
	if !ok {
		// Unreachable through the service, which validates first. It is here
		// because the alternative is not a wrong answer but a syntax error:
		// an empty fragment makes the clause `ORDER BY e. , e.id`.
		order = Sorts[DefaultSort]
	}
	query := `SELECT ` + expenseColumns + expenseFrom + ` WHERE ` + strings.Join(where, " AND ") +
		// Every value in Sorts is a bare column of this table followed by a
		// direction, so the alias goes in front of it; the map is what keeps
		// the fragment out of the caller's hands.
		` ORDER BY e.` + order + `, e.id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select expenses: %w", err)
	}
	defer rows.Close()

	out := []Expense{}
	for rows.Next() {
		e, err := scanExpense(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expense: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expenses: %w", err)
	}
	return out, nil
}

// Summarize adds the matching expenses up, grouped by currency and category.
//
// The aggregation is one GROUP BY in Postgres rather than a loop in Go over
// every row, and that is the point of having the endpoint at all: "what did I
// spend last year" is one indexed scan and a handful of rows out, instead of
// ten thousand rows over the wire so that something else can add them up. It is
// also what lets analyze_spending answer without putting a year of purchases in
// front of a 3B model.
//
// The paging on the filter is ignored: a summary of the first fifty expenses is
// not a summary of anything.
func (r *Repository) Summarize(ctx context.Context, userID uuid.UUID, f Filter) (Summary, error) {
	where, args := f.conditions(userID)
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.currency, e.category_id, c.name, sum(e.amount)::text, count(*)`+
		expenseFrom+` WHERE `+strings.Join(where, " AND ")+`
		GROUP BY e.currency, e.category_id, c.name
		ORDER BY e.currency ASC`, args...)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize expenses: %w", err)
	}
	defer rows.Close()

	out := Summary{Start: f.Start, End: f.End}
	byCurrency := map[string]int{}
	for rows.Next() {
		var currency, total string
		var line CategoryTotal
		var name *string
		if err := rows.Scan(&currency, &line.CategoryID, &name, &total, &line.Count); err != nil {
			return Summary{}, fmt.Errorf("scan spending total: %w", err)
		}
		if line.Total, err = ParseAmount(total); err != nil {
			return Summary{}, fmt.Errorf("read stored total %q: %w", total, err)
		}
		if name != nil {
			line.Category = *name
		}
		i, seen := byCurrency[currency]
		if !seen {
			i = len(out.Currencies)
			byCurrency[currency] = i
			out.Currencies = append(out.Currencies, CurrencyTotal{Currency: currency})
		}
		out.Currencies[i].Total += line.Total
		out.Currencies[i].Count += line.Count
		out.Currencies[i].Categories = append(out.Currencies[i].Categories, line)
		out.Count += line.Count
	}
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("iterate spending totals: %w", err)
	}
	sortSummary(&out)
	return out, nil
}

// conditions is the WHERE the list and the summary share, so the two can never
// disagree about which expenses a window contains.
func (f Filter) conditions(userID uuid.UUID) ([]string, []any) {
	where := []string{"e.user_id = $1"}
	args := []any{userID}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Start != nil {
		add("e.expense_date >= $%d", *f.Start)
	}
	if f.End != nil {
		// Inclusive: `<=`, not `<`. See Filter.
		add("e.expense_date <= $%d", *f.End)
	}
	switch {
	case f.CategoryID != nil:
		add("e.category_id = $%d", *f.CategoryID)
	case f.Uncategorized:
		where = append(where, "e.category_id IS NULL")
	}
	if f.Query != "" {
		// A leading wildcard cannot use an index, so this scans what the
		// clauses above have already narrowed to one person's expenses. See
		// db.Contains for the escaping.
		add(`e.description ILIKE $%d ESCAPE '\'`, db.Contains(f.Query))
	}
	return where, args
}

// Update applies a patch and returns the new row.
func (r *Repository) Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Expense, error) {
	if p.Empty() {
		return r.ByID(ctx, userID, id)
	}

	var set []string
	args := []any{id, userID}
	assign := func(column string, value any) {
		args = append(args, value)
		set = append(set, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if p.Amount != nil {
		set = append(set, fmt.Sprintf("amount = $%d::numeric", len(args)+1))
		args = append(args, p.Amount.String())
	}
	if p.Currency != nil {
		assign("currency", *p.Currency)
	}
	if p.Date != nil {
		assign("expense_date", *p.Date)
	}
	if p.CategoryID.Set {
		assign("category_id", p.CategoryID.Value)
	}
	if p.Description.Set {
		assign("description", p.Description.Value)
	}
	if p.RelatedDocumentID.Set {
		assign("related_document_id", p.RelatedDocumentID.Value)
	}

	var updated uuid.UUID
	err := r.db.QueryRowContext(ctx,
		`UPDATE expenses SET `+strings.Join(set, ", ")+
			` WHERE id = $1 AND user_id = $2 RETURNING id`, args...).Scan(&updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Expense{}, ErrNotFound
	}
	if err != nil {
		return Expense{}, fmt.Errorf("update expense: %w", err)
	}
	return r.ByID(ctx, userID, updated)
}

func (r *Repository) Delete(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM expenses WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete expense: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete expense: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// nullString keeps an empty optional column NULL rather than an empty string,
// so "unset" has exactly one representation in the database.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// IsForeignKeyViolation identifies a category or document id pointing at
// nothing. The service checks ownership up front, but a concurrent delete can
// still race it.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
