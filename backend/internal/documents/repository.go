package documents

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

// Repository is the Postgres store for documents and their chunks. As with
// every other module, `user_id = $n` is part of the statement rather than a
// check applied after the row is already in memory.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// documentColumns is the shared SELECT/RETURNING list. The table is never
// aliased in these statements, so the Sorts fragments -- bare column names --
// stay unambiguous.
//
// extracted_text is absent on purpose: it is the largest column in the schema
// and no caller of a list needs it. ByID adds it explicitly.
const documentColumns = `documents.id, documents.user_id, documents.filename, documents.file_type,
	documents.status, documents.error_message, documents.created_at, documents.updated_at,
	(SELECT count(*) FROM document_chunks WHERE document_chunks.document_id = documents.id) AS chunk_count`

func scanDocument(row interface{ Scan(...any) error }) (Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.UserID, &d.Filename, &d.FileType, &d.Status, &d.ErrorMessage,
		&d.CreatedAt, &d.UpdatedAt, &d.ChunkCount)
	return d, err
}

// Create records the upload before any work happens, so a crash mid-pipeline
// leaves a row the user can see and delete rather than nothing at all.
func (r *Repository) Create(ctx context.Context, userID uuid.UUID, filename, fileType string) (Document, error) {
	d, err := scanDocument(r.db.QueryRowContext(ctx, `
		INSERT INTO documents (user_id, filename, file_type, status)
		VALUES ($1, $2, $3, '`+StatusProcessing+`')
		RETURNING `+documentColumns,
		userID, filename, fileType))
	if err != nil {
		return Document{}, fmt.Errorf("insert document: %w", err)
	}
	return d, nil
}

