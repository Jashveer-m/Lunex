package db_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/embeddings"
)

// These cover the Phase 3 SQL: the pgvector column and its operator, the
// owner-scoped similarity search, and the cascade from documents to chunks.
// Nothing here talks to Ollama -- vectors are constructed by hand, so the
// distances are arithmetic the test can predict rather than a model's opinion.

// unit returns a 768-wide unit vector pointing at one axis, so cosine
// similarity between two of them is 1 when the axes match and 0 otherwise.
func unit(axis int) []float32 {
	v := make([]float32, embeddings.DefaultDimensions)
	v[axis%embeddings.DefaultDimensions] = 1
	return v
}

// mix blends two axes, giving a vector a predictable similarity to each.
func mix(a, b int, weight float64) []float32 {
	v := make([]float32, embeddings.DefaultDimensions)
	v[a%embeddings.DefaultDimensions] = float32(weight)
	v[b%embeddings.DefaultDimensions] = float32(math.Sqrt(1 - weight*weight))
	return v
}

func makeDocument(t *testing.T, repo *documents.Repository, owner uuid.UUID, name string, chunks []documents.Chunk) documents.Document {
	t.Helper()
	ctx := context.Background()
	doc, err := repo.Create(ctx, owner, name, documents.TypeText)
	if err != nil {
		t.Fatalf("create document %s: %v", name, err)
	}
	if doc.Status != documents.StatusProcessing {
		t.Fatalf("a new document is %q, want %q", doc.Status, documents.StatusProcessing)
	}
	text := make([]string, len(chunks))
	for i, c := range chunks {
		text[i] = c.Content
	}
	ready, err := repo.Complete(ctx, owner, doc.ID, strings.Join(text, "\n"), chunks)
	if err != nil {
		t.Fatalf("complete document %s: %v", name, err)
	}
	return ready
}

func TestDocumentRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	doc := makeDocument(t, repo, owner, "field-notes.txt", []documents.Chunk{
		{Index: 0, Content: "Aurora borealis over the tundra.", Embedding: unit(0)},
		{Index: 1, Content: "The dogs slept through it.", Embedding: unit(1)},
	})

	if doc.Status != documents.StatusReady {
		t.Fatalf("status = %q, want %q", doc.Status, documents.StatusReady)
	}
	if doc.ErrorMessage != nil {
		t.Fatalf("error_message = %q, want it cleared on success", *doc.ErrorMessage)
	}
	// chunk_count is counted, not stored, so it cannot disagree with the rows.
	if doc.ChunkCount != 2 {
		t.Fatalf("chunk_count = %d, want 2", doc.ChunkCount)
	}

	got, err := repo.ByID(ctx, owner, doc.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.ExtractedText == nil || !strings.Contains(*got.ExtractedText, "Aurora") {
		t.Fatalf("extracted_text = %v, want the stored text", got.ExtractedText)
	}
	if got.Filename != "field-notes.txt" || got.FileType != documents.TypeText {
		t.Fatalf("round trip lost metadata: %+v", got)
	}

	// A list omits the text column: it is the largest in the schema and no
	// caller of a list needs it.
	listed, err := repo.List(ctx, owner, documents.Filter{Sort: documents.DefaultSort, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d documents, want 1", len(listed))
	}
	if listed[0].ExtractedText != nil {
		t.Fatal("List returned extracted_text; it must be left out of list responses")
	}
	if listed[0].ChunkCount != 2 {
		t.Fatalf("listed chunk_count = %d, want 2", listed[0].ChunkCount)
	}
}

// The similarity the API reports has to match the ordering the index scan
// used, and both have to be cosine.
func TestSearchOrdersByCosineSimilarity(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	makeDocument(t, repo, owner, "notes.txt", []documents.Chunk{
		{Index: 0, Content: "exact match", Embedding: unit(0)},
		{Index: 1, Content: "close match", Embedding: mix(0, 5, 0.8)},
		{Index: 2, Content: "unrelated", Embedding: unit(9)},
	})

	got, err := repo.Search(ctx, owner, unit(0), documents.SearchQuery{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	if got[0].Content != "exact match" || got[1].Content != "close match" || got[2].Content != "unrelated" {
		t.Fatalf("order = %q, want exact, close, unrelated", contents(got))
	}
	if math.Abs(got[0].Similarity-1) > 1e-6 {
		t.Fatalf("similarity of an identical vector = %v, want 1", got[0].Similarity)
	}
	if math.Abs(got[1].Similarity-0.8) > 1e-6 {
		t.Fatalf("similarity of the 0.8 blend = %v, want 0.8", got[1].Similarity)
	}
	if math.Abs(got[2].Similarity) > 1e-6 {
		t.Fatalf("similarity of an orthogonal vector = %v, want 0", got[2].Similarity)
	}
	// The citation a caller needs to attribute the chunk.
	if got[0].Filename != "notes.txt" || got[0].ChunkIndex != 0 || got[0].DocumentID == uuid.Nil {
		t.Fatalf("result is missing its citation: %+v", got[0])
	}
}

func TestSearchLimitAndMinSimilarity(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	makeDocument(t, repo, owner, "notes.txt", []documents.Chunk{
		{Index: 0, Content: "one", Embedding: unit(0)},
		{Index: 1, Content: "two", Embedding: mix(0, 5, 0.9)},
		{Index: 2, Content: "three", Embedding: mix(0, 5, 0.5)},
		{Index: 3, Content: "four", Embedding: unit(9)},
	})

	got, err := repo.Search(ctx, owner, unit(0), documents.SearchQuery{Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want the limit of 2", len(got))
	}

	// Vector search always returns the nearest N however far away they are, so
	// the floor is what stops an unrelated question coming back with
	// confident-looking citations.
	got, err = repo.Search(ctx, owner, unit(0), documents.SearchQuery{Limit: 10, MinSimilarity: 0.85})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results above 0.85, want 2: %q", len(got), contents(got))
	}
	for _, r := range got {
		if r.Similarity < 0.85 {
			t.Fatalf("result %q has similarity %v, below the floor", r.Content, r.Similarity)
		}
	}
}

// The property the denormalized user_id column exists for.
func TestSearchNeverCrossesUsers(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	alice := makeUser(t, pool, "alice@example.com")
	bob := makeUser(t, pool, "bob@example.com")

	makeDocument(t, repo, alice, "alice.txt", []documents.Chunk{
		{Index: 0, Content: "Alice's secret", Embedding: unit(0)},
	})
	makeDocument(t, repo, bob, "bob.txt", []documents.Chunk{
		{Index: 0, Content: "Bob's own note", Embedding: unit(3)},
	})

	// Bob searches with the vector that is an exact match for Alice's chunk.
	got, err := repo.Search(ctx, bob, unit(0), documents.SearchQuery{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range got {
		if strings.Contains(r.Content, "Alice") {
			t.Fatalf("Bob's search returned Alice's chunk: %+v", r)
		}
	}
	if len(got) != 1 || got[0].Content != "Bob's own note" {
		t.Fatalf("Bob got %q, want only his own chunk", contents(got))
	}

	if _, err := repo.ByID(ctx, bob, mustFirstID(t, repo, alice)); !errors.Is(err, documents.ErrNotFound) {
		t.Fatalf("ByID across users = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, bob, mustFirstID(t, repo, alice)); !errors.Is(err, documents.ErrNotFound) {
		t.Fatalf("Delete across users = %v, want ErrNotFound", err)
	}
}

func TestSearchCanBeRestrictedToDocuments(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	first := makeDocument(t, repo, owner, "first.txt", []documents.Chunk{
		{Index: 0, Content: "in the first document", Embedding: unit(0)},
	})
	makeDocument(t, repo, owner, "second.txt", []documents.Chunk{
		{Index: 0, Content: "in the second document", Embedding: unit(0)},
	})

	got, err := repo.Search(ctx, owner, unit(0), documents.SearchQuery{
		Limit: 10, DocumentIDs: []uuid.UUID{first.ID},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].DocumentID != first.ID {
		t.Fatalf("got %q, want only the chunk from the named document", contents(got))
	}
}

// A failed document has no chunks, but a search must not surface them even if
// a future re-processing path leaves some behind.
func TestSearchSkipsDocumentsThatAreNotReady(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	doc := makeDocument(t, repo, owner, "notes.txt", []documents.Chunk{
		{Index: 0, Content: "indexed once", Embedding: unit(0)},
	})
	if _, err := repo.Fail(ctx, owner, doc.ID, "the embedding service is unavailable"); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	got, err := repo.Search(ctx, owner, unit(0), documents.SearchQuery{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %q from a failed document, want none", contents(got))
	}
}

// Re-completing a document must replace its chunks, not add a second copy.
func TestCompleteReplacesChunks(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	doc := makeDocument(t, repo, owner, "notes.txt", []documents.Chunk{
		{Index: 0, Content: "first pass", Embedding: unit(0)},
		{Index: 1, Content: "also first pass", Embedding: unit(1)},
	})
	again, err := repo.Complete(ctx, owner, doc.ID, "second", []documents.Chunk{
		{Index: 0, Content: "second pass", Embedding: unit(0)},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if again.ChunkCount != 1 {
		t.Fatalf("chunk_count = %d after re-processing, want 1", again.ChunkCount)
	}
}

// Failure is not a dead end: the document keeps its id and says why.
func TestFailRecordsTheReason(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	doc, err := repo.Create(ctx, owner, "scan.pdf", documents.TypePDF)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := repo.Fail(ctx, owner, doc.ID, "No text could be extracted.")
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if failed.Status != documents.StatusFailed {
		t.Fatalf("status = %q, want %q", failed.Status, documents.StatusFailed)
	}
	if failed.ErrorMessage == nil || *failed.ErrorMessage != "No text could be extracted." {
		t.Fatalf("error_message = %v, want the reason", failed.ErrorMessage)
	}
	// updated_at moved, so a client can tell the row changed.
	if !failed.UpdatedAt.After(doc.UpdatedAt) {
		t.Fatalf("updated_at = %v, want it after %v -- the trigger did not fire", failed.UpdatedAt, doc.UpdatedAt)
	}
}

// The CHECK constraint is the backstop for a typo in the Go constants.
func TestStatusIsConstrained(t *testing.T) {
	pool := testDB(t)
	owner := makeUser(t, pool, "ada@example.com")

	_, err := pool.Exec(
		`INSERT INTO documents (user_id, filename, file_type, status) VALUES ($1, 'x.txt', 'txt', 'done')`, owner)
	if err == nil {
		t.Fatal("the database accepted a status outside processing/ready/failed")
	}
	if !strings.Contains(err.Error(), "documents_status_check") {
		t.Fatalf("error = %v, want the status CHECK constraint", err)
	}
}

func TestDeletingADocumentTakesItsChunks(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	doc := makeDocument(t, repo, owner, "notes.txt", []documents.Chunk{
		{Index: 0, Content: "one", Embedding: unit(0)},
		{Index: 1, Content: "two", Embedding: unit(1)},
	})
	if err := repo.Delete(ctx, owner, doc.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := countChunks(t, pool); n != 0 {
		t.Fatalf("%d chunks survived the document", n)
	}
	if err := repo.Delete(ctx, owner, doc.ID); !errors.Is(err, documents.ErrNotFound) {
		t.Fatalf("deleting twice = %v, want ErrNotFound", err)
	}
}

func TestDeletingAUserTakesTheirDocumentsAndChunks(t *testing.T) {
	pool := testDB(t)
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	makeDocument(t, repo, owner, "notes.txt", []documents.Chunk{
		{Index: 0, Content: "one", Embedding: unit(0)},
	})
	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"documents", "document_chunks"} {
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows survived in %s after the user was deleted", n, table)
		}
	}
}

// A realistic vector -- 768 dense float32s -- has to survive the text encoding
// the driver sends it as, or every similarity would be subtly wrong.
func TestEmbeddingSurvivesTheDriverRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	rng := rand.New(rand.NewSource(7))
	v := make([]float32, embeddings.DefaultDimensions)
	var norm float64
	for i := range v {
		v[i] = float32(rng.NormFloat64())
		norm += float64(v[i]) * float64(v[i])
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}

	makeDocument(t, repo, owner, "notes.txt", []documents.Chunk{{Index: 0, Content: "dense", Embedding: v}})

	got, err := repo.Search(ctx, owner, v, documents.SearchQuery{Limit: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	// float32 through pgvector's text form is exact, so a vector searched
	// against itself is similarity 1 and not merely close.
	if math.Abs(got[0].Similarity-1) > 1e-6 {
		t.Fatalf("a vector searched against itself scored %v, want 1: the encoding lost precision", got[0].Similarity)
	}
}

func contents(rs []documents.SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Content
	}
	return out
}

func countChunks(t *testing.T, pool *sql.DB) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT count(*) FROM document_chunks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustFirstID(t *testing.T, repo *documents.Repository, owner uuid.UUID) uuid.UUID {
	t.Helper()
	list, err := repo.List(context.Background(), owner, documents.Filter{Sort: documents.DefaultSort, Limit: 1})
	if err != nil || len(list) == 0 {
		t.Fatalf("no document for %s: %v", owner, err)
	}
	return list[0].ID
}
