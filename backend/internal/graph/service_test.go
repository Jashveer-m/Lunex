package graph

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// harness is one graph service with every dependency faked, including the
// model. Nothing here needs Postgres or Ollama.
type harness struct {
	svc      *Service
	store    *fakeStore
	provider *ai.Mock
	user     uuid.UUID
	conv     uuid.UUID
}

func newHarness(t *testing.T, provider *ai.Mock) *harness {
	t.Helper()
	if provider == nil {
		provider = &ai.Mock{}
	}
	h := &harness{
		store:    newFakeStore(),
		provider: provider,
		user:     uuid.New(),
		conv:     uuid.New(),
	}
	h.svc = NewService(Deps{
		Store: h.store, Provider: provider,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h
}

// A substantial exchange, used wherever the test is about something other than
// the length threshold.
const (
	realQuestion = "I have been learning Go this term so I can finish the backend project, " +
		"and my flatmate Priya keeps unblocking me on the concurrency parts."
	realAnswer = "Noted -- the backend project is the only thing you have with a deadline this month."
)

// triples renders a model reply in the JSON the prompt asks for.
func triples(rows ...string) *ai.Mock {
	return &ai.Mock{Reply: "[" + strings.Join(rows, ",") + "]"}
}

func triple(from, fromType, rel, to, toType string, confidence float64) string {
	return `{"from":"` + from + `","from_type":"` + fromType + `","relationship":"` + rel +
		`","to":"` + to + `","to_type":"` + toType + `","confidence":` +
		strconv.FormatFloat(confidence, 'g', -1, 64) + `}`
}

// --- node sync --------------------------------------------------------------

// The property sync-on-write exists for: creating a task puts a node in the
// graph, and doing it twice does not put two there.
func TestSyncNodeIsIdempotentAndFollowsTheLabel(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	taskID := uuid.New()

	h.svc.SyncNode(ctx, h.user, "tasks", taskID, "Write the scheduler")
	h.svc.SyncNode(ctx, h.user, "tasks", taskID, "Write the scheduler")
	if n := h.store.nodeCount(h.user); n != 1 {
		t.Fatalf("syncing the same task twice made %d nodes", n)
	}

	// The update path is what keeps the label honest after a rename.
	h.svc.SyncNode(ctx, h.user, "tasks", taskID, "Write the CFS scheduler")
	if n := h.store.nodeCount(h.user); n != 1 {
		t.Fatalf("renaming the task made %d nodes", n)
	}
	nodes, _ := h.store.Nodes(ctx, h.user, Filter{Limit: 10})
	switch {
	case nodes[0].Label != "Write the CFS scheduler":
		t.Fatalf("label = %q, want the new title", nodes[0].Label)
	case nodes[0].Type != NodeTask:
		t.Fatalf("type = %q, want %q", nodes[0].Type, NodeTask)
	case nodes[0].Extracted():
		t.Fatal("a synced node reports itself as extracted")
	case nodes[0].RefID == nil || *nodes[0].RefID != taskID:
		t.Fatalf("ref = %v, want the task id", nodes[0].RefID)
	}
}

func TestSyncNodeMapsEachTableToItsType(t *testing.T) {
	h := newHarness(t, nil)
	for table, want := range map[string]string{
		"tasks": NodeTask, "goals": NodeGoal, "notes": NodeNote, "documents": NodeDocument,
	} {
		h.svc.SyncNode(context.Background(), h.user, table, uuid.New(), "a "+table+" row")
		nodes, _ := h.store.NodesByLabel(context.Background(), h.user, "a "+table+" row")
		if len(nodes) != 1 || nodes[0].Type != want {
			t.Fatalf("%s produced %+v, want one node of type %q", table, nodes, want)
		}
	}
}

// A table the graph does not mirror is refused rather than written: the CHECK
// constraint would reject it anyway, and inventing a type would be worse.
func TestSyncNodeRefusesAnUnknownTable(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.SyncNode(context.Background(), h.user, "expenses", uuid.New(), "a coffee")
	if n := h.store.nodeCount(h.user); n != 0 {
		t.Fatalf("an unknown source table wrote %d nodes", n)
	}
}

// Node sync is owner-scoped like everything else: two users' identically named
// tasks are two nodes.
func TestSyncNodeKeepsUsersApart(t *testing.T) {
	h := newHarness(t, nil)
	bob := uuid.New()
	h.svc.SyncNode(context.Background(), h.user, "tasks", uuid.New(), "Revise")
	h.svc.SyncNode(context.Background(), bob, "tasks", uuid.New(), "Revise")
	if a, b := h.store.nodeCount(h.user), h.store.nodeCount(bob); a != 1 || b != 1 {
		t.Fatalf("alice has %d nodes and bob %d, want one each", a, b)
	}
}

// --- extraction -------------------------------------------------------------

// The property the phase turns on: an exchange that states a relationship
// leaves two nodes and an edge behind, with its provenance recorded.
func TestExtractFromTurnStoresARelationship(t *testing.T) {
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))

	edges, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
	if err != nil {
		t.Fatalf("ExtractFromTurn: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("stored %d edges, want 1", len(edges))
	}
	e := edges[0]
	switch {
	case e.Relationship != RelStudies:
		t.Fatalf("relationship = %q", e.Relationship)
	case e.UserID != h.user:
		t.Fatalf("owner = %v, want %v", e.UserID, h.user)
	case e.SourceConversationID == nil || *e.SourceConversationID != h.conv:
		t.Fatalf("source = %v, want the conversation it came from", e.SourceConversationID)
	}
	if diff := e.Confidence - 0.9; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("confidence = %v, want 0.9", e.Confidence)
	}

	// "the user" became the one self node; "Go" became a skill.
	labels := h.store.labels(h.user)
	if len(labels) != 2 {
		t.Fatalf("nodes = %v, want two", labels)
	}
	from, _ := h.store.NodeByID(context.Background(), h.user, e.FromNodeID)
	to, _ := h.store.NodeByID(context.Background(), h.user, e.ToNodeID)
	if from.Label != SelfLabel || from.Type != SelfType {
		t.Fatalf("the subject is %q (%s), want the self node", from.Label, from.Type)
	}
	if to.Label != "Go" || to.Type != NodeSkill {
		t.Fatalf("the object is %q (%s), want Go (skill)", to.Label, to.Type)
	}
}

