package chat

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// graphHarness is the Phase 4 harness with both halves of the knowledge graph
// wired in.
type graphHarness struct {
	*harness
	graph  *fakeGraph
	linker *fakeLinker
}

func newGraphHarness(t *testing.T, provider *ai.Mock) *graphHarness {
	t.Helper()
	if provider == nil {
		provider = &ai.Mock{}
	}
	base := &harness{
		store:    newFakeStore(),
		docs:     &fakeDocs{byUser: map[uuid.UUID][]documents.SearchResult{}},
		tasks:    &fakeTasks{byUser: map[uuid.UUID][]tasks.Task{}},
		goals:    &fakeGoals{byUser: map[uuid.UUID][]goals.Goal{}},
		notes:    &fakeNotes{byUser: map[uuid.UUID][]notes.Note{}},
		provider: provider,
		user:     uuid.New(),
	}
	h := &graphHarness{
		harness: base,
		graph:   &fakeGraph{byUser: map[uuid.UUID][]graph.Neighborhood{}},
		linker:  &fakeLinker{},
	}
	base.svc = NewService(Deps{
		Store: base.store, Provider: provider,
		Documents: base.docs, Graph: h.graph, GraphExtractor: h.linker,
		Tasks: base.tasks, Goals: base.goals, Notes: base.notes,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	base.svc.now = func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }
	base.conv = base.store.seed(base.user)
	return h
}

// withNode seeds one node and its 1-hop neighbourhood, the way earlier turns
// and the resource sync would have.
func (h *graphHarness) withNode(owner uuid.UUID, label, kind string, neighbors ...graph.Neighbor) graph.Neighborhood {
	n := graph.Neighborhood{
		Node:      graph.Node{ID: uuid.New(), UserID: owner, Type: kind, Label: label},
		Neighbors: neighbors,
	}
	h.graph.byUser[owner] = append(h.graph.byUser[owner], n)
	return n
}

func neighbor(label, kind, rel string, confidence float64, incoming bool) graph.Neighbor {
	return graph.Neighbor{
		Edge:     graph.Edge{ID: uuid.New(), Relationship: rel, Confidence: confidence},
		Node:     graph.Node{ID: uuid.New(), Type: kind, Label: label},
		Incoming: incoming,
	}
}

// --- retrieval --------------------------------------------------------------

// The property the phase's chat integration exists for: a question that names
// something the user has a node for gets that node's connections in the
// prompt, as a citable source, recorded as cited when the answer uses it.
func TestAGraphNeighbourhoodGroundsAnAnswer(t *testing.T) {
	h := newGraphHarness(t, &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		if strings.Contains(ai.PromptText(msgs), "the backend project") {
			return "Go is what the backend project needs [S1]."
		}
		return "I could not find anything about that."
	}})
	node := h.withNode(h.user, "Go", graph.NodeSkill,
		neighbor("the backend project", graph.NodeGoal, graph.RelRequires, 0.85, true))

	turn, sink, err := h.send(t, "why am I bothering with Go?")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// The whole triple reached the model, in subject-relationship-object order
	// with both types named -- the direction is the meaning.
	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{
		`[S1] graph: "Go"`,
		`"the backend project" (goal) REQUIRES "Go" (skill)`,
		"confidence 0.85",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}

	if len(sink.Retrieved) != 1 || sink.Retrieved[0].Type != SourceGraph {
		t.Fatalf("sink saw %+v, want one graph source", sink.Retrieved)
	}
	got := turn.Assistant.Sources[0]
	switch {
	case got.ID != node.Node.ID:
		t.Fatalf("the recorded source points at %v, not the node", got.ID)
	case got.Title != "Go":
		t.Fatalf("title = %q, want the node label", got.Title)
	case !got.Cited:
		t.Fatal("the answer cited [S1] and the source was not recorded as cited")
	case got.Similarity != nil:
		t.Fatal("a graph source carries a similarity; there is no score for an exact name match")
	}
}

// A question that names nothing retrieves nothing from the graph, and the
// question itself is what the lookup was given.
func TestTheGraphIsSearchedWithTheQuestion(t *testing.T) {
	h := newGraphHarness(t, nil)
	h.withNode(h.user, "Go", graph.NodeSkill,
		neighbor("the backend project", graph.NodeGoal, graph.RelRequires, 0.85, true))

	if _, sink, err := h.send(t, "what is on my calendar this week?"); err != nil {
		t.Fatal(err)
	} else if len(sink.Retrieved) != 0 {
		t.Fatalf("a question naming nothing retrieved %+v", sink.Retrieved)
	}

	if len(h.graph.queries) != 1 || h.graph.queries[0] != "what is on my calendar this week?" {
		t.Fatalf("the lookup was given %q", h.graph.queries)
	}
	if h.graph.limits[0] != MaxGraphNodes {
		t.Fatalf("limit = %d, want MaxGraphNodes", h.graph.limits[0])
	}
	if h.graph.callers[0] != h.user {
		t.Fatalf("the lookup was scoped to %v, not the caller", h.graph.callers[0])
	}
}

