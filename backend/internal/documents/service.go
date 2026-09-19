package documents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/embeddings"
)

// Store is the slice of the repository the service needs. Every method takes
// the owner id, so a document cannot be reached without saying whose it is.
type Store interface {
	Create(ctx context.Context, userID uuid.UUID, filename, fileType string) (Document, error)
	Complete(ctx context.Context, userID, id uuid.UUID, text string, chunks []Chunk) (Document, error)
	Fail(ctx context.Context, userID, id uuid.UUID, message string) (Document, error)
	ByID(ctx context.Context, userID, id uuid.UUID) (Document, error)
	List(ctx context.Context, userID uuid.UUID, f Filter) ([]Document, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
	Search(ctx context.Context, userID uuid.UUID, embedding []float32, q SearchQuery) ([]SearchResult, error)
	Passages(ctx context.Context, userID, documentID uuid.UUID, limit int) ([]Passage, error)
}

// NodeSyncer mirrors a document into the personal knowledge graph. Phase 6's
// internal/graph implements it; nil means no graph is wired and this module
// behaves exactly as it did in Phase 3.
//
// The interface is declared here rather than imported, the same way
// internal/chat declares the searchers it consumes: this package depends on
// "something that records a document in the graph", not on the graph package.
//
// SyncNode returns no error on purpose. It runs after the document row is
// written, so there is nothing useful to do with a failure. The graph logs its
// own failures; see graph.Service.SyncNode.
//
// There is deliberately no delete counterpart: removing the node when the
// document goes is done by a trigger in migration 000006, so it fires on every
// path a row can leave by, not only the ones this service is on.
type NodeSyncer interface {
	SyncNode(ctx context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, label string)
}

// graphRefTable is what a document's node records in `ref_table`. It matches
// the allow-list in migration 000006 and graph's own map; the round trip is
// pinned in internal/db's Phase 6 tests.
const graphRefTable = "documents"

// Option configures a Service at construction. It is variadic rather than a
// parameter because the graph is genuinely optional: every existing caller,
// including this module's own tests, builds a service without one.
type Option func(*Service)

// WithNodeSync wires the knowledge graph into the write path.
func WithNodeSync(g NodeSyncer) Option { return func(s *Service) { s.graph = g } }

// Service holds the document use cases. It is transport agnostic: Search in
// particular is the retrieval entry point the AI chat phase will call directly,
// with no HTTP in the way.
type Service struct {
	store    Store
	embedder embeddings.Embedder
	log      *slog.Logger
	graph    NodeSyncer
	// processTimeout bounds the whole upload pipeline. Processing is
	// synchronous in this phase, so this is the number that decides how long a
	// client waits before an upload is abandoned.
	processTimeout time.Duration
}

func NewService(store Store, embedder embeddings.Embedder, log *slog.Logger, processTimeout time.Duration, opts ...Option) *Service {
	if processTimeout <= 0 {
		processTimeout = 2 * time.Minute
	}
	s := &Service{store: store, embedder: embedder, log: log, processTimeout: processTimeout}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Upload runs the whole pipeline: record the document, extract, chunk, embed,
// store, mark ready.
//
// It returns a Document rather than an error for a pipeline failure. The row
// exists either way -- the client can list it, read why it failed, and delete
// it -- and reporting that as a bare 5xx would leave them with a document they
// were never told the id of. Failures that happen *before* the row is created
// (an unsupported type, an oversized file) are ordinary validation errors and
// do come back as errors.
func (s *Service) Upload(ctx context.Context, userID uuid.UUID, in UploadInput) (Document, error) {
	doc, err := s.store.Create(ctx, userID, in.Filename, in.FileType)
	if err != nil {
		return Document{}, err
	}

	procCtx, cancel := context.WithTimeout(ctx, s.processTimeout)
	defer cancel()

	if err := s.process(procCtx, userID, doc.ID, in); err != nil {
		// The document must not be left in `processing`, and the reason it
		// failed is often that procCtx is done -- so the update runs on a
		// context detached from the deadline that just expired.
		failCtx, failCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer failCancel()

		s.log.Warn("document processing failed",
			"document_id", doc.ID, "user_id", userID, "filename", in.Filename, "error", err)

		failed, ferr := s.store.Fail(failCtx, userID, doc.ID, clientMessage(err))
		if ferr != nil {
			// Nothing left to do but say so: the row is stuck in `processing`
			// and the caller needs a real error, not a misleading document.
			return Document{}, fmt.Errorf("document %s stuck in processing: %w", doc.ID, ferr)
		}
		// On the detached context, for the same reason Fail is: the usual way
		// to arrive here is that the deadline expired, and syncing on a context
		// that is already done would lose the node every time the path that
		// most needs it is taken.
		s.sync(failCtx, userID, failed)
		return failed, nil
	}

	ready, err := s.store.ByID(ctx, userID, doc.ID)
	if err != nil {
		return Document{}, err
	}
	s.sync(ctx, userID, ready)
	return ready, nil
}

// sync mirrors a document into the graph.
//
// It runs for a failed document as well as a ready one, and that is the
// decision: the node stands for the document, not for its text. A failed
// document is a row the user can list, read the error from and delete, and it
// is a filename a later conversation may well name -- and there is no update
// path to mirror it later, because a document cannot be renamed. Leaving it
// out would mean the graph disagreed with the documents list about what the
// user has.
func (s *Service) sync(ctx context.Context, userID uuid.UUID, d Document) {
	if s.graph == nil {
		return
	}
	s.graph.SyncNode(ctx, userID, graphRefTable, d.ID, d.Filename)
}

// process is the extract → chunk → embed → store half of Upload. Every error
// it returns is one the caller records on the document.
func (s *Service) process(ctx context.Context, userID, docID uuid.UUID, in UploadInput) error {
	text, err := Extract(in.FileType, in.Content)
	if err != nil {
		return err
	}

	texts := SplitIntoChunks(text)
	if len(texts) == 0 {
		return ErrEmptyText
	}
	if len(texts) > MaxChunks {
		return fmt.Errorf("%w: the document is too large to process in one request (%d chunks, limit %d)",
			ErrExtraction, len(texts), MaxChunks)
	}

	vectors, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEmbedding, err)
	}
	if len(vectors) != len(texts) {
		return fmt.Errorf("%w: got %d vectors for %d chunks", ErrEmbedding, len(vectors), len(texts))
	}

	chunks := make([]Chunk, len(texts))
	for i := range texts {
		chunks[i] = Chunk{Index: i, Content: texts[i], Embedding: vectors[i]}
	}
	if _, err := s.store.Complete(ctx, userID, docID, text, chunks); err != nil {
		return err
	}
	return nil
}