// "Go" mentioned in two conversations is one node, and the same relationship
// stated twice is one edge with the better confidence kept.
func TestExtractionDoesNotDuplicateNodesOrEdges(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.7)))
	if _, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}

	// A second conversation, saying the same thing in different case and with
	// a different word for the user.
	h.provider.Reply = "[" + triple("I", "person", "studies", "go", "skill", 0.95) + "]"
	if _, err := h.svc.ExtractFromTurn(ctx, h.user, uuid.New(), realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}

	if n := h.store.nodeCount(h.user); n != 2 {
		t.Fatalf("the graph holds %d nodes (%v), want 2", n, h.store.labels(h.user))
	}
	if n := h.store.edgeCount(h.user); n != 1 {
		t.Fatalf("the graph holds %d edges, want 1", n)
	}
	edges, _ := h.store.EdgesAmong(ctx, h.user, nodeIDs(h.store, h.user), 10)
	if edges[0].Confidence != 0.95 {
		t.Fatalf("confidence = %v, want the restatement's higher 0.95", edges[0].Confidence)
	}
	// The provenance stays with where it was first learned.
	if *edges[0].SourceConversationID != h.conv {
		t.Fatal("restating a relationship overwrote where it was first learned")
	}
}

// An extracted name that matches something the user already has attaches to
// that node, rather than creating a parallel idea of the same thing. This is
// the reason the resolver looks at every node and not only the extracted ones.
func TestExtractionAttachesToAnExistingSyncedNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the backend project", "project", "REQUIRES", "Go", "skill", 0.85)))
	goalID := uuid.New()
	h.svc.SyncNode(ctx, h.user, "goals", goalID, "the backend project")

	edges, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("stored %d edges, want 1", len(edges))
	}
	from, _ := h.store.NodeByID(ctx, h.user, edges[0].FromNodeID)
	if from.RefID == nil || *from.RefID != goalID {
		t.Fatalf("the edge attached to %+v, want the goal the user already had", from)
	}
	if n := h.store.nodeCount(h.user); n != 2 {
		t.Fatalf("the graph holds %d nodes (%v), want the goal plus Go", n, h.store.labels(h.user))
	}
}

