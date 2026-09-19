package study

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/optional"
)

func TestCreatePlanSyncsAGraphNode(t *testing.T) {
	h := newHarness(t, groundedReply)

	created, err := h.svc.CreatePlan(context.Background(), h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}

	seen := h.graph.seen()
	if len(seen) != 1 {
		t.Fatalf("creating a plan made %d graph calls, want 1", len(seen))
	}
	switch {
	case seen[0].userID != h.owner:
		t.Fatalf("the node was synced as %v, not the owner", seen[0].userID)
	case seen[0].refTable != graphRefTable:
		t.Fatalf("ref_table = %q, want %q", seen[0].refTable, graphRefTable)
	case seen[0].refID != created.ID:
		t.Fatalf("ref_id = %v, want the plan id", seen[0].refID)
	case seen[0].label != "Linear algebra finals":
		t.Fatalf("label = %q, want the title", seen[0].label)
	}
}

// The ref_table this module writes has to be one the graph mirrors, and it has
// to produce `project`. The string is in three places -- this module, graph's
// map, and migration 000010's CHECK constraint -- and a mismatch is a write
// error at run time rather than a compile error, so it is pinned here as well
// as in internal/db.
func TestTheGraphMirrorsStudyPlansAsProjects(t *testing.T) {
	nodeType, ok := graph.TypeForRefTable(graphRefTable)
	if !ok {
		t.Fatalf("the graph does not mirror %q", graphRefTable)
	}
	if nodeType != graph.NodeProject {
		t.Fatalf("a study plan is a %q node, want %q", nodeType, graph.NodeProject)
	}
}

func TestUpdateResyncsTheGraphNode(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()

	created, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.UpdatePlan(ctx, h.owner, created.ID, UpdatePlanInput{
		Title: optional.Of("Linear algebra resit"),
	}); err != nil {
		t.Fatal(err)
	}

	nodes := h.graph.nodes()
	if len(nodes) != 1 {
		t.Fatalf("renaming made %d nodes, want 1", len(nodes))
	}
	if got := nodes[created.ID]; got != "Linear algebra resit" {
		t.Fatalf("the node label is %q, want the new title", got)
	}
}

func TestARejectedWriteDoesNotSync(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()

	// Each of these fails at a different step: validation, then the ownership
	// check on the document link. Neither may reach the graph.
	if _, err := h.svc.CreatePlan(ctx, h.owner, CreatePlanInput{Title: "  "}); err == nil {
		t.Fatal("a plan with no title was accepted")
	}
	stranger := uuid.New()
	if _, err := h.svc.CreatePlan(ctx, h.owner, CreatePlanInput{
		Title: "Borrowed", DocumentID: &stranger,
	}); err == nil {
		t.Fatal("a link to nothing was accepted")
	}
	if n := len(h.graph.seen()); n != 0 {
		t.Fatalf("a rejected create made %d graph calls", n)
	}
}

// Deleting makes no graph call at all: the node goes with the row through the
// trigger, on every path a row can leave by.
func TestDeleteDoesNotCallTheGraph(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()

	created, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.DeletePlan(ctx, h.owner, created.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.graph.seen()); n != 1 {
		t.Fatalf("delete made a graph call: %d calls after one create", n)
	}
}

// Flashcards are not mirrored. A deck is hundreds of rows of one or two
// sentences, and a node per card would swamp the graph and make the mention
// scan fire on any question sharing a word with an answer.
func TestFlashcardsGetNoGraphNode(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()
	plan, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}
	before := len(h.graph.seen())

	if _, err := h.svc.AddFlashcard(ctx, h.owner, plan.ID, NewCard{Front: "q?", Back: "a."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateFlashcards(ctx, h.owner, []CreateCardInput{
		{StudyPlanID: &plan.ID, Front: "q2?", Back: "a2."},
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.graph.seen()); n != before {
		t.Fatalf("writing cards made %d graph calls", n-before)
	}
}

func TestNoGraphIsANoOp(t *testing.T) {
	svc := NewService(Deps{Store: newFakeStore(), Logger: quiet()})
	if _, err := svc.CreatePlan(context.Background(), uuid.New(), planWithDescription()); err != nil {
		t.Fatalf("a service with no graph wired failed to create a plan: %v", err)
	}
}