// The mirrored node names the record it stands for, which is how the model
// joins a link to the goal it may also have been given as its own source --
// the node carries no deadline and must not appear to.
func TestAMirroredNodeNamesItsRecord(t *testing.T) {
	h := newGraphHarness(t, nil)
	goalID := uuid.New()
	table := "goals"
	n := graph.Neighborhood{
		Node: graph.Node{
			ID: uuid.New(), UserID: h.user, Type: graph.NodeGoal,
			Label: "the backend project", RefTable: &table, RefID: &goalID,
		},
		Neighbors: []graph.Neighbor{neighbor("Go", graph.NodeSkill, graph.RelRequires, 0.85, false)},
	}
	h.graph.byUser[h.user] = append(h.graph.byUser[h.user], n)

	if _, _, err := h.send(t, "how is the backend project coming along?"); err != nil {
		t.Fatal(err)
	}
	prompt := ai.PromptText(h.provider.LastPrompt())
	if !strings.Contains(prompt, "this is the goal "+goalID.String()) {
		t.Fatalf("the prompt does not name the record the node mirrors:\n%s", prompt)
	}
}

// Graph sources sit after the memories and before the heuristic items: they
// fire on a real mention, but they carry links rather than the user's words.
func TestGraphSourcesAreOrderedAfterSemanticMatchesAndBeforeItems(t *testing.T) {
	h := newGraphHarness(t, nil)
	h.docs.byUser[h.user] = []documents.SearchResult{{
		ChunkID: uuid.New(), DocumentID: uuid.New(), Filename: "notes.md",
		Content: "Go is a language.", Similarity: 0.8,
	}}
	h.withNode(h.user, "Go", graph.NodeSkill,
		neighbor("the backend project", graph.NodeGoal, graph.RelRequires, 0.85, true))
	h.tasks.byUser[h.user] = []tasks.Task{{
		ID: uuid.New(), Title: "Finish the Go tutorial", Priority: "high", Status: "pending",
	}}

	_, sink, err := h.send(t, "what should I know about Go?")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, s := range sink.Retrieved {
		kinds = append(kinds, s.Type)
	}
	want := []string{SourceDocument, SourceGraph, SourceTask}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("retrieved %v, want %v", kinds, want)
	}
}

// An assistant with no graph wired retrieves exactly what Phase 5 did.
func TestANilGraphIsANoOp(t *testing.T) {
	h := newHarness(t, nil)
	if _, sink, err := h.send(t, "what should I know about Go?"); err != nil {
		t.Fatal(err)
	} else if len(sink.Retrieved) != 0 {
		t.Fatalf("retrieved %+v with no graph wired", sink.Retrieved)
	}
}

// A graph failure fails the turn, for the same reason a task-list failure
// does: answering "I found no connections" when the query errored is a false
// statement about the user's data.
func TestAGraphFailureFailsTheTurn(t *testing.T) {
	h := newGraphHarness(t, nil)
	h.graph.err = errors.New("boom")
	if _, _, err := h.send(t, "what should I know about Go?"); err == nil {
		t.Fatal("a failing graph lookup produced an answer")
	}
	if len(h.provider.Calls()) != 0 {
		t.Fatal("the model was called after retrieval failed")
	}
}

// --- extraction -------------------------------------------------------------

