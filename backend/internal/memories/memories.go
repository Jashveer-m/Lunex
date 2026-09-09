// Package memories owns the assistant's long-term memory: the durable facts it
// extracts from conversations, the vector search that brings them back, and
// the endpoints a user manages them through.
//
// The shape follows internal/documents: a fact is a short text with an
// embedding, retrieval is a cosine search scoped to the owner, and the service
// method the chat orchestrator calls (Search) takes a user id and returns typed
// results rather than being reachable only over HTTP.
//
// What it deliberately is not: there is no knowledge graph, no entity
// resolution and no background job queue. Extraction runs inline in the chat
// turn that produced the material, and a memory is a sentence with a type and
// two scores -- nothing links one to another.
package memories

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// ErrNotFound covers both "no such memory" and "that memory is somebody
// else's", which is what keeps the API from confirming foreign ids.
var ErrNotFound = errors.New("memory not found")

// ErrEmbedding classifies a failure to turn text into a vector. It exists so
// the chat orchestrator can tell an embedding outage (a 503 the client should
// retry) from a programming error, the same way documents.ErrEmbedding does.
var ErrEmbedding = errors.New("embedding failed")

// ErrExtraction classifies a failure to get usable facts out of the model:
// the call failed, or the reply could not be parsed in any of the accepted
// shapes. It never reaches the user -- the chat turn swallows it -- but it is
// what the log distinguishes from a store failure.
var ErrExtraction = errors.New("memory extraction failed")

// The memory types from the spec.
//
// They are a closed set, mirrored by the CHECK constraint in migration 000005,
// because they are what a client filters on and what the extraction prompt is
// allowed to return. A sixth kind of memory is a schema change, not a string.
const (
	// TypeEpisodic is something that happened: "the user finished the OS
	// assignment on 3 March".
	TypeEpisodic = "episodic"
	// TypeSemantic is a standing fact about the world or the user: "the user
	// knows Go".
	TypeSemantic = "semantic"
	// TypePreference is how the user likes to work: "the user prefers studying
	// in the morning".
	TypePreference = "preference"
	// TypeProject ties a fact to a piece of ongoing work.
	TypeProject = "project"
	// TypeGoal ties a fact to something the user is trying to achieve.
	TypeGoal = "goal"
)

// Types is the allow-list, used by validation, by the extraction parser and by
// the ?type= list filter.
var Types = []string{TypeEpisodic, TypeSemantic, TypePreference, TypeProject, TypeGoal}

