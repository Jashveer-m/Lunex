// Package documents owns uploaded files: their extracted text, the chunks that
// text is split into, and the vector search over those chunks.
//
// The retrieval half of this package (Service.Search) is the function the AI
// chat phase calls. It is deliberately a plain service method taking a user id
// and returning typed results, not something reachable only through HTTP.
package documents

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound covers both "no such document" and "that document is somebody
// else's", which is what keeps the API from confirming foreign ids.
var ErrNotFound = errors.New("document not found")

// ErrExtraction and ErrEmbedding classify a pipeline failure. They exist so
// the service can store a message that says what went wrong without leaking
// the underlying error text to the client.
var (
	ErrExtraction = errors.New("text extraction failed")
	ErrEmbedding  = errors.New("embedding failed")
	// ErrEmptyText is extraction succeeding but finding nothing to index --
	// a scanned PDF with no text layer is the usual cause, and OCR is a later
	// phase.
	ErrEmptyText = errors.New("no extractable text")
)

// Processing states. A document is created `processing`, and the upload
// request leaves it in exactly one of the other two before returning.
const (
	StatusProcessing = "processing"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

// Statuses is the allow-list for the ?status= list filter.
var Statuses = []string{StatusProcessing, StatusReady, StatusFailed}

// File types this phase can extract. DOCX, images (OCR) and CSV are
// deliberately absent; see docs/decisions.md.
const (
	TypePDF      = "pdf"
	TypeText     = "txt"
	TypeMarkdown = "md"
)

// Document mirrors a row of the documents table.
type Document struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	Filename      string
	FileType      string
	Status        string
	ErrorMessage  *string
	ExtractedText *string
	// ChunkCount is not a column: it is counted on read, so it can never
	// disagree with the rows in document_chunks.
	ChunkCount int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Chunk is one indexed slice of a document's text.
type Chunk struct {
	Index     int
	Content   string
	Embedding []float32
}

// SearchResult is one retrieved chunk plus the citation a caller needs to
// attribute it.
type SearchResult struct {
	ChunkID    uuid.UUID
	DocumentID uuid.UUID
	Filename   string
	ChunkIndex int
	Content    string
	// Similarity is cosine similarity in [-1, 1]; 1 is identical direction.
	// It is 1 - cosine distance, computed in SQL so it describes the same
	// ordering the index scan used.
	Similarity float64
}

// Passage is one stored chunk read back by position rather than by similarity.
//
// It is what a caller wants when the question is "what does this document
// say", with no query to rank against -- Phase 10a's flashcard generation is
// the first. It is deliberately not a SearchResult: nothing scored it, and a
// Similarity field carrying a zero that means "not measured" reads exactly
// like one meaning "no resemblance at all".
type Passage struct {
	ChunkID    uuid.UUID
	DocumentID uuid.UUID
	Filename   string
	ChunkIndex int
	Content    string
}

// MaxPassages bounds one read of a document's text. A document can be 800
// chunks (MaxChunks) and no caller of this wants all of them in a prompt; the
// caller names how many it needs and this is the ceiling on that.
const MaxPassages = 50

// UploadInput is a validated file ready to be processed.
type UploadInput struct {
	Filename string
	FileType string
	Content  []byte
}

// SearchQuery is the retrieval request. Zero values are filled in by
// ValidateSearch, so a caller that only sets Query gets the defaults.
type SearchQuery struct {
	Query string
	Limit int
	// MinSimilarity drops weak matches. Vector search always returns the
	// nearest `Limit` chunks, however far away they are, so without a floor an
	// unrelated question still comes back with confident-looking citations.
	MinSimilarity float64
	// DocumentIDs, when set, restricts the search to those documents. Phase 6
	// needs it for "ask about this document"; the HTTP endpoint exposes it too.
	DocumentIDs []uuid.UUID
}

// Filter is the query behind GET /documents.
type Filter struct {
	Status string
	Sort   string
	Limit  int
	Offset int
}

// Sorts is the fixed allow-list of ORDER BY fragments; no request text ever
// reaches the SQL.
var Sorts = map[string]string{
	"created_at":  "created_at ASC",
	"-created_at": "created_at DESC",
	"updated_at":  "updated_at ASC",
	"-updated_at": "updated_at DESC",
	"filename":    "lower(filename) ASC",
	"-filename":   "lower(filename) DESC",
}

const DefaultSort = "-created_at"

const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Search paging.
const (
	DefaultSearchLimit = 5
	MaxSearchLimit     = 50
	MaxQueryLen        = 4_000
)

// MaxFilenameLen bounds the stored name. The value is never used as a path.
const MaxFilenameLen = 255
