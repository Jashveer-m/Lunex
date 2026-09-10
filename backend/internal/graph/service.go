package graph

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
)

// Store is the slice of the repository the service needs. Every method takes
// the owner id, so a node or an edge cannot be reached without saying whose it
// is -- which is what makes the isolation structural rather than a rule each
// call site has to remember.
type Store interface {
	EnsureRefNode(ctx context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, nodeType, label string) (Node, error)
	EnsureExtractedNode(ctx context.Context, userID uuid.UUID, nodeType, label string) (Node, error)
	NodesByLabel(ctx context.Context, userID uuid.UUID, label string) ([]Node, error)
	NodeByID(ctx context.Context, userID, id uuid.UUID) (Node, error)
	Nodes(ctx context.Context, userID uuid.UUID, f Filter) ([]Node, error)
	MentionCandidates(ctx context.Context, userID uuid.UUID, text string, limit int) ([]Node, error)
	DeleteNode(ctx context.Context, userID, id uuid.UUID) error

	CreateEdge(ctx context.Context, userID uuid.UUID, in EdgeInput) (Edge, error)
	EdgesAmong(ctx context.Context, userID uuid.UUID, nodeIDs []uuid.UUID, limit int) ([]Edge, error)
	Neighbors(ctx context.Context, userID, nodeID uuid.UUID, limit int) ([]Neighbor, error)
	DeleteEdge(ctx context.Context, userID, id uuid.UUID) error
}

// Options are the extraction knobs, resolved from configuration at boot. They
// mirror memories.Options field for field, because the call they configure is
// the same shape: a small classifier prompt, parsed defensively, bounded by a
// timeout that is the latency the feature adds to the end of a chat turn.
type Options struct {
	// Model overrides the provider's own model for extraction.
	Model string
	// Temperature is near-zero by default: extraction is a reading task, and a
	// creatively rephrased relationship is a fabricated one.
	Temperature float64
	// MaxTokens caps the extraction reply.
	MaxTokens int
	// Timeout bounds the whole extraction.
	Timeout time.Duration
	// JSONMode asks the provider to constrain decoding to well-formed JSON.
	// Off by default for the reason memories.Options.JSONMode records: on
	// llama3.2:3b it makes extraction strictly worse.
	JSONMode bool
}

// Extraction defaults, applied to a zero Options.
const (
	DefaultExtractionTemperature = 0.1
	DefaultExtractionMaxTokens   = 512
	DefaultExtractionTimeout     = 60 * time.Second
)

// Deps are everything the service needs. Provider may be nil: a graph with no
// model still syncs nodes, answers the API and serves the 1-hop lookup -- it
// just never grows an edge on its own.
type Deps struct {
	Store    Store
	Provider ai.Provider
	Logger   *slog.Logger
	Options  Options
}

// Service holds the knowledge-graph use cases. It is transport agnostic:
// SyncNode, ExtractFromTurn and Mentioned are called by other packages as
// plain functions, with no HTTP in the way.
type Service struct {
	store    Store
	provider ai.Provider
	log      *slog.Logger
	opts     Options
}

func NewService(d Deps) *Service {
	opts := d.Options
	if opts.Temperature == 0 {
		opts.Temperature = DefaultExtractionTemperature
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = DefaultExtractionMaxTokens
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultExtractionTimeout
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: d.Store, provider: d.Provider, log: log, opts: opts}
}

// --- node sync --------------------------------------------------------------

