package calendar

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

// Repository is the Postgres store for calendar events.
//
// Every statement carries `user_id = $n`. Ownership is not a check the service
// performs after loading a row -- it is part of the WHERE clause, so an event
// belonging to another user is never fetched in the first place and an UPDATE
// or DELETE aimed at one matches zero rows.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// eventColumns is the RETURNING/SELECT list, shared so the scan order cannot
// drift.
const eventColumns = `id, user_id, title, description, start_time, end_time, all_day,
	location, recurrence_rule, related_task_id, related_goal_id, created_at, updated_at`

func scanEvent(row interface{ Scan(...any) error }) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.UserID, &e.Title, &e.Description, &e.StartTime, &e.EndTime,
		&e.AllDay, &e.Location, &e.RecurrenceRule, &e.RelatedTaskID, &e.RelatedGoalID,
		&e.CreatedAt, &e.UpdatedAt)
	return e, err
}

func (r *Repository) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Event, error) {
	e, err := scanEvent(r.db.QueryRowContext(ctx, `
		INSERT INTO calendar_events (user_id, title, description, start_time, end_time,
			all_day, location, recurrence_rule, related_task_id, related_goal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+eventColumns,
		userID, in.Title, nullString(in.Description), in.StartTime, in.EndTime,
		in.AllDay, nullString(in.Location), nullString(in.RecurrenceRule),
		in.RelatedTaskID, in.RelatedGoalID))
	if err != nil {
		return Event{}, fmt.Errorf("insert calendar event: %w", err)
	}
	return e, nil
}

func (r *Repository) ByID(ctx context.Context, userID, id uuid.UUID) (Event, error) {
	e, err := scanEvent(r.db.QueryRowContext(ctx,
		`SELECT `+eventColumns+` FROM calendar_events WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("select calendar event: %w", err)
	}
	return e, nil
}

// List returns the owner's events overlapping [f.Start, f.End).
//
// The overlap test is the only interesting thing in this file:
//
//	start_time < :end AND (end_time > :start OR start_time >= :start)
//
// The first clause and the first half of the second are the ordinary half-open
// overlap, which is what makes "what is on on Thursday" include the conference
// that began on Tuesday. The second half is for a zero-length event -- a
// reminder at a moment, which the CHECK allows -- whose end is not *after* the
// window start even when it is inside the window.
//
// `start_time < :end` is what the (user_id, start_time) index serves, so the
// scan is bounded by the far end of the window rather than by the table.
func (r *Repository) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Event, error) {
	where := []string{"user_id = $1", "start_time < $2", "(end_time > $3 OR start_time >= $3)"}
	args := []any{userID, f.End, f.Start}
	if f.Query != "" {
		// A leading wildcard cannot use an index, so this scans what the clauses
		// above have already narrowed to one person's window. See db.Contains
		// for the escaping.
		args = append(args, db.Contains(f.Query))
		where = append(where, fmt.Sprintf(
			`(title ILIKE $%[1]d ESCAPE '\' OR description ILIKE $%[1]d ESCAPE '\' OR location ILIKE $%[1]d ESCAPE '\')`,
			len(args)))
	}

	query := `SELECT ` + eventColumns + ` FROM calendar_events WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + Sorts[f.Sort] + `, id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select calendar events: %w", err)
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan calendar event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate calendar events: %w", err)
	}
	return out, nil
}

// Update applies a patch and returns the new row. Building the SET list from
// the patch keeps an untouched column untouched, so two clients patching
// different fields do not overwrite each other.
func (r *Repository) Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Event, error) {
	if p.Empty() {
		return r.ByID(ctx, userID, id)
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
	if p.StartTime != nil {
		assign("start_time", *p.StartTime)
	}
	if p.EndTime != nil {
		assign("end_time", *p.EndTime)
	}
	if p.AllDay != nil {
		assign("all_day", *p.AllDay)
	}
	if p.Description.Set {
		assign("description", p.Description.Value)
	}
	if p.Location.Set {
		assign("location", p.Location.Value)
	}
	if p.RecurrenceRule.Set {
		assign("recurrence_rule", p.RecurrenceRule.Value)
	}
	if p.RelatedTaskID.Set {
		assign("related_task_id", p.RelatedTaskID.Value)
	}
	if p.RelatedGoalID.Set {
		assign("related_goal_id", p.RelatedGoalID.Value)
	}

	e, err := scanEvent(r.db.QueryRowContext(ctx,
		`UPDATE calendar_events SET `+strings.Join(set, ", ")+
			` WHERE id = $1 AND user_id = $2 RETURNING `+eventColumns, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("update calendar event: %w", err)
	}
	return e, nil
}

func (r *Repository) Delete(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM calendar_events WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete calendar event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete calendar event: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TaskExists and GoalExists back the ownership checks on related_task_id and
// related_goal_id.
//
// They read another module's table, which nothing else in this codebase does,
// and that is a considered exception rather than an oversight. The coupling
// already exists in the schema -- calendar_events has a foreign key to each --
// and the foreign key alone is not enough: it would happily accept *another
// user's* task id, which is precisely the leak the whole ownership design
// exists to prevent. The alternative, wiring the task and goal services into
// this one, would make the calendar depend on two services to answer a
// question that is one owner-scoped index probe. See docs/decisions.md.
func (r *Repository) TaskExists(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	return r.ownsRow(ctx, "tasks", userID, id)
}

func (r *Repository) GoalExists(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	return r.ownsRow(ctx, "goals", userID, id)
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

// nullString keeps an empty optional column NULL rather than an empty string,
// so "unset" has exactly one representation in the database.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// IsForeignKeyViolation identifies a related id pointing at nothing. The
// service checks ownership up front, but a concurrent delete can still race it.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
