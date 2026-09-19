package documents

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/embeddings"
)

// fakeStore keys everything by (owner, id), the same shape the SQL has, so a
// service that forgot to pass the caller's id shows up here as a miss.
type fakeStore struct {
	mu     sync.Mutex
	byUser map[uuid.UUID]map[uuid.UUID]Document
	chunks map[uuid.UUID][]Chunk
	calls  []uuid.UUID
	// failErr, when set, makes Fail itself fail -- the "document is stuck in
	// processing" path.
	failErr error
	// completeErr simulates a write failure after the embeddings came back.
	completeErr error

	lastVector []float32
	lastQuery  SearchQuery
}

func newFakeStore() *fakeStore {
	return &fakeStore{byUser: map[uuid.UUID]map[uuid.UUID]Document{}, chunks: map[uuid.UUID][]Chunk{}}
}

func (f *fakeStore) put(d Document) {
	if f.byUser[d.UserID] == nil {
		f.byUser[d.UserID] = map[uuid.UUID]Document{}
	}
	f.byUser[d.UserID][d.ID] = d
}

func (f *fakeStore) Create(_ context.Context, userID uuid.UUID, filename, fileType string) (Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	d := Document{ID: uuid.New(), UserID: userID, Filename: filename, FileType: fileType, Status: StatusProcessing}
	f.put(d)
	return d, nil
}

func (f *fakeStore) Complete(_ context.Context, userID, id uuid.UUID, text string, chunks []Chunk) (Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if f.completeErr != nil {
		return Document{}, f.completeErr
	}
	d, ok := f.byUser[userID][id]
	if !ok {
		return Document{}, ErrNotFound
	}
	d.Status, d.ExtractedText, d.ErrorMessage, d.ChunkCount = StatusReady, &text, nil, len(chunks)
	f.put(d)
	f.chunks[id] = chunks
	return d, nil
}

func (f *fakeStore) Fail(_ context.Context, userID, id uuid.UUID, message string) (Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if f.failErr != nil {
		return Document{}, f.failErr
	}
	d, ok := f.byUser[userID][id]
	if !ok {
		return Document{}, ErrNotFound
	}
	d.Status, d.ErrorMessage = StatusFailed, &message
	f.put(d)
	return d, nil
}

func (f *fakeStore) ByID(_ context.Context, userID, id uuid.UUID) (Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	d, ok := f.byUser[userID][id]
	if !ok {
		return Document{}, ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) List(_ context.Context, userID uuid.UUID, _ Filter) ([]Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	out := []Document{}
	for _, d := range f.byUser[userID] {
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeStore) Delete(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if _, ok := f.byUser[userID][id]; !ok {
		return ErrNotFound
	}
	delete(f.byUser[userID], id)
	delete(f.chunks, id)
	return nil
}

func (f *fakeStore) Search(_ context.Context, userID uuid.UUID, embedding []float32, q SearchQuery) ([]SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	f.lastVector = embedding
	f.lastQuery = q
	out := []SearchResult{}
	for id, chunks := range f.chunks {
		if _, mine := f.byUser[userID][id]; !mine {
			continue
		}
		for _, c := range chunks {
			out = append(out, SearchResult{DocumentID: id, ChunkIndex: c.Index, Content: c.Content, Similarity: 1})
		}
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// Passages is the same store read in document order, which is what the
// no-query read returns.
func (f *fakeStore) Passages(_ context.Context, userID, documentID uuid.UUID, limit int) ([]Passage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	out := []Passage{}
	if _, mine := f.byUser[userID][documentID]; !mine {
		return out, nil
	}
	for _, c := range f.chunks[documentID] {
		out = append(out, Passage{DocumentID: documentID, ChunkIndex: c.Index, Content: c.Content})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkIndex < out[j].ChunkIndex })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// fakeEmbedder returns a fixed-width vector per text without a model server.
type fakeEmbedder struct {
	dims  int
	err   error
	calls [][]string
	mu    sync.Mutex
}

func newFakeEmbedder() *fakeEmbedder { return &fakeEmbedder{dims: 4} }

func (e *fakeEmbedder) Dimensions() int { return e.dims }
func (e *fakeEmbedder) Model() string   { return "fake" }

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, texts)
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, e.dims)
		for j, r := range t {
			v[j%e.dims] += float32(r)
		}
		out[i] = v
	}
	return out, nil
}

