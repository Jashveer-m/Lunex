// Package chat owns the AI assistant: conversations, their messages, and the
// orchestrator that grounds an answer in the user's own data.
//
// The orchestrator is the core of the package. For each user message it
// retrieves document chunks through internal/documents' service-level search,
// picks up the user's current tasks, goals and notes, assembles a prompt, and
// streams the model's reply back while recording exactly what was retrieved
// and which of it the answer actually cited.
//
// What it deliberately is not: there is no memory extraction, no agents, no
// knowledge graph and no action engine. The assistant reads and answers. It
// cannot create, modify or delete anything, and the system prompt says so, so
// a user who asks for an action is told rather than quietly ignored.
package chat

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
)

// ErrNotFound covers both "no such conversation" and "that conversation is
// somebody else's", which is what keeps the API from confirming foreign ids.
var ErrNotFound = errors.New("conversation not found")

// The roles a stored message can carry, mirrored from the provider package so
// the two cannot drift apart.
const (
	RoleSystem    = ai.RoleSystem
	RoleUser      = ai.RoleUser
	RoleAssistant = ai.RoleAssistant
)

// DefaultTitle is what a conversation is called until its first message
// supplies a better one. It matches the column default in migration 000004,
// and the service compares against it to decide whether a title is still
// untouched.
const DefaultTitle = "New conversation"

// Conversation mirrors a row of the conversations table.
type Conversation struct {
	ID     uuid.UUID
	UserID uuid.UUID
	Title  string
	// MessageCount is not a column: it is counted on read, so it can never
	// disagree with the rows in messages.
	MessageCount int
	// Messages is populated by the single-conversation read only; a list
	// leaves it nil.
	Messages  []Message
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Message mirrors a row of the messages table.
type Message struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	Role           string
	Content        string
	// Sources is what grounded this message: nil on a user message and on an
	// assistant message that had nothing to work from, which is stored as SQL
	// NULL rather than an empty array so "retrieved nothing" and "never ran
	// retrieval" stay distinguishable.
	Sources   []Source
	CreatedAt time.Time
}

// NewMessage is a message about to be persisted.
type NewMessage struct {
	Role    string
	Content string
	Sources []Source
}

// The kinds of thing that can ground an answer.
const (
	SourceDocument = "document"
	SourceTask     = "task"
	SourceGoal     = "goal"
	SourceNote     = "note"
)

// Source is one retrieved item, recorded on the assistant message that was
// generated from it.
//
// The struct is the jsonb payload: the tags below are the stored shape and the
// wire shape at once, so what the API returns is what the column holds.
type Source struct {
	Type string `json:"type"`
	// ID is the document, task, goal or note id -- something the client can
	// follow to the underlying record.
	ID uuid.UUID `json:"id"`
	// Label is the marker the prompt showed the model ("S1", "S2", ...). It is
	// stored because Cited is derived from finding it in the answer, and
	// because a client rendering "[S1]" needs to know what S1 was.
	Label string `json:"label"`
	// Title is the filename, or the task/goal/note title.
	Title string `json:"title"`
	// ChunkIndex and Similarity are set for documents only.
	ChunkIndex *int     `json:"chunk_index,omitempty"`
	Similarity *float64 `json:"similarity,omitempty"`
	// Excerpt is the text the model was actually shown, truncated the same way
	// the prompt truncated it. Storing it is what makes a citation checkable
	// after the fact, even if the underlying note is edited later.
	Excerpt string `json:"excerpt,omitempty"`
	// Cited reports whether the answer referenced this source's label. It is
	// the difference between "offered to the model" and "used", and it is
	// measured from the generated text rather than assumed.
	Cited bool `json:"cited"`
}

// Filter is the query behind GET /conversations.
type Filter struct {
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
	"title":       "lower(title) ASC",
	"-title":      "lower(title) DESC",
}

// DefaultSort is most-recently-active first: a conversation list is a list of
// things to continue, and updated_at moves every time a turn is added.
const DefaultSort = "-updated_at"

// Paging bounds for the conversation list.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// MaxMessageLen bounds one user message. It is well under the model's context
// window, so a long question leaves room for the retrieved context rather than
// crowding it out.
const MaxMessageLen = 8_000

// MaxMessagesPerRead caps the single-conversation read. A conversation with
// more turns than this returns its most recent ones.
const MaxMessagesPerRead = 500