// Complete stores the extracted text and every chunk, then flips the document
// to `ready` -- all in one transaction. A document is therefore never
// half-indexed: either the search can see all of its chunks or none of them.
func (r *Repository) Complete(ctx context.Context, userID, id uuid.UUID, text string, chunks []Chunk) (Document, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has run

	// Re-processing an existing document would otherwise duplicate its chunks.
	// Nothing does that yet; the delete costs one indexed lookup and removes
	// the trap.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM document_chunks WHERE document_id = $1 AND user_id = $2`, id, userID); err != nil {
		return Document{}, fmt.Errorf("clear chunks: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO document_chunks (document_id, user_id, chunk_index, content, embedding)
		VALUES ($1, $2, $3, $4, $5::vector)`)
	if err != nil {
		return Document{}, fmt.Errorf("prepare chunk insert: %w", err)
	}
	defer stmt.Close()

	for _, c := range chunks {
		if _, err := stmt.ExecContext(ctx, id, userID, c.Index, c.Content, db.Vector(c.Embedding)); err != nil {
			return Document{}, fmt.Errorf("insert chunk %d: %w", c.Index, err)
		}
	}

	d, err := scanDocument(tx.QueryRowContext(ctx, `
		UPDATE documents
		SET status = '`+StatusReady+`', extracted_text = $3, error_message = NULL
		WHERE id = $1 AND user_id = $2
		RETURNING `+documentColumns,
		id, userID, text))
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("mark document ready: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit: %w", err)
	}
	return d, nil
}

// Fail records why processing stopped. A document must never be left in
// `processing`, so the service calls this on every pipeline error path.
func (r *Repository) Fail(ctx context.Context, userID, id uuid.UUID, message string) (Document, error) {
	d, err := scanDocument(r.db.QueryRowContext(ctx, `
		UPDATE documents SET status = '`+StatusFailed+`', error_message = $3
		WHERE id = $1 AND user_id = $2
		RETURNING `+documentColumns,
		id, userID, message))
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("mark document failed: %w", err)
	}
	return d, nil
}

// ByID returns one document including its extracted text. The text is the only
// way to read a document's content back in this phase -- there is no object
// store holding the original file -- so it is not held back behind a flag.
func (r *Repository) ByID(ctx context.Context, userID, id uuid.UUID) (Document, error) {
	var d Document
	err := r.db.QueryRowContext(ctx, `
		SELECT `+documentColumns+`, documents.extracted_text
		FROM documents WHERE documents.id = $1 AND documents.user_id = $2`, id, userID).
		Scan(&d.ID, &d.UserID, &d.Filename, &d.FileType, &d.Status, &d.ErrorMessage,
			&d.CreatedAt, &d.UpdatedAt, &d.ChunkCount, &d.ExtractedText)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("select document: %w", err)
	}
	return d, nil
}

func (r *Repository) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Document, error) {
	where := []string{"user_id = $1"}
	args := []any{userID}
	if f.Status != "" {
		args = append(args, f.Status)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}

	// Sorts is a fixed map; no request text reaches the ORDER BY.
	query := `SELECT ` + documentColumns + ` FROM documents WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + Sorts[f.Sort] + `, id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select documents: %w", err)
	}
	defer rows.Close()

	out := []Document{}
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate documents: %w", err)
	}
	return out, nil
}

// Delete removes the document; document_chunks cascades from it.
func (r *Repository) Delete(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM documents WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Passages returns one document's chunks in the order they appear in it,
// owner-scoped.
//
// It is the read behind "what does this document say" -- a question with no
// query to rank against -- and it is a plain indexed scan of
// document_chunks.document_id rather than a vector search: there is nothing to
// be near to, and the first chunks of a document are the ones a reader would
// start with.
//
// The owner is in the WHERE clause on the chunk itself, not only on the
// document: another user's document produces no rows here for the same
// structural reason it produces none anywhere else.
func (r *Repository) Passages(ctx context.Context, userID, documentID uuid.UUID, limit int) ([]Passage, error) {
	if limit <= 0 || limit > MaxPassages {
		limit = MaxPassages
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, c.document_id, d.filename, c.chunk_index, c.content
		FROM document_chunks c
		JOIN documents d ON d.id = c.document_id AND d.user_id = c.user_id
		WHERE c.user_id = $1 AND c.document_id = $2
		ORDER BY c.chunk_index ASC
		LIMIT $3`, userID, documentID, limit)
	if err != nil {
		return nil, fmt.Errorf("select document passages: %w", err)
	}
	defer rows.Close()

	out := []Passage{}
	for rows.Next() {
		var p Passage
		if err := rows.Scan(&p.ChunkID, &p.DocumentID, &p.Filename, &p.ChunkIndex, &p.Content); err != nil {
			return nil, fmt.Errorf("scan document passage: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate document passages: %w", err)
	}
	return out, nil
}

// efSearch is how many candidates the HNSW graph walk keeps in flight.
//
// It matters more than usual here because every search is filtered by
// user_id. An HNSW scan collects its candidates from the graph first and
// applies the WHERE clause afterwards, so with the default (40) a user whose
// chunks are a small fraction of the table can have most candidates filtered
// away and get back fewer rows than they asked for. Raising the number widens
// the walk; it costs a little latency and buys recall that a search silently
// returning three of five results would otherwise lose.
const efSearch = 200

// Search returns the chunks nearest to the query vector, owner-scoped.
//
// `<=>` is cosine distance, matching the vector_cosine_ops HNSW index. The
// ORDER BY is written on the operator rather than on the derived similarity
// column so the planner can still use that index; ordering by
// `1 - (a <=> b) DESC` would force a sequential scan over every chunk.
func (r *Repository) Search(ctx context.Context, userID uuid.UUID, embedding []float32, q SearchQuery) ([]SearchResult, error) {
	// Only ready documents are searchable. A failed one has no chunks anyway;
	// this also keeps a future re-processing path from serving partial results.
	where := []string{"c.user_id = $1", "c.embedding IS NOT NULL", "d.status = '" + StatusReady + "'"}
	args := []any{userID, db.Vector(embedding)}
	if len(q.DocumentIDs) > 0 {
		ids := make([]string, 0, len(q.DocumentIDs))
		for _, id := range q.DocumentIDs {
			ids = append(ids, id.String())
		}
		args = append(args, ids)
		where = append(where, fmt.Sprintf("c.document_id = ANY($%d::uuid[])", len(args)))
	}
	if q.MinSimilarity > 0 {
		// Converted to a distance here rather than in SQL, and cast
		// explicitly. Writing the bound as `<= 1 - $n` would leave the
		// parameter's type to be inferred from the literal `1`, which makes it
		// an integer: 0.85 would arrive as 0 and the floor would silently
		// admit everything.
		args = append(args, 1-q.MinSimilarity)
		where = append(where, fmt.Sprintf("(c.embedding <=> $2::vector) <= $%d::float8", len(args)))
	}
	args = append(args, q.Limit)

	// A transaction only so SET LOCAL has a scope to be local to: the setting
	// reverts on commit instead of leaking to the next query on this pooled
	// connection.
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin search: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; nothing to lose

	if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.ef_search = `+strconv.Itoa(efSearch)); err != nil {
		return nil, fmt.Errorf("set ef_search: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.document_id, d.filename, c.chunk_index, c.content,
		       1 - (c.embedding <=> $2::vector) AS similarity
		FROM document_chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY c.embedding <=> $2::vector
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	defer rows.Close()

	out := []SearchResult{}
	for rows.Next() {
		var s SearchResult
		if err := rows.Scan(&s.ChunkID, &s.DocumentID, &s.Filename, &s.ChunkIndex, &s.Content, &s.Similarity); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search results: %w", err)
	}
	return out, nil
}
