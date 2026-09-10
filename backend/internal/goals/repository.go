package goals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/db"
)

// Repository is the Postgres store for goals and their milestones.
//
// As with tasks, `user_id = $n` is part of every goal statement rather than a
// check performed after loading. Milestones have no user_id of their own, so
// every milestone statement joins back to goals and filters there — the owner
// clause is never dropped, only moved.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const goalColumns = `id, user_id, title, description, type, status, deadline, created_at, updated_at`

func scanGoal(row interface{ Scan(...any) error }) (Goal, error) {
	var g Goal
	err := row.Scan(&g.ID, &g.UserID, &g.Title, &g.Description, &g.Type, &g.Status,
		&g.Deadline, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

func (r *Repository) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Goal, error) {
	g, err := scanGoal(r.db.QueryRowContext(ctx, `
		INSERT INTO goals (user_id, title, description, type, status, deadline)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+goalColumns,
		userID, in.Title, nullString(in.Description), in.Type, in.Status, in.Deadline))
	if err != nil {
		return Goal{}, fmt.Errorf("insert goal: %w", err)
	}
	g.Milestones = []Milestone{}
	return g, nil
}

func (r *Repository) ByID(ctx context.Context, userID, id uuid.UUID) (Goal, error) {
	g, err := scanGoal(r.db.QueryRowContext(ctx,
		`SELECT `+goalColumns+` FROM goals WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Goal{}, ErrNotFound
	}
	if err != nil {
		return Goal{}, fmt.Errorf("select goal: %w", err)
	}
	byGoal, err := r.milestonesFor(ctx, g.ID)
	if err != nil {
		return Goal{}, err
	}
	g.Milestones = byGoal[g.ID]
	if g.Milestones == nil {
		g.Milestones = []Milestone{}
	}
	return g, nil
}

func (r *Repository) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Goal, error) {
	where := []string{"user_id = $1"}
	args := []any{userID}
	add := func(clause string, arg any) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Status != "" {
		add("status = $%d", f.Status)
	}
	if f.Type != "" {
		add("type = $%d", f.Type)
	}
	if f.Query != "" {
		// See tasks.Repository.List: a scan of the owner's rows, escaped by
		// db.Contains.
		add(`(title ILIKE $%[1]d ESCAPE '\' OR description ILIKE $%[1]d ESCAPE '\')`, db.Contains(f.Query))
	}

	query := `SELECT ` + goalColumns + ` FROM goals WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + Sorts[f.Sort] + `, id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select goals: %w", err)
	}
	defer rows.Close()

	out := []Goal{}
	ids := []uuid.UUID{}
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan goal: %w", err)
		}
		g.Milestones = []Milestone{}
		out = append(out, g)
		ids = append(ids, g.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate goals: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	// One batched query rather than one per goal: the list endpoint should not
	// get slower in proportion to the page size.
	byGoal, err := r.milestonesFor(ctx, ids...)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if ms := byGoal[out[i].ID]; ms != nil {
			out[i].Milestones = ms
		}
	}
	return out, nil
}

func (r *Repository) Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Goal, error) {
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
	if p.Type != nil {
		assign("type", *p.Type)
	}
	if p.Status != nil {
		assign("status", *p.Status)
	}
	if p.Description.Set {
		assign("description", p.Description.Value)
	}
	if p.Deadline.Set {
		assign("deadline", p.Deadline.Value)
	}

	g, err := scanGoal(r.db.QueryRowContext(ctx,
		`UPDATE goals SET `+strings.Join(set, ", ")+
			` WHERE id = $1 AND user_id = $2 RETURNING `+goalColumns, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Goal{}, ErrNotFound
	}
	if err != nil {
		return Goal{}, fmt.Errorf("update goal: %w", err)
	}
	byGoal, err := r.milestonesFor(ctx, g.ID)
	if err != nil {
		return Goal{}, err
	}
	g.Milestones = byGoal[g.ID]
	if g.Milestones == nil {
		g.Milestones = []Milestone{}
	}
	return g, nil
}

func (r *Repository) Delete(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM goals WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete goal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete goal: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountMilestones backs the per-goal cap. It is scoped through the goal, so it
// also answers "is this goal mine".
func (r *Repository) CountMilestones(ctx context.Context, userID, goalID uuid.UUID) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT count(m.*) FROM goals g
		LEFT JOIN goal_milestones m ON m.goal_id = g.id
		WHERE g.id = $1 AND g.user_id = $2
		GROUP BY g.id`, goalID, userID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("count milestones: %w", err)
	}
	return n, nil
}

// AddMilestone inserts only if the goal belongs to the caller. The ownership
// test is the INSERT's own SELECT, so there is no window between checking and
// writing.
func (r *Repository) AddMilestone(ctx context.Context, userID, goalID uuid.UUID, in MilestoneInput) (Milestone, error) {
	var m Milestone
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO goal_milestones (goal_id, title, target_date)
		SELECT g.id, $2, $3 FROM goals g WHERE g.id = $1 AND g.user_id = $4
		RETURNING id, goal_id, title, target_date, completed, created_at`,
		goalID, in.Title, in.TargetDate, userID,
	).Scan(&m.ID, &m.GoalID, &m.Title, &m.TargetDate, &m.Completed, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Milestone{}, ErrNotFound
	}
	if err != nil {
		return Milestone{}, fmt.Errorf("insert milestone: %w", err)
	}
	return m, nil
}

