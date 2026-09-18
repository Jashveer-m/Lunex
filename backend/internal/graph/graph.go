// Package graph owns the personal knowledge graph: the nodes that stand for
// the things a user has, the edges between them, the relationship extraction
// that grows the graph out of conversations, and the 1-hop lookup the chat
// orchestrator uses to put a connection in front of the model.
//
// The shape follows internal/memories: an extraction pipeline gated by a
// length threshold, an LLM call, a lenient parser, a quality filter and a
// store; a Search-like method the orchestrator calls as a plain function with
// the owner id as an argument; and an HTTP handler for managing what was
// learned. Nodes for tasks, goals, notes and documents are not extracted at
// all -- they are mirrored from those tables when a row is written.
//
// What it deliberately is not: there is no graph database, no traversal deeper
// than one hop, and no graph algorithm. Two relational tables, an index on
// each endpoint, and a query that answers "what is this connected to". Nothing
// here computes a path, a centrality or a community; see docs/decisions.md for
// why that is the whole of the phase.
package graph

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound covers both "no such node or edge" and "that one is somebody
// else's", which is what keeps the API from confirming foreign ids.
var ErrNotFound = errors.New("not found")

// ErrNodeIsBacked is returned when a caller tries to delete a node that
// mirrors a task, goal, note or document. Those nodes follow their source
// row's lifecycle: the way to remove one is to delete the thing it stands for,
// which takes the node with it through the foreign key.
var ErrNodeIsBacked = errors.New("node is backed by a record")

// ErrExtraction classifies a failure to get usable relationships out of the
// model: the call failed, or the reply could not be parsed in any of the
// accepted shapes. It never reaches the user -- the chat turn swallows it --
// but it is what the log distinguishes from a store failure.
var ErrExtraction = errors.New("relationship extraction failed")

// The node types this phase scopes to, mirrored by the CHECK constraint in
// migration 000006 -- widened by 000008, which added `event`.
//
// The first five mirror a row in another table and are created by sync, never
// by extraction. The last three exist only because a conversation named them:
// there is no skills table, no people table and no projects table, and a
// `project` node is not the same thing as a `project`-typed goal.
const (
	NodeTask     = "task"
	NodeGoal     = "goal"
	NodeNote     = "note"
	NodeDocument = "document"
	// NodeEvent mirrors a row of calendar_events (Phase 8).
	NodeEvent = "event"
	// NodeExpense mirrors a row of expenses (Phase 9). Expense *categories* are
	// not mirrored: a category is a label on other rows rather than a thing
	// that happened, and a node per category would match the mention scan on
	// the word "food".
	NodeExpense = "expense"
	NodeSkill   = "skill"
	NodePerson  = "person"
	NodeProject = "project"
)

// NodeTypes is the allow-list, used by validation, by the extraction parser
// and by the ?type= filter on the graph read.
var NodeTypes = []string{NodeTask, NodeGoal, NodeNote, NodeDocument, NodeEvent, NodeExpense,
	NodeSkill, NodePerson, NodeProject}

// ExtractedTypes are the node types a conversation can create, and therefore
// the only ones a user is allowed to delete directly. The rest are mirrors.
var ExtractedTypes = []string{NodeSkill, NodePerson, NodeProject}

// The tables a node can mirror, and the node type each produces. This map is
// the only place the correspondence is written down, and its keys are the same
// six strings the ref_table CHECK constraint allows.
var refTableTypes = map[string]string{
	"tasks":           NodeTask,
	"goals":           NodeGoal,
	"notes":           NodeNote,
	"documents":       NodeDocument,
	"calendar_events": NodeEvent,
	"expenses":        NodeExpense,
}

// TypeForRefTable reports the node type a mirrored table produces, and whether
// the table is one this phase mirrors at all. A caller passing an unknown
// table is a bug, and it is answered by refusing rather than by inventing a
// type the CHECK constraint would reject anyway.
func TypeForRefTable(table string) (string, bool) {
	t, ok := refTableTypes[table]
	return t, ok
}

// The relationships this phase scopes to, mirrored by the CHECK constraint in
// migration 000006.
//
// SPENT_ON and VISITED from the spec are absent. VISITED points at trips, which
// do not exist. SPENT_ON now could have a real node on its far side -- Phase 9
// mirrors expenses -- but adding it means widening the relationship CHECK and
// teaching the extraction prompt when to use it, which is a change to how the
// model reads every turn rather than a new table. See docs/decisions.md.
const (
	// RelRelatedTo is the unspecific link, and the fallback for a relationship
	// the model named in words the allow-list does not have.
	RelRelatedTo = "RELATED_TO"
	// RelRequires is "this needs that": a project requires a skill.
	RelRequires = "REQUIRES"
	// RelDependsOn is "this is blocked by that", between two pieces of work.
	RelDependsOn = "DEPENDS_ON"
	// RelWorksOn points a person at a project.
	RelWorksOn = "WORKS_ON"
	// RelKnows points a person at another person, or at a skill they already
	// have.
	RelKnows = "KNOWS"
	// RelInterestedIn is weaker than KNOWS or STUDIES: an interest, not a
	// commitment.
	RelInterestedIn = "INTERESTED_IN"
	// RelStudies is "is learning", the active form of KNOWS.
	RelStudies = "STUDIES"
	// RelCompleted points at something finished.
	RelCompleted = "COMPLETED"
	// RelGoalOf points a goal at whoever holds it.
	RelGoalOf = "GOAL_OF"
)