// Memory mirrors a row of the memories table. The embedding is not a field:
// nothing in the API returns a vector and the similarity is computed in SQL,
// exactly as with document chunks.
type Memory struct {
	ID      uuid.UUID
	UserID  uuid.UUID
	Type    string
	Content string
	// Importance is how much the model thought this was worth keeping, and
	// Confidence how sure it was that the extraction is accurate. Both in
	// [0, 1]. They are the model's own judgement, recorded rather than trusted:
	// the retrieval floor is a similarity, not an importance.
	Importance float64
	Confidence float64
	// SourceConversationID is where the fact came from, or nil once that
	// conversation has been deleted.
	SourceConversationID *uuid.UUID
	// Enabled is the user's switch. A disabled memory is never retrieved and
	// never reaches a prompt; it is still listed, and can be switched back on.
	Enabled bool
	// ExpiresAt bounds a fact that should not persist forever. Nothing sets it
	// in this phase; retrieval already skips a memory past it.
	ExpiresAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Candidate is one fact the model proposed, before it has been embedded or
// stored. It is the output of parsing an extraction reply and the input to the
// store.
type Candidate struct {
	Type       string
	Content    string
	Importance float64
	Confidence float64
}

// CreateInput is a candidate that has been embedded and is ready to be written.
// Nothing outside this package builds one: memories are created by the
// extraction that runs at the end of a chat turn, and there is no POST.
type CreateInput struct {
	Candidate
	SourceConversationID *uuid.UUID
	Embedding            []float32
}

// UpdateInput is a PATCH body as it arrives. Both fields are three-state so an
// omitted key leaves the column alone and an explicit null is rejected rather
// than read as "clear it": neither column is nullable.
type UpdateInput struct {
	Content optional.Field[string]
	Enabled optional.Field[bool]
}

// Patch is the validated, column-level form of a PATCH body. Only the two
// things the user can change are here: the text of the fact, and whether it is
// live. Type and the model's scores are a record of what the extraction said
// and are not editable -- rewriting them would make the scores mean nothing.
type Patch struct {
	Content *string
	Enabled *bool
}

func (p Patch) Empty() bool { return p.Content == nil && p.Enabled == nil }

// Filter is the query behind GET /memories.
type Filter struct {
	Type string
	// Enabled is three-state: nil lists both, so the default view is every
	// memory the user has rather than only the live ones.
	Enabled *bool
	Sort    string
	Limit   int
	Offset  int
}

// Sorts is the fixed allow-list of ORDER BY fragments; no request text ever
// reaches the SQL.
var Sorts = map[string]string{
	"created_at":  "created_at ASC",
	"-created_at": "created_at DESC",
	"updated_at":  "updated_at ASC",
	"-updated_at": "updated_at DESC",
	"importance":  "importance ASC",
	"-importance": "importance DESC",
	"confidence":  "confidence ASC",
	"-confidence": "confidence DESC",
}

// DefaultSort is newest first: a memory list is a record of what the assistant
// has learned, and the most recent additions are what a user checks.
const DefaultSort = "-created_at"

// Paging bounds for the list.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// SearchQuery is the retrieval request the chat orchestrator makes.
type SearchQuery struct {
	Query string
	Limit int
	// MinSimilarity drops weak matches. As with documents, vector search always
	// returns the nearest `Limit` rows however far away they are, so without a
	// floor every question is handed the user's whole memory in a plausible
	// order. See DefaultMinSimilarity.
	MinSimilarity float64
}

// SearchResult is one retrieved memory plus the score that retrieved it.
type SearchResult struct {
	Memory
	// Similarity is cosine similarity in [-1, 1]; 1 is identical direction. It
	// is 1 - cosine distance, computed in SQL so it describes the same ordering
	// the index scan used.
	Similarity float64
}

// DefaultMinSimilarity is the floor under a retrieved memory's cosine score.
//
// It is higher than the document floor (chat.DefaultMinSimilarity, 0.5) on
// purpose, and the difference is the point of tuning it separately:
//
//   - A memory is one sentence, not a ~500-token passage. Two short texts have
//     little content to disagree about, so a weak topical relation scores
//     higher between them than between a question and a chunk -- the same
//     number does not mean the same closeness.
//   - A memory reaches the prompt as a stated fact about the user, and a
//     wrongly retrieved one is a false claim about them rather than an
//     irrelevant quotation they can dismiss.
//   - Memories are searched on *every* turn regardless of topic. The floor is
//     the only thing keeping an old preference out of an unrelated
//     conversation; there is no other filter to fall back on.
//
// 0.6 is where nomic-embed-text puts a question and a memory that are actually
// about the same thing (~0.65-0.85), above where it puts a memory that merely
// shares a register ("the user prefers mornings" against "what is on my
// calendar?", ~0.45-0.58). Tunable with MEMORY_MIN_SIMILARITY.
const DefaultMinSimilarity = 0.6

// Retrieval paging. The top-k is small for the same reason the document one is:
// a 3B model spends its attention on whatever is nearest the question, and five
// half-relevant facts about the user make an answer worse, not better.
const (
	DefaultSearchLimit = 5
	MaxSearchLimit     = 25
	MaxQueryLen        = 4_000
)

// MaxContentLen bounds one fact. A memory is a sentence by construction -- the
// extraction prompt says so -- and this is the ceiling that keeps a model that
// ignored the instruction, or a user editing one by hand, from turning the
// table into blob storage.
const MaxContentLen = 1_000
