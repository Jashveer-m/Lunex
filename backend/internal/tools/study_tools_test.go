package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/study"
)

// --- search_study_plans --------------------------------------------------------

func TestSearchStudyPlansFindsAndRunsWithoutApproval(t *testing.T) {
	w := newWorld()
	w.study.seedPlan(w.user, "Linear algebra finals")
	w.study.seedPlan(w.user, "Operating systems")

	call, err := w.reg.Prepare(context.Background(), w.user, SearchStudyPlans, Args{"query": "algebra"})
	if err != nil {
		t.Fatal(err)
	}
	if call.Permission != Read {
		t.Fatalf("permission = %q", call.Permission)
	}
	result, err := w.reg.RunRead(context.Background(), w.user, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plans) != 1 || result.Plans[0].Title != "Linear algebra finals" {
		t.Fatalf("found %+v", result.Plans)
	}
	if w.totalWrites() != 0 {
		t.Fatal("a search wrote something")
	}
}

// The status is a filter, so a value the message does not give is the router's
// to drop -- and a value this tool cannot place is dropped here rather than
// making the search come back empty.
func TestAnUnplaceableStatusIsDroppedRatherThanSearchedFor(t *testing.T) {
	w := newWorld()
	w.study.seedPlan(w.user, "Linear algebra finals")

	call, err := w.reg.Prepare(context.Background(), w.user, SearchStudyPlans, Args{"status": "studying"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(call.Input), "studying") {
		t.Fatalf("input = %s, want the unplaceable status dropped", call.Input)
	}
	result, err := w.reg.RunRead(context.Background(), w.user, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plans) != 1 {
		t.Fatalf("the search came back with %d plans", len(result.Plans))
	}
	// And a synonym the user would say does place.
	call, err = w.reg.Prepare(context.Background(), w.user, SearchStudyPlans, Args{"status": "finished"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(call.Input), study.StatusCompleted) {
		t.Fatalf("input = %s, want %q", call.Input, study.StatusCompleted)
	}
}

// --- create_study_plan ----------------------------------------------------------

func TestCreateStudyPlanIsProposedNotCreated(t *testing.T) {
	w := newWorld()

	call, err := w.reg.Prepare(context.Background(), w.user, CreateStudyPlan, Args{
		"title": "Linear algebra finals", "description": "Eigenvalues and SVD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Permission != Write {
		t.Fatalf("permission = %q", call.Permission)
	}
	if w.totalWrites() != 0 {
		t.Fatal("preparing a write created something")
	}
	if _, err := w.reg.RunRead(context.Background(), w.user, call); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("err = %v, want ErrApprovalRequired", err)
	}

	// Only an approval runs it.
	id := w.ledger.add(w.user, call, "proposed")
	exec, err := w.reg.RunApproved(context.Background(), w.user, id)
	if err != nil || exec.Err != nil {
		t.Fatalf("approving: %v / %v", err, exec.Err)
	}
	if len(w.study.creates) != 1 || w.study.creates[0].Title != "Linear algebra finals" {
		t.Fatalf("created %+v", w.study.creates)
	}
}

func TestCreateStudyPlanNamesItsDocumentByFilename(t *testing.T) {
	w := newWorld()

	call, err := w.reg.Prepare(context.Background(), w.user, CreateStudyPlan, Args{
		"title": "Field notes revision", "document": "field-notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The summary a user approves says the filename, not a uuid.
	if !strings.Contains(call.Summary, "field-notes.txt") {
		t.Fatalf("summary = %q", call.Summary)
	}
	// And the id is what runs.
	var in struct {
		DocumentID string `json:"document_id"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		t.Fatal(err)
	}
	if in.DocumentID == "" {
		t.Fatalf("input = %s, want a resolved document id", call.Input)
	}
}

func TestADocumentTheUserDoesNotHaveIsAnArgumentError(t *testing.T) {
	w := newWorld()
	_, err := w.reg.Prepare(context.Background(), w.user, CreateStudyPlan, Args{
		"title": "Revision", "document": "lecture-3.pdf",
	})
	if !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("err = %v, want an argument error", err)
	}
	if !strings.Contains(err.Error(), "lecture-3.pdf") {
		t.Fatalf("err = %v, want it to name what was asked for", err)
	}
}

// --- generate_flashcards ------------------------------------------------------

// The proposal is the cards. That is the whole approval story for this tool:
// nothing is written, and what the user reads is the actual questions and
// answers rather than "create 8 flashcards".
func TestGenerateFlashcardsProposesTheCardsThemselves(t *testing.T) {
	w := newWorld()

	call, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
		"document": "field-notes.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Permission != Write {
		t.Fatalf("permission = %q", call.Permission)
	}
	if w.totalWrites() != 0 {
		t.Fatalf("preparing wrote %d times", w.totalWrites())
	}
	// Generation happened once, during preparation.
	if len(w.study.proposals) != 1 {
		t.Fatalf("%d generations while preparing, want 1", len(w.study.proposals))
	}
	// The cards are in the stored input.
	var in struct {
		Cards []struct{ Front, Back string } `json:"cards"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Cards) != 2 {
		t.Fatalf("input carries %d cards: %s", len(in.Cards), call.Input)
	}
	// And in the summary, in full.
	for _, want := range []string{"About forty minutes.", "A new fuel filter.", "field-notes.txt"} {
		if !strings.Contains(call.Summary, want) {
			t.Fatalf("summary does not show %q:\n%s", want, call.Summary)
		}
	}
}

// Approving writes exactly the cards that were shown, and does not generate
// again -- which would store sentences nobody had read.
func TestApprovingFlashcardsWritesWhatWasShownAndDoesNotRegenerate(t *testing.T) {
	w := newWorld()
	call, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
		"document": "field-notes.txt",
	})
	if err != nil {
		t.Fatal(err)
	}

	id := w.ledger.add(w.user, call, "proposed")
	exec, err := w.reg.RunApproved(context.Background(), w.user, id)
	if err != nil || exec.Err != nil {
		t.Fatalf("approving: %v / %v", err, exec.Err)
	}
	if len(w.study.proposals) != 1 {
		t.Fatalf("approving generated again: %d generations", len(w.study.proposals))
	}
	if len(w.study.cards) != 1 || len(w.study.cards[0]) != 2 {
		t.Fatalf("wrote %+v", w.study.cards)
	}
	for _, c := range w.study.cards[0] {
		if c.DocumentID == nil {
			t.Fatalf("a generated card does not record its source: %+v", c)
		}
	}
	if !strings.Contains(string(mustJSON(t, exec.Result.Output)), "forty minutes") {
		t.Fatalf("result = %s", mustJSON(t, exec.Result.Output))
	}
}

func TestGeneratedCardsAreFiledUnderANamedPlan(t *testing.T) {
	w := newWorld()
	plan := w.study.seedPlan(w.user, "Field notes revision")

	call, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
		"document": "field-notes.txt", "study_plan": "field notes revision",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(call.Summary, "Field notes revision") {
		t.Fatalf("summary = %q", call.Summary)
	}
	id := w.ledger.add(w.user, call, "proposed")
	if _, err := w.reg.RunApproved(context.Background(), w.user, id); err != nil {
		t.Fatal(err)
	}
	for _, c := range w.study.cards[0] {
		if c.StudyPlanID == nil || *c.StudyPlanID != plan.ID {
			t.Fatalf("card filed under %v, want %v", c.StudyPlanID, plan.ID)
		}
	}
}

func TestAPlanTheUserDoesNotHaveIsAnArgumentError(t *testing.T) {
	w := newWorld()
	_, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
		"document": "field-notes.txt", "study_plan": "thermodynamics",
	})
	if !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("err = %v, want an argument error", err)
	}
	if w.totalWrites() != 0 {
		t.Fatal("a refused call wrote something")
	}
}