// SyncNode mirrors one row of tasks, goals, notes or documents into the graph,
// creating the node if it is missing and refreshing its label if it is not.
//
// It is called inline from each resource service, on create and on update.
// Both halves of that are decisions; docs/decisions.md has the argument and
// the alternative that was rejected (computing nodes lazily on the first graph
// read). Two things about the signature carry the rest of it:
//
// It reports no error. By the time this runs the task is written and the user
// has been told so, and there is nothing useful for the caller to do with a
// failure -- refusing the request would be a lie about a row that exists, and
// rolling it back would throw away the thing the user asked for to protect an
// index derived from it. So the failure is logged here, where there is a
// logger and the context to describe it, and the resource module is left with
// a one-line dependency it cannot misuse.
//
// It is idempotent, which is what makes calling it on every update sensible:
// the update path keeps the label honest, and it doubles as the repair for a
// create whose sync failed. There is no backfill for rows written before this
// phase; see docs/decisions.md.
func (s *Service) SyncNode(ctx context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, label string) {
	nodeType, ok := TypeForRefTable(refTable)
	if !ok {
		// A caller naming a table the graph does not mirror is a programming
		// error, and it is answered by refusing rather than by writing a row
		// the CHECK constraint would reject anyway.
		s.log.Error("graph sync refused: unknown source table",
			"ref_table", refTable, "ref_id", refID, "user_id", userID)
		return
	}
	label = NormalizeLabel(label)
	if label == "" {
		// Every mirrored table requires a non-empty title, so this is
		// unreachable through the API; it is here because a node with no label
		// is invisible to the mention scan and unrenderable by a client.
		s.log.Error("graph sync refused: empty label",
			"ref_table", refTable, "ref_id", refID, "user_id", userID)
		return
	}
	if _, err := s.store.EnsureRefNode(ctx, userID, refTable, refID, nodeType, label); err != nil {
		s.log.Warn("graph sync failed",
			"error", err, "user_id", userID, "ref_table", refTable, "ref_id", refID)
	}
}

// --- the extraction pipeline ------------------------------------------------

// ExtractFromTurn reads one completed exchange and stores the relationships in
// it. It returns the edges it wrote, which is nil far more often than not --
// most turns state no connection, and that is the correct outcome.
//
// It is called by the chat orchestrator after the answer has been streamed and
// the turn persisted, alongside the memory extraction, and its error is logged
// rather than returned to the user: a failure to record a relationship must
// never turn a good answer into a 500. See chat.Service.SendMessage.
func (s *Service) ExtractFromTurn(ctx context.Context, userID, conversationID uuid.UUID, userMessage, assistantMessage string) ([]Edge, error) {
	if s.provider == nil {
		return nil, nil
	}
	if !WorthExtracting(userMessage, assistantMessage) {
		s.log.Debug("relationship extraction skipped: turn is too short",
			"user_id", userID, "conversation_id", conversationID)
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	candidates, err := s.propose(ctx, userMessage, assistantMessage)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	source := conversationID
	stored := make([]Edge, 0, len(candidates))
	for _, c := range candidates {
		from, err := s.resolve(ctx, userID, c.From, c.FromType)
		if err != nil {
			return stored, err
		}
		to, err := s.resolve(ctx, userID, c.To, c.ToType)
		if err != nil {
			return stored, err
		}
		if from.ID == to.ID {
			// Two different names that resolved to one node -- "I" and "the
			// user", most often. The CHECK constraint would reject the edge;
			// dropping it here says why in the log.
			s.log.Debug("relationship dropped: both ends resolved to one node",
				"user_id", userID, "label", from.Label)
			continue
		}
		e, err := s.store.CreateEdge(ctx, userID, EdgeInput{
			FromNodeID: from.ID, ToNodeID: to.ID, Relationship: c.Relationship,
			Confidence: c.Confidence, SourceConversationID: &source,
		})
		if err != nil {
			// Whatever was written before this point stays written: the
			// relationships are independent, and rolling back a stored one
			// because the next insert failed would lose it for no gain.
			return stored, err
		}
		stored = append(stored, e)
	}

	if len(stored) > 0 {
		s.log.Info("relationships extracted",
			"user_id", userID, "conversation_id", conversationID,
			"proposed", len(candidates), "stored", len(stored), "edges", summarize(candidates))
	}
	return stored, nil
}

// propose runs the model and turns its reply into candidate relationships that
// have passed the quality gate.
func (s *Service) propose(ctx context.Context, userMessage, assistantMessage string) ([]Candidate, error) {
	format := ""
	if s.opts.JSONMode {
		format = ai.FormatJSON
	}
	stream, err := s.provider.Chat(ctx, ExtractionPrompt(userMessage, assistantMessage), ai.Options{
		Model:       s.opts.Model,
		Temperature: s.opts.Temperature,
		MaxTokens:   s.opts.MaxTokens,
		Format:      format,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrExtraction, err)
	}
	defer stream.Close() //nolint:errcheck // releases the upstream connection

	reply, err := ai.Collect(stream)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrExtraction, err)
	}

	candidates := ParseExtraction(reply)
	kept := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if reason, ok := ValidateCandidate(c, userMessage); !ok {
			s.log.Debug("relationship dropped: "+reason,
				"from", c.From, "relationship", c.Relationship, "to", c.To,
				"confidence", c.Confidence)
			continue
		}
		kept = append(kept, c)
	}
	return kept, nil
}

