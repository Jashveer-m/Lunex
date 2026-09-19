package chat

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/study"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// What the orchestrator does with Phase 10a: a study plan reaches the model as
// a source, and the rule that goes with it says what the source does *not*
// carry.

// The deck's contents are the one thing a study-plan source names the
// existence of without showing, which is exactly the shape that invites an
// invention: "24 flashcards" and nothing about any of them.
func TestTheStudyRuleForbidsInventingWhatIsOnACard(t *testing.T) {
	h := newHarness(t, &ai.Mock{})
	if _, _, err := h.send(t, "What is on my linear algebra flashcards?"); err != nil {
		t.Fatal(err)
	}

	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{
		// what the source is, and what it leaves out
		`A source of type "study_plan" is something the user is studying`,
		"The cards themselves are NOT in the context",
		// the thing the model must not write
		"Never state, quote, summarise or guess the content of a flashcard",
		"tell them to open the plan",
		// and where a card's answer came from
		"comes from the user's own document, not from you",
		"do not correct one",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the study rule is missing %q from the prompt:\n%s", want, prompt)
		}
	}
}

// Like the finance rule, it is in every prompt rather than only the ones a
// study tool ran for: "what is on my cards" retrieves nothing when the user
// has no plan by that name, and that is the turn where a model with no rule in
// front of it invents a deck.
func TestTheStudyRuleIsInThePromptWithNothingRetrieved(t *testing.T) {
	h := newHarness(t, &ai.Mock{})
	if _, _, err := h.send(t, "Read me my flashcards."); err != nil {
		t.Fatal(err)
	}
	prompt := ai.PromptText(h.provider.LastPrompt())
	if !strings.Contains(prompt, "The cards themselves are NOT in the context") {
		t.Fatalf("the study rule is missing from a turn with no study tool:\n%s", prompt)
	}
}

// A plan a read tool found becomes a source the model can cite, rendered like
// every other record: what it is, what state it is in, how big the deck is and
// which file it came from.
func TestAStudyPlanBecomesACitableSource(t *testing.T) {
	name := "relay-handbook.txt"
	plan := study.Plan{
		ID: uuid.New(), Title: "Kestrel relay handbook", Status: study.StatusActive,
		CardCount: 24, DocumentName: &name,
	}

	sources := toolSources(tools.SearchStudyPlans, tools.Result{Plans: []study.Plan{plan}})
	if len(sources) != 1 {
		t.Fatalf("%d sources, want 1", len(sources))
	}
	s := sources[0]
	switch {
	case s.Type != SourceStudyPlan:
		t.Fatalf("type = %q, want %q", s.Type, SourceStudyPlan)
	case s.ID != plan.ID:
		t.Fatalf("id = %v, want the plan's", s.ID)
	case s.Title != plan.Title:
		t.Fatalf("title = %q", s.Title)
	case s.Tool != tools.SearchStudyPlans:
		t.Fatalf("tool = %q", s.Tool)
	}
	for _, want := range []string{"status active", "24 flashcards", "built from " + name} {
		if !strings.Contains(s.Excerpt, want) {
			t.Fatalf("the excerpt does not carry %q: %q", want, s.Excerpt)
		}
	}

	// One card is "1 flashcard", not "1 flashcards": a plan with a single card
	// is the state a new plan spends its first minute in.
	one := toolSources(tools.SearchStudyPlans, tools.Result{
		Plans: []study.Plan{{ID: uuid.New(), Title: "x", Status: study.StatusActive, CardCount: 1}},
	})
	if !strings.Contains(one[0].Excerpt, "1 flashcard ") && !strings.HasSuffix(one[0].Excerpt, "1 flashcard") {
		t.Fatalf("excerpt = %q, want a singular noun", one[0].Excerpt)
	}
}

// The flashcards themselves are never a source. A deck is hundreds of one-line
// answers, and putting it in front of the model would spend the whole context
// budget restating a document it can retrieve properly.
func TestFlashcardsAreNotSources(t *testing.T) {
	result := tools.Result{Plans: []study.Plan{{ID: uuid.New(), Title: "Deck", CardCount: 200}}}
	if got := result.Count(); got != 1 {
		t.Fatalf("a plan with 200 cards counts as %d records, want 1", got)
	}
	for _, s := range toolSources(tools.GenerateFlashcards, result) {
		if s.Type != SourceStudyPlan {
			t.Fatalf("an unexpected source type reached the model: %q", s.Type)
		}
	}
}