// The quality gate is between the model and the database, not after it.
//
// Note that MaxRelationshipsPerTurn is applied in ParseExtraction, *before*
// this gate -- the same order memories uses -- so three candidates here is the
// most a reply can carry into it. That ordering has a cost, recorded in
// docs/decisions.md: junk listed first crowds out a good relationship listed
// fourth.
func TestExtractionDropsWhatTheGateRejects(t *testing.T) {
	h := newHarness(t, triples(
		triple("the user", "person", "STUDIES", "Go", "skill", 0.2),
		// Lifted out of the assistant's half of the exchange rather than the
		// user's: a good name for something the user never mentioned.
		triple("the user", "person", "INTERESTED_IN", "the deadline calendar", "project", 0.9),
		triple("the user", "person", "KNOWS", "Priya", "person", 0.8),
	))

	edges, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("stored %d edges, want only the good one: %+v", len(edges), edges)
	}
	to, _ := h.store.NodeByID(context.Background(), h.user, edges[0].ToNodeID)
	if to.Label != "Priya" {
		t.Fatalf("kept the edge to %q", to.Label)
	}
	// Nothing rejected left a node behind: resolution happens after the gate.
	if n := h.store.nodeCount(h.user); n != 2 {
		t.Fatalf("the graph holds %d nodes (%v), want the user and Priya", n, h.store.labels(h.user))
	}
}

// Two names that fold to one node cannot make an edge -- the CHECK constraint
// would reject it, and the node must not be created for its sake either.
func TestExtractionDropsASelfEdge(t *testing.T) {
	h := newHarness(t, triples(triple("the user", "person", "KNOWS", "me", "person", 0.9)))
	edges, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("stored %d edges, want none", len(edges))
	}
}

func TestExtractionSkipsATrivialTurn(t *testing.T) {
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	edges, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, "hi", "Hello!")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("stored %d edges from a greeting", len(edges))
	}
	if len(h.provider.Calls()) != 0 {
		t.Fatal("a greeting cost a model call")
	}
}

func TestExtractionSurfacesAModelFailure(t *testing.T) {
	h := newHarness(t, &ai.Mock{Err: ai.ErrUnavailable})
	_, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
	if !errors.Is(err, ErrExtraction) || !errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("err = %v, want both ErrExtraction and ErrUnavailable", err)
	}
}

// A store failure partway through keeps what was already written: the
// relationships are independent, and rolling one back gains nothing.
func TestExtractionKeepsWhatItWroteBeforeAFailure(t *testing.T) {
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	h.store.createEdgeErr = errors.New("boom")
	stored, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
	if err == nil {
		t.Fatal("a failing store did not produce an error")
	}
	if stored != nil && len(stored) != 0 {
		t.Fatalf("stored = %+v", stored)
	}
}

// A service with no provider still syncs and still reads; it simply never
// grows an edge on its own.
func TestExtractionIsANoOpWithoutAProvider(t *testing.T) {
	store := newFakeStore()
	svc := NewService(Deps{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	user := uuid.New()
	svc.SyncNode(context.Background(), user, "tasks", uuid.New(), "Revise")

	edges, err := svc.ExtractFromTurn(context.Background(), user, uuid.New(), realQuestion, realAnswer)
	if err != nil || len(edges) != 0 {
		t.Fatalf("edges = %+v, err = %v", edges, err)
	}
	if store.nodeCount(user) != 1 {
		t.Fatal("a provider-less graph stopped syncing nodes")
	}
}

// --- the chat lookup --------------------------------------------------------

// The property the chat integration exists for: a question that names a node
// comes back with what that node is connected to.
func TestMentionedReturnsTheNeighbourhoodOfANamedNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the backend project", "project", "REQUIRES", "Go", "skill", 0.85)))
	if _, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}

	found, err := h.svc.Mentioned(ctx, h.user, "how is Go going for me?", MaxNeighbors)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d neighbourhoods, want 1: %+v", len(found), found)
	}
	if found[0].Node.Label != "Go" {
		t.Fatalf("matched %q", found[0].Node.Label)
	}
	if len(found[0].Neighbors) != 1 {
		t.Fatalf("Go has %d neighbours, want 1", len(found[0].Neighbors))
	}
	nb := found[0].Neighbors[0]
	if nb.Node.Label != "the backend project" || nb.Edge.Relationship != RelRequires {
		t.Fatalf("neighbour = %+v", nb)
	}
	// Direction is carried, not flattened: the project requires Go, not the
	// other way round.
	if !nb.Incoming {
		t.Fatal("the edge is reported as outgoing from Go; it points at Go")
	}
}

