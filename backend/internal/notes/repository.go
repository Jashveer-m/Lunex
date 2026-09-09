package notes

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

// Repository is the Postgres store for notes. As with tasks and goals,
// `user_id = $n` is part of every statement rather than a check applied after
// the row is already in memory.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// noteColumns is the shared SELECT/RETURNING list; tags is rendered as JSON,
// see db.TextArray for why.
var noteColumns = `id, user_id, title, content, ` + db.ArrayToJSON("tags") +
	`, created_at, updated_at`

func scanNote(row interface{ Scan(...any) error }) (Note, error) {
	var n Note
	var tags db.TextArray
	err := row.Scan(&n.ID, &n.UserID, &n.Title, &n.Content, &tags, &n.CreatedAt, &n.UpdatedAt)
	n.Tags = tags
	return n, err
}

func (r *Repository) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Note, error) {
	n, err := scanNote(r.db.QueryRowContext(ctx, `
		INSERT INTO notes (user_id, title, content, tags)
		VALUES ($1, $2, $3, $4)
		RETURNING `+noteColumns,
		userID, in.Title, in.Content, db.TextArrayParam(in.Tags)))
	if err != nil {
		return Note{}, fmt.Errorf("insert note: %w", err)
	}
	return n, nil
}

func (r *Repository) ByID(ctx context.Context, userID, id uuid.UUID) (Note, error) {
	n, err := scanNote(r.db.QueryRowContext(ctx,
		`SELECT `+noteColumns+` FROM notes WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, ErrNotFound
	}
	if err != nil {
		return Note{}, fmt.Errorf("select note: %w", err)
	}
	return n, nil
}

func (r *Repository) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Note, error) {
	where := []string{"user_id = $1"}
	args := []any{userID}
	if f.Tag != "" {
		args = append(args, f.Tag)
		// Containment rather than `= ANY(tags)`: only the former uses the GIN index.
		where = append(where, fmt.Sprintf("tags @> ARRAY[$%d]::text[]", len(args)))
	}

	query := `SELECT ` + noteColumns + ` FROM notes WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + Sorts[f.Sort] + `, id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select notes: %w", err)
	}
	defer rows.Close()

	out := []Note{}
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, fmt.Errorf("scan note: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notes: %w", err)
	}
	return out, nil
}

func (r *Repository) Update(ctx context.Context, userID, id uuid.UUID, p Patch) (Note, error) {
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
	if p.Content != nil {
		assign("content", *p.Content)
	}
	if p.Tags != nil {
		assign("tags", db.TextArrayParam(*p.Tags))
	}

	n, err := scanNote(r.db.QueryRowContext(ctx,
		`UPDATE notes SET `+strings.Join(set, ", ")+
			` WHERE id = $1 AND user_id = $2 RETURNING `+noteColumns, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, ErrNotFound
	}
	if err != nil {
		return Note{}, fmt.Errorf("update note: %w", err)
	}
	return n, nil
}

func (r *Repository) Delete(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM notes WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete note: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete note: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