// Relationships is the allow-list, used by validation and by the extraction
// parser.
var Relationships = []string{
	RelRelatedTo, RelRequires, RelDependsOn, RelWorksOn, RelKnows,
	RelInterestedIn, RelStudies, RelCompleted, RelGoalOf,
}

// SelfLabel is the label of the node standing for the user themselves, and
// SelfType is its type.
//
// The graph needs one, because most of what a conversation states is a
// relationship the user is one end of ("I am learning Go"). It is an ordinary
// extracted `person` node rather than a special row: it is created the first
// time an extraction refers to the user, it is listed and traversed like any
// other, and the only thing that makes it special is that SelfAliases all
// resolve to it -- which is what stops "I", "me" and "the user" becoming three
// people.
const (
	SelfLabel = "You"
	SelfType  = NodePerson
)

// SelfAliases are the labels a model reaches for when it means the user. They
// are matched after NormalizeLabel, so case and spacing do not matter.
var SelfAliases = map[string]struct{}{
	"you": {}, "user": {}, "the user": {}, "i": {}, "me": {},
	"myself": {}, "self": {}, "my": {},
}

// Node mirrors a row of the knowledge_nodes table.
type Node struct {
	ID     uuid.UUID
	UserID uuid.UUID
	Type   string
	Label  string
	// RefTable and RefID are the row this node mirrors, or both nil for a node
	// that exists only because a conversation named it. They are the
	// difference between a node the user may delete and one they may not.
	RefTable  *string
	RefID     *uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Extracted reports whether this node exists only because a conversation named
// it, which is exactly the set of nodes DELETE allows.
func (n Node) Extracted() bool { return n.RefTable == nil }

// Edge mirrors a row of the knowledge_edges table.
type Edge struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	FromNodeID   uuid.UUID
	ToNodeID     uuid.UUID
	Relationship string
	// Confidence is the model's estimate that it read the relationship rather
	// than inferred it, in [0, 1].
	Confidence float64
	// SourceConversationID is where the edge came from, or nil once that
	// conversation has been deleted.
	SourceConversationID *uuid.UUID
	CreatedAt            time.Time
}

// Candidate is one relationship the model proposed, before its ends have been
// resolved to nodes. It is the output of parsing an extraction reply.
type Candidate struct {
	From         string
	FromType     string
	To           string
	ToType       string
	Relationship string
	Confidence   float64
}

// EdgeInput is a candidate whose ends are now real nodes, ready to be written.
// Nothing outside this package builds one: edges are created by the extraction
// that runs at the end of a chat turn, and there is no POST.
type EdgeInput struct {
	FromNodeID           uuid.UUID
	ToNodeID             uuid.UUID
	Relationship         string
	Confidence           float64
	SourceConversationID *uuid.UUID
}

// Graph is the whole thing, as GET /knowledge-graph returns it.
//
// Edges is closed over Nodes: every edge in it has both of its endpoints in
// Nodes, so a filtered read is still a graph a client can draw rather than a
// node list with dangling references.
type Graph struct {
	Nodes []Node
	Edges []Edge
}

// Neighbor is one step out from a node: the edge, whatever is on the far end,
// and which way the edge points.
type Neighbor struct {
	Edge Edge
	Node Node
	// Incoming is true when the edge points *at* the node being asked about.
	// Direction is part of the meaning -- "the user STUDIES Go" and "Go
	// STUDIES the user" are not the same claim -- so it is carried rather than
	// flattened away.
	Incoming bool
}

// Neighborhood is a node and one hop out from it. It is what
// GET /knowledge-graph/nodes/{id} returns and what the chat orchestrator puts
// in front of the model. One hop is the whole of it: see the package comment.
type Neighborhood struct {
	Node      Node
	Neighbors []Neighbor
}

// Filter is the query behind GET /knowledge-graph.
type Filter struct {
	Type   string
	Limit  int
	Offset int
}

// Paging bounds for the graph read. A personal graph is small, so the default
// is generous enough to return all of it in one call -- but an unbounded list
// endpoint on a table that grows without limit is a denial-of-service lever,
// so the cap is not optional.
const (
	DefaultLimit = 500
	MaxLimit     = 2_000
)

// MaxLabelLen bounds a node's label. A label is a name, not a sentence; see
// MaxLabelWords in extract.go for the other half of that rule.
const MaxLabelLen = 200

// MaxNeighbors caps one node's 1-hop expansion, in the API read and in
// retrieval alike. A node with two hundred edges would otherwise be the whole
// of a chat prompt.
const MaxNeighbors = 25