// The SQL prefilter over-collects on purpose; the word-boundary test is what
// decides. A node whose label only appears inside another word is not a match.
func TestMentionedAppliesTheWordBoundaryTest(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	if _, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}

	found, err := h.svc.Mentioned(ctx, h.user, "I am going to the shop", MaxNeighbors)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf(`"going" matched the node "Go": %+v`, found)
	}
}

// The self node is labelled "You", and in a message written by the *user* the
// word "you" means the assistant. Matching on it would fire on nearly every
// turn and be wrong about what it matched every time.
func TestMentionedSkipsTheSelfNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	if _, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}

	// A question that names the self node's label and nothing else.
	found, err := h.svc.Mentioned(ctx, h.user, "can you check what is on my list?", MaxNeighbors)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf(`"you" matched the self node: %+v`, found)
	}

	// The rest of the graph still matches, so the skip is not a blanket one.
	found, err = h.svc.Mentioned(ctx, h.user, "how is Go going?", MaxNeighbors)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Node.Label != "Go" {
		t.Fatalf("skipping the self node also lost the real match: %+v", found)
	}
	// And the self node is still on the far end of the edge that was returned,
	// so it is invisible to the *scan*, not to the graph.
	if found[0].Neighbors[0].Node.Label != SelfLabel {
		t.Fatalf("the self node vanished from the neighbourhood too: %+v", found[0].Neighbors)
	}
}

// An isolated node says nothing the rest of retrieval does not already say, so
// it does not spend a source.
func TestMentionedSkipsANodeWithNoEdges(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.svc.SyncNode(ctx, h.user, "tasks", uuid.New(), "Write the scheduler")

	found, err := h.svc.Mentioned(ctx, h.user, "how is the scheduler going?", MaxNeighbors)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("an edgeless node was returned: %+v", found)
	}
}

// The lookup is owner-scoped: Bob's message never reaches Alice's nodes.
func TestMentionedKeepsUsersApart(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	if _, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}

	bob := uuid.New()
	found, err := h.svc.Mentioned(ctx, bob, "how is Go going?", MaxNeighbors)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("bob's question reached alice's graph: %+v", found)
	}
	for _, caller := range h.store.callersSeen() {
		if caller != h.user && caller != bob {
			t.Fatalf("the store was called with %v, which is nobody", caller)
		}
	}
}

// --- reads and management ---------------------------------------------------

// A filtered read is still a graph: every edge it returns has both of its
// endpoints in the node list.
func TestGraphReturnsAClosedSubgraph(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	if _, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}

	whole, err := h.svc.Graph(ctx, h.user, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Nodes) != 2 || len(whole.Edges) != 1 {
		t.Fatalf("the whole graph is %d nodes and %d edges, want 2 and 1", len(whole.Nodes), len(whole.Edges))
	}

	// Filtering to skills leaves the edge with only one endpoint present, so
	// the edge must go too.
	skills, err := h.svc.Graph(ctx, h.user, Filter{Type: NodeSkill})
	if err != nil {
		t.Fatal(err)
	}
	if len(skills.Nodes) != 1 || skills.Nodes[0].Label != "Go" {
		t.Fatalf("the skill filter returned %+v", skills.Nodes)
	}
	if len(skills.Edges) != 0 {
		t.Fatalf("the filtered read returned %d dangling edges", len(skills.Edges))
	}
}