// resolve turns an extracted name into a node, matching it against what the
// user already has before creating anything.
//
// The match is exact on the folded label -- see label.go for why it is not
// fuzzy -- and it looks at *every* node the user has, not only the extracted
// ones. That is the point of the lookup: a conversation that mentions "the
// backend project" should attach its edge to the goal the user already has by
// that name, so asking about it later reaches the real record rather than a
// second, parallel idea of the same thing.
//
// When several nodes share a label, a type match wins and the oldest node
// wins after that. A name the user already has a record for is much more
// likely to be that record than a new entity that happens to be spelled the
// same, so the tie is broken towards what already exists rather than towards
// what the model just said.
func (s *Service) resolve(ctx context.Context, userID uuid.UUID, label, nodeType string) (Node, error) {
	label = NormalizeLabel(label)
	if IsSelf(label) {
		label, nodeType = SelfLabel, SelfType
	}

	existing, err := s.store.NodesByLabel(ctx, userID, label)
	if err != nil {
		return Node{}, fmt.Errorf("resolve %q: %w", label, err)
	}
	if len(existing) > 0 {
		for _, n := range existing {
			if n.Type == nodeType {
				return n, nil
			}
		}
		return existing[0], nil
	}

	n, err := s.store.EnsureExtractedNode(ctx, userID, nodeType, label)
	if err != nil {
		return Node{}, fmt.Errorf("resolve %q: %w", label, err)
	}
	return n, nil
}

// --- retrieval --------------------------------------------------------------

// MaxMentionCandidates bounds the prefilter behind Mentioned. It is well above
// MaxMentioned so the word-boundary test still has a choice to make after the
// substring scan has over-collected.
const MaxMentionCandidates = 50

