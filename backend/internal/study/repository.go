package study

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

// Repository is the Postgres store for study plans and their flashcards.
//
// Every statement carries `user_id = $n`. Ownership is not a check the service
// performs after loading a row -- it is part of the WHERE clause, so a plan
// belonging to another user is never fetched in the first place and an UPDATE
// or DELETE aimed at one matches zero rows.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// --- plans ---------------------------------------------------------------------

// planColumns is the shared SELECT/RETURNING list, so the scan order cannot
// drift.
//
// The card count is a correlated subquery rather than a stored column, for the
// reason documents.chunk_count is: a counter kept in a column is a counter
// that can be wrong, and this one cannot. It is scoped to the owner as well as
// to the plan -- the cascade makes a foreign card impossible, and carrying the
// owner means the isolation holds on this statement rather than because of
// something else being true.
const planColumns = `p.id, p.user_id, p.title, p.description, p.document_id, d.filename,
	p.status, p.created_at, p.updated_at,
	(SELECT count(*) FROM flashcards f
	  WHERE f.study_plan_id = p.id AND f.user_id = p.user_id) AS card_count`

// planFrom is the join that puts the source document's name on every row, so a
// client never has to resolve an id and a deleted document reads back as no
// document rather than as a dangling one.
const planFrom = ` FROM study_plans p
	LEFT JOIN documents d ON d.id = p.document_id AND d.user_id = p.user_id`

func scanPlan(row interface{ Scan(...any) error }) (Plan, error) {
	var p Plan
	err := row.Scan(&p.ID, &p.UserID, &p.Title, &p.Description, &p.DocumentID, &p.DocumentName,
		&p.Status, &p.CreatedAt, &p.UpdatedAt, &p.CardCount)
	return p, err
}

func (r *Repository) CreatePlan(ctx context.Context, userID uuid.UUID, in CreatePlanInput) (Plan, error) {
	var id uuid.UUID
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO study_plans (user_id, title, description, document_id, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		userID, in.Title, nullString(in.Description), in.DocumentID, in.Status).Scan(&id)
	if err != nil {
		return Plan{}, fmt.Errorf("insert study plan: %w", err)
	}
	// Read back through the join rather than RETURNING, so a created plan and
	// a listed one are built by the same query and carry the document's name
	// and the card count the same way.
	return r.PlanByID(ctx, userID, id)
}