func newTestService(t *testing.T) (*Service, *fakeStore, *fakeEmbedder) {
	t.Helper()
	store, embedder := newFakeStore(), newFakeEmbedder()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(store, embedder, discard, 5*time.Second), store, embedder
}

func upload(t *testing.T, svc *Service, userID uuid.UUID, name, body string) Document {
	t.Helper()
	doc, err := svc.Upload(context.Background(), userID, UploadInput{
		Filename: name, FileType: DetectType(name, []byte(body)), Content: []byte(body),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	return doc
}

func TestUploadIndexesTheDocument(t *testing.T) {
	svc, store, embedder := newTestService(t)
	user := uuid.New()

	doc := upload(t, svc, user, "notes.txt", "Aurora borealis over the tundra.")

	if doc.Status != StatusReady {
		t.Fatalf("status = %q (%v), want %q", doc.Status, doc.ErrorMessage, StatusReady)
	}
	if doc.ErrorMessage != nil {
		t.Fatalf("error_message = %q, want none on a ready document", *doc.ErrorMessage)
	}
	chunks := store.chunks[doc.ID]
	if len(chunks) != 1 {
		t.Fatalf("stored %d chunks, want 1", len(chunks))
	}
	if chunks[0].Index != 0 || chunks[0].Content != "Aurora borealis over the tundra." {
		t.Fatalf("chunk = %+v, want index 0 holding the file's text", chunks[0])
	}
	if len(chunks[0].Embedding) != embedder.Dimensions() {
		t.Fatalf("embedding width = %d, want %d", len(chunks[0].Embedding), embedder.Dimensions())
	}
	// One batched call, not one per chunk.
	if len(embedder.calls) != 1 {
		t.Fatalf("embedder called %d times, want 1 batched call", len(embedder.calls))
	}
}

func TestUploadChunkIndexesAreDenseAndOrdered(t *testing.T) {
	svc, store, _ := newTestService(t)
	user := uuid.New()

	words := make([]string, 0, ChunkWords*3)
	for i := range cap(words) {
		words = append(words, wordFor(i))
	}
	doc := upload(t, svc, user, "long.md", strings.Join(words, " "))

	chunks := store.chunks[doc.ID]
	if len(chunks) < 3 {
		t.Fatalf("stored %d chunks, want at least 3", len(chunks))
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Fatalf("chunk %d has index %d; indexes must be dense and ordered so a citation can name one", i, c.Index)
		}
	}
}

// A document must never be left in `processing`, whatever went wrong.
func TestUploadRecordsExtractionFailure(t *testing.T) {
	svc, store, _ := newTestService(t)
	user := uuid.New()

	doc, err := svc.Upload(context.Background(), user, UploadInput{
		Filename: "broken.pdf", FileType: TypePDF, Content: []byte("%PDF-1.4\nnot really"),
	})
	if err != nil {
		t.Fatalf("Upload returned an error; a pipeline failure is a failed document, not a 500: %v", err)
	}
	if doc.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", doc.Status, StatusFailed)
	}
	if doc.ErrorMessage == nil || *doc.ErrorMessage == "" {
		t.Fatal("a failed document must say why")
	}
	if len(store.chunks[doc.ID]) != 0 {
		t.Fatal("a failed document must have no chunks")
	}
}