// The exchange handed to the relationship extractor is the one that was
// stored, and it is handed over after the turn is persisted.
func TestTheTurnIsHandedToTheRelationshipExtractor(t *testing.T) {
	h := newGraphHarness(t, &ai.Mock{Reply: "Noted."})
	edge := graph.Edge{
		ID: uuid.New(), FromNodeID: uuid.New(), ToNodeID: uuid.New(),
		Relationship: graph.RelStudies, Confidence: 0.9,
	}
	h.linker.stored = []graph.Edge{edge}

	turn, _, err := h.send(t, "I am learning Go for the backend project.")
	if err != nil {
		t.Fatal(err)
	}

	seen := h.linker.seen()
	if len(seen) != 1 {
		t.Fatalf("the extractor was called %d times, want 1", len(seen))
	}
	switch {
	case seen[0].userID != h.user:
		t.Fatalf("extraction ran as %v, not the caller", seen[0].userID)
	case seen[0].convID != h.conv.ID:
		t.Fatalf("extraction was told conversation %v", seen[0].convID)
	case seen[0].question != "I am learning Go for the backend project.":
		t.Fatalf("extraction saw the question %q", seen[0].question)
	case seen[0].answer != "Noted.":
		t.Fatalf("extraction saw the answer %q", seen[0].answer)
	}

	// The turn reports what it linked, the same way it reports what it
	// remembered: an assistant that records claims about a user without
	// telling them is the version of this nobody asked for.
	if len(turn.Linked) != 1 || turn.Linked[0].ID != edge.ID {
		t.Fatalf("turn.Linked = %+v, want the stored edge", turn.Linked)
	}
	// And the turn is already persisted by then.
	if len(h.store.stored(h.conv.ID)) != 2 {
		t.Fatal("extraction ran before the turn was written")
	}
}

// A failure to record a relationship must never turn a good answer into a 500.
func TestARelationshipExtractionFailureDoesNotFailTheTurn(t *testing.T) {
	h := newGraphHarness(t, &ai.Mock{Reply: "Noted."})
	h.linker.err = errors.New("the model timed out")

	turn, sink, err := h.send(t, "I am learning Go for the backend project.")
	if err != nil {
		t.Fatalf("a failed extraction failed the turn: %v", err)
	}
	if sink.Text.String() != "Noted." {
		t.Fatalf("the answer was %q", sink.Text.String())
	}
	if len(turn.Linked) != 0 {
		t.Fatalf("turn.Linked = %+v after a failure", turn.Linked)
	}
	if len(h.store.stored(h.conv.ID)) != 2 {
		t.Fatal("the turn was not stored")
	}
}

// A nil extractor is an assistant that uses the graph without growing it.
func TestANilRelationshipExtractorIsANoOp(t *testing.T) {
	h := newHarness(t, nil)
	turn, _, err := h.send(t, "I am learning Go for the backend project.")
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Linked) != 0 {
		t.Fatalf("turn.Linked = %+v with no extractor wired", turn.Linked)
	}
}

// Every graph call the orchestrator makes carries the caller's id, and no
// other.
func TestGraphRetrievalAndExtractionAreOwnerScoped(t *testing.T) {
	h := newGraphHarness(t, nil)
	bob := uuid.New()
	h.withNode(bob, "Go", graph.NodeSkill,
		neighbor("bob's project", graph.NodeGoal, graph.RelRequires, 0.9, true))

	_, sink, err := h.send(t, "what should I know about Go?")
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.Retrieved) != 0 {
		t.Fatalf("alice's question retrieved bob's graph: %+v", sink.Retrieved)
	}
	for _, caller := range append(h.graph.callers, h.linker.seen()[0].userID) {
		if caller != h.user {
			t.Fatalf("a graph call was made as %v, want %v", caller, h.user)
		}
	}
}

// --- the prompt contract ----------------------------------------------------

// The grounding rules have to name the graph, or a 3B model reads a rendered
// triple as a sentence it may quote as fact.
func TestTheSystemPromptExplainsAGraphSource(t *testing.T) {
	h := newGraphHarness(t, nil)
	h.withNode(h.user, "Go", graph.NodeSkill,
		neighbor("the backend project", graph.NodeGoal, graph.RelRequires, 0.85, true))
	if _, _, err := h.send(t, "what should I know about Go?"); err != nil {
		t.Fatal(err)
	}
	system := h.provider.LastPrompt()[0].Content
	for _, want := range []string{`type "graph"`, "connected", "RELATIONSHIP"} {
		if !strings.Contains(system, want) {
			t.Fatalf("the system prompt does not explain a graph source (%q missing):\n%s", want, system)
		}
	}
}

func TestGraphSummaryRendersBothDirections(t *testing.T) {
	n := graph.Neighborhood{
		Node: graph.Node{Type: graph.NodeSkill, Label: "Go"},
		Neighbors: []graph.Neighbor{
			neighbor("the backend project", graph.NodeGoal, graph.RelRequires, 0.85, true),
			neighbor("Godot", graph.NodeProject, graph.RelRelatedTo, 0.5, false),
		},
	}
	got := graphSummary(n)
	for _, want := range []string{
		`"the backend project" (goal) REQUIRES "Go" (skill) · confidence 0.85`,
		`"Go" (skill) RELATED_TO "Godot" (project) · confidence 0.50`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary is missing %q:\n%s", want, got)
		}
	}
}