// Guessing which file to build a deck from is the user's decision: the cards
// from the wrong lecture will look perfectly well made.
func TestAnAmbiguousDocumentIsRefusedWithTheCandidates(t *testing.T) {
	w := newWorld()
	w.study.seedDocument(w.user, "lecture-3-notes.txt")

	_, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{"document": "notes"})
	if !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("err = %v, want an argument error", err)
	}
	for _, want := range []string{"field-notes.txt", "lecture-3-notes.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to name %s", err, want)
		}
	}
}

// A generation the service refused -- because nothing the model wrote was in
// the document -- is an argument error the assistant can explain, not a
// proposal of cards that are not about the document.
func TestAGroundingFailureBecomesSomethingTheAssistantCanSay(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"nothing grounded":      {study.ErrGeneration, "does not have enough"},
		"not processed yet":     {study.ErrNotStudyable, "finished processing"},
		"topic not in the file": {study.ErrNoPassages, "which part"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.study.proposeErr = tc.err

			_, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
				"document": "field-notes.txt", "topic": "photosynthesis",
			})
			if !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("err = %v, want an argument error", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if w.totalWrites() != 0 {
				t.Fatal("a refused generation wrote something")
			}
		})
	}
}

func TestGenerateFlashcardsNeedsADocument(t *testing.T) {
	w := newWorld()
	_, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{"topic": "the aurora"})
	if !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("err = %v, want an argument error", err)
	}
	if len(w.study.proposals) != 0 {
		t.Fatal("the model was called with no document named")
	}
}