func TestUploadRecordsEmptyText(t *testing.T) {
	svc, _, _ := newTestService(t)
	doc, err := svc.Upload(context.Background(), uuid.New(), UploadInput{
		Filename: "blank.txt", FileType: TypeText, Content: []byte("   \n\n  "),
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", doc.Status, StatusFailed)
	}
	// The message has to point at OCR: an empty scanned PDF is the common case
	// and "no text" alone sends the user looking for a corrupt file.
	if !strings.Contains(*doc.ErrorMessage, "OCR") {
		t.Fatalf("message = %q, want it to mention OCR", *doc.ErrorMessage)
	}
}

func TestUploadRecordsEmbeddingOutage(t *testing.T) {
	svc, store, embedder := newTestService(t)
	embedder.err = embeddings.ErrUnavailable
	user := uuid.New()

	doc, err := svc.Upload(context.Background(), user, UploadInput{
		Filename: "notes.txt", FileType: TypeText, Content: []byte("some text"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", doc.Status, StatusFailed)
	}
	if !strings.Contains(*doc.ErrorMessage, "embedding service") {
		t.Fatalf("message = %q, want it to name the embedding service", *doc.ErrorMessage)
	}
	// The message must not claim the document was saved: Complete never ran,
	// so neither the chunks nor the extracted text were stored.
	if doc.ExtractedText != nil {
		t.Fatalf("extracted_text = %q, want none -- nothing was committed", *doc.ExtractedText)
	}
	if len(store.chunks[doc.ID]) != 0 {
		t.Fatal("nothing should be stored when the embeddings never arrived")
	}
}

// The stored message is read back over the API, so it must be one of this
// package's own sentences and never the wrapped driver error.
func TestUploadDoesNotLeakInternalErrors(t *testing.T) {
	svc, store, _ := newTestService(t)
	store.completeErr = errors.New("pq: connection to server at \"10.0.0.7\", port 5432 failed")

	doc, err := svc.Upload(context.Background(), uuid.New(), UploadInput{
		Filename: "notes.txt", FileType: TypeText, Content: []byte("some text"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", doc.Status, StatusFailed)
	}
	if strings.Contains(*doc.ErrorMessage, "10.0.0.7") || strings.Contains(*doc.ErrorMessage, "pq:") {
		t.Fatalf("error_message = %q, which leaks the underlying error", *doc.ErrorMessage)
	}
}

// If even the "mark it failed" write fails, the row really is stuck; the
// caller has to hear about that rather than get a document that looks fine.
func TestUploadSurfacesAStuckDocument(t *testing.T) {
	svc, store, embedder := newTestService(t)
	embedder.err = errors.New("boom")
	store.failErr = errors.New("database is gone")

	_, err := svc.Upload(context.Background(), uuid.New(), UploadInput{
		Filename: "notes.txt", FileType: TypeText, Content: []byte("some text"),
	})
	if err == nil {
		t.Fatal("want an error when the document cannot even be marked failed")
	}
	if !strings.Contains(err.Error(), "stuck in processing") {
		t.Fatalf("error = %v, want it to say the document is stuck", err)
	}
}

// The context governing the pipeline is cancelled, but marking the document
// failed must still land -- otherwise a timed-out upload is stuck forever.
func TestUploadMarksFailedAfterTheContextExpires(t *testing.T) {
	store, embedder := newFakeStore(), newFakeEmbedder()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A processing budget short enough that the embedder cannot finish.
	svc := NewService(store, embedder, discard, time.Millisecond)
	embedder.err = context.DeadlineExceeded

	doc, err := svc.Upload(context.Background(), uuid.New(), UploadInput{
		Filename: "notes.txt", FileType: TypeText, Content: []byte("some text"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", doc.Status, StatusFailed)
	}
	if !strings.Contains(*doc.ErrorMessage, "too long") {
		t.Fatalf("message = %q, want it to say processing took too long", *doc.ErrorMessage)
	}
}

// A document too large to process synchronously is refused as a whole rather
// than indexed halfway.
func TestUploadRefusesTooManyChunks(t *testing.T) {
	svc, store, _ := newTestService(t)
	words := make([]string, 0, (MaxChunks+2)*(ChunkWords-OverlapWords))
	for i := range cap(words) {
		words = append(words, wordFor(i))
	}

	doc, err := svc.Upload(context.Background(), uuid.New(), UploadInput{
		Filename: "huge.txt", FileType: TypeText, Content: []byte(strings.Join(words, " ")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", doc.Status, StatusFailed)
	}
	if !strings.Contains(*doc.ErrorMessage, "too large") {
		t.Fatalf("message = %q, want it to say the document is too large", *doc.ErrorMessage)
	}
	if len(store.chunks[doc.ID]) != 0 {
		t.Fatal("nothing should be stored for a refused document")
	}
}

// The authenticated user id -- and nothing else -- is what reaches the store.
func TestEveryCallIsScopedToTheCaller(t *testing.T) {
	svc, store, _ := newTestService(t)
	user := uuid.New()

	doc := upload(t, svc, user, "notes.txt", "Aurora borealis.")
	if _, err := svc.Get(context.Background(), user, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.List(context.Background(), user, Filter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Search(context.Background(), user, SearchQuery{Query: "aurora"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), user, doc.ID); err != nil {
		t.Fatal(err)
	}

	if len(store.calls) == 0 {
		t.Fatal("the store was never called")
	}
	for i, got := range store.calls {
		if got != user {
			t.Fatalf("store call %d used user %s, want the caller %s", i, got, user)
		}
	}
}

func TestAnotherUsersDocumentIsNotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	alice, bob := uuid.New(), uuid.New()
	doc := upload(t, svc, alice, "notes.txt", "Aurora borealis.")

	if _, err := svc.Get(context.Background(), bob, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get as another user = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(context.Background(), bob, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete as another user = %v, want ErrNotFound", err)
	}
	results, err := svc.Search(context.Background(), bob, SearchQuery{Query: "aurora"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("search as another user returned %d results, want 0", len(results))
	}
}

func TestSearchEmbedsTheQueryAndAppliesDefaults(t *testing.T) {
	svc, store, embedder := newTestService(t)
	user := uuid.New()
	upload(t, svc, user, "notes.txt", "Aurora borealis.")
	embedder.calls = nil

	if _, err := svc.Search(context.Background(), user, SearchQuery{Query: "  aurora  "}); err != nil {
		t.Fatal(err)
	}
	if len(embedder.calls) != 1 || len(embedder.calls[0]) != 1 || embedder.calls[0][0] != "aurora" {
		t.Fatalf("embedder calls = %v, want one call with the trimmed query", embedder.calls)
	}
	if store.lastQuery.Limit != DefaultSearchLimit {
		t.Fatalf("limit reaching the store = %d, want the default %d", store.lastQuery.Limit, DefaultSearchLimit)
	}
	if len(store.lastVector) != embedder.Dimensions() {
		t.Fatalf("vector width reaching the store = %d, want %d", len(store.lastVector), embedder.Dimensions())
	}
}

func TestSearchValidatesBeforeEmbedding(t *testing.T) {
	svc, _, embedder := newTestService(t)
	if _, err := svc.Search(context.Background(), uuid.New(), SearchQuery{Query: "  "}); err == nil {
		t.Fatal("want a validation error for an empty query")
	}
	if len(embedder.calls) != 0 {
		t.Fatal("an invalid query must not reach the model server")
	}
}

// Search cannot fall back to anything when the model server is down, so the
// error has to be recognisable as an outage rather than a 500.
func TestSearchReportsAnEmbeddingOutage(t *testing.T) {
	svc, _, embedder := newTestService(t)
	embedder.err = embeddings.ErrUnavailable

	_, err := svc.Search(context.Background(), uuid.New(), SearchQuery{Query: "aurora"})
	if !errors.Is(err, ErrEmbedding) {
		t.Fatalf("error = %v, want it to wrap ErrEmbedding", err)
	}
	if !errors.Is(err, embeddings.ErrUnavailable) {
		t.Fatalf("error = %v, want the cause to survive as ErrUnavailable", err)
	}
}