// UpdateMilestone patches a milestone, matching on the goal *and* the owner so
// a milestone id guessed from another account updates nothing.
func (r *Repository) UpdateMilestone(ctx context.Context, userID, goalID, milestoneID uuid.UUID, p MilestonePatch) (Milestone, error) {
	if p.Empty() {
		return r.MilestoneByID(ctx, userID, goalID, milestoneID)
	}

	var set []string
	args := []any{milestoneID, goalID, userID}
	assign := func(column string, value any) {
		args = append(args, value)
		set = append(set, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if p.Title != nil {
		assign("title", *p.Title)
	}
	if p.Completed != nil {
		assign("completed", *p.Completed)
	}
	if p.TargetDate.Set {
		assign("target_date", p.TargetDate.Value)
	}

	var m Milestone
	err := r.db.QueryRowContext(ctx, `
		UPDATE goal_milestones m SET `+strings.Join(set, ", ")+`
		FROM goals g
		WHERE m.id = $1 AND m.goal_id = $2 AND g.id = m.goal_id AND g.user_id = $3
		RETURNING m.id, m.goal_id, m.title, m.target_date, m.completed, m.created_at`, args...,
	).Scan(&m.ID, &m.GoalID, &m.Title, &m.TargetDate, &m.Completed, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Milestone{}, ErrMilestoneNotFound
	}
	if err != nil {
		return Milestone{}, fmt.Errorf("update milestone: %w", err)
	}
	return m, nil
}

func (r *Repository) MilestoneByID(ctx context.Context, userID, goalID, milestoneID uuid.UUID) (Milestone, error) {
	var m Milestone
	err := r.db.QueryRowContext(ctx, `
		SELECT m.id, m.goal_id, m.title, m.target_date, m.completed, m.created_at
		FROM goal_milestones m
		JOIN goals g ON g.id = m.goal_id
		WHERE m.id = $1 AND m.goal_id = $2 AND g.user_id = $3`,
		milestoneID, goalID, userID,
	).Scan(&m.ID, &m.GoalID, &m.Title, &m.TargetDate, &m.Completed, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Milestone{}, ErrMilestoneNotFound
	}
	if err != nil {
		return Milestone{}, fmt.Errorf("select milestone: %w", err)
	}
	return m, nil
}

// milestonesFor batch-loads by goal id. Callers pass ids they have already
// matched on user_id, so no owner clause is repeated here.
func (r *Repository) milestonesFor(ctx context.Context, goalIDs ...uuid.UUID) (map[uuid.UUID][]Milestone, error) {
	ids := make([]string, 0, len(goalIDs))
	for _, id := range goalIDs {
		ids = append(ids, id.String())
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, goal_id, title, target_date, completed, created_at
		FROM goal_milestones
		WHERE goal_id = ANY($1::uuid[])
		ORDER BY target_date ASC NULLS LAST, created_at ASC`, ids)
	if err != nil {
		return nil, fmt.Errorf("select milestones: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID][]Milestone{}
	for rows.Next() {
		var m Milestone
		if err := rows.Scan(&m.ID, &m.GoalID, &m.Title, &m.TargetDate, &m.Completed, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan milestone: %w", err)
		}
		out[m.GoalID] = append(out[m.GoalID], m)
	}
	return out, rows.Err()
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