func TestTheCountIsPassedThroughToGeneration(t *testing.T) {
	w := newWorld()
	// float64 rather than int: arguments arrive from JSON, and that is what a
	// number decodes to.
	if _, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
		"document": "field-notes.txt", "count": float64(1),
	}); err != nil {
		t.Fatal(err)
	}
	if len(w.study.proposals) != 1 || w.study.proposals[0].Count != 1 {
		t.Fatalf("generations = %+v", w.study.proposals)
	}
	// Something unreadable is "they did not say", not a refusal: there is no
	// sense in losing a whole generation because a model wrote "a few".
	w = newWorld()
	if _, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
		"document": "field-notes.txt", "count": "a few",
	}); err != nil {
		t.Fatal(err)
	}
	if w.study.proposals[0].Count != 0 {
		t.Fatalf("count = %d, want it left to the service's default", w.study.proposals[0].Count)
	}
}

// The derived `cards` key is declared, so the canonical input is still exactly
// the declaration -- and it is not offered to the model, so nothing invites a
// model to write cards in place of the ones the document produced.
func TestTheCardsKeyIsDeclaredButNotOffered(t *testing.T) {
	tool, ok := newWorld().reg.Tool(GenerateFlashcards)
	if !ok {
		t.Fatal("generate_flashcards is not registered")
	}
	var cards *Param
	for i, p := range tool.Params {
		if p.Name == "cards" {
			cards = &tool.Params[i]
		}
	}
	if cards == nil {
		t.Fatal("cards is not a declared param, so the stored input carries an undeclared key")
	}
	if !cards.Derived || cards.Offered() {
		t.Fatalf("cards = %+v, want it derived and not offered", *cards)
	}
	if _, offered := tool.InputSchema().Properties["cards"]; offered {
		t.Fatal("the input schema offers the derived key to the model")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Nothing in this module reaches another user's plans, documents or cards.
func TestStudyToolsCannotReachAnotherUsersData(t *testing.T) {
	w := newWorld()
	stranger := uuid.New()
	their := w.study.seedPlan(stranger, "Their revision")
	theirDoc := w.study.seedDocument(stranger, "their-notes.txt")

	if _, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
		"document": theirDoc.ID.String(),
	}); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("err = %v, want another user's document to be unresolvable", err)
	}
	if _, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
		"document": "field-notes.txt", "study_plan": their.ID.String(),
	}); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("err = %v, want another user's plan to be unresolvable", err)
	}
	call, err := w.reg.Prepare(context.Background(), w.user, SearchStudyPlans, Args{"query": "revision"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := w.reg.RunRead(context.Background(), w.user, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plans) != 0 {
		t.Fatalf("the search found %+v", result.Plans)
	}
}

// A card's answer usually ends in a full stop already, and the registry's
// summaries all end in one -- so the summary must not read "58.4 volts..".
func TestTheFlashcardSummaryEndsInOneFullStop(t *testing.T) {
	w := newWorld()
	w.study.propose = []study.NewCard{{Front: "What is the voltage?", Back: "58.4 volts."}}

	call, err := w.reg.Prepare(context.Background(), w.user, GenerateFlashcards, Args{
		"document": "field-notes.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(call.Summary, "..") {
		t.Fatalf("summary ends in two stops: %q", call.Summary)
	}
	if !strings.HasSuffix(call.Summary, ".") {
		t.Fatalf("summary does not end in a stop: %q", call.Summary)
	}
}
