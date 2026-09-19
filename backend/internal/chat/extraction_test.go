package chat

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/agents"
	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/study"
	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// extractingToolHarness is the Phase 7 orchestrator with both extractors wired
// as fakes, so a test can see exactly what each was allowed to read.
func extractingToolHarness(t *testing.T, provider *ai.Mock) (*toolHarness, *fakeExtractor, *fakeLinker) {
	t.Helper()
	h := newToolHarness(t, provider)
	ex, ln := &fakeExtractor{}, &fakeLinker{}
	now := h.svc.now
	h.svc = NewService(Deps{
		Store: h.store, Provider: provider,
		Documents: h.docs, Tasks: h.tools, Goals: h.goals, Notes: h.notes,
		MemoryExtractor: ex, GraphExtractor: ln,
		Router:  agents.NewRouter(provider, h.reg, slog.New(slog.NewTextHandler(io.Discard, nil)), agents.Options{}),
		Tools:   h.reg,
		Actions: h.log,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.svc.now = now
	return h, ex, ln
}

// A turn that proposes a change tells both extractors it has not happened. The
// memory extractor used to store a rejected "book a flight to Delhi" as a thing
// the user did, and the graph extractor invented a node for a task that did
// not exist yet.
func TestAProposalIsHandedToTheExtractorsAsUnconfirmed(t *testing.T) {
	h, ex, ln := extractingToolHarness(t, routingReply(
		`{"tool": "create_task", "arguments": {"title": "Book a flight to Delhi", "priority": "high"}}`, answerText))
	if _, _, err := h.send(t, "Add a task to book a flight to Delhi, it's urgent"); err != nil {
		t.Fatal(err)
	}
	for name, calls := range map[string][]extraction{"memory": ex.seen(), "graph": ln.seen()} {
		if len(calls) != 1 || !slices.Equal(calls[0].unconfirmed, []string{"Book a flight to Delhi"}) {
			t.Fatalf("%s extractor got %+v, want the proposal's title as unconfirmed", name, calls)
		}
	}
}

// Earlier changes count by their outcome: one rejected or still waiting is not
// reality, one that was carried out is.
func TestEarlierChangesAreUnconfirmedUnlessTheyWereCarriedOut(t *testing.T) {
	h, ex, _ := extractingToolHarness(t, &ai.Mock{Reply: "Noted."})
	conv := h.conv.ID
	for status, title := range map[string]string{
		actions.StatusExecuted: "Renew my passport",
		actions.StatusRejected: "Book a flight to Delhi",
		actions.StatusProposed: "Water the plants",
	} {
		h.log.before = append(h.log.before, actions.Action{
			ID: uuid.New(), UserID: h.user, ConversationID: &conv, ToolName: tools.CreateTask,
			Permission: tools.Write, Status: status, Input: json.RawMessage(`{"title":"` + title + `"}`),
		})
	}
	if _, _, err := h.send(t, "thanks, that is everything for today"); err != nil {
		t.Fatal(err)
	}
	got := ex.seen()[0].unconfirmed
	slices.Sort(got)
	if !slices.Equal(got, []string{"Book a flight to Delhi", "Water the plants"}) {
		t.Fatalf("unconfirmed = %v, want the rejected and the waiting change only", got)
	}
}

// The memory extractor is given the document passages the answer drew on, so
// it can refuse a "fact" that is only the document restated.
func TestRetrievedDocumentsAreHandedToTheMemoryExtractor(t *testing.T) {
	h, ex, _ := extractingToolHarness(t, &ai.Mock{Reply: "Your log says tap water stunted them [S1]."})
	h.docs.byUser[h.user] = []documents.SearchResult{{
		DocumentID: uuid.New(), Filename: "garden-log.md", Content: "Tap water stunted last year's seedlings.", Similarity: 0.8,
	}}
	if _, _, err := h.send(t, "What does my garden log say about the seedlings?"); err != nil {
		t.Fatal(err)
	}
	if got := ex.seen()[0].retrieved; !slices.Equal(got, []string{"Tap water stunted last year's seedlings."}) {
		t.Fatalf("retrieved = %v", got)
	}
}

// The other half: once a change is approved and made, the turn that proposed
// it is read again for relationships -- the same user message, an answer that
// says what was done, and the new record as the anchor.
func TestAnExecutedActionIsReadAgainAnchoredToItsRecord(t *testing.T) {
	h, _, ln := extractingToolHarness(t, routingReply(
		`{"tool": "create_task", "arguments": {"title": "Renew my passport"}}`, answerText))
	const question = "Add a task to renew my passport before the Lisbon trip"
	if _, _, err := h.send(t, question); err != nil {
		t.Fatal(err)
	}
	// A later turn, so the lookup has to find the right message.
	if _, _, err := h.send(t, "thanks"); err != nil {
		t.Fatal(err)
	}
	proposed := h.store.recordedActions()[0]
	executed := proposed
	executed.Status = actions.StatusExecuted
	task := tasks.Task{ID: uuid.New(), Title: "Renew my passport"}

	h.svc.ActionExecuted(context.Background(), h.user, executed, `Create a task "Renew my passport".`,
		tools.Result{Tasks: []tasks.Task{task}})

	calls := ln.seen()
	last := calls[len(calls)-1]
	switch {
	case len(calls) != 3:
		t.Fatalf("linker ran %d times, want two turns and one approval", len(calls))
	case last.anchor == nil || last.anchor.RefTable != "tasks" || last.anchor.RefID != task.ID || last.anchor.Label != task.Title:
		t.Fatalf("anchor = %+v", last.anchor)
	case last.question != question:
		t.Fatalf("read %q, want the proposing message", last.question)
	case last.convID != h.conv.ID || last.userID != h.user:
		t.Fatalf("read in the wrong scope: %+v", last)
	case len(last.unconfirmed) != 0:
		t.Fatalf("an executed change was still unconfirmed: %v", last.unconfirmed)
	}
}

// An approved study plan anchors on `study_plans`, which the graph mirrors as
// a `project`. It is worth its own case because the node type is not the
// table's name -- the one mirrored table where those differ -- so an anchor
// that got it wrong would be a write the graph refuses at run time rather than
// anything the compiler catches.
func TestAnApprovedStudyPlanAnchorsOnItsTable(t *testing.T) {
	h, _, ln := extractingToolHarness(t, routingReply(
		`{"tool": "create_study_plan", "arguments": {"title": "Linear algebra finals"}}`, answerText))
	if _, _, err := h.send(t, "Create a study plan for my linear algebra finals"); err != nil {
		t.Fatal(err)
	}
	// The recorded proposal, so the timestamp lines up with the message that
	// produced it -- which is how the approval finds the turn to re-read.
	executed := h.store.recordedActions()[0]
	executed.Status = actions.StatusExecuted
	plan := study.Plan{ID: uuid.New(), Title: "Linear algebra finals"}

	h.svc.ActionExecuted(context.Background(), h.user, executed,
		`Create a study plan "Linear algebra finals".`, tools.Result{Plans: []study.Plan{plan}})

	calls := ln.seen()
	last := calls[len(calls)-1]
	if last.anchor == nil || last.anchor.RefTable != "study_plans" ||
		last.anchor.RefID != plan.ID || last.anchor.Label != plan.Title {
		t.Fatalf("anchor = %+v", last.anchor)
	}
	if _, mirrored := graph.TypeForRefTable(last.anchor.RefTable); !mirrored {
		t.Fatalf("the graph does not mirror %q", last.anchor.RefTable)
	}
}

// A result with no record, or an action with no conversation, has nothing to
// anchor and nothing is read.
func TestAnExecutedActionWithNothingToAnchorIsNotRead(t *testing.T) {
	h, _, ln := extractingToolHarness(t, &ai.Mock{Reply: "ok"})
	conv := h.conv.ID
	h.svc.ActionExecuted(context.Background(), h.user, actions.Action{ConversationID: &conv}, "x", tools.Result{})
	h.svc.ActionExecuted(context.Background(), h.user, actions.Action{}, "x", tools.Result{Tasks: []tasks.Task{{ID: uuid.New()}}})
	if n := len(ln.seen()); n != 0 {
		t.Fatalf("linker ran %d times", n)
	}
}