// Search embeds the query and returns the nearest owner-scoped chunks.
//
// This is the retrieval function Phase 6 calls. It takes the user id as an
// argument rather than reading it from a request context, so it is usable from
// an agent loop, a background job or a test with no HTTP anywhere.
func (s *Service) Search(ctx context.Context, userID uuid.UUID, q SearchQuery) ([]SearchResult, error) {
	q, err := ValidateSearch(q)
	if err != nil {
		return nil, err
	}

	vectors, err := s.embedder.Embed(ctx, []string{q.Query})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEmbedding, err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("%w: got %d vectors for the query", ErrEmbedding, len(vectors))
	}

	return s.store.Search(ctx, userID, vectors[0], q)
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Document, error) {
	return s.store.ByID(ctx, userID, id)
}

// Passages returns a document's chunks in document order, owner-scoped.
//
// It is Search's counterpart for a caller that has no query: Phase 10a
// generates flashcards from a document the user named, and "the start of this
// document" is what grounds them when they named no topic within it. Like
// Search it is a plain service method taking a user id, so it is usable from a
// tool, a job or a test with no HTTP anywhere.
//
// It embeds nothing and calls no model, so it costs one indexed scan.
func (s *Service) Passages(ctx context.Context, userID, documentID uuid.UUID, limit int) ([]Passage, error) {
	return s.store.Passages(ctx, userID, documentID, limit)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Document, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.List(ctx, userID, f)
}

func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.Delete(ctx, userID, id)
}

// clientMessage turns a pipeline error into the sentence stored in
// error_message and shown to the user.
//
// It is a fixed set of strings rather than err.Error(): that column is read
// back over the API, and a wrapped driver error would put connection strings
// and internal paths in an HTTP response. The full error goes to the log.
func clientMessage(err error) string {
	switch {
	case errors.Is(err, ErrEmptyText):
		return "No text could be extracted. Scanned documents need OCR, which this version does not do."
	case errors.Is(err, context.DeadlineExceeded):
		return "Processing took too long and was stopped. Try a smaller document."
	case errors.Is(err, context.Canceled):
		return "Processing was cancelled before it finished."
	case errors.Is(err, embeddings.ErrUnavailable):
		// Not "saved but not indexed": Complete never ran, so the extracted
		// text was not stored either. The row is an empty placeholder.
		return "The embedding service is unavailable, so the document could not be indexed. Upload it again once the service is back."
	case errors.Is(err, ErrEmbedding):
		return "The document could not be indexed for search."
	case errors.Is(err, ErrExtraction):
		// Extraction messages are authored in this package and name only what
		// the file was, so the sentence after the classification prefix is
		// safe to pass through.
		return sentence(strings.TrimPrefix(err.Error(), ErrExtraction.Error()+": "))
	default:
		return "Processing failed."
	}
}

// sentence upper-cases the first letter and adds a full stop, so a stored
// error_message reads the same way as the fixed strings above.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Processing failed."
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	s = string(r)
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
