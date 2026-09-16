package graph

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The failure behind Turn, reproduced: the user asks for a task, the model
// names the task's subject as an entity, and the task does not exist yet.
const (
	passportQuestion = "Add a task to renew my passport before the trip to Lisbon, I keep putting it off and it expires soon."
	passportProposal = "I have prepared a task to renew your passport. It will only be created once you approve it."
)

// Reading the turn as it happened, a relationship naming the proposed change
// is not stored -- no extracted "passport renewal" node is invented for a task
// that does not exist -- while one about something else in the same message is.
func TestARelationshipAboutAnUnconfirmedChangeIsNotStored(t *testing.T) {
	h := newHarness(t, triples(
		triple("the user", "person", "WORKS_ON", "passport renewal", "project", 0.9),
		triple("the user", "person", "INTERESTED_IN", "Lisbon", "project", 0.9),
	))
	edges, err := h.svc.Extract(context.Background(), h.user, h.conv, Turn{
		UserMessage: passportQuestion, AssistantMessage: passportProposal,
		Unconfirmed: []string{"Renew my passport"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("stored %d edges, want only the one not about the proposal", len(edges))
	}
	for _, l := range h.store.labels(h.user) {
		if l == "passport renewal" {
			t.Fatalf("a node was invented for an unapproved task: %v", h.store.labels(h.user))
		}
	}
}

// Reading it again once the change was approved and made, the relationship
// lands on the task's own node -- the one sync created -- and no parallel
// extracted node appears. Relationships that do not touch the task were read
// the first time and are not read again.
func TestAnApprovedChangeAnchorsTheRelationshipToItsRecord(t *testing.T) {
	h := newHarness(t, triples(
		triple("the user", "person", "WORKS_ON", "passport renewal", "project", 0.9),
		triple("the user", "person", "INTERESTED_IN", "Lisbon", "project", 0.9),
	))
	ctx := context.Background()
	taskID := uuid.New()
	h.svc.SyncNode(ctx, h.user, "tasks", taskID, "Renew my passport")

	edges, err := h.svc.Extract(ctx, h.user, h.conv, Turn{
		UserMessage:      passportQuestion,
		AssistantMessage: `The user approved this change and it has been made: Create a task "Renew my passport".`,
		Anchor:           &Anchor{RefTable: "tasks", RefID: taskID, Label: "Renew my passport"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("stored %d edges, want the one touching the task", len(edges))
	}
	task, err := h.store.NodeByID(ctx, h.user, edges[0].ToNodeID)
	if err != nil {
		t.Fatal(err)
	}
	if task.RefID == nil || *task.RefID != taskID || task.Type != NodeTask {
		t.Fatalf("the edge points at %+v, want the task's own node", task)
	}
	for _, l := range h.store.labels(h.user) {
		if l == "passport renewal" || l == "Lisbon" {
			t.Fatalf("anchored extraction created node %q: %v", l, h.store.labels(h.user))
		}
	}
}

// An anchor whose node was never synced is ensured rather than lost: the
// relationship still reaches the real record.
func TestAnAnchorRepairsAMissingSync(t *testing.T) {
	h := newHarness(t, triples(triple("the user", "person", "WORKS_ON", "passport renewal", "project", 0.9)))
	taskID := uuid.New()
	edges, err := h.svc.Extract(context.Background(), h.user, h.conv, Turn{
		UserMessage: passportQuestion, AssistantMessage: "done: Renew my passport",
		Anchor: &Anchor{RefTable: "tasks", RefID: taskID, Label: "Renew my passport"},
	})
	if err != nil || len(edges) != 1 {
		t.Fatalf("Extract = %d edges, %v", len(edges), err)
	}
	n, _ := h.store.NodeByID(context.Background(), h.user, edges[0].ToNodeID)
	if n.RefID == nil || *n.RefID != taskID {
		t.Fatalf("the edge points at %+v", n)
	}
}

// A relationship written backwards is turned round when its ends' types say
// so, and left alone when they do not.
func TestParseExtractionOrientsBackwardsRelationships(t *testing.T) {
	for name, tc := range map[string]struct {
		reply    string
		from, to string
	}{
		"a skill requiring a project": {
			`[{"from":"TypeScript","from_type":"skill","relationship":"REQUIRES","to":"the recipe app","to_type":"project","confidence":0.9}]`,
			"the recipe app", "TypeScript"},
		"a skill studying the user": {
			`[{"from":"Go","from_type":"skill","relationship":"STUDIES","to":"the user","to_type":"person","confidence":0.9}]`,
			"the user", "Go"},
		"a person as the goal": {
			`[{"from":"the user","from_type":"person","relationship":"GOAL_OF","to":"run a marathon","to_type":"project","confidence":0.9}]`,
			"run a marathon", "the user"},
		"already the right way round": {
			`[{"from":"the recipe app","from_type":"project","relationship":"REQUIRES","to":"TypeScript","to_type":"skill","confidence":0.9}]`,
			"the recipe app", "TypeScript"},
		"no types to judge by": {
			`[{"from":"TypeScript","relationship":"REQUIRES","to":"the recipe app","confidence":0.9}]`,
			"TypeScript", "the recipe app"},
		"undirected": {
			`[{"from":"Go","from_type":"skill","relationship":"RELATED_TO","to":"the user","to_type":"person","confidence":0.9}]`,
			"Go", "the user"},
	} {
		t.Run(name, func(t *testing.T) {
			got := ParseExtraction(tc.reply)
			if len(got) != 1 || got[0].From != tc.from || got[0].To != tc.to {
				t.Fatalf("ParseExtraction = %+v, want %s -> %s", got, tc.from, tc.to)
			}
		})
	}
}