func TestGraphRejectsAnUnknownType(t *testing.T) {
	var verrs validate.Errors
	_, err := h(t).svc.Graph(context.Background(), uuid.New(), Filter{Type: "expense"})
	if !errors.As(err, &verrs) || verrs[0].Field != "type" {
		t.Fatalf("err = %v, want a validation error on `type`", err)
	}
}

// The rule the DELETE endpoint exists to enforce: an extracted node is the
// user's to remove, a mirrored one follows its record.
func TestDeleteNodeRefusesAMirroredNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	if _, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}
	h.svc.SyncNode(ctx, h.user, "tasks", uuid.New(), "Write the scheduler")

	nodes, _ := h.store.Nodes(ctx, h.user, Filter{Limit: 10})
	var extracted, mirrored Node
	for _, n := range nodes {
		if n.Extracted() {
			extracted = n
		} else {
			mirrored = n
		}
	}

	if err := h.svc.DeleteNode(ctx, h.user, mirrored.ID); !errors.Is(err, ErrNodeIsBacked) {
		t.Fatalf("deleting a mirrored node returned %v, want ErrNodeIsBacked", err)
	}
	if err := h.svc.DeleteNode(ctx, h.user, extracted.ID); err != nil {
		t.Fatalf("deleting an extracted node: %v", err)
	}
	// Its edges went with it.
	if n := h.store.edgeCount(h.user); n != 0 {
		t.Fatalf("%d edges survived their node", n)
	}
}

// A node or edge belonging to somebody else is indistinguishable from one that
// does not exist.
func TestReadsAndDeletesAreOwnerScoped(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	edges, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer)
	if err != nil {
		t.Fatal(err)
	}
	bob := uuid.New()

	if _, err := h.svc.Node(ctx, bob, edges[0].FromNodeID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob read alice's node: %v", err)
	}
	if err := h.svc.DeleteNode(ctx, bob, edges[0].FromNodeID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob deleted alice's node: %v", err)
	}
	if err := h.svc.DeleteEdge(ctx, bob, edges[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob deleted alice's edge: %v", err)
	}
	g, err := h.svc.Graph(ctx, bob, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 0 || len(g.Edges) != 0 {
		t.Fatalf("bob's graph is %+v", g)
	}
	if h.store.edgeCount(h.user) != 1 || h.store.nodeCount(h.user) != 2 {
		t.Fatal("alice's graph was modified by bob's requests")
	}
}

func TestDeleteEdgeLeavesItsNodes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	edges, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.DeleteEdge(ctx, h.user, edges[0].ID); err != nil {
		t.Fatal(err)
	}
	if h.store.edgeCount(h.user) != 0 {
		t.Fatal("the edge survived")
	}
	if h.store.nodeCount(h.user) != 2 {
		t.Fatal("deleting an edge took its nodes with it")
	}
}

func TestNodeReturnsItsNeighbourhood(t *testing.T) {
	ctx := context.Background()
	hh := newHarness(t, triples(triple("the user", "person", "STUDIES", "Go", "skill", 0.9)))
	edges, err := hh.svc.ExtractFromTurn(ctx, hh.user, hh.conv, realQuestion, realAnswer)
	if err != nil {
		t.Fatal(err)
	}
	got, err := hh.svc.Node(ctx, hh.user, edges[0].ToNodeID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Node.Label != "Go" || len(got.Neighbors) != 1 {
		t.Fatalf("neighbourhood = %+v", got)
	}
	if !got.Neighbors[0].Incoming || got.Neighbors[0].Node.Label != SelfLabel {
		t.Fatalf("neighbour = %+v, want the incoming edge from the self node", got.Neighbors[0])
	}
}

// --- helpers ----------------------------------------------------------------

func h(t *testing.T) *harness { return newHarness(t, nil) }

func nodeIDs(store *fakeStore, userID uuid.UUID) []uuid.UUID {
	nodes, _ := store.Nodes(context.Background(), userID, Filter{Limit: 100})
	out := make([]uuid.UUID, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}