// Mentioned returns the neighbourhoods of the nodes a piece of text names.
//
// This is the whole of the chat integration, and it is a lookup rather than a
// traversal: find the nodes whose label appears in the message, fetch one hop
// out from each, stop. No path finding, no ranking by graph structure, no
// second hop. See the package comment and docs/decisions.md.
//
// It takes the user id as an argument rather than reading it from a request
// context, so it is usable from an agent loop, a job or a test with no HTTP
// anywhere -- the same shape as documents.Search and memories.Search.
func (s *Service) Mentioned(ctx context.Context, userID uuid.UUID, text string, limit int) ([]Neighborhood, error) {
	if limit <= 0 {
		return nil, nil
	}
	candidates, err := s.store.MentionCandidates(ctx, userID, text, MaxMentionCandidates)
	if err != nil {
		return nil, fmt.Errorf("find mentioned nodes: %w", err)
	}

	out := make([]Neighborhood, 0, limit)
	for _, n := range candidates {
		// The self node never matches a mention, and that is a correctness
		// rule rather than a tuning choice.
		//
		// Its label is "You", and in a message *written by the user* the word
		// "you" means the assistant -- "can you check my deadlines" is not the
		// user naming themselves. So the one node the scan would fire on most
		// often is the one it would be wrong about every time, and it would
		// fire on almost every turn, which is not what "the question named
		// something" is supposed to mean.
		//
		// Matching it on first-person pronouns instead was the alternative and
		// is no better: "I" and "my" are in most messages too, so the graph
		// would become a source on every turn rather than on the turns that
		// name a thing. What the user is like is what the memory system
		// retrieves; what a named thing connects to is what this does.
		if IsSelf(n.Label) {
			continue
		}
		if !Mentions(text, n.Label) {
			continue
		}
		neighbors, err := s.store.Neighbors(ctx, userID, n.ID, MaxNeighbors)
		if err != nil {
			return nil, fmt.Errorf("expand %q: %w", n.Label, err)
		}
		if len(neighbors) == 0 {
			// A node with no edges says nothing the rest of retrieval does not
			// already say: the task it mirrors is already reachable through
			// the task list, and an isolated extracted node is a name with no
			// claim attached. Spending a source on it would displace one that
			// carries something.
			continue
		}
		out = append(out, Neighborhood{Node: n, Neighbors: neighbors})
		if len(out) == limit {
			break
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// --- reads and management ---------------------------------------------------

// Graph returns the user's whole graph, or the part of it with one node type.
//
// The edges returned are those with *both* endpoints among the returned nodes,
// so a filtered or paged read is a subgraph a client can draw rather than a
// node list with edges pointing at things it was not given.
func (s *Service) Graph(ctx context.Context, userID uuid.UUID, f Filter) (Graph, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return Graph{}, err
	}
	nodes, err := s.store.Nodes(ctx, userID, f)
	if err != nil {
		return Graph{}, err
	}
	ids := make([]uuid.UUID, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	// Two endpoints per node at most in a simple graph is not a bound that
	// holds, so the edge cap is its own number rather than derived: MaxLimit
	// nodes can carry many more than MaxLimit edges, and a client asking for
	// the whole graph should get all of the edges among what it was given.
	edges, err := s.store.EdgesAmong(ctx, userID, ids, MaxLimit*4)
	if err != nil {
		return Graph{}, err
	}
	return Graph{Nodes: nodes, Edges: edges}, nil
}

// Node returns one node with its 1-hop neighbourhood.
func (s *Service) Node(ctx context.Context, userID, id uuid.UUID) (Neighborhood, error) {
	n, err := s.store.NodeByID(ctx, userID, id)
	if err != nil {
		return Neighborhood{}, err
	}
	neighbors, err := s.store.Neighbors(ctx, userID, id, MaxNeighbors)
	if err != nil {
		return Neighborhood{}, err
	}
	return Neighborhood{Node: n, Neighbors: neighbors}, nil
}

// DeleteNode removes an extracted node and its edges.
//
// A node that mirrors a task, goal, note or document is refused. Those nodes
// are not independent things a user can curate: they exist because the row
// exists, sync would recreate one on the next update, and deleting the node
// while keeping the task would make the graph disagree with the tasks table
// until something happened to repair it. The way to remove one is to delete
// what it stands for, which takes the node with it through the foreign key.
func (s *Service) DeleteNode(ctx context.Context, userID, id uuid.UUID) error {
	n, err := s.store.NodeByID(ctx, userID, id)
	if err != nil {
		return err
	}
	if !n.Extracted() {
		return ErrNodeIsBacked
	}
	return s.store.DeleteNode(ctx, userID, id)
}

// DeleteEdge removes one relationship, leaving both of its nodes in place.
//
// Nothing stops the same relationship being extracted again from a later
// conversation that states it again. That is the correct behaviour for a
// derived graph and it is worth being explicit about: deleting an edge says
// "this is wrong", not "never record this", and there is no suppression list
// in this phase.
func (s *Service) DeleteEdge(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.DeleteEdge(ctx, userID, id)
}
