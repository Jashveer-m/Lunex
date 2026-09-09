package memories

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

// Repository is the Postgres store for memories. As with every other module,
// `user_id = $n` is part of the statement rather than a check applied after the
// row is already in memory.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// memoryColumns is the shared SELECT/RETURNING list. `embedding` is absent on
// purpose: nothing reads a vector back, the similarity is computed in SQL, and
// a 768-float column on every list row would be the largest thing the response
// never uses.
const memoryColumns = `id, user_id, type, content, importance, confidence,
	source_conversation_id, enabled, expires_at, created_at, updated_at`

func scanMemory(row interface{ Scan(...any) error }) (Memory, error) {
	var m Memory
	err := row.Scan(&m.ID, &m.UserID, &m.Type, &m.Content, &m.Importance, &m.Confidence,
		&m.SourceConversationID, &m.Enabled, &m.ExpiresAt, &m.CreatedAt, &m.UpdatedAt)
	return m, err
}

// Create writes one extracted fact.
func (r *Repository) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Memory, error) {
	m, err := scanMemory(r.db.QueryRowContext(ctx, `
		INSERT INTO memories (user_id, type, content, importance, confidence,
		                      source_conversation_id, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7::vector)
		RETURNING `+memoryColumns,
		userID, in.Type, in.Content, in.Importance, in.Confidence,
		in.SourceConversationID, db.Vector(in.Embedding)))
	if err != nil {
		return Memory{}, fmt.Errorf("insert memory: %w", err)
	}
	return m, nil
}

func (r *Repository) ByID(ctx context.Context, userID, id uuid.UUID) (Memory, error) {
	m, err := scanMemory(r.db.QueryRowContext(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, fmt.Errorf("select memory: %w", err)
	}
	return m, nil
}

func (r *Repository) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Memory, error) {
	where := []string{"user_id = $1"}
	args := []any{userID}
	if f.Type != "" {
		args = append(args, f.Type)
		where = append(where, fmt.Sprintf("type = $%d", len(args)))
	}
	if f.Enabled != nil {
		args = append(args, *f.Enabled)
		where = append(where, fmt.Sprintf("enabled = $%d", len(args)))
	}

	// Sorts is a fixed map; no request text reaches the ORDER BY.
	query := `SELECT ` + memoryColumns + ` FROM memories WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + Sorts[f.Sort] + `, id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select memories: %w", err)
	}
	defer rows.Close()

	out := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memories: %w", err)
	}
	return out, nil
}

// Update applies a patch. A non-nil embedding replaces the stored vector, which
// is what an edited content means: leaving the old vector in place would make
// the memory retrievable by its previous wording and not by its current one.
func (r *Repository) Update(ctx context.Context, userID, id uuid.UUID, p Patch, embedding []float32) (Memory, error) {
	if p.Empty() {
		return r.ByID(ctx, userID, id)
	}

	var set []string
	args := []any{id, userID}
	assign := func(column string, value any) {
		args = append(args, value)
		set = append(set, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if p.Content != nil {
		assign("content", *p.Content)
	}
	if p.Enabled != nil {
		assign("enabled", *p.Enabled)
	}
	if embedding != nil {
		args = append(args, db.Vector(embedding))
		set = append(set, fmt.Sprintf("embedding = $%d::vector", len(args)))
	}

	m, err := scanMemory(r.db.QueryRowContext(ctx,
		`UPDATE memories SET `+strings.Join(set, ", ")+
			` WHERE id = $1 AND user_id = $2 RETURNING `+memoryColumns, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, fmt.Errorf("update memory: %w", err)
	}
	return m, nil
}

func (r *Repository) Delete(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM memories WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAll empties one user's memory and reports how many rows went.
//
// The count is the point: "forget everything about me" is a destructive action
// and the response says what it destroyed, rather than a bare 204 that looks
// identical whether there were four hundred memories or none.
func (r *Repository) DeleteAll(ctx context.Context, userID uuid.UUID) (int, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM memories WHERE user_id = $1`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete memories: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete memories: %w", err)
	}
	return int(n), nil
}

// efSearch is how many candidates the HNSW graph walk keeps in flight. It is
// the same number, for the same reason, as documents.efSearch: the scan
// collects candidates from the graph first and applies `user_id = $1`
// afterwards, so a user whose memories are a small fraction of the table would
// otherwise get back fewer rows than they asked for.
const efSearch = 200

// Search returns the memories nearest to the query vector, owner-scoped.
//
// `<=>` is cosine distance, matching the vector_cosine_ops HNSW index. The
// ORDER BY is written on the operator rather than on the derived similarity
// column so the planner can still use that index.
//
// Three conditions narrow what is searchable at all, and each is in the WHERE
// clause rather than applied to the results: a memory the user switched off, a
// memory past its expiry, and a memory that was never embedded are all invisible
// to retrieval. `enabled` also matches the index's own predicate, so a disabled
// memory is not merely filtered out -- it is not in the graph being walked.
func (r *Repository) Search(ctx context.Context, userID uuid.UUID, embedding []float32, q SearchQuery) ([]SearchResult, error) {
	where := []string{
		"user_id = $1",
		"enabled",
		"embedding IS NOT NULL",
		"(expires_at IS NULL OR expires_at > now())",
	}
	args := []any{userID, db.Vector(embedding)}
	if q.MinSimilarity > 0 {
		// Converted to a distance here rather than in SQL, and cast explicitly.
		// Writing the bound as `<= 1 - $n` would leave the parameter's type to
		// be inferred from the literal `1`, which makes it an integer: 0.85
		// would arrive as 0 and the floor would silently admit everything.
		args = append(args, 1-q.MinSimilarity)
		where = append(where, fmt.Sprintf("(embedding <=> $2::vector) <= $%d::float8", len(args)))
	}
	args = append(args, q.Limit)

	// A transaction only so SET LOCAL has a scope to be local to: the setting
	// reverts on commit instead of leaking to the next query on this pooled
	// connection.
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin memory search: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; nothing to lose

	if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.ef_search = `+strconv.Itoa(efSearch)); err != nil {
		return nil, fmt.Errorf("set ef_search: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+memoryColumns+`, 1 - (embedding <=> $2::vector) AS similarity
		FROM memories
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY embedding <=> $2::vector
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	defer rows.Close()

	out := []SearchResult{}
	for rows.Next() {
		var s SearchResult
		if err := rows.Scan(&s.ID, &s.UserID, &s.Type, &s.Content, &s.Importance, &s.Confidence,
			&s.SourceConversationID, &s.Enabled, &s.ExpiresAt, &s.CreatedAt, &s.UpdatedAt,
			&s.Similarity); err != nil {
			return nil, fmt.Errorf("scan memory search result: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory search results: %w", err)
	}
	return out, nil
}