func (r *Repository) PlanByID(ctx context.Context, userID, id uuid.UUID) (Plan, error) {
	p, err := scanPlan(r.db.QueryRowContext(ctx,
		`SELECT `+planColumns+planFrom+` WHERE p.id = $1 AND p.user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	if err != nil {
		return Plan{}, fmt.Errorf("select study plan: %w", err)
	}
	return p, nil
}

func (r *Repository) Plans(ctx context.Context, userID uuid.UUID, f Filter) ([]Plan, error) {
	where := []string{"p.user_id = $1"}
	args := []any{userID}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Status != "" {
		add("p.status = $%d", f.Status)
	}
	if f.DocumentID != nil {
		add("p.document_id = $%d", *f.DocumentID)
	}
	if f.Query != "" {
		// Title or description, both against the one parameter -- so the
		// placeholder is named twice and this clause is written out rather
		// than going through `add`, which formats one.
		//
		// A leading wildcard cannot use an index, so this scans what the owner
		// clause has already narrowed to one person's plans. See db.Contains
		// for the escaping.
		args = append(args, db.Contains(f.Query))
		n := len(args)
		where = append(where, fmt.Sprintf(
			`(p.title ILIKE $%d ESCAPE '\' OR p.description ILIKE $%d ESCAPE '\')`, n, n))
	}

	order, ok := Sorts[f.Sort]
	if !ok {
		// Unreachable through the service, which validates first. It is here
		// because the alternative is not a wrong answer but a syntax error.
		order = Sorts[DefaultSort]
	}
	query := `SELECT ` + planColumns + planFrom + ` WHERE ` + strings.Join(where, " AND ") +
		// Every value in Sorts is a bare column of this table or a function of
		// one, so the alias goes in front of it; the map is what keeps the
		// fragment out of the caller's hands.
		` ORDER BY ` + qualify(order) + `, p.id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select study plans: %w", err)
	}
	defer rows.Close()

	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan study plan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate study plans: %w", err)
	}
	return out, nil
}

// qualify puts the table alias on a Sorts fragment. The fragments are written
// bare -- "lower(title) ASC" -- so they can be read, and the alias is added
// here rather than baked into the map, which the ?sort= allow-list also
// reports on.
func qualify(fragment string) string {
	if i := strings.Index(fragment, "lower("); i == 0 {
		return "lower(p." + fragment[len("lower("):]
	}
	return "p." + fragment
}

func (r *Repository) UpdatePlan(ctx context.Context, userID, id uuid.UUID, p Patch) (Plan, error) {
	if p.Empty() {
		return r.PlanByID(ctx, userID, id)
	}

	var set []string
	args := []any{id, userID}
	assign := func(column string, value any) {
		args = append(args, value)
		set = append(set, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if p.Title != nil {
		assign("title", *p.Title)
	}
	if p.Status != nil {
		assign("status", *p.Status)
	}
	if p.Description.Set {
		assign("description", p.Description.Value)
	}
	if p.DocumentID.Set {
		assign("document_id", p.DocumentID.Value)
	}

	var updated uuid.UUID
	err := r.db.QueryRowContext(ctx,
		`UPDATE study_plans SET `+strings.Join(set, ", ")+
			` WHERE id = $1 AND user_id = $2 RETURNING id`, args...).Scan(&updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	if err != nil {
		return Plan{}, fmt.Errorf("update study plan: %w", err)
	}
	return r.PlanByID(ctx, userID, updated)
}

// DeletePlan removes the plan; its flashcards cascade from it, and its graph
// node goes through the trigger migration 000010 puts on this table.
func (r *Repository) DeletePlan(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM study_plans WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete study plan: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete study plan: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PlanExists backs the ownership check on study_plan_id. Owner-scoped, so
// another user's plan is indistinguishable from one that does not exist.
func (r *Repository) PlanExists(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	return r.ownsRow(ctx, "study_plans", userID, id)
}

// DocumentExists backs the ownership check on document_id.
//
// It reads another module's table, which is the same considered exception
// calendar.Repository.TaskExists and finance.Repository.DocumentExists make,
// and for the same reason: the coupling already exists in the schema -- both
// tables here have a foreign key to documents -- and the foreign key alone is
// not enough, since it would happily accept *another user's* document id,
// which is precisely the leak the whole ownership design exists to prevent.
// See docs/decisions.md.
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

// --- flashcards ------------------------------------------------------------------

const cardColumns = `id, user_id, study_plan_id, document_id, front, back, created_at`

func scanCard(row interface{ Scan(...any) error }) (Flashcard, error) {
	var c Flashcard
	err := row.Scan(&c.ID, &c.UserID, &c.StudyPlanID, &c.DocumentID, &c.Front, &c.Back, &c.CreatedAt)
	return c, err
}

// CreateFlashcards writes a batch of cards in one transaction.
//
// One transaction rather than a loop of inserts, because a batch is what an
// approved generation is: the user approved eight cards, and eight is what
// they should get. Half a deck landing because the fifth insert raced a
// deleted plan is an outcome nobody asked for and nobody can tell apart from
// the model having written four.
func (r *Repository) CreateFlashcards(ctx context.Context, userID uuid.UUID, in []CreateCardInput) ([]Flashcard, error) {
	if len(in) == 0 {
		return []Flashcard{}, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin flashcard insert: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to nothing

	out := make([]Flashcard, 0, len(in))
	for _, card := range in {
		// clock_timestamp(), not the column's now() default. now() is the
		// transaction's start time, so every card in a batch would carry the
		// identical timestamp and the deck's read order would fall to the
		// tie-break on a random uuid -- which shuffles a generated deck out of
		// the order the model wrote it in, and therefore out of the order of
		// the document it came from. It is the same call chat.Repository makes
		// for the two messages of a turn, for the same reason.
		c, err := scanCard(tx.QueryRowContext(ctx, `
			INSERT INTO flashcards (user_id, study_plan_id, document_id, front, back, created_at)
			VALUES ($1, $2, $3, $4, $5, clock_timestamp())
			RETURNING `+cardColumns,
			userID, card.StudyPlanID, card.DocumentID, card.Front, card.Back))
		if err != nil {
			return nil, fmt.Errorf("insert flashcard: %w", err)
		}
		out = append(out, c)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit flashcards: %w", err)
	}
	return out, nil
}

// Flashcards returns the owner's cards matching the filter, oldest first --
// the order they were made in, which for a generated deck is the order the
// model wrote it in. That holds inside one batch as well as between batches,
// because CreateFlashcards stamps each row with clock_timestamp() rather than
// letting a whole transaction share one; see there.
func (r *Repository) Flashcards(ctx context.Context, userID uuid.UUID, f CardFilter) ([]Flashcard, error) {
	where := []string{"user_id = $1"}
	args := []any{userID}
	if f.StudyPlanID != nil {
		args = append(args, *f.StudyPlanID)
		where = append(where, fmt.Sprintf("study_plan_id = $%d", len(args)))
	}
	query := `SELECT ` + cardColumns + ` FROM flashcards WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at ASC, id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select flashcards: %w", err)
	}
	defer rows.Close()

	out := []Flashcard{}
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, fmt.Errorf("scan flashcard: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate flashcards: %w", err)
	}
	return out, nil
}

func (r *Repository) DeleteFlashcard(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM flashcards WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete flashcard: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete flashcard: %w", err)
	}
	if n == 0 {
		return ErrFlashcardNotFound
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

// IsForeignKeyViolation identifies a plan or document id pointing at nothing.
// The service checks ownership up front, but a concurrent delete can still
// race it.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
