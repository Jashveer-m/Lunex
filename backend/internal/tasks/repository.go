package tasks

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

// Repository is the Postgres store for tasks and their dependency edges.
//
// Every statement carries `user_id = $n`. Ownership is not a check the service
// performs after loading a row — it is part of the WHERE clause, so a task
// belonging to another user is never fetched in the first place and an UPDATE
// or DELETE aimed at one matches zero rows.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// taskColumns is the RETURNING/SELECT list, shared so the scan order cannot
// drift. tags is rendered as JSON; see db.TextArray for why.
var taskColumns = `id, user_id, title, description, priority, status, category, ` +
	db.ArrayToJSON("tags") + `, deadline, parent_task_id, estimated_effort_minutes,
	actual_effort_minutes, created_at, updated_at`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var tags db.TextArray
	err := row.Scan(&t.ID, &t.UserID, &t.Title, &t.Description, &t.Priority, &t.Status,
		&t.Category, &tags, &t.Deadline, &t.ParentTaskID, &t.EstimatedEffortMinutes,
		&t.ActualEffortMinutes, &t.CreatedAt, &t.UpdatedAt)
	t.Tags = tags
	return t, err
}

func (r *Repository) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Task, error) {
	t, err := scanTask(r.db.QueryRowContext(ctx, `
		INSERT INTO tasks (user_id, title, description, priority, status, category, tags,
			deadline, parent_task_id, estimated_effort_minutes, actual_effort_minutes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+taskColumns,
		userID, in.Title, nullString(in.Description), in.Priority, in.Status,
		nullString(in.Category), db.TextArrayParam(in.Tags), in.Deadline, in.ParentTaskID,
		in.EstimatedEffortMinutes, in.ActualEffortMinutes))
	if err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	return t, nil
}

func (r *Repository) ByID(ctx context.Context, userID, id uuid.UUID) (Task, error) {
	t, err := scanTask(r.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("select task: %w", err)
	}
	if t.DependsOn, err = r.dependencies(ctx, t.ID); err != nil {
		return Task{}, err
	}
	return t, nil
}

// List applies the filter, always scoped to the owner. The ORDER BY fragment
// comes from the Sorts map, never from the request.
func (r *Repository) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Task, error) {
	where := []string{"user_id = $1"}
	args := []any{userID}
	add := func(clause string, arg any) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Status != "" {
		add("status = $%d", f.Status)
	}
	if f.Category != "" {
		add("category = $%d", f.Category)
	}
	if f.Tag != "" {
		// Containment rather than `= ANY(tags)`: only the former uses the GIN index.
		add("tags @> ARRAY[$%d]::text[]", f.Tag)
	}

	query := `SELECT ` + taskColumns + ` FROM tasks WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + Sorts[f.Sort] + `, id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select tasks: %w", err)
	}
	defer rows.Close()

	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return out, nil
}

// Update applies a patch and returns the new row. Building the SET list from
// the patch keeps an untouched column untouched, so two clients patching
// different fields do not overwrite each other.
func (r *Repository) Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Task, error) {
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
	if p.Priority != nil {
		assign("priority", *p.Priority)
	}
	if p.Status != nil {
		assign("status", *p.Status)
	}
	if p.Tags != nil {
		assign("tags", db.TextArrayParam(*p.Tags))
	}
	if p.Description.Set {
		assign("description", p.Description.Value)
	}
	if p.Category.Set {
		assign("category", p.Category.Value)
	}
	if p.Deadline.Set {
		assign("deadline", p.Deadline.Value)
	}
	if p.ParentTaskID.Set {
		assign("parent_task_id", p.ParentTaskID.Value)
	}
	if p.EstimatedEffortMinutes.Set {
		assign("estimated_effort_minutes", p.EstimatedEffortMinutes.Value)
	}
	if p.ActualEffortMinutes.Set {
		assign("actual_effort_minutes", p.ActualEffortMinutes.Value)
	}

	t, err := scanTask(r.db.QueryRowContext(ctx,
		`UPDATE tasks SET `+strings.Join(set, ", ")+
			` WHERE id = $1 AND user_id = $2 RETURNING `+taskColumns, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("update task: %w", err)
	}
	if t.DependsOn, err = r.dependencies(ctx, t.ID); err != nil {
		return Task{}, err
	}
	return t, nil
}

func (r *Repository) Delete(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Exists reports whether the user owns that task. It backs the parent-task and
// dependency checks without paying for a full row.
func (r *Repository) Exists(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx,
		`SELECT true FROM tasks WHERE id = $1 AND user_id = $2`, id, userID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check task exists: %w", err)
	}
	return ok, nil
}

// AddDependency records that task depends on dependsOn.
//
// Ownership, the cycle check and the insert all happen inside one transaction
// holding a per-user advisory lock. Without the lock, two concurrent additions
// could each see an acyclic graph and together close a loop; with it, every
// dependency edit for a user is serialized, and the lock is released when the
// transaction ends whatever the outcome.
//
// The insert is idempotent: adding the same edge twice is not an error, it is
// the state the client asked for.
func (r *Repository) AddDependency(ctx context.Context, userID, taskID, dependsOn uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the tx is committed

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, userID.String()); err != nil {
		return fmt.Errorf("lock dependency graph: %w", err)
	}

	// Both endpoints of the edge must be the caller's. Checking them together
	// means a foreign task is reported as "not found", exactly like a missing
	// one, so the endpoint never confirms another user's id.
	var owned int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM tasks WHERE id IN ($1, $2) AND user_id = $3`,
		taskID, dependsOn, userID).Scan(&owned); err != nil {
		return fmt.Errorf("check task ownership: %w", err)
	}
	if owned != 2 {
		return ErrNotFound
	}

	// task is already reachable from dependsOn, so the new edge closes a loop.
	var cycles bool
	if err := tx.QueryRowContext(ctx, `
		WITH RECURSIVE reachable(id) AS (
			SELECT $1::uuid
			UNION
			SELECT d.depends_on_task_id
			FROM task_dependencies d
			JOIN reachable r ON d.task_id = r.id
		)
		SELECT EXISTS (SELECT 1 FROM reachable WHERE id = $2)`,
		dependsOn, taskID).Scan(&cycles); err != nil {
		return fmt.Errorf("check dependency cycle: %w", err)
	}
	if cycles {
		return ErrDependencyCycle
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_dependencies (task_id, depends_on_task_id)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, taskID, dependsOn); err != nil {
		return fmt.Errorf("insert dependency: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// dependencies is only ever called for a task already matched on user_id, so it
// needs no owner clause of its own.
func (r *Repository) dependencies(ctx context.Context, taskID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT depends_on_task_id FROM task_dependencies WHERE task_id = $1 ORDER BY depends_on_task_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("select dependencies: %w", err)
	}
	defer rows.Close()

	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan dependency: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// nullString keeps an empty optional column NULL rather than an empty string, so "unset" has
// exactly one representation in the database.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// IsForeignKeyViolation identifies a parent_task_id pointing at nothing. The
// service checks ownership up front, but a concurrent delete can still race it.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
